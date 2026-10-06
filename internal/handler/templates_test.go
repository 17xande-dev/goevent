package handler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/blob"
)

// Every page, rendered with the data its handler passes.
//
// One template set per page means a page can only call a partial, its own layout,
// or something it defines itself — and Go resolves template names at execute
// time, so a call to anything out of scope is a 500 on that page rather than a
// parse error at boot. Rendering all of them is what puts that failure back in CI.
func testPages() map[string]any {
	p := page{Title: "A page", SiteName: "Test Events"}
	return map[string]any{
		"not_found":       errorPageData{page: p, Status: 404},
		"error_client":    errorPageData{page: p, Status: 403, Heading: "No"},
		"error_server":    errorPageData{page: p, Status: 500, Heading: "Sorry"},
		"admin_login":     loginPage{page: p},
		"admin_setup":     setupPage{page: p},
		"admin_home":      p,
		"admin_users":     usersPage{page: p},
		"admin_user_form": userFormPage{page: p},
		"admin_account":   accountPage{page: p},
	}
}

func TestParseTemplates_EmbeddedDefaultsRender(t *testing.T) {
	tmpl, err := ParseTemplates("", blob.NewFake())
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}

	pages := testPages()
	if len(pages) < 9 {
		t.Fatalf("only %d pages to render", len(pages))
	}
	for name, data := range pages {
		w := httptest.NewRecorder()
		if err := tmpl.Render(w, http.StatusOK, name, data); err != nil {
			t.Errorf("render %s: %v", name, err)
			continue
		}
		if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Errorf("%s: Content-Type = %q", name, got)
		}
		if !strings.Contains(w.Body.String(), "Test Events") {
			t.Errorf("%s: the site name from config is not in the page", name)
		}
	}
}

func TestParseTemplates_FontStylesheetLinkedOnlyWhenConfigured(t *testing.T) {
	tmpl, err := ParseTemplates("", blob.NewFake())
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}

	// The default is no web font at all, so the head must carry no third-party
	// stylesheet. A deployment that never configured one should have nothing in
	// its markup pointing off this origin.
	w := httptest.NewRecorder()
	if err := tmpl.Render(w, http.StatusOK, "not_found", errorPageData{page: page{SiteName: "Test Events"}}); err != nil {
		t.Fatalf("render: %v", err)
	}
	if strings.Contains(w.Body.String(), "typekit") {
		t.Error("a font stylesheet is linked with FONT_CSS_URL unset")
	}

	// And with one configured, it is linked — the <link> form only. This is the
	// half that pairs with the CSP: widening style-src achieves nothing if no page
	// actually asks for the kit.
	w = httptest.NewRecorder()
	data := errorPageData{page: page{
		SiteName:   "Test Events",
		FontCSSURL: "https://use.typekit.net/abc1def.css",
	}}
	if err := tmpl.Render(w, http.StatusOK, "not_found", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<link rel="stylesheet" href="https://use.typekit.net/abc1def.css">`) {
		t.Errorf("the font stylesheet is not linked: %s", body)
	}
	// Never as a script: a font service's JS loader would need script-src widened
	// and a nonce for the inline snippet, which this project does not offer.
	if strings.Contains(body, `<script src="https://use.typekit.net`) {
		t.Error("the font kit is loaded as a script")
	}
}

// The property the whole per-page-set design exists for, and the reason the layout
// carries blocks rather than the pages carrying a head/foot sandwich: a definition
// in one page file reaches that page and no other.
func TestParseTemplates_ABlockIsFilledForOnePageOnly(t *testing.T) {
	dir := t.TempDir()
	writeOverride(t, dir, "pages/not_found.gohtml",
		`{{define "content"}}NOT HERE{{end}}`+
			`{{define "nav_extra"}}<a id="extra">Extra</a>{{end}}`)

	tmpl, err := ParseTemplates(dir, blob.NewFake())
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}

	w := httptest.NewRecorder()
	if err := tmpl.Render(w, http.StatusOK, "not_found", errorPageData{}); err != nil {
		t.Fatalf("render not_found: %v", err)
	}
	if !strings.Contains(w.Body.String(), `id="extra"`) {
		t.Error("the page did not fill the nav_extra block it defines")
	}

	// Every other page shares that layout and must still render its empty default.
	w = httptest.NewRecorder()
	if err := tmpl.Render(w, http.StatusOK, "admin_login", loginPage{}); err != nil {
		t.Fatalf("render admin_login: %v", err)
	}
	if strings.Contains(w.Body.String(), `id="extra"`) {
		t.Error("one page's nav_extra leaked onto another")
	}
}

func TestParseTemplates_OverrideDirWins(t *testing.T) {
	dir := t.TempDir()
	// The override mirrors the embedded tree: same subdirectory, same file name,
	// and it defines the same names that file defines — "content" for a page.
	writeOverride(t, dir, "admin/admin_home.gohtml", `{{define "content"}}OVERRIDDEN{{end}}`)

	tmpl, err := ParseTemplates(dir, blob.NewFake())
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}

	if body := render(t, tmpl, "admin_home"); !strings.Contains(body, "OVERRIDDEN") {
		t.Errorf("body = %q, want the override", body)
	}

	// Templates the override directory says nothing about must still come from
	// the embedded defaults.
	w := httptest.NewRecorder()
	if err := tmpl.Render(w, http.StatusOK, "admin_user_form", userFormPage{}); err != nil {
		t.Fatalf("render default: %v", err)
	}
	if !strings.Contains(w.Body.String(), "password") {
		t.Error("the embedded user form was lost when an override was loaded")
	}
}

// The point of THEME_RELOAD: editing a template in the override directory shows on
// the next refresh, without a restart.
func TestSetReload_PicksUpAnEditWithoutReparsing(t *testing.T) {
	dir := t.TempDir()
	file := writeOverride(t, dir, "admin/admin_home.gohtml", `{{define "content"}}FIRST{{end}}`)

	tmpl, err := ParseTemplates(dir, blob.NewFake())
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}
	tmpl.SetReload(true)

	if got := render(t, tmpl, "admin_home"); !strings.Contains(got, "FIRST") {
		t.Fatalf("body = %q, want FIRST", got)
	}

	if err := os.WriteFile(file, []byte(`{{define "content"}}SECOND{{end}}`), 0o600); err != nil {
		t.Fatalf("rewrite override: %v", err)
	}
	if got := render(t, tmpl, "admin_home"); !strings.Contains(got, "SECOND") {
		t.Errorf("body = %q, want SECOND: the edit needed a restart to appear", got)
	}
}

// Without it, the set is what was read at startup — which is what a deployment
// wants, and what makes a broken override a boot failure rather than a 500.
func TestParseTemplates_WithoutReloadAnEditIsNotPickedUp(t *testing.T) {
	dir := t.TempDir()
	file := writeOverride(t, dir, "admin/admin_home.gohtml", `{{define "content"}}FIRST{{end}}`)

	tmpl, err := ParseTemplates(dir, blob.NewFake())
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}
	if err := os.WriteFile(file, []byte(`{{define "content"}}SECOND{{end}}`), 0o600); err != nil {
		t.Fatalf("rewrite override: %v", err)
	}
	if got := render(t, tmpl, "admin_home"); !strings.Contains(got, "FIRST") {
		t.Errorf("body = %q, want the set read at startup", got)
	}
}

// A theme being edited is a theme that is broken half the time. A save mid-edit is
// an error on that request — never a half-written page — and fixing the file is the
// whole recovery.
func TestSetReload_ABrokenEditIsAnErrorAndRecoversOnTheNextSave(t *testing.T) {
	dir := t.TempDir()
	file := writeOverride(t, dir, "admin/admin_home.gohtml", `{{define "content"}}GOOD{{end}}`)

	tmpl, err := ParseTemplates(dir, blob.NewFake())
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}
	tmpl.SetReload(true)
	if got := render(t, tmpl, "admin_home"); !strings.Contains(got, "GOOD") {
		t.Fatalf("body = %q, want GOOD", got)
	}

	if err := os.WriteFile(file, []byte(`{{define "content"}}{{if}}`), 0o600); err != nil {
		t.Fatalf("write broken override: %v", err)
	}
	w := httptest.NewRecorder()
	if err := tmpl.Render(w, http.StatusOK, "admin_home", page{}); err == nil {
		t.Error("a template that does not parse rendered without an error")
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want nothing written", w.Body.String())
	}

	// Fixing the file is all it takes; nothing has to be restarted to recover.
	if err := os.WriteFile(file, []byte(`{{define "content"}}FIXED{{end}}`), 0o600); err != nil {
		t.Fatalf("write fixed override: %v", err)
	}
	if got := render(t, tmpl, "admin_home"); !strings.Contains(got, "FIXED") {
		t.Errorf("body = %q, want FIXED", got)
	}
}

func render(t *testing.T, tmpl *Templates, name string) string {
	t.Helper()
	w := httptest.NewRecorder()
	if err := tmpl.Render(w, http.StatusOK, name, page{}); err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return w.Body.String()
}

func TestRender_UnknownTemplateWritesNothing(t *testing.T) {
	tmpl, err := ParseTemplates("", blob.NewFake())
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}

	w := httptest.NewRecorder()
	if err := tmpl.Render(w, http.StatusOK, "no_such_template", nil); err == nil {
		t.Fatal("expected an error for an unknown template, got nil")
	}
	// Buffering first is the point: a failed render must not have already sent
	// a partial page with a success status.
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want nothing written", w.Body.String())
	}
}
