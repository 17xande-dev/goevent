package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ImagePrefix is the URL path the server serves disk-backed images from. It is a
// relative URL, so it works whatever BASE_URL is and stays same-origin — which is
// the point: an image the store serves itself needs no CSP allowance beyond 'self'.
const ImagePrefix = "/images/"

// Disk stores images in a directory and serves them from the store's own origin.
//
// It exists so that a shop needs no object storage at all: one binary, one
// directory, working product photographs. That is the right shape for a single VM
// with a volume, which is a large share of the deployments this project is for.
//
// The trade, stated plainly because it is the thing that will bite: **two instances
// do not share a directory.** Behind a load balancer, or on a platform that scales
// to zero and starts elsewhere, an image uploaded by one instance is a 404 from the
// other. Use a bucket for those. Nothing here detects that situation, because
// nothing here can.
type Disk struct {
	dir string
}

// NewDisk checks the directory is usable and returns Storage.
//
// Writability is checked now rather than at the first upload: a directory the
// server cannot write to is a misconfiguration, and finding out at boot beats
// finding out from an operator whose upload failed.
func NewDisk(dir string) (*Disk, error) {
	if dir == "" {
		return nil, errors.New("blob: image directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("blob: image directory %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("blob: create image directory %q: %w", abs, err)
	}

	probe, err := os.CreateTemp(abs, ".writable-*")
	if err != nil {
		return nil, fmt.Errorf("blob: image directory %q is not writable: %w", abs, err)
	}
	name := probe.Name()
	probe.Close()
	os.Remove(name)

	return &Disk{dir: abs}, nil
}

// Dir is the directory being served, for the handler that serves it.
func (d *Disk) Dir() string { return d.dir }

// Put writes the object, creating its parent directories.
//
// The write is to a temporary file and then a rename, which is atomic on every
// filesystem this will run on. A crash or a full disk halfway through therefore
// leaves no partial file for the storefront to serve as a broken image — the
// object either exists complete or does not exist.
func (d *Disk) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) (string, error) {
	full, err := d.resolve(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("blob: create directory for %s: %w", key, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(full), ".upload-*")
	if err != nil {
		return "", fmt.Errorf("blob: create temporary file for %s: %w", key, err)
	}
	tmpName := tmp.Name()
	// Removed unless the rename below claims it. Ignoring the error is right: by
	// then either the rename succeeded and there is nothing to remove, or the
	// failure being returned is the more interesting one.
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return "", fmt.Errorf("blob: write %s: %w", key, err)
	}
	// Synced before the rename, so the bytes are on the device and not only in the
	// page cache when the name starts pointing at them.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", fmt.Errorf("blob: sync %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("blob: close %s: %w", key, err)
	}
	// 0644: the file is about to be served publicly, and a mode nobody can read is
	// a confusing way to discover the difference between private and unreadable.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return "", fmt.Errorf("blob: chmod %s: %w", key, err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return "", fmt.Errorf("blob: install %s: %w", key, err)
	}
	return d.URL(key), nil
}

// Delete removes the object. A missing file is not an error, for the same reason it
// is not one in the bucket implementation: the caller wanted it gone.
func (d *Disk) Delete(_ context.Context, key string) error {
	full, err := d.resolve(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("blob: delete %s: %w", key, err)
	}

	// Take the product's directory with it when it empties, so a shop that replaces
	// images for years does not accumulate thousands of empty directories. A
	// non-empty directory fails here, which is exactly the intended no-op.
	os.Remove(filepath.Dir(full))
	return nil
}

// URL is the same-origin path the image is served at.
func (d *Disk) URL(key string) string { return ImagePrefix + key }

// resolve turns a key into a path inside the directory, refusing anything that
// would escape it.
//
// Keys come from ImageKey and are not user input, so this is belt and braces — but
// it is belt and braces on a function that writes to and deletes from the
// filesystem, which is where belt and braces belongs. filepath.IsLocal rejects
// absolute paths, "..", and the Windows special names.
func (d *Disk) resolve(key string) (string, error) {
	if key == "" || !filepath.IsLocal(key) {
		return "", fmt.Errorf("blob: refusing unsafe object key %q", key)
	}
	return filepath.Join(d.dir, filepath.FromSlash(key)), nil
}
