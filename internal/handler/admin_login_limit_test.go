package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/goevent/internal/config"
)

func loginAttempt(t *testing.T, s *app, email, password string, source int) *httptest.ResponseRecorder {
	t.Helper()
	r := newRequest(t, s.srv, http.MethodPost, "/admin/login", url.Values{
		"email": {email}, "password": {password},
	})
	r.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", source)
	for _, c := range s.srv.Client().Jar.Cookies(r.URL) {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.srv.Config.Handler.ServeHTTP(w, r)
	return w
}

func TestLoginThrottlesByAddressBeforeVerifying(t *testing.T) {
	s := newApp(t, func(c *config.Config) { c.RateLimits.LoginPerMinute = 90 })
	verify := s.users.Authenticate
	checks := 0
	s.handler.authenticateLogin = func(ctx context.Context, email, password string) (auth.User, error) {
		checks++
		return verify(ctx, email, password)
	}
	for i := 1; i <= 5; i++ {
		if w := loginAttempt(t, s, strings.ToUpper(testEmail), "wrong password", i); w.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status %d, want 401", i, w.Code)
		}
	}
	if w := loginAttempt(t, s, testEmail, testPassword, 6); w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "60" || checks != 5 {
		t.Fatalf("exhausted address: status %d, Retry-After %q, %d verifications; want 429, 60, 5", w.Code, w.Header().Get("Retry-After"), checks)
	}
	for i := 1; i <= 5; i++ {
		if w := loginAttempt(t, s, "other@example.com", "wrong password", i); w.Code != http.StatusUnauthorized {
			t.Fatalf("other address from IP %d: status %d, want 401", i, w.Code)
		}
	}
}

func TestSuccessfulLoginDoesNotChargeAddress(t *testing.T) {
	s := newApp(t)
	for i := 1; i <= 6; i++ {
		if w := loginAttempt(t, s, testEmail, testPassword, i); w.Code != http.StatusSeeOther {
			t.Fatalf("successful login %d: status %d, want 303", i, w.Code)
		}
	}
}

func TestOverlongLoginAddressesGetSeparateBuckets(t *testing.T) {
	s := newApp(t)
	email := strings.Repeat("A", 300) + "@example.com"
	for i := 1; i <= 5; i++ {
		if w := loginAttempt(t, s, email, "wrong password", i); w.Code != http.StatusUnauthorized {
			t.Fatalf("overlong address failure %d: status %d, want 401", i, w.Code)
		}
	}
	if w := loginAttempt(t, s, strings.ToLower(email), "wrong password", 6); w.Code != http.StatusTooManyRequests {
		t.Fatalf("exhausted overlong address: status %d, want 429", w.Code)
	}
	if w := loginAttempt(t, s, "b"+email, "wrong password", 6); w.Code != http.StatusUnauthorized {
		t.Fatalf("other overlong address: status %d, want 401", w.Code)
	}
}
