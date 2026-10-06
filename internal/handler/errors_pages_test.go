package handler

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/goevent/internal/blob"
	"github.com/17xande-dev/goevent/internal/config"
	"github.com/17xande-dev/goevent/internal/dbtest"
	"github.com/17xande-dev/goevent/internal/middleware"
	"github.com/17xande-dev/goevent/internal/payment"
	"github.com/17xande-dev/mailer"
)

// The invariant these tests defend: an error response either fills the target it
// was aimed at, or replaces the document. htmx is configured to swap error
// responses — without that, every refusal the server sends is discarded — and the
// price of that is that a whole page must never be sent to a fragment's target
// without saying so.

func TestHTMXError_PageReplacesTheDocument(t *testing.T) {
	// The other half. A full error page sent to an htmx request must say "replace
	// the document", or — now that error responses swap — it would be pasted into
	// whatever small target the request named.
	srv := newPublic(t, testConfig(), "")

	req, err := http.NewRequest("GET", srv.URL+"/nope", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("HX-Request", "true")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /nope: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /nope = %d", res.StatusCode)
	}
	if got := res.Header.Get("HX-Retarget"); got != "body" {
		t.Errorf("HX-Retarget = %q, want body", got)
	}
	if got := res.Header.Get("HX-Reswap"); got != "innerHTML" {
		t.Errorf("HX-Reswap = %q, want innerHTML", got)
	}
}

func TestHTMXConfig_SwapsErrorResponses(t *testing.T) {
	// The configuration half of the same invariant. Without this entry htmx drops
	// every 4xx body, and the test above would pass while the visitor still saw
	// nothing.
	srv := newPublic(t, testConfig(), "")

	_, body := get(t, srv, "/admin/login")
	if !strings.Contains(body, "responseHandling") {
		t.Fatal("the htmx config does not mention responseHandling")
	}
	if !strings.Contains(body, `{"code":"[45]..","swap":true`) {
		t.Errorf("htmx is not configured to swap error responses:\n%s",
			between(t, body, `<meta name="htmx-config"`, ">"))
	}
}

func TestErrorPages_StatusByStatus(t *testing.T) {
	srv := newPublic(t, testConfig(), "")

	t.Run("405 names the methods that do work", func(t *testing.T) {
		req, err := http.NewRequest("POST", srv.URL+"/static/styles.css", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		res, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("POST /static/styles.css: %v", err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("POST /static/styles.css = %d, want 405", res.StatusCode)
		}
		allow := res.Header.Get("Allow")
		for _, want := range []string{"GET", "HEAD"} {
			if !strings.Contains(allow, want) {
				t.Errorf("Allow = %q, missing %s", allow, want)
			}
		}
		if strings.Contains(allow, "POST") {
			t.Errorf("Allow = %q names the method that was just refused", allow)
		}
	})

	t.Run("an unknown path is still 404, not 405", func(t *testing.T) {
		// The failure mode of asking the mux which methods a path allows: if the
		// probe counted the catch-all itself, every 404 would become a 405.
		res, body := get(t, srv, "/no/such/path")
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET /no/such/path = %d, want 404", res.StatusCode)
		}
		if !strings.Contains(body, "Page not found") {
			t.Error("an unknown path did not get the 404 page")
		}
	})
}

func TestErrorPages_CSRFFailureIsAPage(t *testing.T) {
	s := newApp(t)

	// An empty token, which is what post() treats as "control it yourself".
	res, body := post(t, s.srv, "/admin/login",
		url.Values{"csrf_token": {""}, "email": {testEmail}})

	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a tokenless post = %d, want 403", res.StatusCode)
	}
	if !strings.Contains(body, "That form has expired") {
		t.Errorf("the 403 is not the error page:\n%s", excerpt(body))
	}
}

func TestErrorPages_DetailShownInDevAndHiddenInProduction(t *testing.T) {
	// Both directions, because asserting only the development case would pass just
	// as well with the flag ignored entirely.
	cases := []struct {
		name    string
		baseURL string
		want    bool
	}{
		{"development", "http://localhost:8080", true},
		{"production", "https://events.example", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig()
			cfg.BaseURL = tc.baseURL
			cfg.CookieSecure = strings.HasPrefix(tc.baseURL, "https://")
			cfg.ShowErrorDetail = !cfg.CookieSecure

			// A closed pool, so any read is a real fault and reaches serverError
			// rather than a validation path.
			srv := brokenApp(t, cfg)
			res, body := get(t, srv, brokenPath)
			if res.StatusCode != http.StatusInternalServerError {
				t.Fatalf("a broken read = %d, want 500", res.StatusCode)
			}

			hasDetail := strings.Contains(body, "Development detail")
			if hasDetail != tc.want {
				t.Errorf("detail shown = %v, want %v (BASE_URL %s)", hasDetail, tc.want, tc.baseURL)
			}
			// The reference is shown either way: it is what makes a production error
			// reportable at all.
			if !strings.Contains(body, "Reference") {
				t.Error("the error page carries no reference")
			}
		})
	}
}

func TestConfig_ShowErrorDetailFollowsBaseURL(t *testing.T) {
	t.Setenv("EMAIL_QUEUE_KEY", strings.Repeat("ab", 32))
	// The derivation itself, without a server in the way.
	t.Setenv("DATABASE_URL", "postgres://x/y")
	// Images and mail are required, and this test is about neither — it just has
	// to get past Load.
	t.Setenv("IMAGE_DIR", t.TempDir())
	t.Setenv("SMTP_HOST", "localhost")
	t.Setenv("EMAIL_FROM", "events@example.com")

	for baseURL, wantDetail := range map[string]bool{
		"http://localhost:8080":  true,
		"https://events.example": false,
	} {
		t.Setenv("BASE_URL", baseURL)
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load with BASE_URL=%s: %v", baseURL, err)
		}
		if cfg.ShowErrorDetail != wantDetail {
			t.Errorf("BASE_URL=%s: ShowErrorDetail = %v, want %v", baseURL, cfg.ShowErrorDetail, wantDetail)
		}
		// The same signal, so they must never disagree.
		if cfg.ShowErrorDetail == cfg.CookieSecure {
			t.Errorf("BASE_URL=%s: ShowErrorDetail and CookieSecure agree; they are opposites", baseURL)
		}
	}
}

func TestRequestID_IsEchoedAndShownOnTheErrorPage(t *testing.T) {
	srv := brokenApp(t, testConfig())

	res, body := get(t, srv, brokenPath)
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a broken read = %d, want 500", res.StatusCode)
	}

	id := res.Header.Get(middleware.Header)
	if id == "" {
		t.Fatal("no request id was echoed")
	}
	// The page and the header must name the same request, or the reference a
	// customer quotes leads nowhere.
	if !strings.Contains(body, id) {
		t.Errorf("the page does not carry the id %q it was served under", id)
	}
}

func TestRequestID_AdoptsAnIncomingID(t *testing.T) {
	srv := brokenApp(t, testConfig())

	cases := map[string]struct{ header, value, want string }{
		"cloud trace": {"X-Cloud-Trace-Context", "abc123def456/9876;o=1", "abc123def456"},
		"request id":  {"X-Request-Id", "from-the-proxy", "from-the-proxy"},
		"sanitised":   {"X-Request-Id", "bad id; with <junk>", "badidwithjunk"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			req, err := http.NewRequest("GET", srv.URL+brokenPath, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			req.Header.Set(tc.header, tc.value)
			res, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", brokenPath, err)
			}
			defer res.Body.Close()

			if got := res.Header.Get(middleware.Header); got != tc.want {
				t.Errorf("request id = %q, want %q", got, tc.want)
			}
		})
	}

	// With neither header, one is minted rather than left empty.
	res, _ := get(t, srv, brokenPath)
	if got := res.Header.Get(middleware.Header); len(got) != 16 {
		t.Errorf("generated id = %q, want 16 hex characters", got)
	}
}

// brokenPath is a page that reads the database for an anonymous visitor: the
// setup form asks whether anybody has claimed the admin yet.
const brokenPath = "/admin/setup"

// brokenApp is a server whose database is closed, so every read is a genuine
// fault. It is the only honest way to exercise the 500 path: the handlers have no
// other way to fail on demand, and a fake that returned an error would test the
// fake.
//
// Request IDs are wired here as main.go wires them, because the reference on the
// error page is half of what these tests are about.
func brokenApp(t *testing.T, cfg config.Config) *httptest.Server {
	t.Helper()

	pool := dbtest.Pool(t)
	images := blob.NewFake()
	tmpl, err := ParseTemplates("", images)
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}
	log := slog.New(slog.DiscardHandler)
	users := auth.NewStore(pool)
	h := New(Deps{
		Config: cfg, Log: log, Tmpl: tmpl,
		Gateways: registryOf(t, payment.NewFake()), Mail: mailer.NewFake(), Images: images,
		Users: users,
	})

	mux := http.NewServeMux()
	h.RegisterPublic(mux)
	mux.Handle("/admin/", h.FirstPartyHandler(middleware.RequireAdmin(users, log)))
	srv := httptest.NewServer(middleware.Chain(mux, middleware.RequestID))
	srv.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(srv.Close)

	// Closed after the server is built, so construction succeeds and only the
	// queries fail — which is what a database that goes away mid-life looks like.
	pool.Close()
	return srv
}
