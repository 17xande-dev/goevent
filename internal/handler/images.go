package handler

import (
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"

	"github.com/17xande-dev/goevent/internal/blob"
)

// Serving disk-backed event images.
//
// Registered only when IMAGE_DIR is configured; a bucket-backed site serves its
// images from the bucket and this route does not exist. Reads are the only thing
// here — uploads go through the admin.
//
// Traversal is handled by os.Root rather than by string checking: the root cannot
// be escaped even by a symlink inside the directory, which a filepath.Clean on the
// request path would not catch.

// RegisterImages wires the image route for a disk-backed site.
func (h *Handler) RegisterImages(mux *http.ServeMux, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	fsys := root.FS()

	mux.HandleFunc("GET "+blob.ImagePrefix+"{key...}", func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		// A key is events/<id>/<name>.<ext>. Anything that is not a plain relative
		// path is refused before it reaches the filesystem.
		if key == "" || !filepath.IsLocal(key) {
			http.NotFound(w, r)
			return
		}

		info, err := fs.Stat(fsys, key)
		if err != nil || !info.Mode().IsRegular() {
			// A directory, a device node or a missing file are all "no such image".
			// Directory listings in particular must never happen: this is a public
			// route and the keys are the only thing keeping one event's images from
			// being enumerated alongside another's.
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				h.log.Error("serve image", "key", key, "error", err)
			}
			http.NotFound(w, r)
			return
		}

		// Immutable is honest because a key carries a random component: replacing an
		// image writes a new key, so a cached copy of this one can never be stale.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeFileFS(w, r, fsys, key)
	})
	return nil
}
