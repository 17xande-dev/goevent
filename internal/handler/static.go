package handler

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Bundled assets: htmx, the payment redirect script, and the store's own images —
// a logo and a placeholder for products without a photograph.
//
// These are *not* product images. A product image is uploaded, keyed, and deleted
// by the application; these ship with the binary and are replaced by an operator.
// Keeping the two apart means a cleanup sweep over uploaded objects can never
// consider a logo an orphan.
//
// # Overriding
//
// STATIC_DIR is to assets what TEMPLATE_DIR is to templates. A file there shadows a
// bundled one of the same name, and a new name is served too — so rebranding is
// dropping a logo.svg into a directory, not forking the project. Read at startup, so
// a change needs a restart and never a rebuild — unless SetAssetReload is on, which
// is what the development stack does so that a refresh is enough.
//
// Everything is served from this origin, which is why the CSP needs no allowance
// beyond 'self' for any of it.

//go:embed static
var staticFS embed.FS

// contentTypes maps an extension to what the file is served as.
//
// An explicit map rather than mime.TypeByExtension: this is a public route, and the
// set of things a store needs to serve is small and worth stating. An extension not
// listed here is not served at all, so dropping a .php or a .html into STATIC_DIR
// cannot publish it.
var contentTypes = map[string]string{
	".js":      "text/javascript; charset=utf-8",
	".css":     "text/css; charset=utf-8",
	".svg":     "image/svg+xml",
	".png":     "image/png",
	".jpg":     "image/jpeg",
	".jpeg":    "image/jpeg",
	".gif":     "image/gif",
	".webp":    "image/webp",
	".ico":     "image/x-icon",
	".woff2":   "font/woff2",
	".LICENSE": "text/plain; charset=utf-8",
}

// staticAsset is one file, with the URL it is served at.
type staticAsset struct {
	body        []byte
	contentType string
	etag        string
	url         string
}

// assets is the whole served set: the bundled files, with STATIC_DIR layered over
// them. Loaded once on first use, since both sources are fixed for the life of the
// process.
var assets = sync.OnceValues(loadAssets)

// staticDir is where an override directory is read from. It is a package variable
// set at startup rather than a parameter, because the asset URL helper is a template
// function with no place to thread configuration through.
var staticDir string

// reloadAssets re-reads the whole set on every lookup instead of once, so editing a
// styles.css in STATIC_DIR and refreshing shows it. Development only: it is a read
// of every asset per request. Set at startup, like staticDir.
var reloadAssets bool

// SetStaticDir names the override directory and discards anything already loaded.
//
// It must be called before the server starts serving — main does it while building
// the handler — because it is not safe against concurrent asset resolution. That is
// not a limitation in practice: the directory is fixed for the life of a process.
func SetStaticDir(dir string) {
	staticDir = dir
	assets = sync.OnceValues(loadAssets)
}

// SetAssetReload turns per-request reloading on. Call it before serving, with the
// same caveat as SetStaticDir. See reloadAssets.
func SetAssetReload(on bool) {
	reloadAssets = on
}

// currentAssets is the set as of now: reloaded if that is on, otherwise the one
// loaded at startup. loadAssets builds a fresh map every time and nothing mutates
// one afterwards, so a concurrent reader is reading a map no writer can reach.
func currentAssets() (map[string]staticAsset, error) {
	if reloadAssets {
		return loadAssets()
	}
	return assets()
}

func loadAssets() (map[string]staticAsset, error) {
	out := map[string]staticAsset{}

	// The bundled files first.
	entries, err := fs.ReadDir(staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("handler: read bundled assets: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := staticFS.ReadFile("static/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("handler: read bundled asset %s: %w", e.Name(), err)
		}
		if a, ok := newAsset(e.Name(), body); ok {
			out[e.Name()] = a
		}
		// Anything with an unlisted extension — the provenance README, notably — is
		// embedded but not served. That is the point of the extension map.
	}

	// Then the override directory, shadowing by name.
	if staticDir == "" {
		return out, nil
	}
	overrides, err := os.ReadDir(staticDir)
	if err != nil {
		return nil, fmt.Errorf("handler: read STATIC_DIR %q: %w", staticDir, err)
	}
	for _, e := range overrides {
		if e.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(staticDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("handler: read override asset %s: %w", e.Name(), err)
		}
		if a, ok := newAsset(e.Name(), body); ok {
			out[e.Name()] = a
		}
	}
	return out, nil
}

// newAsset prepares one file for serving, or reports that its extension is not one
// this store serves.
func newAsset(name string, body []byte) (staticAsset, bool) {
	contentType, ok := contentTypes[filepath.Ext(name)]
	if !ok {
		return staticAsset{}, false
	}

	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])[:12]
	return staticAsset{
		body:        body,
		contentType: contentType,
		etag:        `"` + digest + `"`,
		// The URL carries a hash of the content, so it can be cached forever and a
		// replacement invalidates it automatically — no version number to remember
		// to bump, and an overridden logo appears immediately.
		url: "/static/" + name + "?v=" + digest,
	}, true
}

// assetURL is the template function behind {{asset "logo.svg"}}.
func assetURL(name string) string {
	set, err := currentAssets()
	if err != nil {
		// Loading failed at startup and was reported there; a template should not be
		// the place this surfaces a second time.
		return "/static/unavailable/" + name
	}
	a, ok := set[name]
	if !ok {
		// Visible in the page rather than silently absent: a mistyped asset name
		// should fail loudly while it is still being written.
		return "/static/missing/" + name
	}
	return a.url
}

// CheckAssets reports a problem loading the asset set, so a bad STATIC_DIR is a
// boot failure rather than a page full of broken links.
func CheckAssets() error {
	_, err := currentAssets()
	return err
}

func (h *Handler) static(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	set, err := currentAssets()
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	a, ok := set[name]
	if !ok {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", a.contentType)
	w.Header().Set("ETag", a.etag)
	// Immutable is honest here only because the URL is content-addressed.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(a.body))
}
