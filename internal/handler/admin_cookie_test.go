package handler

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/config"
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
			send := func(method, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
				r := httptest.NewRequest(method, origin+path, strings.NewReader(form.Encode()))
				r.Header.Set("Origin", origin)
				r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
				for _, c := range cookies {
					r.AddCookie(c)
				}
				w := httptest.NewRecorder()
				s.srv.Config.Handler.ServeHTTP(w, r)
				return w
			}
			cookie := func(w *httptest.ResponseRecorder, name, path string) *http.Cookie {
				t.Helper()
				for _, c := range w.Result().Cookies() {
					if c.Name != name {
						continue
					}
					if c.Path != path || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure != secure {
						t.Errorf("attributes for %s: Path=%q HttpOnly=%t SameSite=%v Secure=%t", name, c.Path, c.HttpOnly, c.SameSite, c.Secure)
					}
					return c
				}
				t.Fatalf("response status %d: missing %s cookie", w.Code, name)
				return nil
			}
			loginForm := send(http.MethodGet, "/admin/login", nil)
			if loginForm.Code != http.StatusOK {
				t.Fatalf("login form: status %d", loginForm.Code)
			}
			csrf := cookie(loginForm, csrfName, "/")
			// Session-only: it used to live 365 days, which outlasts every session.
			if csrf.MaxAge != 0 || !csrf.Expires.IsZero() || csrf.Value == "" {
				t.Error("CSRF cookie must carry a value and end with the browser session (no Max-Age, no Expires)")
			}
			before := time.Now()
			login := send(http.MethodPost, "/admin/login", url.Values{
				"email": {testEmail}, "password": {testPassword}, "csrf_token": {csrfFromResponse(t, loginForm)},
			}, csrf)
			if login.Code != http.StatusSeeOther {
				t.Fatalf("sign in: status %d", login.Code)
			}
			session := cookie(login, sessionName, "/admin")
			// The kit floors Max-Age, so it may be a second under the TTL but never over.
			if session.Value == "" || session.MaxAge > int(ttl.Seconds()) || session.MaxAge < int(ttl.Seconds())-1 ||
				session.Expires.Before(before.Add(ttl).Truncate(time.Second)) || session.Expires.After(time.Now().Add(ttl)) {
				t.Error("session cookie must carry a value and expire at the configured TTL")
			}
			// The sign-in renews the CSRF cookie, so the one served with the form is replaced.
			if renewed := cookie(login, csrfName, "/"); renewed.Value == csrf.Value {
				t.Error("sign-in did not replace the CSRF cookie")
			}

			logout := send(http.MethodPost, "/admin/logout", url.Values{
				"csrf_token": {csrfFromResponse(t, loginForm)},
			}, csrf, session)
			if logout.Code != http.StatusSeeOther || logout.Header().Get("Location") != "/admin/login" {
				t.Fatalf("logout: status %d, Location %q", logout.Code, logout.Header().Get("Location"))
			}
			cleared := cookie(logout, sessionName, "/admin")
			// The kit also sends an epoch Expires, for clients that ignore Max-Age.
			if cleared.Value != "" || cleared.MaxAge >= 0 || cleared.Expires.After(time.Unix(0, 0).Add(time.Second)) {
				t.Error("logout must clear the cookie immediately with an empty value and negative MaxAge")
			}
		})
	}
}

func csrfFromResponse(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	m := csrfFieldRE.FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatal("login form has no CSRF token")
	}
	return html.UnescapeString(m[1])
}
