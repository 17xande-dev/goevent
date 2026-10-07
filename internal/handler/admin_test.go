package handler

import (
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/goevent/internal/blob"
	"github.com/17xande-dev/goevent/internal/config"
	"github.com/17xande-dev/goevent/internal/dbtest"
	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/middleware"
	"github.com/17xande-dev/goevent/internal/outbox"
	"github.com/17xande-dev/goevent/internal/payment"
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/17xande-dev/mailer"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testEmail and testPassword are the owner account every test in this package
// signs in as. The account is created by newApp, not by configuration: there is
// no admin credential in the environment.
const (
	testEmail    = "owner@example.com"
	testPassword = "correct horse battery staple"
)

// cheapHash is argon2id at its cheapest: these tests assert on authentication
// behaviour, not on how expensive the hash is, and DefaultParams would add 64 MiB
// and a tenth of a second to every account created here.
//
// The claim and change-password handlers hash at auth.DefaultParams, because they
// are the real thing — this is only for accounts a test creates directly.
func cheapHash(t *testing.T, password string) string {
	t.Helper()
	h, err := auth.HashPassword(password,
		auth.Params{Memory: 64, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return h
}

func testConfig() config.Config {
	return config.Config{
		SiteName:   "Test Events",
		Currency:   "ZAR",
		SessionTTL: time.Hour,
	}
}

// app is a running server and everything behind it a test might assert on: what
// reached the database, what the checkout handed the gateway, and what mail went
// out. The fakes are the point — a test can inspect both sides of every boundary
// without a payment provider or a mail server.
type app struct {
	srv     *httptest.Server
	gateway *payment.Fake
	mail    *mailer.Fake
	images  *blob.Fake
	outbox  *outbox.Store
	events  *events.Store
	regs    *registrations.Store
	signer  registrations.Signer

	// pool is the database behind all of the above, for the tests that need to
	// take it away — an outage is a response an admin page has to get right.
	pool *pgxpool.Pool

	// handler is the one the server is mounted on, for the tests that ask it what
	// it registered — the route sweep and the role matrix both work from
	// AdminProtectedRoutes rather than from a list of their own.
	handler *Handler

	// users is the administrator accounts. Tests reach for it to create a second
	// administrator, to revoke a session, or to assert on what a handler did to an
	// account.
	users *auth.Store
	// owner is the account newApp creates and signIn signs in as. It is the zero
	// User on an app from newUnclaimedApp, which has no accounts at all.
	owner auth.User
}

// registryOf wraps gateways for Deps. A test that wants to exercise the chooser
// passes two; most pass one, and get a checkout with no chooser at all — which is
// the production shape for a deployment that has configured a single provider.
func registryOf(t *testing.T, gateways ...payment.Gateway) payment.Registry {
	t.Helper()
	r, err := payment.NewRegistry(gateways...)
	if err != nil {
		t.Fatalf("payment.NewRegistry: %v", err)
	}
	return r
}

// newApp is newUnclaimedApp with one enabled owner already in it, which is the
// state every test but the setup-flow ones wants: an admin area with an account
// to sign in to.
//
// edit lets a test change the configuration before the handler reads it, which is
// the only chance it gets — the handler takes a copy at construction.
func newApp(t *testing.T, edit ...func(*config.Config)) *app {
	t.Helper()
	return newAppWith(t, nil, edit...)
}

// newAppWith is newApp with more payment gateways than the default one, for the
// tests about a deployment that offers a choice.
func newAppWith(t *testing.T, extraGateways []payment.Gateway, edit ...func(*config.Config)) *app {
	t.Helper()

	s := newUnclaimedAppWith(t, extraGateways, edit...)
	s.owner = mustAccount(t, s, testEmail, testPassword, auth.RoleOwner)
	return s
}

// newUnclaimedApp mounts the server against a database with no administrators, as
// a fresh deployment is before anybody claims it. Only the setup-flow tests want
// this; everything else takes newApp.
func newUnclaimedApp(t *testing.T, edit ...func(*config.Config)) *app {
	t.Helper()
	return newUnclaimedAppWith(t, nil, edit...)
}

// The server is mounted exactly as main.go does it — same subtrees, same CSRF
// wrapper, same middleware — with a cookie jar but no session yet. Tests that
// build their own routing would stop testing what actually runs.
func newUnclaimedAppWith(t *testing.T, extraGateways []payment.Gateway, edit ...func(*config.Config)) *app {
	t.Helper()

	cfg := testConfig()
	for _, e := range edit {
		e(&cfg)
	}

	pool := dbtest.Pool(t)

	// One storage fake, shared: the templates resolve an event's image key through
	// the same backend the upload handler wrote to, exactly as in production.
	images := blob.NewFake()
	tmpl, err := ParseTemplates("", images)
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}
	log := slog.New(slog.DiscardHandler)
	users := auth.NewStore(pool)
	eventStore := events.NewStore(pool)
	gateway := payment.NewFake()
	mail := mailer.NewFake()
	queue, err := outbox.New(pool, strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	regs := registrations.NewStore(pool)
	signer := registrations.NewSigner([]byte("test secret"))
	h := New(Deps{
		Config:        cfg,
		Log:           log,
		Tmpl:          tmpl,
		Events:        eventStore,
		Registrations: regs,
		Signer:        signer,
		Gateways:      registryOf(t, append([]payment.Gateway{gateway}, extraGateways...)...),
		Mail:          mail,
		Outbox:        queue,
		Images:        images,
		Users:         users,
	})

	mux := http.NewServeMux()
	// Everything main.go mounts, mounted the same way: the callback outside the
	// CSRF group, the registration form inside it.
	h.RegisterPublic(mux)
	h.RegisterPayments(mux)
	firstParty := h.FirstPartyHandler(middleware.RequireAdmin(users, log))
	mux.Handle("/admin/", firstParty)
	mux.Handle(RegisterPath, firstParty)

	srv := httptest.NewServer(mux)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	srv.Client().Jar = jar
	// Redirects are the assertion in several tests, so they must not be
	// followed away.
	srv.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(srv.Close)
	return &app{
		srv: srv, handler: h, gateway: gateway, mail: mail, images: images,
		outbox: queue, users: users, pool: pool, events: eventStore, regs: regs, signer: signer,
	}
}

// setup returns a signed-in server for the admin routes.
func setup(t *testing.T) *httptest.Server {
	t.Helper()
	return setupApp(t).srv
}

// setupApp is setup with everything else the server is made of, for the admin
// tests that need to reach past the response — into the accounts, the
// registrations or the mail that went out.
func setupApp(t *testing.T) *app {
	t.Helper()

	s := newApp(t)
	signIn(t, s.srv)
	return s
}

// mustAccount creates an administrator, for the setup half of a test whose
// subject is something else.
func mustAccount(t *testing.T, s *app, email, password string, role auth.Role) auth.User {
	t.Helper()

	u, err := s.users.Create(t.Context(), email, "", cheapHash(t, password), role, false)
	if err != nil {
		t.Fatalf("create %s (%s): %v", email, role, err)
	}
	return u
}

// signIn signs in as the owner newApp created. signInAs is the same thing for
// any other account a test has made.
func signIn(t *testing.T, srv *httptest.Server) {
	t.Helper()
	signInAs(t, srv, testEmail, testPassword)
}

func signInAs(t *testing.T, srv *httptest.Server, email, password string) {
	t.Helper()

	res, body := post(t, srv, "/admin/login", url.Values{
		"email":    {email},
		"password": {password},
	})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in as %s = %d %s", email, res.StatusCode, body)
	}
	if len(res.Cookies()) == 0 {
		t.Fatal("sign in set no cookie")
	}
}

func TestAdmin_HomeIsTheEventsListForAnySignedInRole(t *testing.T) {
	s := newApp(t)
	mustAccount(t, s, "door@example.com", testPassword, auth.RoleCheckin)
	signInAs(t, s.srv, "door@example.com", testPassword)

	res, _ := get(t, s.srv, "/admin/")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != adminHome {
		t.Fatalf("GET /admin/ = %d %q, want 303 to %s", res.StatusCode, res.Header.Get("Location"), adminHome)
	}
	// A door volunteer's first page must not be a 403.
	res, body := get(t, s.srv, adminHome)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s as check-in = %d %s", adminHome, res.StatusCode, body)
	}
	if !strings.Contains(body, "Test Events") {
		t.Errorf("the admin does not name the site:\n%s", excerpt(body))
	}
}

func get(t *testing.T, srv *httptest.Server, path string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return do(t, srv, req)
}

// post submits a form with a valid CSRF token, so ordinary tests exercise the
// same path a browser takes rather than being exempted from it. Pass a
// csrf_token explicitly — including an empty one — to control it, which is how
// the CSRF tests themselves get a rejection.
func post(t *testing.T, srv *httptest.Server, path string, form url.Values) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path,
		strings.NewReader(withToken(t, srv, form).Encode()))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// nosurf checks the request's origin as well as its token, via
	// Sec-Fetch-Site, Origin or Referer — a browser sends all three on a
	// same-origin form post, so a test that sends none is not emulating one.
	req.Header.Set("Origin", srv.URL)
	req.Header.Set("Referer", srv.URL+path)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	return do(t, srv, req)
}

// newRequest builds a request against the test server without sending it, for a
// test that has to set a header or a cookie of its own before it goes.
func newRequest(t *testing.T, srv *httptest.Server, method, path string, form url.Values) *http.Request {
	t.Helper()

	var body io.Reader
	if form != nil {
		body = strings.NewReader(withToken(t, srv, form).Encode())
	}
	req, err := http.NewRequestWithContext(t.Context(), method, srv.URL+path, body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", srv.URL)
		req.Header.Set("Referer", srv.URL+path)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	return req
}

// withToken returns form with a CSRF token in it, as a copy.
//
// A copy because the caller's map may be shared — a table of cases reused
// across subtests is the ordinary way to write these — and writing the token
// into it would pin the first server's token to every later request. That
// failure looks exactly like a refused request, which is what several of these
// tests assert, so it passes while proving nothing.
func withToken(t *testing.T, srv *httptest.Server, form url.Values) url.Values {
	t.Helper()

	out := url.Values{}
	for k, v := range form {
		out[k] = slices.Clone(v)
	}
	if _, set := out["csrf_token"]; !set {
		out.Set("csrf_token", csrfToken(t, srv))
	}
	return out
}

// csrfToken reads a token out of a rendered form. nosurf validates the
// submitted token against the client's cookie, so any token issued to this jar
// works for any later request from it.
//
// Which page depends on the site's state: the login form redirects away once
// the client is signed in and once nobody has claimed the site yet, and the
// setup page exists only in that second case. Your own password form is the
// third, for a signed-in client — it is the one admin page every role can open,
// including an account being forced to change its password, which is bounced
// away from all the others.
func csrfToken(t *testing.T, srv *httptest.Server) string {
	t.Helper()

	res, body := get(t, srv, "/admin/login")
	if res.StatusCode != http.StatusOK {
		if res, body = get(t, srv, "/admin/setup"); res.StatusCode != http.StatusOK {
			_, body = get(t, srv, accountPath)
		}
	}

	m := csrfFieldRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no csrf_token field in the page: %s", body)
	}
	// The token is base64, so html/template entity-escapes any "+" in it. A
	// browser decodes that before submitting; this has to do the same.
	return html.UnescapeString(m[1])
}

var csrfFieldRE = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func do(t *testing.T, srv *httptest.Server, req *http.Request) (*http.Response, string) {
	t.Helper()
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return res, string(body)
}
