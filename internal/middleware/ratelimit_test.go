package middleware

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/17xande-dev/ratelimit/clientip"
)

// forwarded is what CLIENT_IP_SOURCE=forwarded parses to.
var forwarded = mustSource("forwarded")

func mustSource(v string) clientip.Source {
	s, err := clientip.ParseSource(v)
	if err != nil {
		panic(err)
	}
	return s
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// limited wraps a counting handler in a limiter, and reports how many requests got
// through. The counter is atomic because the concurrency test drives it from twenty
// goroutines at once — the limiter is safe for that, and the test's own bookkeeping
// has to be too.
func limited(cfg RateLimitConfig, source clientip.Source) (http.Handler, *atomic.Int64) {
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
	h, served := limited(RateLimitConfig{Name: "test", Every: time.Second, Burst: 5}, clientip.Source{})

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
	h, _ := limited(RateLimitConfig{Name: "test", Every: 2 * time.Second, Burst: 1}, clientip.Source{})

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
	// One registrant hitting a limit must not stop everybody else registering.
	h, served := limited(RateLimitConfig{Name: "test", Every: time.Hour, Burst: 1}, clientip.Source{})

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
	h, _ := limited(RateLimitConfig{Name: "test", Every: 20 * time.Millisecond, Burst: 1}, clientip.Source{})

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
	h, _ := limited(RateLimitConfig{Name: "test", Every: time.Hour, Burst: 1}, clientip.Source{})

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
	h, _ = limited(RateLimitConfig{Name: "test", Every: time.Hour, Burst: 1}, forwarded)
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

func TestRateLimit_ConcurrentClients(t *testing.T) {
	// The map is shared, so this is worth racing rather than assuming. Run with
	// -race, which CI does.
	h, _ := limited(RateLimitConfig{Name: "test", Every: time.Millisecond, Burst: 50}, clientip.Source{})

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
	h, served := limited(RateLimitConfig{Name: "test", Every: time.Hour, Burst: 0}, clientip.Source{})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, requestFrom("203.0.113.7"))
	if w.Code != http.StatusOK || served.Load() != 1 {
		t.Errorf("a zero burst refused the first request: %d, served %d", w.Code, served.Load())
	}
}

func TestRateLimit_CloudflareHeaderIgnoredWhenRemote(t *testing.T) {
	// An untrusted source must not let CF-Connecting-IP pick the bucket; the
	// cloudflare source must.
	for _, tc := range []struct {
		name   string
		source clientip.Source
		second int
	}{
		{"remote", clientip.Source{}, http.StatusTooManyRequests},
		{"cloudflare", mustSource("cloudflare"), http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := limited(RateLimitConfig{Name: "test", Every: time.Hour, Burst: 1}, tc.source)
			for i, want := range []int{http.StatusOK, tc.second} {
				r := requestFrom("203.0.113.7")
				r.Header.Set("CF-Connecting-IP", "198.51.100."+strconv.Itoa(i+1))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != want {
					t.Errorf("request %d = %d, want %d", i, w.Code, want)
				}
			}
		})
	}
}

func TestRateLimit_IPv6SharesItsSlash64(t *testing.T) {
	h, served := limited(RateLimitConfig{Name: "test", Every: time.Hour, Burst: 3}, clientip.Source{})
	for i := range 20 {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = "[2001:db8:1:2::" + strconv.FormatInt(int64(i+1), 16) + "]:4000"
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	if got := served.Load(); got != 3 {
		t.Errorf("%d requests from one /64 served, want the burst of 3", got)
	}
}

func TestRateLimit_CustomExceededKeepsRetryAfterAndLogs(t *testing.T) {
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, nil))
	page := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte("the page"))
	})
	h := RateLimit(RateLimitConfig{Name: "surface", Every: 7500 * time.Millisecond, Burst: 1, Exceeded: page},
		clientip.Source{}, log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	h.ServeHTTP(httptest.NewRecorder(), requestFrom("203.0.113.7"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, requestFrom("203.0.113.7"))

	if w.Code != http.StatusTooManyRequests || w.Body.String() != "the page" {
		t.Errorf("refusal = %d %q, want the custom 429 page", w.Code, w.Body.String())
	}
	// Truncated, as before: 7.5s advertises 7.
	if got := w.Header().Get("Retry-After"); got != "7" {
		t.Errorf("Retry-After = %q, want 7", got)
	}
	for _, want := range []string{"rate limited", "limiter=surface", "client_ip=203.0.113.7"} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log %q lacks %q", logged.String(), want)
		}
	}

	// With no Exceeded: the plain-text default.
	h, _ = limited(RateLimitConfig{Name: "t", Every: time.Hour, Burst: 1}, clientip.Source{})
	h.ServeHTTP(httptest.NewRecorder(), requestFrom("203.0.113.7"))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, requestFrom("203.0.113.7"))
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "slow down") {
		t.Errorf("default refusal = %d %q", w.Code, w.Body.String())
	}
}

func TestRateLimit_FullTableRefusesNewClientsAndLogsOnce(t *testing.T) {
	var logged strings.Builder
	log := slog.New(slog.NewTextHandler(&logged, nil))
	var served atomic.Int64
	h := RateLimit(RateLimitConfig{Name: "flood", Every: time.Hour, Burst: 1}, clientip.Source{}, log)(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served.Add(1) }))

	const capacity = 10_000 // the module's default MaxKeys
	for i := range capacity {
		h.ServeHTTP(httptest.NewRecorder(), requestFrom("10."+strconv.Itoa(i>>16)+"."+strconv.Itoa(i>>8&255)+"."+strconv.Itoa(i&255)))
	}
	if got := served.Load(); got != capacity {
		t.Fatalf("%d of %d clients served before the cap", got, capacity)
	}
	for range 3 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, requestFrom("198.51.100.1"))
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("a newcomer at the cap = %d, want 429 (fail closed)", w.Code)
		}
	}
	if n := strings.Count(logged.String(), "rate limiter full"); n != 1 {
		t.Errorf("full-table line logged %d times for 3 refusals, want 1", n)
	}
	if !strings.Contains(logged.String(), "limiter=flood") {
		t.Errorf("full-table line does not name the limiter: %s", logged.String())
	}
}
