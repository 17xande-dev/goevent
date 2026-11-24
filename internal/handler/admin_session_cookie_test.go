package handler

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/blob"
	"github.com/17xande-dev/goevent/internal/config"
	"github.com/17xande-dev/goevent/internal/dbtest"
)

// secureApp is the production shape: https BASE_URL, so the prefixed cookie
// names a browser would enforce. A plain httptest server cannot hold Secure
// cookies in a jar, so these tests drive the real handler with explicit cookies.
type secureApp struct {
	*app
	origin string
}

func newSecureApp(t *testing.T) secureApp {
	t.Helper()
	const origin = "https://events.example"
	return secureApp{newApp(t, func(c *config.Config) {
		c.CookieSecure, c.BaseURL, c.SessionTTL = true, origin, time.Hour
	}), origin}
}

func (s secureApp) send(t *testing.T, method, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, s.origin+path, body)
	r.Header.Set("Origin", s.origin)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.srv.Config.Handler.ServeHTTP(w, r)
	return w
}

func setCookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("status %d: response set no %s cookie", w.Code, name)
	return nil
}

const (
	secureCSRFName    = "__Host-csrf_token"
	secureSessionName = "__Secure-admin_session"
)

// The CSRF token is replaced at sign-in, sign-out and the user's own password
// change, and the one issued before is refused afterwards. A cookie planted
// before sign-in (cookie injection) would otherwise give its planter a valid
// token for the session.
func TestCSRFTokenIsRenewedAtSignInSignOutAndPasswordChange(t *testing.T) {
	s := newSecureApp(t)

	loginForm := s.send(t, http.MethodGet, "/admin/login", nil)
	before := setCookie(t, loginForm, secureCSRFName)
	beforeToken := csrfFromResponse(t, loginForm)

	login := s.send(t, http.MethodPost, "/admin/login", url.Values{
		"email": {testEmail}, "password": {testPassword}, "csrf_token": {beforeToken},
	}, before)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("sign in: status %d", login.Code)
	}
	session := setCookie(t, login, secureSessionName)
	signedIn := setCookie(t, login, secureCSRFName)
	if signedIn.Value == "" || signedIn.Value == before.Value {
		t.Fatal("sign-in did not replace the CSRF cookie")
	}

	// What the browser now holds: the new CSRF cookie and the session. The
	// pre-sign-in token is refused against it.
	forged := s.send(t, http.MethodPost, "/admin/events", url.Values{
		"csrf_token": {beforeToken}, "title": {"Forged"},
	}, signedIn, session)
	if forged.Code != http.StatusForbidden {
		t.Errorf("the pre-sign-in token after sign-in = %d, want 403", forged.Code)
	}
	page := s.send(t, http.MethodGet, accountPath, nil, signedIn, session)
	if page.Code != http.StatusOK {
		t.Fatalf("account page: status %d", page.Code)
	}
	signedInToken := csrfFromResponse(t, page)

	// Own password change: every session ends, and the token of the signed-in
	// page must not survive into the next sign-in.
	change := s.send(t, http.MethodPost, accountPath, url.Values{
		"csrf_token": {signedInToken}, "current_password": {testPassword},
		"password": {"a different long passphrase"}, "password_confirm": {"a different long passphrase"},
	}, signedIn, session)
	if change.Code != http.StatusSeeOther {
		t.Fatalf("password change: status %d", change.Code)
	}
	afterChange := setCookie(t, change, secureCSRFName)
	if afterChange.Value == "" || afterChange.Value == signedIn.Value {
		t.Fatal("password change did not replace the CSRF cookie")
	}
	if cleared := setCookie(t, change, secureSessionName); cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Error("password change did not clear the session cookie")
	}
	replay := s.send(t, http.MethodPost, "/admin/login", url.Values{
		"email": {testEmail}, "password": {"a different long passphrase"}, "csrf_token": {signedInToken},
	}, afterChange)
	if replay.Code != http.StatusForbidden {
		t.Errorf("the signed-in token after a password change = %d, want 403", replay.Code)
	}

	// Sign-out, from a fresh sign-in.
	form := s.send(t, http.MethodGet, "/admin/login", nil, afterChange)
	login = s.send(t, http.MethodPost, "/admin/login", url.Values{
		"email": {testEmail}, "password": {"a different long passphrase"}, "csrf_token": {csrfFromResponse(t, form)},
	}, afterChange)
	if login.Code != http.StatusSeeOther {
		t.Fatalf("second sign in: status %d", login.Code)
	}
	session, signedIn = setCookie(t, login, secureSessionName), setCookie(t, login, secureCSRFName)
	page = s.send(t, http.MethodGet, accountPath, nil, signedIn, session)
	signedInToken = csrfFromResponse(t, page)
	logout := s.send(t, http.MethodPost, "/admin/logout", url.Values{"csrf_token": {signedInToken}}, signedIn, session)
	if logout.Code != http.StatusSeeOther {
		t.Fatalf("sign out: status %d", logout.Code)
	}
	after := setCookie(t, logout, secureCSRFName)
	if after.Value == "" || after.Value == signedIn.Value {
		t.Fatal("sign-out did not replace the CSRF cookie")
	}
	if cleared := setCookie(t, logout, secureSessionName); cleared.Value != "" || cleared.MaxAge >= 0 {
		t.Error("sign-out did not clear the session cookie")
	}
	// The signed-in token, replayed into the signed-out browser, is refused.
	replay = s.send(t, http.MethodPost, "/admin/login", url.Values{
		"email": {testEmail}, "password": {"a different long passphrase"}, "csrf_token": {signedInToken},
	}, after)
	if replay.Code != http.StatusForbidden {
		t.Errorf("the signed-in token after sign-out = %d, want 403", replay.Code)
	}
}

// A session cookie under its pre-migration bare name is not a session: a sibling
// subdomain can set a bare cookie, and the prefixed name is the one a browser
// refuses to let it overwrite.
func TestBareNamedSessionCookieIsIgnored(t *testing.T) {
	s := newSecureApp(t)
	token, _, err := s.users.IssueSession(t.Context(), s.owner.ID, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	bare := &http.Cookie{Name: "admin_session", Value: token}
	prefixed := &http.Cookie{Name: secureSessionName, Value: token}

	if w := s.send(t, http.MethodGet, "/admin/events", nil, prefixed); w.Code != http.StatusOK {
		t.Fatalf("prefixed session cookie = %d, want 200 (the fixture is wrong)", w.Code)
	}
	w := s.send(t, http.MethodGet, "/admin/events", nil, bare)
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/admin/login") {
		t.Errorf("bare-named session cookie = %d to %q, want a redirect to sign in", w.Code, w.Header().Get("Location"))
	}
	// The login form's "already signed in" shortcut reads the cookie too.
	if w := s.send(t, http.MethodGet, "/admin/login", nil, bare); w.Code != http.StatusOK {
		t.Errorf("sign-in page with a bare-named cookie = %d, want 200 (not signed in)", w.Code)
	}
}

// A BASE_URL the CSRF origin check cannot use is an error from New, not a panic
// and not a server that rejects every form.
func TestNewRefusesABaseURLThatCannotBeAnOrigin(t *testing.T) {
	cfg := testConfig()
	cfg.BaseURL = "https://events.example/app"
	d := testDeps(t, dbtest.Pool(t), cfg, "", blob.NewFake())
	if _, err := New(d); err == nil || !strings.Contains(err.Error(), "BASE_URL") {
		t.Fatalf("New with a path in BASE_URL = %v, want an error naming BASE_URL", err)
	}
}
