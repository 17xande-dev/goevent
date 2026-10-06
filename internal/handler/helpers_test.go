package handler

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/goevent/internal/blob"
	"github.com/17xande-dev/goevent/internal/config"
	"github.com/17xande-dev/goevent/internal/dbtest"
	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/middleware"
	"github.com/17xande-dev/goevent/internal/payment"
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/17xande-dev/mailer"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newPublic mirrors how main.go mounts the server for an anonymous visitor:
// security headers around everything, the public routes, and the first-party
// group behind CSRF. templateDir is an override directory, "" for none.
func newPublic(t *testing.T, cfg config.Config, templateDir string) *httptest.Server {
	t.Helper()

	pool := dbtest.Pool(t)
	gateway := payment.NewFake()
	d := testDeps(t, pool, cfg, templateDir, blob.NewFake(), gateway)
	h := New(d)

	mux := http.NewServeMux()
	h.RegisterPublic(mux)
	h.RegisterPayments(mux)
	firstParty := h.FirstPartyHandler(middleware.RequireAdmin(d.Users, d.Log))
	mux.Handle("/admin/", firstParty)
	mux.Handle(RegisterPath, firstParty)

	srv := httptest.NewServer(middleware.Chain(mux, middleware.SecurityHeaders(middleware.Policy{
		FrameAncestors: cfg.EmbedOrigins,
		FormActions:    []string{gateway.CSP().FormAction},
	})))
	srv.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(srv.Close)
	return srv
}

// testDeps is everything a Handler needs, real stores on pool and fakes at the
// network boundaries, for the harnesses that do not need a handle on each part.
func testDeps(t *testing.T, pool *pgxpool.Pool, cfg config.Config, templateDir string, images blob.Storage, gateways ...payment.Gateway) Deps {
	t.Helper()
	tmpl, err := ParseTemplates(templateDir, images)
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}
	if len(gateways) == 0 {
		gateways = []payment.Gateway{payment.NewFake()}
	}
	return Deps{
		Config: cfg, Log: slog.New(slog.DiscardHandler), Tmpl: tmpl,
		Events: events.NewStore(pool), Registrations: registrations.NewStore(pool),
		Signer:   registrations.NewSigner([]byte("test secret")),
		Gateways: registryOf(t, gateways...), Mail: mailer.NewFake(), Images: images,
		Users: auth.NewStore(pool),
	}
}

// testJPEG is enough of a JPEG for content sniffing to call it one.
var testJPEG = append([]byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01"), bytes.Repeat([]byte{0x42}, 200)...)

// writeOverride drops a template override into a TEMPLATE_DIR for a test, in the
// subdirectory the embedded tree puts it in — an override is found by path, not
// by the name it happens to define. It returns the file, for a test that edits it.
func writeOverride(t *testing.T, dir, name, content string) string {
	t.Helper()
	file := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatalf("mkdir for override %s: %v", name, err)
	}
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatalf("write override %s: %v", name, err)
	}
	return file
}

// between returns the part of a page between two markers, so an assertion about
// one part of a page is not answered by the site header.
func between(t *testing.T, body, start, end string) string {
	t.Helper()
	_, rest, found := strings.Cut(body, start)
	if !found {
		t.Fatalf("the page has no %q:\n%s", start, excerpt(body))
	}
	inner, _, found := strings.Cut(rest, end)
	if !found {
		t.Fatalf("the page has no %q after %q", end, start)
	}
	return inner
}

// excerpt trims a page down to something readable in a failure message.
func excerpt(body string) string {
	if len(body) > 1500 {
		return body[:1500] + "\n…"
	}
	return body
}

func postHTMX(t *testing.T, srv *httptest.Server, path string, form url.Values) (*http.Response, string) {
	t.Helper()
	if _, set := form["csrf_token"]; !set {
		form.Set("csrf_token", csrfToken(t, srv))
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Origin", srv.URL)
	return do(t, srv, req)
}
