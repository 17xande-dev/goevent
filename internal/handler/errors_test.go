package handler

import (
	"net/http"
	"strings"
	"testing"
)

func TestNotFound_UnknownURLGetsThePage(t *testing.T) {
	srv := newPublic(t, testConfig(), "")

	for _, path := range []string{"/nope", "/deep/nested/nonsense"} {
		res, body := get(t, srv, path)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, res.StatusCode)
		}
		// The status is the part that must be right; the page is the part that
		// makes it useful.
		if !strings.Contains(body, "Page not found") {
			t.Errorf("GET %s did not get the 404 page:\n%s", path, excerpt(body))
		}
		if !strings.Contains(body, `href="/events"`) {
			t.Errorf("GET %s: the 404 page offers no way onward", path)
		}
		if !strings.Contains(body, "<html") {
			t.Errorf("GET %s: the 404 is not a rendered page", path)
		}
	}
}

func TestNotFound_ByteEndpointsStayPlain(t *testing.T) {
	// A missing asset answers with the plain 404, deliberately. Nothing is going to
	// read an HTML page out of an <img> tag or a <script> src, and sending one only
	// makes the failure bigger.
	srv := newPublic(t, testConfig(), "")

	res, body := get(t, srv, "/static/no-such-file.css")
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("GET a missing asset = %d", res.StatusCode)
	}
	if strings.Contains(body, "<html") {
		t.Errorf("a missing asset served an HTML page:\n%s", excerpt(body))
	}
}

func TestNotFound_IsOverridable(t *testing.T) {
	dir := t.TempDir()
	writeOverride(t, dir, "pages/not_found.gohtml", `{{define "content"}}MY OWN 404{{end}}`)

	srv := newPublic(t, testConfig(), dir)

	res, body := get(t, srv, "/nope")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("an overridden 404 answered %d", res.StatusCode)
	}
	if !strings.Contains(body, "MY OWN 404") {
		t.Errorf("the override did not take: %q", excerpt(body))
	}
}

func TestNotFound_BrokenTemplateStillAnswers404(t *testing.T) {
	// The status is what a crawler and a browser act on, so a theme that cannot
	// render must not turn a missing page into a 200 with an empty body. This is
	// why notFound renders directly rather than through h.render, which logs the
	// failure and leaves the status alone.
	dir := t.TempDir()
	writeOverride(t, dir, "pages/not_found.gohtml", `{{define "content"}}{{.NoSuchField}}{{end}}`)

	srv := newPublic(t, testConfig(), dir)

	res, body := get(t, srv, "/nope")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("a broken 404 template answered %d, want 404", res.StatusCode)
	}
	// Specifically the plain-text fallback, which is what proves it ran rather than
	// the override having quietly rendered to nothing.
	if !strings.Contains(body, "Not Found") || strings.Contains(body, "<html") {
		t.Errorf("the plain fallback did not run: %q", excerpt(body))
	}
}

func TestNotFound_ReachesUnknownPathsUnderTheFirstPartyGroup(t *testing.T) {
	// /admin/ is a subtree pattern on the outer mux, so it always matches and
	// hands over — which means an unknown path beneath it never reaches the outer
	// catch-all. Without a catch-all inside that group too, /admin/nonsense would
	// be the one URL still answering Go's plain 404.
	s := newApp(t)

	res, body := get(t, s.srv, "/admin/nonsense")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /admin/nonsense = %d, want 404", res.StatusCode)
	}
	if !strings.Contains(body, "Page not found") {
		t.Errorf("GET /admin/nonsense did not get the 404 page:\n%s", excerpt(body))
	}
}
