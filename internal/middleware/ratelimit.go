package middleware

import (
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/17xande-dev/ratelimit"
	"github.com/17xande-dev/ratelimit/clientip"
	"golang.org/x/time/rate"
)

// Per-IP rate limiting.
//
// The keyed, bounded token buckets and the client-address policy live in the
// shared github.com/17xande-dev/ratelimit module (and its clientip package). This
// file is goevent's wiring of them: the per-surface settings, the refusal log
// line, Retry-After, and the page or bare answer a refusal gets.
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

// RateLimit limits requests per client IP.
//
// source decides where the client's address comes from; see clientip.Source. It
// matters more here than anywhere else in the server: set to a header nothing in
// front is actually replacing, every client can invent an address and have a
// bucket to itself, which is a limiter that limits nothing.
//
// A full table (10,000 clients) refuses newcomers rather than admitting them, and
// says so once per TTL window.
func RateLimit(cfg RateLimitConfig, source clientip.Source, log *slog.Logger) Middleware {
	if cfg.TTL <= 0 {
		cfg.TTL = limiterTTL
	}
	if cfg.Burst <= 0 {
		cfg.Burst = 1
	}
	if cfg.Every < time.Nanosecond {
		cfg.Every = time.Nanosecond // a finite rate; zero would mean unlimited
	}
	if log == nil {
		log = slog.Default()
	}

	// capLogged is when the full-table line last logged (Unix nanoseconds), so a
	// flood is one line per TTL window rather than one per refused request.
	var capLogged atomic.Int64
	var l *ratelimit.Limiter
	l, err := ratelimit.New(ratelimit.Config{
		Rate:    rate.Every(cfg.Every),
		Burst:   cfg.Burst,
		IdleTTL: cfg.TTL,
		OnFull: func() {
			now := time.Now().UnixNano()
			last := capLogged.Load()
			if last != 0 && now-last < int64(cfg.TTL) {
				return
			}
			if capLogged.CompareAndSwap(last, now) {
				log.Warn("rate limiter full; refusing new clients", "limiter", cfg.Name, "buckets", l.Len())
			}
		},
	})
	if err != nil {
		// Unreachable: the rate and burst were made valid above.
		panic("middleware: rate limiter: " + err.Error())
	}

	return l.Middleware(source, ratelimit.HTTPOptions{
		// Retry-After tells a well-behaved client, and a payment gateway, when
		// to come back rather than leaving it to guess or give up. The module
		// sets it before Exceeded runs, so whatever answers carries it.
		RetryAfter: time.Duration(max(1, int(cfg.Every.Seconds()))) * time.Second,
		Exceeded: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.Warn("rate limited", "limiter", cfg.Name, "client_ip", source.Addr(r).String(),
				"method", r.Method, "path", r.URL.Path,
				"request_id", RequestIDFrom(r.Context()))
			if cfg.Exceeded != nil {
				cfg.Exceeded.ServeHTTP(w, r)
				return
			}
			http.Error(w, "too many requests; slow down and try again", http.StatusTooManyRequests)
		}),
	})
}
