package middleware

import (
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Per-IP rate limiting.
//
// The split here is deliberate: the token bucket comes from x/time/rate, because
// that is the part with the clock edge cases and the off-by-ones already found in
// it, while the keyed map, the eviction and the choice of key stay in this file
// where they can be read. A bucket is simple arithmetic; a bucket per client, with
// bounded memory, is where the actual decisions are.
//
// # What this protects, and how it answers
//
// Three surfaces, with different limits and — importantly — different meanings for
// a refusal:
//
//   - **Admin login.** Brute force. The argon2id verification already costs real
//     time, but cost is not a limit.
//   - **Checkout.** Order-row spam. Refusing is a nuisance to a real shopper, so
//     the limit is loose enough that nobody hits it by clicking twice.
//   - **The payment callback.** Unauthenticated, and every request makes the store
//     POST to the gateway to validate it — an amplifier. This one is the reason a
//     limit exists at all.
//
// The callback deserves its own note. The webhook handler answers 200 to
// everything, deliberately, so a gateway does not retry a notification that was
// forged. A *throttled* request is the opposite case: it has not been looked at,
// and it must be retried. So the limiter answers 429 with Retry-After, and because
// it sits in front of the handler, the gateway retries as it should. "Always 200"
// applies to notifications the store has actually read.

// RateLimitConfig is one limiter's settings.
type RateLimitConfig struct {
	// Name appears in the log line for a refusal, so a rejection can be traced to
	// the surface it happened on.
	Name string

	// Every is the interval at which one request's worth of allowance accrues, and
	// Burst is how much may be saved up. Together they are the whole limit: Every
	// = time.Second/3 with Burst = 10 permits ten immediately, then three a
	// second.
	Every time.Duration
	Burst int

	// TTL is how long an idle client's bucket is kept before eviction. Too short
	// and a client's allowance resets by waiting; long enough to outlast the
	// window is what matters.
	TTL time.Duration

	// Exceeded answers a throttled request. It is supplied rather than built here
	// because rendering the store's error page is the handler package's job and
	// this package cannot import it — and because the payment gateway, which is
	// also throttled, wants a bare answer rather than a page.
	//
	// Retry-After is already set when it runs. Nil falls back to plain text, so a
	// limiter is still correct on its own.
	Exceeded http.Handler
}

// limiterTTL is the default idle lifetime of a bucket.
const limiterTTL = 10 * time.Minute

// RateLimit limits requests per client IP.
//
// source decides where the client's address comes from; see ClientIP. It matters
// more here than anywhere else in the server: set to a header nothing in front is
// actually replacing, every client can invent an address and have a bucket to
// itself, which is a limiter that limits nothing.
func RateLimit(cfg RateLimitConfig, source ClientIPSource, log *slog.Logger) Middleware {
	if cfg.TTL <= 0 {
		cfg.TTL = limiterTTL
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 1
	}
	if log == nil {
		log = slog.Default()
	}

	l := &ipLimiter{
		limit:    rate.Every(cfg.Every),
		burst:    cfg.Burst,
		ttl:      cfg.TTL,
		buckets:  make(map[string]*bucket),
		lastKeep: time.Now(),
	}
	retryAfter := strconv.Itoa(max(1, int(cfg.Every.Seconds())))

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ClientIP(r, source)
			if l.allow(ip, time.Now()) {
				next.ServeHTTP(w, r)
				return
			}

			log.Warn("rate limited", "limiter", cfg.Name, "client_ip", ip,
				"method", r.Method, "path", r.URL.Path,
				"request_id", RequestIDFrom(r.Context()))
			// Retry-After tells a well-behaved client — and a payment gateway — when
			// to come back, rather than leaving it to guess or give up. Set before
			// Exceeded runs, so whatever answers carries it.
			w.Header().Set("Retry-After", retryAfter)
			if cfg.Exceeded != nil {
				cfg.Exceeded.ServeHTTP(w, r)
				return
			}
			http.Error(w, "too many requests; slow down and try again", http.StatusTooManyRequests)
		})
	}
}

// ipLimiter is a bucket per client, with the map bounded by eviction.
type ipLimiter struct {
	limit rate.Limit
	burst int
	ttl   time.Duration

	mu      sync.Mutex
	buckets map[string]*bucket
	// lastKeep is when eviction last ran. The sweep is lazy — done on the way
	// through an ordinary request — rather than on a ticker, so there is no
	// goroutine to start, own, and shut down for a map that is usually tiny.
	lastKeep time.Time
}

type bucket struct {
	limiter *rate.Limiter
	seen    time.Time
}

func (l *ipLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastKeep) >= l.ttl {
		l.evict(now)
		l.lastKeep = now
	}

	b, ok := l.buckets[ip]
	if !ok {
		b = &bucket{limiter: rate.NewLimiter(l.limit, l.burst)}
		l.buckets[ip] = b
	}
	b.seen = now
	return b.limiter.Allow()
}

// evict drops buckets nobody has used for a TTL. A full bucket is indistinguishable
// from no bucket, so forgetting an idle client costs nothing.
func (l *ipLimiter) evict(now time.Time) {
	for ip, b := range l.buckets {
		if now.Sub(b.seen) >= l.ttl {
			delete(l.buckets, ip)
		}
	}
}

// size reports how many buckets are held. Tests use it to check eviction actually
// happens; nothing else needs it.
func (l *ipLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
