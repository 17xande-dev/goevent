package handler

import (
	"io"
	"log/slog"
	"net/http"
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
	"github.com/17xande-dev/goevent/internal/outbox"
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/17xande-dev/mailer"
	"github.com/17xande-dev/password"
	"github.com/17xande-dev/payment"
	"github.com/17xande-dev/websec/webtest"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testEmail and testPassword are the owner account every test in this package
// signs in as. The account is created by newApp, not by configuration: there is
// no admin credential in the environment.
const (
	testEmail    = "owner@example.com"
	testPassword = "correct horse battery staple"
)

func TestMain(m *testing.M) {
	// Test handler behaviour without paying the production KDF cost for each request.
	defer password.SetParamsForTesting(password.Params{Memory: 64, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})()
	m.Run()
}

func cheapHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := auth.HashPassword(t.Context(), pw)
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
	srv     *testServer
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

type testServer struct {
	*httptest.Server
	browser *webtest.Client
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
	// The CSRF origin must be known before the handler is built, so the listener
	// is opened first and the server started once the handler exists.
	srv := httptest.NewUnstartedServer(nil)
	ownOrigin(&cfg, srv)

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
	h := mustNew(t, Deps{
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
	firstParty := h.FirstPartyHandler(h.RequireAdmin())
	mux.Handle("/admin/", firstParty)
	mux.Handle(RegisterPath, firstParty)

	srv.Config.Handler = mux
	srv.Start()
	t.Cleanup(srv.Close)
	return &app{
		srv:     &testServer{srv, webtest.NewServer(t, srv, browserFormHeaders)},
		handler: h, gateway: gateway, mail: mail, images: images,
		outbox: queue, users: users, pool: pool, events: eventStore, regs: regs, signer: signer,
	}
}

// setup returns a signed-in server for the admin routes.
func setup(t *testing.T) *testServer {
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
func signIn(t *testing.T, srv *testServer) {
	t.Helper()
	signInAs(t, srv, testEmail, testPassword)
}

// signInAs signs in as email in a fresh browser: switching accounts is a different
// person, not the same jar. Session replacement on sign-in is pinned separately by
// TestAdminAuth_SignInReplacesAnExistingSession.
func signInAs(t *testing.T, srv *testServer, email, password string) {
	t.Helper()

	srv.browser = webtest.NewServer(t, srv.Server, browserFormHeaders)
	srv.browser.With(t).SignIn("/admin/login", url.Values{
		"email":    {email},
		"password": {password},
	})
}

func TestAdmin_LoginAcceptsPreMigrationHash(t *testing.T) {
	for _, tc := range []struct {
		name, hash, pw string
	}{
		// Generated with this app's hashpw command before switching modules.
		{"argon2id", "$argon2id$v=19$m=65536,t=3,p=4$TfHpY+Wzr6MW9NZsKpLxQw$MenE6eZr2jkncrccxt54q4/Odju1fD1qYDjzW2WB93g", testPassword},
		// The module's compatibility fixture checked against the old bcrypt reader.
		{"bcrypt", "$2a$10$twjQA8BMDdFuHP3afXoWe.PWv/NGS/A2zpHSrgDmWAVc/fzuPmJ66", "fixture password — correct horse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newUnclaimedApp(t)
			u, err := s.users.Create(t.Context(), testEmail, "", tc.hash, auth.RoleOwner, false)
			if err != nil {
				t.Fatal(err)
			}
			signInAs(t, s.srv, testEmail, tc.pw)
			if res, _ := get(t, s.srv, adminHome); res.StatusCode != http.StatusOK {
				t.Fatalf("protected page after legacy sign-in = %d, want 200", res.StatusCode)
			}
			var stored string
			if err := s.pool.QueryRow(t.Context(), `SELECT password_hash FROM admin_users WHERE id = $1`, u.ID).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if ok, rehash, err := password.Check(t.Context(), stored, tc.pw); !ok || rehash || err != nil {
				t.Errorf("stored hash must verify at current parameters: ok=%t, rehash=%t, err=%v", ok, rehash, err)
			}
		})
	}
}

func TestAdmin_MalformedStoredHashIsARateLimitedLoginFailure(t *testing.T) {
	s := newApp(t, func(c *config.Config) { c.RateLimits.LoginPerMinute = 2 })
	// Bypass write-time validation to simulate corrupt data already on disk.
	if _, err := s.pool.Exec(t.Context(), `UPDATE admin_users SET password_hash = 'broken' WHERE id = $1`, s.owner.ID); err != nil {
		t.Fatal(err)
	}
	form := url.Values{"email": {testEmail}, "password": {testPassword}}
	for range 2 {
		res, body := post(t, s.srv, "/admin/login", form)
		if res.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "That email and password do not match an account.") {
			t.Fatalf("login with corrupt hash = %d %s, want generic 401", res.StatusCode, body)
		}
	}
	if res, _ := post(t, s.srv, "/admin/login", form); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third attempt = %d, want 429", res.StatusCode)
	}
}

func TestAdmin_HomeIsAPageEveryRoleCanOpen(t *testing.T) {
	for _, c := range []struct {
		role auth.Role
		want string
	}{
		{auth.RoleViewer, adminHome},
		// A door volunteer's first page must not be a 403.
		{auth.RoleCheckin, checkinHome},
	} {
		t.Run(string(c.role), func(t *testing.T) {
			s := newApp(t)
			email := string(c.role) + "@example.com"
			mustAccount(t, s, email, testPassword, c.role)
			res, _ := post(t, s.srv, "/admin/login", url.Values{"email": {email}, "password": {testPassword}})
			if loc := res.Header.Get("Location"); loc != c.want {
				t.Errorf("signing in lands on %q, want %s", loc, c.want)
			}
			res, _ = get(t, s.srv, "/admin/")
			if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != c.want {
				t.Fatalf("GET /admin/ = %d %q, want 303 to %s", res.StatusCode, res.Header.Get("Location"), c.want)
			}
			res, body := get(t, s.srv, c.want)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("GET %s as %s = %d %s", c.want, c.role, res.StatusCode, body)
			}
			if !strings.Contains(body, "Test Events") {
				t.Errorf("the admin does not name the site:\n%s", excerpt(body))
			}
		})
	}
}

func get(t *testing.T, srv *testServer, path string) (*http.Response, string) {
	t.Helper()
	return responseParts(srv.browser.With(t).Get(path))
}

// browserFormHeaders preserves the browser headers for ordinary form POSTs.
// do preserves the headers that custom requests set or intentionally omit.
func browserFormHeaders(r *http.Request) {
	if r.Method == http.MethodPost {
		webtest.SecFetchSite("same-origin")(r)
		webtest.Header("Referer", r.URL.Scheme+"://"+r.URL.Host+r.URL.RequestURI())(r)
	}
}

// post submits a form with a valid CSRF token, so ordinary tests exercise the
// same path a browser takes rather than being exempted from it. Pass a
// csrf_token explicitly — including an empty one — to control it, which is how
// the CSRF tests themselves get a rejection.
func post(t *testing.T, srv *testServer, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	return responseParts(srv.browser.With(t).PostForm(path, withToken(t, srv, form)))
}

// newRequest builds a request against the test server without sending it, for a
// test that has to set a header or a cookie of its own before it goes.
func newRequest(t *testing.T, srv *testServer, method, path string, form url.Values) *http.Request {
	t.Helper()

	var body io.Reader
	if form != nil {
		body = strings.NewReader(withToken(t, srv, form).Encode())
	}
	req := srv.browser.With(t).NewRequest(method, path, body)
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
func withToken(t *testing.T, srv *testServer, form url.Values) url.Values {
	t.Helper()

	out := url.Values{}
	for k, v := range form {
		out[k] = slices.Clone(v)
	}
	if _, set := out[webtest.TokenField]; !set {
		out.Set(webtest.TokenField, csrfToken(t, srv))
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
func csrfToken(t *testing.T, srv *testServer) string {
	t.Helper()
	return csrfPage(t, srv).Page().Token()
}

func csrfPage(t *testing.T, srv *testServer) *webtest.Response {
	t.Helper()
	c := srv.browser.With(t)
	for _, path := range []string{"/admin/login", "/admin/setup"} {
		if res := c.Get(path); res.Status == http.StatusOK {
			return res
		}
	}
	res := c.Get(accountPath)
	res.WantStatus(http.StatusOK)
	return res
}

var csrfFieldRE = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

func do(t *testing.T, srv *testServer, req *http.Request) (*http.Response, string) {
	t.Helper()
	res := srv.browser.With(t).Do(req, func(r *http.Request) {
		for _, name := range []string{"Origin", "Referer", "Sec-Fetch-Site"} {
			r.Header.Del(name)
			for _, value := range req.Header.Values(name) {
				r.Header.Add(name, value)
			}
		}
	})
	return responseParts(res)
}

// Keep the existing status, header and cookie assertions on their original
// response shape while webtest owns transport and drains the body.
func responseParts(res *webtest.Response) (*http.Response, string) {
	return &http.Response{StatusCode: res.Status, Header: res.Header, Request: res.Request}, res.Body
}
