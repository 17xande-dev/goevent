package middleware

import (
	"log/slog"
	"net/http"
	"net/netip"
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
//   - **Registration.** Registration-row spam. Refusing is a nuisance to a real
//     registrant, so
//     the limit is loose enough that nobody hits it by clicking twice.
//   - **The payment callback.** Unauthenticated, and every request makes the server
//     POST to the gateway to validate it — an amplifier. This one is the reason a
//     limit exists at all.
//
// The callback deserves its own note. The webhook handler answers 200 to
// everything, deliberately, so a gateway does not retry a notification that was
// forged. A *throttled* request is the opposite case: it has not been looked at,
// and it must be retried. So the limiter answers 429 with Retry-After, and because
// it sits in front of the handler, the gateway retries as it should. "Always 200"
// applies to notifications the server has actually read.

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
	// because rendering the site's error page is the handler package's job and
	// this package cannot import it — and because the payment gateway, which is
	// also throttled, wants a bare answer rather than a page.
	//
	// Retry-After is already set when it runs. Nil falls back to plain text, so a
	// limiter is still correct on its own.
	Exceeded http.Handler
}

// limiterTTL is the default idle lifetime of a bucket.
const limiterTTL = 10 * time.Minute

// maxBuckets is the most buckets one limiter holds. Keying by /64 stops a single
// prefix minting buckets, but an attacker can still cycle through prefixes (or
// IPv4 addresses), and the lazy sweep only runs once per TTL. The cap is what
// makes the map's size a bound rather than a hope.
const maxBuckets = 10_000

// capSweepEvery is the least time between sweeps triggered by hitting the cap.
// A sweep scans every bucket under the limiter's lock, and at the cap every
// refused newcomer would otherwise run one: an attacker's request rate would
// become the lock's busy time, and existing clients (the payment callback among
// them) would queue behind it. Spaced out, the scan is a rare cost, while the cap
// still heals within roughly the TTL plus this interval of buckets expiring.
const capSweepEvery = 5 * time.Second

// limiterKey is the identity a client is throttled under.
//
// IPv4 is keyed by the full address. IPv6 is keyed by its /64, because that is
// the smallest block an ISP hands a single subscriber: a household or an
// attacker with one prefix holds 2^64 addresses, and keying on the full address
// gives each of them a fresh bucket — a limiter anyone with IPv6 can walk
// around. An IPv4-mapped address (::ffff:a.b.c.d) is unmapped first so it
// shares its bucket with the plain IPv4 form. Anything unparseable keeps its
// raw string, as before.
func limiterKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	// Drop any zone: fe80::1%eth0 and fe80::1%eth1 are one client to us.
	return netip.PrefixFrom(addr.WithZone(""), 64).Masked().String()
}

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
		max:      maxBuckets,
		name:     cfg.Name,
		log:      log,
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

	// max caps len(buckets); zero means maxBuckets. A field rather than only the
	// constant so a test can lower it instead of creating ten thousand entries.
	max int
	// capLogged is when the cap last logged, so a flood is one line per TTL
	// window rather than one per request.
	capLogged time.Time
	// capSwept is when a cap-triggered sweep last ran; see capSweepEvery.
	capSwept time.Time
	name     string
	log      *slog.Logger
}

type bucket struct {
	limiter *rate.Limiter
	seen    time.Time
}

func (l *ipLimiter) allow(ip string, now time.Time) bool {
	ip = limiterKey(ip)

	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastKeep) >= l.ttl {
		l.evict(now)
		l.lastKeep = now
	}

	b, ok := l.buckets[ip]
	if !ok {
		limit := l.max
		if limit <= 0 {
			limit = maxBuckets
		}
		if len(l.buckets) >= limit {
			// Make room the cheap way first: idle buckets cost nothing to forget.
			// At most once per capSweepEvery, so a flood of newcomers is refused
			// without rescanning the map on every request.
			if l.capSwept.IsZero() || now.Sub(l.capSwept) >= capSweepEvery {
				l.evict(now)
				l.lastKeep = now
				l.capSwept = now
			}
		}
		if len(l.buckets) >= limit {
			// Still full of live clients. Refuse the newcomer: failing open would
			// let a flood of fresh addresses switch the limiter off, and existing
			// clients, who are already in the map, are unaffected.
			if l.log != nil && (l.capLogged.IsZero() || now.Sub(l.capLogged) >= l.ttl) {
				l.capLogged = now
				l.log.Warn("rate limiter full; refusing new clients", "limiter", l.name, "buckets", len(l.buckets))
			}
			return false
		}
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
