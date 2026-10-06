package handler

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/17xande-dev/goevent/internal/config"
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

func TestHardening_UnlimitedByDefaultInTests(t *testing.T) {
	// The rest of the suite depends on this: a zero limit means the surface is
	// unlimited, so no existing test is quietly throttled into failing.
	s := newApp(t)
	for i := range 12 {
		res, _ := post(t, s.srv, "/admin/login", url.Values{"password": {"wrong"}})
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d with limits unset, want 401", i, res.StatusCode)
		}
	}
}
