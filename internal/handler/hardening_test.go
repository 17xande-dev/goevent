package handler

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/config"
	"github.com/17xande-dev/ratelimit/clientip"
)

// Assertions about the running server, as opposed to the units underneath it.
// Every one of these is something that would be silently wrong rather than
// obviously broken. The checkout and callback limits join these when those
// routes exist.

func TestHardening_LoginIsRateLimited(t *testing.T) {
	// Two a minute, so the burst is the floor of 2.
	s := newApp(t, func(c *config.Config) { c.RateLimits.LoginPerMinute = 2 })

	var limited bool
	for range 6 {
		res, _ := post(t, s.srv, "/admin/login", url.Values{"password": {"wrong"}})
		switch res.StatusCode {
		case http.StatusUnauthorized: // a refused attempt, as expected
		case http.StatusTooManyRequests:
			limited = true
			if res.Header.Get("Retry-After") == "" {
				t.Error("a rate-limited login has no Retry-After")
			}
		default:
			t.Fatalf("login attempt = %d", res.StatusCode)
		}
	}
	if !limited {
		t.Error("six login attempts against a limit of two a minute were all served")
	}

	// The form itself keeps working: limiting the GET would lock an operator out of
	// the page they need to read the message on.
	if res, _ := get(t, s.srv, "/admin/login"); res.StatusCode != http.StatusOK {
		t.Errorf("GET /admin/login = %d after the POST limit was hit, want 200", res.StatusCode)
	}
}

func TestHardening_PerIPLimitDisabledByDefaultInTests(t *testing.T) {
	// Use distinct addresses to isolate the configurable per-IP tier from the
	// always-on per-address tier.
	s := newApp(t)
	for i := range 12 {
		res, _ := post(t, s.srv, "/admin/login", url.Values{
			"email": {"user" + strconv.Itoa(i) + "@example.com"}, "password": {"wrong"},
		})
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d with limits unset, want 401", i, res.StatusCode)
		}
	}
}

// limiterProbe sends one request to a rate-limited surface carrying the given
// CF-Connecting-IP and returns the status. Every surface here is mounted exactly
// as main does it; the request is refused by the limiter before the handler runs
// (a 429) or reaches the handler (anything else), so the handler's own answer is
// irrelevant.
type limiterProbe struct {
	name string
	send func(t *testing.T, s *app, cf string) int
}

func limiterProbes() []limiterProbe {
	withCF := func(req *http.Request, cf string) *http.Response {
		req.Header.Set("CF-Connecting-IP", cf)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			panic(err)
		}
		res.Body.Close()
		return res
	}
	form := func(t *testing.T, s *app, method, path, cf string) int {
		t.Helper()
		req := newRequest(t, s.srv, method, path, url.Values{"x": {"y"}})
		req.Header.Set("CF-Connecting-IP", cf)
		res, _ := do(t, s.srv, req)
		return res.StatusCode
	}
	return []limiterProbe{
		{"admin login", func(t *testing.T, s *app, cf string) int {
			return form(t, s, http.MethodPost, "/admin/login", cf)
		}},
		{"registration", func(t *testing.T, s *app, cf string) int {
			return form(t, s, http.MethodPost, "/events/some-event/register", cf)
		}},
		{"payment status", func(t *testing.T, s *app, cf string) int {
			t.Helper()
			req := newRequest(t, s.srv, http.MethodGet, "/checkout/status?payment=x", nil)
			req.Header.Set("CF-Connecting-IP", cf)
			res, _ := do(t, s.srv, req)
			return res.StatusCode
		}},
		{"payment callback", func(t *testing.T, s *app, cf string) int {
			t.Helper()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				s.srv.URL+"/payments/fake/callback", strings.NewReader("x=y"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return withCF(req, cf).StatusCode
		}},
	}
}

func limitedApp(t *testing.T, source string) *app {
	t.Helper()
	src, err := clientip.ParseSource(source) // what config.Load does with CLIENT_IP_SOURCE
	if err != nil {
		t.Fatal(err)
	}
	// Two a minute on every surface: the burst is the floor of 2.
	return newApp(t, func(c *config.Config) {
		c.ClientIPSource = src
		c.RateLimits = config.RateLimits{LoginPerMinute: 2, CheckoutPerMinute: 2, CallbackPerMinute: 2, StatusPerMinute: 2}
	})
}

func TestHardening_LimitersKeyOnTheConfiguredSource(t *testing.T) {
	// One peer (the test server's loopback address) throughout. With
	// CLIENT_IP_SOURCE=cloudflare the bucket belongs to the CF-Connecting-IP value,
	// so a second value is a second client and a repeated one is the same client.
	s := limitedApp(t, "cloudflare")
	for _, p := range limiterProbes() {
		t.Run(p.name, func(t *testing.T) {
			const a, b = "198.51.100.1", "198.51.100.2"
			for i := range 2 {
				if got := p.send(t, s, a); got == http.StatusTooManyRequests {
					t.Fatalf("request %d from %s was limited inside the burst", i, a)
				}
			}
			if got := p.send(t, s, a); got != http.StatusTooManyRequests {
				t.Fatalf("third request from %s = %d, want 429", a, got)
			}
			if got := p.send(t, s, b); got == http.StatusTooManyRequests {
				t.Errorf("a different CF-Connecting-IP (%s) shared %s's bucket", b, a)
			}
			if got := p.send(t, s, a); got != http.StatusTooManyRequests {
				t.Errorf("the same CF-Connecting-IP (%s) escaped its bucket: %d", a, got)
			}
		})
	}
}

func TestHardening_SpoofedHeaderDoesNotEscapeALimiterUnderRemote(t *testing.T) {
	// With CLIENT_IP_SOURCE=remote the header is attacker input: a fresh claim on
	// every request must still land in the one bucket of the real peer.
	s := limitedApp(t, "remote")
	for _, p := range limiterProbes() {
		t.Run(p.name, func(t *testing.T) {
			var last int
			for i := range 3 {
				last = p.send(t, s, "198.51.100."+strconv.Itoa(i+1))
			}
			if last != http.StatusTooManyRequests {
				t.Errorf("a spoofed CF-Connecting-IP per request got a fresh bucket each time: third = %d", last)
			}
		})
	}
}
