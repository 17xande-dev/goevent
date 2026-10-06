package middleware

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// limited wraps a counting handler in a limiter, and reports how many requests got
// through. The counter is atomic because the concurrency test drives it from twenty
// goroutines at once — the limiter is safe for that, and the test's own bookkeeping
// has to be too.
func limited(cfg RateLimitConfig, source ClientIPSource) (http.Handler, *atomic.Int64) {
	var served atomic.Int64
	h := RateLimit(cfg, source, discard())(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		served.Add(1)
	}))
	return h, &served
}

func requestFrom(ip string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/admin/login", nil)
	r.RemoteAddr = ip + ":54321"
	return r
}

func TestRateLimit_AllowsTheBurstThenRefuses(t *testing.T) {
	// One request a second, five saved up.
	h, served := limited(RateLimitConfig{Name: "test", Every: time.Second, Burst: 5}, ClientIPRemote)

	codes := make([]int, 0, 8)
	for range 8 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, requestFrom("203.0.113.7"))
		codes = append(codes, w.Code)
	}

	if got := served.Load(); got != 5 {
		t.Errorf("%d requests reached the handler, want the burst of 5", got)
	}
	for i, code := range codes {
		want := http.StatusOK
		if i >= 5 {
			want = http.StatusTooManyRequests
		}
		if code != want {
			t.Errorf("request %d = %d, want %d", i, code, want)
		}
	}
}

func TestRateLimit_TellsTheClientWhenToReturn(t *testing.T) {
	// Retry-After matters more than usual here: the payment gateway reads it, and a
	// throttled notification that is never retried is a lost payment.
	h, _ := limited(RateLimitConfig{Name: "test", Every: 2 * time.Second, Burst: 1}, ClientIPRemote)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, requestFrom("203.0.113.7"))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, requestFrom("203.0.113.7"))

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429", w.Code)
	}
	after := w.Header().Get("Retry-After")
	if after == "" {
		t.Fatal("no Retry-After on a 429, so a gateway has to guess when to retry")
	}
	if n, err := strconv.Atoi(after); err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want a positive number of seconds", after)
	}
}

func TestRateLimit_IsPerClient(t *testing.T) {
	// One shopper hitting a limit must not stop everybody else buying things.
	h, served := limited(RateLimitConfig{Name: "test", Every: time.Hour, Burst: 1}, ClientIPRemote)

	for _, ip := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, requestFrom(ip))
		if w.Code != http.StatusOK {
			t.Errorf("first request from %s = %d, want 200", ip, w.Code)
		}
	}
	if got := served.Load(); got != 3 {
		t.Errorf("%d requests served, want one per client", got)
	}

	// And the second from any one of them is refused.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, requestFrom("203.0.113.1"))
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("second request from the same client = %d, want 429", w.Code)
	}
}

func TestRateLimit_RecoversOverTime(t *testing.T) {
	// The bucket refills, so a client that waits is served again — otherwise a
	// limiter is a ban.
	h, _ := limited(RateLimitConfig{Name: "test", Every: 20 * time.Millisecond, Burst: 1}, ClientIPRemote)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, requestFrom("203.0.113.7"))
	if w.Code != http.StatusOK {
		t.Fatalf("first request = %d", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, requestFrom("203.0.113.7"))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("immediate second request = %d, want 429", w.Code)
	}

	time.Sleep(40 * time.Millisecond)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, requestFrom("203.0.113.7"))
	if w.Code != http.StatusOK {
		t.Errorf("request after waiting = %d, want 200 — the bucket did not refill", w.Code)
	}
}

func TestRateLimit_KeyedByForwardedIPOnlyWhenTrusted(t *testing.T) {
	// With remote, a client cannot escape its bucket by claiming an address:
	// every request below shares one limiter because RemoteAddr is the same.
	h, _ := limited(RateLimitConfig{Name: "test", Every: time.Hour, Burst: 1}, ClientIPRemote)

	first := requestFrom("203.0.113.7")
	first.Header.Set("X-Forwarded-For", "198.51.100.1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, first)
	if w.Code != http.StatusOK {
		t.Fatalf("first request = %d", w.Code)
	}

	second := requestFrom("203.0.113.7")
	second.Header.Set("X-Forwarded-For", "198.51.100.2") // a different claim
	w = httptest.NewRecorder()
	h.ServeHTTP(w, second)
	if w.Code != http.StatusTooManyRequests {
		t.Error("a client escaped its bucket by changing X-Forwarded-For with CLIENT_IP_SOURCE=remote")
	}

	// With forwarded, the header is the key — which is correct behind a proxy that
	// replaces the header, and a limiter that limits nothing behind one that does
	// not. That trade is documented on ClientIP.
	h, _ = limited(RateLimitConfig{Name: "test", Every: time.Hour, Burst: 1}, ClientIPForwarded)
	for _, claimed := range []string{"198.51.100.1", "198.51.100.2"} {
		r := requestFrom("203.0.113.7")
		r.Header.Set("X-Forwarded-For", claimed)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("trusted proxy, client %s = %d, want 200", claimed, w.Code)
		}
	}
}

func TestRateLimit_EvictsIdleBuckets(t *testing.T) {
	// The map must not grow forever: one bucket per address ever seen would be a
	// memory leak an attacker controls the size of.
	l := &ipLimiter{
		limit:    rate.Every(time.Hour),
		burst:    1,
		ttl:      50 * time.Millisecond,
		buckets:  map[string]*bucket{},
		lastKeep: time.Now(),
	}

	now := time.Now()
	for i := range 100 {
		l.allow("203.0.113."+strconv.Itoa(i), now)
	}
	if got := l.size(); got != 100 {
		t.Fatalf("%d buckets, want 100", got)
	}

	// A request after the TTL sweeps everything idle, leaving only the new arrival.
	l.allow("198.51.100.1", now.Add(time.Second))
	if got := l.size(); got != 1 {
		t.Errorf("%d buckets after eviction, want 1", got)
	}

	// A full bucket is indistinguishable from no bucket, so a client whose bucket
	// was forgotten is not thereby given extra allowance beyond its burst.
	w := 0
	for range 3 {
		if l.allow("198.51.100.1", now.Add(time.Second)) {
			w++
		}
	}
	if w != 0 {
		t.Errorf("%d extra requests allowed straight after eviction", w)
	}
}

func TestRateLimit_ConcurrentClients(t *testing.T) {
	// The map is shared, so this is worth racing rather than assuming. Run with
	// -race, which CI does.
	h, _ := limited(RateLimitConfig{Name: "test", Every: time.Millisecond, Burst: 50}, ClientIPRemote)

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, requestFrom("203.0.113."+strconv.Itoa(i)))
			}
		}()
	}
	wg.Wait()
}

func TestRateLimit_ZeroBurstStillAllowsSomething(t *testing.T) {
	// A misconfigured burst of zero would otherwise refuse every request forever,
	// turning a limit into an outage.
	h, served := limited(RateLimitConfig{Name: "test", Every: time.Hour, Burst: 0}, ClientIPRemote)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, requestFrom("203.0.113.7"))
	if w.Code != http.StatusOK || served.Load() != 1 {
		t.Errorf("a zero burst refused the first request: %d, served %d", w.Code, served.Load())
	}
}
