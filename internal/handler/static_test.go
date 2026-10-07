package handler

import (
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/events"
)

// The asset set is loaded once per process, so a test that changes STATIC_DIR has to
// go through SetStaticDir, which discards what is loaded. Nothing in production calls
// it more than once — the directory is fixed for the life of the server.
func withStaticDir(t *testing.T, dir string) {
	t.Helper()
	previous := staticDir
	SetStaticDir(dir)
	t.Cleanup(func() { SetStaticDir(previous) })
}

// Reloading is likewise a process-wide flag set once at startup, so a test that
// wants it has to put it back.
func withAssetReload(t *testing.T, on bool) {
	t.Helper()
	previous := reloadAssets
	SetAssetReload(on)
	t.Cleanup(func() { SetAssetReload(previous) })
}

func TestAssets_BundledImagesAreServed(t *testing.T) {
	withStaticDir(t, "")
	srv := newPublic(t, testConfig(), "")

	// The logo and the placeholder ship in the binary: a store with no configuration
	// at all still has a mark in its header and a picture on every product card.
	for name, wantType := range map[string]string{
		"logo.svg":        "image/svg+xml",
		"placeholder.svg": "image/svg+xml",
		"htmx.min.js":     "javascript",
		"styles.css":      "text/css",
	} {
		url := assetURL(name)
		if strings.Contains(url, "missing") || strings.Contains(url, "unavailable") {
			t.Errorf("assetURL(%q) = %q", name, url)
			continue
		}

		res, body := get(t, srv, url)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", url, res.StatusCode)
			continue
		}
		if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, wantType) {
			t.Errorf("%s: Content-Type = %q, want %q", name, ct, wantType)
		}
		if !strings.Contains(res.Header.Get("Cache-Control"), "immutable") {
			t.Errorf("%s: Cache-Control = %q", name, res.Header.Get("Cache-Control"))
		}
		if body == "" {
			t.Errorf("%s served an empty body", name)
		}
	}
}

// htmx injects a <style> block for the indicator classes at load, which style-src
// 'self' blocks — silently, and open: the rule that hides an hx-indicator never
// applies and a spinner stays visible. The injection is therefore turned off in the
// page and the rules ship in the stylesheet. Neither half works alone, so both are
// asserted here rather than trusted to survive an edit to one file.
func TestTheme_HtmxIndicatorStylesReplaceTheInjectedBlock(t *testing.T) {
	withStaticDir(t, "")
	srv := newPublic(t, testConfig(), "")

	_, page := get(t, srv, "/admin/login")
	if !strings.Contains(page, `"includeIndicatorStyles":false`) {
		t.Error("the page does not disable htmx's injected indicator styles, which the CSP blocks")
	}

	_, css := get(t, srv, assetURL("styles.css"))
	for _, rule := range []string{".htmx-indicator", ".htmx-request .htmx-indicator"} {
		if !strings.Contains(css, rule) {
			t.Errorf("styles.css does not define %s, so an indicator would never be hidden", rule)
		}
	}
}

func TestAssets_UnlistedExtensionsAreNotServed(t *testing.T) {
	withStaticDir(t, "")
	srv := newPublic(t, testConfig(), "")

	// The provenance note lives in the same directory and is embedded with everything
	// else, but its extension is not in the content-type map, so it is not published.
	// That is what keeps a file dropped into that directory from becoming a URL.
	if res, _ := get(t, srv, "/static/README.md"); res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /static/README.md = %d, want 404", res.StatusCode)
	}
	if url := assetURL("README.md"); !strings.Contains(url, "missing") {
		t.Errorf("assetURL(README.md) = %q, want a visibly missing URL", url)
	}
}

func TestAssets_StaticDirShadowsABundledFile(t *testing.T) {
	// The whole point: rebranding is dropping a logo.svg into a directory, not forking
	// the project.
	dir := t.TempDir()
	const ours = `<svg xmlns="http://www.w3.org/2000/svg"><title>OUR BRAND</title></svg>`
	if err := os.WriteFile(filepath.Join(dir, "logo.svg"), []byte(ours), 0o644); err != nil {
		t.Fatalf("write override: %v", err)
	}

	withStaticDir(t, "")
	bundledURL := assetURL("logo.svg")

	withStaticDir(t, dir)
	srv := newPublic(t, testConfig(), "")

	overriddenURL := assetURL("logo.svg")
	if overriddenURL == bundledURL {
		t.Fatal("the override has the same URL as the bundled file, so a cache would keep serving the old logo")
	}

	res, body := get(t, srv, overriddenURL)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", overriddenURL, res.StatusCode)
	}
	if !strings.Contains(body, "OUR BRAND") {
		t.Errorf("the override was not served: %s", body)
	}

	// And the page references it, so replacing the file is the whole operation.
	_, page := get(t, srv, "/admin/login")
	if !strings.Contains(page, overriddenURL) {
		t.Errorf("the page does not reference the overridden logo: %s", page)
	}
}

// THEME_RELOAD's asset half: editing a stylesheet in STATIC_DIR and refreshing is
// enough. The URL is content-addressed, so the refreshed page asks for a URL no
// cache has, which is what makes the edit visible rather than merely served.
func TestAssets_ReloadPicksUpAnEdit(t *testing.T) {
	dir := t.TempDir()
	css := filepath.Join(dir, "styles.css")
	if err := os.WriteFile(css, []byte("body{color:red}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	withStaticDir(t, dir)
	withAssetReload(t, true)
	srv := newPublic(t, testConfig(), "")

	first := assetURL("styles.css")
	if err := os.WriteFile(css, []byte("body{color:blue}"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	second := assetURL("styles.css")
	if second == first {
		t.Fatal("the edited stylesheet has the URL of the old one, so the browser would keep the cached copy")
	}
	res, body := get(t, srv, second)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", second, res.StatusCode)
	}
	if !strings.Contains(body, "blue") {
		t.Errorf("served the stale stylesheet: %s", body)
	}
	// And the page points at the new one, so a refresh is the whole operation.
	if _, page := get(t, srv, "/admin/login"); !strings.Contains(page, second) {
		t.Error("the page still references the stylesheet from before the edit")
	}
}

func TestAssets_WithoutReloadAnEditIsNotPickedUp(t *testing.T) {
	// A deployment reads the set once. Anything else would be a disk read per
	// request for a directory nobody is editing.
	dir := t.TempDir()
	css := filepath.Join(dir, "styles.css")
	if err := os.WriteFile(css, []byte("body{color:red}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	withStaticDir(t, dir)
	withAssetReload(t, false)

	first := assetURL("styles.css")
	if err := os.WriteFile(css, []byte("body{color:blue}"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if got := assetURL("styles.css"); got != first {
		t.Errorf("assetURL = %q, want the URL of the set read at startup (%q)", got, first)
	}
}

func TestAssets_StaticDirCanAddNewNames(t *testing.T) {
	// An adopter whose overridden template references hero.jpg needs somewhere to put
	// hero.jpg. Shadowing alone would not be enough.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hero.png"), []byte("\x89PNG\r\n\x1a\nnot-really"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	withStaticDir(t, dir)
	srv := newPublic(t, testConfig(), "")

	url := assetURL("hero.png")
	if strings.Contains(url, "missing") {
		t.Fatalf("assetURL(hero.png) = %q", url)
	}
	res, _ := get(t, srv, url)
	if res.StatusCode != http.StatusOK {
		t.Errorf("GET %s = %d", url, res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestAssets_StaticDirCannotPublishAnythingItLikes(t *testing.T) {
	// The extension map is the gate on the override directory too. Dropping an .html
	// or a .php in there must not make it a URL on the store's own origin.
	dir := t.TempDir()
	for _, name := range []string{"evil.html", "shell.php", "notes.txt", "secrets.env"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("<script>alert(1)</script>"), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	withStaticDir(t, dir)
	srv := newPublic(t, testConfig(), "")

	for _, name := range []string{"evil.html", "shell.php", "notes.txt", "secrets.env"} {
		if url := assetURL(name); !strings.Contains(url, "missing") {
			t.Errorf("assetURL(%q) = %q, want it unserved", name, url)
		}
		res, _ := get(t, srv, "/static/"+name)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET /static/%s = %d, want 404", name, res.StatusCode)
		}
	}
}

func TestAssets_MissingStaticDirIsABootFailure(t *testing.T) {
	// Reported at startup rather than as a page full of broken links.
	withStaticDir(t, filepath.Join(t.TempDir(), "does-not-exist"))
	if err := CheckAssets(); err == nil {
		t.Error("CheckAssets accepted a directory that does not exist")
	}
}

func TestAssets_ThemeIsLinkedAndSelfHosted(t *testing.T) {
	withStaticDir(t, "")
	srv := newPublic(t, testConfig(), "")

	_, page := get(t, srv, "/admin/login")

	// The stylesheet is linked with its content-hashed URL, so replacing it through
	// STATIC_DIR takes effect without waiting for a cache to expire.
	if !strings.Contains(page, `rel="stylesheet" href="/static/styles.css?v=`) {
		t.Errorf("the page does not link the bundled stylesheet: %s", page)
	}
	// Nothing is fetched from anywhere else. A web font or a CDN stylesheet would
	// mean widening style-src and putting a request to somebody else's server on
	// every page — including the ones in the payment path.
	for _, forbidden := range []string{"https://fonts.", "cdn.", "//unpkg", "//cdnjs"} {
		if strings.Contains(page, forbidden) {
			t.Errorf("the page references an external origin %q", forbidden)
		}
	}
	// And no style attribute survives, which is what lets style-src stay closed.
	if strings.Contains(page, "style=\"") {
		t.Errorf("the page carries an inline style attribute, which the CSP now forbids: %s", page)
	}
}

// inlineHandler matches an inline event handler attribute — onclick=, onchange=
// and the rest — anywhere in a tag.
var inlineHandler = regexp.MustCompile(`(?i)<[^>]*\son[a-z]+\s*=`)

func TestAssets_EveryServedPageIsFreeOfInlineStylesAndHandlers(t *testing.T) {
	// The CSP has no 'unsafe-inline' in style-src or script-src, so a style
	// attribute or an onchange= anywhere in a *served* page is a silently broken
	// element: it renders, returns 200, passes every handler test, and does
	// nothing in a real browser. Email bodies are exempt: no CSP applies to them.
	withStaticDir(t, "")
	s := setupApp(t)
	other := mustAccount(t, s, "other@example.com", testPassword, "viewer")
	e := createEvent(t, s, "Camp")
	addTicket(t, s, e, ticketForm("Adult", "0"))
	tts, _ := s.events.TicketTypes(t.Context(), e.ID)
	post(t, s.srv, "/admin/events/"+e.ID+"/questions", url.Values{
		"label": {"Dietary needs"}, "scope": {"attendee"}, "kind": {"text"},
	})
	qs, _ := s.events.Questions(t.Context(), e.ID)
	if len(tts) != 1 || len(qs) != 1 {
		t.Fatalf("fixture: %d ticket types, %d questions", len(tts), len(qs))
	}
	ev := "/admin/events/" + e.ID

	var checked int
	for _, path := range []string{
		"/nope", "/admin/events", "/admin/events/new", ev,
		ev + "/tickets/new", ev + "/tickets/" + tts[0].ID,
		ev + "/questions/new", ev + "/questions/" + qs[0].ID,
		"/admin/users", "/admin/users/new",
		"/admin/users/" + other.ID + "/edit", "/admin/account",
	} {
		res, page := get(t, s.srv, path)
		if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d; the sweep is not reaching the page", path, res.StatusCode)
			continue
		}
		checked++
		if strings.Contains(page, "style=\"") {
			t.Errorf("GET %s carries an inline style attribute", path)
		}
		if m := inlineHandler.FindString(page); m != "" {
			t.Errorf("GET %s carries an inline event handler: %s", path, m)
		}
	}
	// The public pages, through a registration and its hand-over.
	pe, adult, child := published(t, s, 10)
	if _, err := s.events.CreateQuestion(t.Context(), events.Question{EventID: pe.ID, Scope: events.ScopeAttendee,
		Kind: events.KindSelect, Label: "Size", Options: []string{"S", "M"}}); err != nil {
		t.Fatal(err)
	}
	form := registerForm(t, s, pe, map[string]int{child.ID: 1})
	res, _ := submit(t, s, pe, form)
	manage := res.Header.Get("Location")
	handover := registerForm(t, s, pe, map[string]int{adult.ID: 1})
	_, handoverPage := submit(t, s, pe, handover)
	paymentID := s.gateway.Requests()[0].PaymentID
	pages := map[string]string{"hand-over": handoverPage}
	for _, path := range []string{
		"/events", "/events/" + pe.Slug, "/events/" + pe.Slug + "/register?qty." + child.ID + "=2",
		manage, "/checkout/success?payment=" + paymentID, "/checkout/status?payment=" + paymentID,
		// And the admin's view of what just happened.
		"/admin/registrations", "/admin/registrations/" + onlyPendingRegistration(t, s),
		"/admin/events/" + pe.ID,
		"/admin/events/" + pe.ID + "/checkin?q=a&result=unknown",
	} {
		res, page := get(t, s.srv, path)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d; the sweep is not reaching the page", path, res.StatusCode)
			continue
		}
		pages[path] = page
	}
	// The anonymous pages, from a client with no session.
	anon := newPublic(t, testConfig(), "")
	_, pages["/admin/login"] = get(t, anon, "/admin/login")
	for path, page := range pages {
		checked++
		if strings.Contains(page, "style=\"") || inlineHandler.MatchString(page) {
			t.Errorf("%s carries an inline style or handler", path)
		}
	}
	if checked < 23 {
		t.Fatalf("only %d pages checked", checked)
	}
}

// onlyPendingRegistration is the id of the one registration awaiting payment.
func onlyPendingRegistration(t *testing.T, s *app) string {
	t.Helper()
	var id string
	if err := s.pool.QueryRow(t.Context(), `SELECT id FROM registrations WHERE status = 'pending'`).Scan(&id); err != nil {
		t.Fatalf("one pending registration: %v", err)
	}
	return id
}
