package blob

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisk_PutGetURLAndDelete(t *testing.T) {
	dir := t.TempDir()
	d, err := NewDisk(dir)
	if err != nil {
		t.Fatalf("NewDisk: %v", err)
	}

	const key = "products/3f2504e0/9f86d081b1e2.jpg"
	url, err := d.Put(t.Context(), key, bytes.NewReader(jpegBytes), int64(len(jpegBytes)), "image/jpeg")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// A same-origin path, which is what lets the CSP be img-src 'self' with no
	// external origin allowed at all.
	if url != "/images/"+key {
		t.Errorf("Put returned %q, want /images/%s", url, key)
	}
	if url != d.URL(key) {
		t.Errorf("Put and URL disagree: %q vs %q", url, d.URL(key))
	}
	if strings.Contains(url, "http") {
		t.Errorf("URL %q is absolute; it should be same-origin so BASE_URL cannot make it wrong", url)
	}

	// The bytes are on disk, under the key, complete.
	onDisk, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(onDisk, jpegBytes) {
		t.Error("the stored bytes differ from what was written")
	}
	// Readable, because it is about to be served: a mode nobody can read is a
	// confusing way to discover the difference between private and unreadable.
	info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(key)))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o044 == 0 {
		t.Errorf("mode is %v, which the server cannot serve", info.Mode().Perm())
	}

	if err := d.Delete(t.Context(), key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(key))); !os.IsNotExist(err) {
		t.Error("the file survived Delete")
	}
	// Deleting again is not an error: the caller wanted it gone and it is gone.
	if err := d.Delete(t.Context(), key); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

func TestDisk_PutLeavesNoTemporaryFiles(t *testing.T) {
	// The write is temp-then-rename so a crash cannot leave a truncated image being
	// served. The temporary file must not survive the successful case either.
	dir := t.TempDir()
	d, err := NewDisk(dir)
	if err != nil {
		t.Fatalf("NewDisk: %v", err)
	}

	const key = "products/abc/def.png"
	if _, err := d.Put(t.Context(), key, bytes.NewReader(pngBytes), int64(len(pngBytes)), "image/png"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "products", "abc"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("%d files in the product directory, want 1: %v", len(entries), names)
	}
	if entries[0].Name() != "def.png" {
		t.Errorf("the stored file is %q, want def.png", entries[0].Name())
	}
}

func TestDisk_DeleteTidiesTheProductDirectory(t *testing.T) {
	// A shop that replaces images for years should not accumulate thousands of empty
	// directories.
	dir := t.TempDir()
	d, err := NewDisk(dir)
	if err != nil {
		t.Fatalf("NewDisk: %v", err)
	}

	if _, err := d.Put(t.Context(), "products/one/a.jpg", bytes.NewReader(jpegBytes), 0, "image/jpeg"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := d.Put(t.Context(), "products/two/a.jpg", bytes.NewReader(jpegBytes), 0, "image/jpeg"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := d.Put(t.Context(), "products/two/b.jpg", bytes.NewReader(jpegBytes), 0, "image/jpeg"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Emptying a directory removes it.
	if err := d.Delete(t.Context(), "products/one/a.jpg"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "products", "one")); !os.IsNotExist(err) {
		t.Error("an emptied product directory was left behind")
	}

	// A directory with another image in it is left alone — the tidy-up is a no-op
	// rather than a hazard.
	if err := d.Delete(t.Context(), "products/two/a.jpg"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "products", "two", "b.jpg")); err != nil {
		t.Errorf("deleting one image took its sibling with it: %v", err)
	}
}

func TestDisk_RefusesKeysThatEscapeTheDirectory(t *testing.T) {
	// Keys come from ImageKey and are not user input, so this is belt and braces — on
	// a function that writes to and deletes from the filesystem, which is where belt
	// and braces belongs.
	dir := t.TempDir()
	d, err := NewDisk(dir)
	if err != nil {
		t.Fatalf("NewDisk: %v", err)
	}

	outside := filepath.Join(filepath.Dir(dir), "escaped.txt")
	bad := []string{
		"",
		"../escaped.txt",
		"products/../../escaped.txt",
		"/etc/passwd",
		"/absolute.jpg",
		"products/./../../escaped.txt",
	}
	for _, key := range bad {
		if _, err := d.Put(t.Context(), key, strings.NewReader("x"), 1, "image/jpeg"); err == nil {
			t.Errorf("Put accepted key %q", key)
		}
		if err := d.Delete(t.Context(), key); err == nil {
			t.Errorf("Delete accepted key %q", key)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("a key escaped the image directory and wrote outside it")
	}
}

func TestNewDisk_ChecksTheDirectory(t *testing.T) {
	// Creating it is a convenience; the writability probe is the point. A directory
	// the server cannot write to is a misconfiguration, and boot is a better place to
	// find out than an operator's failed upload.
	nested := filepath.Join(t.TempDir(), "a", "b", "images")
	d, err := NewDisk(nested)
	if err != nil {
		t.Fatalf("NewDisk did not create a nested directory: %v", err)
	}
	if _, err := os.Stat(nested); err != nil {
		t.Errorf("the directory was not created: %v", err)
	}
	// The path is absolute afterwards, so a later change of working directory cannot
	// move the store's images.
	if !filepath.IsAbs(d.Dir()) {
		t.Errorf("Dir() = %q, want an absolute path", d.Dir())
	}

	if _, err := NewDisk(""); err == nil {
		t.Error("NewDisk accepted an empty directory")
	}

	// A directory that cannot be written to is refused. Skipped as root, which can
	// write to it regardless.
	if os.Geteuid() == 0 {
		t.Skip("running as root, so an unwritable directory is still writable")
	}
	readonly := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(readonly, 0o500); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if _, err := NewDisk(readonly); err == nil {
		t.Error("NewDisk accepted a directory it cannot write to")
	}
}

func TestDisk_SatisfiesStorage(t *testing.T) {
	// The whole point of the interface: the handler does not know which of these it
	// has.
	dir := t.TempDir()
	d, err := NewDisk(dir)
	if err != nil {
		t.Fatalf("NewDisk: %v", err)
	}
	var _ Storage = d
	var _ Storage = &S3{}
	var _ Storage = Unconfigured{}
	var _ Storage = NewFake()
}
