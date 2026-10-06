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
	"github.com/17xande-dev/goevent/internal/dbtest"
	"github.com/17xande-dev/goevent/internal/payment"
	"github.com/17xande-dev/mailer"
)

// diskApp is a deployment whose images are files this server serves — the
// IMAGE_DIR deployment, with no object storage at all.
func diskApp(t *testing.T) (*httptest.Server, *blob.Disk, string) {
	t.Helper()

	dir := t.TempDir()
	storage, err := blob.NewDisk(dir)
	if err != nil {
		t.Fatalf("NewDisk: %v", err)
	}

	cfg := testConfig()
	cfg.ImageDir = dir

	pool := dbtest.Pool(t)
	tmpl, err := ParseTemplates("", storage)
	if err != nil {
		t.Fatalf("ParseTemplates: %v", err)
	}
	h := New(Deps{
		Config: cfg, Log: slog.New(slog.DiscardHandler), Tmpl: tmpl,
		Gateways: registryOf(t, payment.NewFake()), Mail: mailer.NewFake(), Images: storage,
		Users: auth.NewStore(pool),
	})

	mux := http.NewServeMux()
	h.RegisterPublic(mux)
	if err := h.RegisterImages(mux, dir); err != nil {
		t.Fatalf("RegisterImages: %v", err)
	}

	srv := httptest.NewServer(mux)
	srv.Client().CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	t.Cleanup(srv.Close)
	return srv, storage, dir
}

func TestImages_ServesAnUploadedFile(t *testing.T) {
	srv, storage, _ := diskApp(t)

	const key = "events/3f2504e0/9f86d081b1e2.jpg"
	url, err := storage.Put(t.Context(), key, bytes.NewReader(testJPEG), int64(len(testJPEG)), "image/jpeg")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, err := srv.Client().Get(srv.URL + url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, res.StatusCode)
	}
	// The Content-Type comes from the extension, and the extension came from the
	// sniffed type at upload — so it cannot disagree with the bytes.
	if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "image/jpeg") {
		t.Errorf("Content-Type = %q", ct)
	}
	// Immutable is honest because the key carries a random component: a replacement
	// gets a new key, so a cached copy of this one can never be stale.
	if cc := res.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control = %q", cc)
	}

	body := make([]byte, len(testJPEG)+16)
	n, _ := res.Body.Read(body)
	if !bytes.Equal(body[:n], testJPEG) {
		t.Error("the served bytes differ from the uploaded ones")
	}
}

func TestImages_RefusesTraversalAndListings(t *testing.T) {
	srv, _, dir := diskApp(t)

	// A secret next to the image directory, which no request should be able to reach.
	secret := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(secret, []byte("SENSITIVE"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	// And one inside it, to prove the guard is not merely about leaving the directory.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("INSIDE"), 0o600); err != nil {
		t.Fatalf("write notes: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "events", "abc"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cases := []string{
		"/images/../secret.txt",
		"/images/events/../../secret.txt",
		"/images/%2e%2e/secret.txt",
		"/images/..%2fsecret.txt",
		"/images/",           // a listing of the root
		"/images/events/",    // a listing of a subdirectory
		"/images/events",     // the directory itself
		"/images/events/abc", // ditto, nested
	}
	for _, path := range cases {
		res, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body := make([]byte, 4096)
		n, _ := res.Body.Read(body)
		res.Body.Close()

		if res.StatusCode == http.StatusOK {
			t.Errorf("GET %s = 200: %s", path, body[:n])
		}
		if strings.Contains(string(body[:n]), "SENSITIVE") {
			t.Errorf("GET %s served a file outside the image directory", path)
		}
		// A directory listing would name the files in it, which is what keeps one
		// event's images from being enumerated alongside another's.
		if strings.Contains(string(body[:n]), "notes.txt") || strings.Contains(string(body[:n]), "events") {
			t.Errorf("GET %s produced a directory listing: %s", path, body[:n])
		}
	}
}

func TestImages_MissingFileIs404(t *testing.T) {
	srv, _, _ := diskApp(t)

	for _, path := range []string{
		"/images/events/nothing/here.jpg",
		"/images/nope.jpg",
	} {
		res, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, res.StatusCode)
		}
	}
}

func TestImages_RouteAbsentWithoutADiskBackend(t *testing.T) {
	// A bucket-backed deployment serves its own images, so this route does not exist —
	// there is no directory for it to serve and registering it would be a 404
	// generator with filesystem access.
	s := newApp(t) // no ImageDir
	res, _ := get(t, s.srv, "/images/events/a/b.jpg")
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /images/... = %d with no disk backend, want 404", res.StatusCode)
	}
}

func TestImages_CSPAllowsOnlySelfWhenDiskBacked(t *testing.T) {
	// The whole reason a same-origin path was chosen over an absolute URL: img-src
	// needs no external origin at all.
	srv := newPublic(t, testConfig(), "")

	res, _ := get(t, srv, "/admin/login")
	csp := res.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self'") {
		t.Errorf("img-src is not 'self': %s", csp)
	}
	// And crucially no blanket: an image from the general internet is refused by the
	// browser as well as by the admin.
	if strings.Contains(csp, "img-src 'self' https:") || strings.Contains(csp, "data:") {
		t.Errorf("img-src still allows arbitrary origins: %s", csp)
	}
}
