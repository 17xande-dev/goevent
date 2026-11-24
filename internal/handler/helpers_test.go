package handler

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/17xande-dev/mailer"
	"github.com/17xande-dev/payment"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newPublic mirrors how main.go mounts the server for an anonymous visitor:
// security headers around everything, the public routes, and the first-party
// group behind CSRF. templateDir is an override directory, "" for none.
func newPublic(t *testing.T, cfg config.Config, templateDir string) *httptest.Server {
	t.Helper()

	pool := dbtest.Pool(t)
	// The CSRF origin must be known before the handler is built, so the listener
	// is opened first and the server started once the handler exists.
	srv := httptest.NewUnstartedServer(nil)
	ownOrigin(&cfg, srv)
	d := testDeps(t, pool, cfg, templateDir, blob.NewFake())
	h := mustNew(t, d)

	mux := http.NewServeMux()
	h.RegisterPublic(mux)
	h.RegisterPayments(mux)
	firstParty := h.FirstPartyHandler(h.RequireAdmin())
	mux.Handle("/admin/", firstParty)
	mux.Handle(RegisterPath, firstParty)

	forms, images := d.Gateways.CSP()
	srv.Config.Handler = middleware.Chain(mux, middleware.SecurityHeaders(middleware.Policy{
		FrameAncestors: cfg.EmbedOrigins,
		FormActions:    forms,
		ImgSources:     images,
	}))
	srv.Start()
	srv.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(srv.Close)
	return srv
}

// ownOrigin makes the server's own listener address the site's BASE_URL, which
// is what the CSRF check trusts as the app's own origin, unless the test set one.
func ownOrigin(cfg *config.Config, srv *httptest.Server) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://" + srv.Listener.Addr().String()
	}
}

// mustNew is New for a configuration the test knows is valid.
func mustNew(t *testing.T, d Deps) *Handler {
	t.Helper()
	h, err := New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
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
