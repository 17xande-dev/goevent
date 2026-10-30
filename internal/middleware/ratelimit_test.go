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
	// One registrant hitting a limit must not stop everybody else registering.
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

// newTestLimiter builds a limiter with a tiny allowance so one request per client
// is all it ever gets in the test's frozen time.
func newTestLimiter(burst, maxB int, ttl time.Duration) *ipLimiter {
	return &ipLimiter{
		limit:    rate.Every(time.Hour),
		burst:    burst,
		ttl:      ttl,
		buckets:  map[string]*bucket{},
		lastKeep: time.Unix(0, 0),
		max:      maxB,
	}
}

func TestRateLimit_IPv6KeyedBySlash64(t *testing.T) {
	// Anyone with an IPv6 prefix owns 2^64 addresses; keyed on the full address
	// each would get a fresh bucket and the limiter would limit nothing.
	l := newTestLimiter(3, 0, time.Hour)
	now := time.Unix(1000, 0)
	allowed := 0
	for i := range 1000 {
		if l.allow("2001:db8:1:2::"+strconv.FormatInt(int64(i+1), 16), now) {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("%d of 1000 addresses in one /64 allowed, want the burst of 3", allowed)
	}
	if got := l.size(); got != 1 {
		t.Errorf("%d buckets for one /64, want 1", got)
	}

	if !l.allow("2001:db8:1:3::1", now) {
		t.Error("a different /64 shared the first one's bucket")
	}
	if got := l.size(); got != 2 {
		t.Errorf("%d buckets for two /64s, want 2", got)
	}
}

func TestRateLimit_MappedIPv4SharesItsBucket(t *testing.T) {
	l := newTestLimiter(1, 0, time.Hour)
	now := time.Unix(1000, 0)
	if !l.allow("1.2.3.4", now) {
		t.Fatal("first request refused")
	}
	if l.allow("::ffff:1.2.3.4", now) {
		t.Error("::ffff:1.2.3.4 got its own bucket instead of sharing 1.2.3.4's")
	}
	if got := l.size(); got != 1 {
		t.Errorf("%d buckets, want 1", got)
	}
	// Garbage still keys as itself, and does not collide with real clients.
	if !l.allow("not-an-ip", now) || l.size() != 2 {
		t.Error("an unparseable address was not keyed by its raw string")
	}
}

func TestRateLimit_CapRefusesNewClientsAndFailsClosed(t *testing.T) {
	var logged strings.Builder
	l := newTestLimiter(1, 5, time.Hour)
	l.name = "test"
	l.log = slog.New(slog.NewTextHandler(&logged, nil))
	now := time.Unix(1000, 0)

	for i := range 5 {
		if !l.allow("203.0.113."+strconv.Itoa(i), now) {
			t.Fatalf("client %d refused before the cap", i)
		}
	}
	// A new key at the cap is refused, repeatedly, without the map growing.
	for range 3 {
		if l.allow("198.51.100.1", now) {
			t.Fatal("a new client was admitted at the cap")
		}
	}
	if got := l.size(); got != 5 {
		t.Errorf("%d buckets, want it held at the cap of 5", got)
	}
	// An existing client with allowance left is not locked out by the cap.
	l2 := newTestLimiter(2, 5, time.Hour)
	for i := range 5 {
		l2.allow("203.0.113."+strconv.Itoa(i), now)
	}
	if !l2.allow("203.0.113.0", now) {
		t.Error("an existing client was refused at the cap")
	}
	if l2.allow("198.51.100.1", now) {
		t.Error("a new client was admitted at the cap")
	}

	// One log line per sweep window, not one per refused request.
	if n := strings.Count(logged.String(), "rate limiter full"); n != 1 {
		t.Errorf("cap logged %d times for 3 refusals, want 1", n)
	}

	// Once the buckets have expired the sweep makes room and new keys are admitted.
	later := now.Add(2 * time.Hour)
	if !l.allow("198.51.100.1", later) {
		t.Error("a new client was still refused after every bucket expired")
	}
	if got := l.size(); got != 1 {
		t.Errorf("%d buckets after expiry, want 1", got)
	}
}

func TestRateLimit_FloodOfAddressesNeverGrowsTheMap(t *testing.T) {
	// Through the middleware: a flood of distinct IPv4 addresses and distinct
	// /64s cannot make the limiter hold more than its cap, and once full a
	// newcomer is answered 429 rather than let through.
	var served atomic.Int64
	l := newTestLimiter(1, 50, time.Hour)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(ClientIP(r, ClientIPRemote), time.Unix(1000, 0)) {
			http.Error(w, "limited", http.StatusTooManyRequests)
			return
		}
		served.Add(1)
	})
	for i := range 500 {
		var addr string
		if i%2 == 0 {
			addr = "10.1." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256)
		} else {
			addr = "[2001:db8:" + strconv.FormatInt(int64(i), 16) + "::1]"
		}
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = addr + ":4000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
	}
	if got := l.size(); got != 50 {
		t.Errorf("%d buckets after a flood, want the cap of 50", got)
	}
	if got := served.Load(); got != 50 {
		t.Errorf("%d flood requests served, want 50", got)
	}
}

func TestRateLimit_CapSweepReclaimsExpiredBucketsOnly(t *testing.T) {
	// The ordinary once-per-TTL sweep must not be what frees room here, or the
	// cap-time sweep is never exercised: it ran at t=1000, and the next is not due
	// until t=1060, but the cap-time sweep at t=1061 resets that clock.
	l := newTestLimiter(2, 3, 60*time.Second)
	l.allow("10.0.0.1", time.Unix(1000, 0)) // regular sweep runs
	l.allow("10.0.0.2", time.Unix(1050, 0))
	l.allow("10.0.0.3", time.Unix(1050, 0))
	l.allow("10.0.0.1", time.Unix(1059, 0))

	if l.allow("10.9.9.9", time.Unix(1061, 0)) {
		t.Fatal("a newcomer was admitted while every bucket was live")
	}
	// .2 and .3 are now 61s idle, .1 only 52s, and no regular sweep is due.
	if !l.allow("10.9.9.9", time.Unix(1111, 0)) {
		t.Fatal("the cap-time sweep did not reclaim the expired buckets")
	}
	if got := l.size(); got != 2 {
		t.Errorf("%d buckets, want 2 (the live one kept, plus the newcomer)", got)
	}
	if _, ok := l.buckets["10.0.0.1"]; !ok {
		t.Error("a live bucket was dropped by the cap-time sweep")
	}
}

func TestRateLimit_CapSweepIsNotRunPerRefusedRequest(t *testing.T) {
	// Each sweep scans the whole map under the lock, so a flood of newcomers must
	// not trigger one apiece.
	l := newTestLimiter(1, 2, 60*time.Second)
	l.allow("10.0.0.1", time.Unix(1000, 0))
	l.allow("10.0.0.2", time.Unix(1000, 0))

	if l.allow("10.9.9.9", time.Unix(1058, 0)) { // cap-time sweep: nothing expired yet
		t.Fatal("admitted while full")
	}
	// Both buckets expired at 1060, but the last cap-time sweep was 3s ago, so
	// the newcomer is refused without a scan and nothing is reclaimed.
	if l.allow("10.9.9.9", time.Unix(1061, 0)) {
		t.Error("a refused newcomer triggered a second sweep inside capSweepEvery")
	}
	if got := l.size(); got != 2 {
		t.Errorf("%d buckets, want 2 (no rescan yet)", got)
	}
	// Once the interval has passed the sweep runs and the cap heals.
	if !l.allow("10.9.9.9", time.Unix(1058, 0).Add(capSweepEvery+time.Second)) {
		t.Error("the cap did not heal once capSweepEvery had passed")
	}
}
