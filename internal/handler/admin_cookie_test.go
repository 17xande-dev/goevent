package handler

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/config"
	"github.com/17xande-dev/websec/webtest"
)

func TestAdminCookieAttributes(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprintf("secure=%t", secure), func(t *testing.T) {
			const ttl = time.Hour
			// The prefixes are the production shape: https gets the browser-enforced
			// names, plain-HTTP development keeps the bare ones a browser would not
			// drop. The origin is the app's own BASE_URL, which the CSRF check trusts.
			origin, csrfName, sessionName := "http://example.test", "csrf_token", "admin_session"
			if secure {
				origin, csrfName, sessionName = "https://example.test", "__Host-csrf_token", "__Secure-admin_session"
			}
			s := newApp(t, func(c *config.Config) {
				c.CookieSecure, c.SessionTTL, c.BaseURL = secure, ttl, origin
			})
			send := func(method, path string, form url.Values, cookies ...*http.Cookie) *webtest.Response {
				// Each request specifies its cookies, including the deliberate use
				// of the pre-login CSRF cookie and token on logout.
				c := webtest.New(t, s.srv.Config.Handler, origin, webtest.WithOrigin(origin))
				r := c.NewRequest(method, path, strings.NewReader(form.Encode()))
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				for _, ck := range cookies {
					r.AddCookie(ck)
				}
				return c.Do(r)
			}
			cookie := func(r *webtest.Response, name, path string, session, cleared bool) *http.Cookie {
				t.Helper()
				return webtest.WantCookie(t, r, webtest.CookieWant{
					Name: name, Path: path, HttpOnly: true, SameSite: http.SameSiteLaxMode,
					Secure: secure, Session: session, Cleared: cleared,
				})
			}
			loginForm := send(http.MethodGet, "/admin/login", nil)
			if loginForm.Status != http.StatusOK {
				t.Fatalf("login form: status %d", loginForm.Status)
			}
			csrf := cookie(loginForm, csrfName, "/", true, false)
			// Session-only: it used to live 365 days, which outlasts every session.
			if csrf.Value == "" {
				t.Error("CSRF cookie must carry a value and end with the browser session (no Max-Age, no Expires)")
			}
			before := time.Now()
			login := send(http.MethodPost, "/admin/login", url.Values{
				"email": {testEmail}, "password": {testPassword}, "csrf_token": {csrfFromResponse(t, loginForm)},
			}, csrf)
			if login.Status != http.StatusSeeOther {
				t.Fatalf("sign in: status %d", login.Status)
			}
			session := cookie(login, sessionName, "/admin", false, false)
			// The kit floors Max-Age, so it may be a second under the TTL but never over.
			if session.Value == "" || session.MaxAge > int(ttl.Seconds()) || session.MaxAge < int(ttl.Seconds())-1 ||
				session.Expires.Before(before.Add(ttl).Truncate(time.Second)) || session.Expires.After(time.Now().Add(ttl)) {
				t.Error("session cookie must carry a value and expire at the configured TTL")
			}
			// The sign-in renews the CSRF cookie, so the one served with the form is replaced.
			if renewed := cookie(login, csrfName, "/", false, false); renewed.Value == csrf.Value {
				t.Error("sign-in did not replace the CSRF cookie")
			}

			logout := send(http.MethodPost, "/admin/logout", url.Values{
				"csrf_token": {csrfFromResponse(t, loginForm)},
			}, csrf, session)
			if logout.Status != http.StatusSeeOther || logout.Location() != "/admin/login" {
				t.Fatalf("logout: status %d, Location %q", logout.Status, logout.Location())
			}
			cleared := cookie(logout, sessionName, "/admin", false, true)
			// The kit also sends an epoch Expires, for clients that ignore Max-Age.
			if cleared.Value != "" || cleared.Expires.After(time.Unix(0, 0).Add(time.Second)) {
				t.Error("logout must clear the cookie immediately with an empty value and negative MaxAge")
			}
		})
	}
}

func csrfFromResponse(t *testing.T, r *webtest.Response) string {
	t.Helper()
	return r.Page().Token()
}
