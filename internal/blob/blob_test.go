package blob

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// Real magic bytes, because that is the only part of an upload that is evidence of
// anything. These are the shortest byte sequences http.DetectContentType accepts
// for each format.
var (
	jpegBytes = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01")
	pngBytes  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	gifBytes  = []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00")
	webpBytes = []byte("RIFF\x24\x00\x00\x00WEBPVP8 ")
)

func TestValidate_AcceptsImages(t *testing.T) {
	cases := map[string]struct {
		body            []byte
		wantType, wantX string
	}{
		"jpeg": {jpegBytes, "image/jpeg", ".jpg"},
		"png":  {pngBytes, "image/png", ".png"},
		"gif":  {gifBytes, "image/gif", ".gif"},
		"webp": {webpBytes, "image/webp", ".webp"},
	}
	for name, tc := range cases {
		gotType, gotExt, err := Validate(tc.body)
		if err != nil {
			t.Errorf("%s: Validate = %v", name, err)
			continue
		}
		if gotType != tc.wantType || gotExt != tc.wantX {
			t.Errorf("%s: got %q %q, want %q %q", name, gotType, gotExt, tc.wantType, tc.wantX)
		}
	}
}

func TestValidate_RefusesAnythingElse(t *testing.T) {
	// The bucket is publicly readable, so anything that is not an image is a file
	// served from a hostname you own with content somebody else chose. HTML there is
	// a cross-site scripting hole.
	cases := map[string][]byte{
		"html":        []byte("<!doctype html><html><body><script>alert(1)</script>"),
		"svg":         []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"plain text":  []byte("just some text, honestly"),
		"pdf":         []byte("%PDF-1.7\n%\xc7\xec\x8f\xa2\n"),
		"zip":         []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00"),
		"elf binary":  []byte("\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00"),
		"empty":       {},
		"nearly jpeg": []byte("\xff\xd8\xfe not quite"),
	}
	for name, body := range cases {
		gotType, gotExt, err := Validate(body)
		if !errors.Is(err, ErrUnsupportedType) {
			t.Errorf("%s was accepted as %q %q (err %v)", name, gotType, gotExt, err)
		}
	}
}

func TestValidate_IgnoresAClaimedType(t *testing.T) {
	// An upload named cat.jpg, announced as image/jpeg, containing HTML. Neither the
	// name nor the header is consulted, so it is refused on its contents.
	html := []byte("<!doctype html><h1>not a cat</h1>")
	if _, _, err := Validate(html); !errors.Is(err, ErrUnsupportedType) {
		t.Errorf("HTML dressed as a JPEG was accepted: %v", err)
	}

	// And the reverse: a real JPEG is accepted whatever it claims to be, because
	// the bytes are what matter.
	if _, ext, err := Validate(jpegBytes); err != nil || ext != ".jpg" {
		t.Errorf("a real JPEG was refused: %q, %v", ext, err)
	}
}

func TestImageKey(t *testing.T) {
	const eventID = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

	key, err := ImageKey(eventID, ".jpg")
	if err != nil {
		t.Fatalf("ImageKey: %v", err)
	}
	if !strings.HasPrefix(key, "events/"+eventID+"/") {
		t.Errorf("key %q is not under the event's prefix", key)
	}
	if !strings.HasSuffix(key, ".jpg") {
		t.Errorf("key %q lost its extension", key)
	}

	// A fresh key every time is what makes replacing an image work behind a CDN: a
	// new URL rather than a cache purge this store cannot perform.
	seen := map[string]bool{}
	for range 100 {
		k, err := ImageKey(eventID, ".jpg")
		if err != nil {
			t.Fatalf("ImageKey: %v", err)
		}
		if seen[k] {
			t.Fatalf("ImageKey repeated %q, so replacing an image would serve the cached old one", k)
		}
		seen[k] = true
	}

	// An extension without its dot is accepted, since callers get it from Validate
	// either way.
	if k, err := ImageKey(eventID, "png"); err != nil || !strings.HasSuffix(k, ".png") {
		t.Errorf("ImageKey(%q, \"png\") = %q, %v", eventID, k, err)
	}
}

func TestNewS3_ValidatesConfiguration(t *testing.T) {
	valid := S3Config{
		Endpoint: "localhost:9000", Bucket: "goevent",
		AccessKey: "key", SecretKey: "secret",
		PublicBaseURL: "http://localhost:9000/goevent",
	}
	s, err := NewS3(valid)
	if err != nil {
		t.Fatalf("a valid configuration was rejected: %v", err)
	}
	if got, want := s.URL("products/x/ab.jpg"), "http://localhost:9000/goevent/products/x/ab.jpg"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}

	// A trailing slash on the base must not produce a double slash in the URL.
	withSlash := valid
	withSlash.PublicBaseURL = "https://images.example/"
	s, err = NewS3(withSlash)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	if got, want := s.URL("a/b.jpg"), "https://images.example/a/b.jpg"; got != want {
		t.Errorf("URL = %q, want %q", got, want)
	}

	cases := map[string]func(*S3Config){
		"no endpoint":        func(c *S3Config) { c.Endpoint = "" },
		"no bucket":          func(c *S3Config) { c.Bucket = "" },
		"no access key":      func(c *S3Config) { c.AccessKey = "" },
		"no secret":          func(c *S3Config) { c.SecretKey = "" },
		"no public base":     func(c *S3Config) { c.PublicBaseURL = "" },
		"relative base":      func(c *S3Config) { c.PublicBaseURL = "/images" },
		"scheme in endpoint": func(c *S3Config) { c.Endpoint = "https://localhost:9000" },
	}
	for name, edit := range cases {
		cfg := valid
		edit(&cfg)
		if _, err := NewS3(cfg); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	// The scheme-in-endpoint message has to say what to do instead, because
	// minio-go's own error for it is opaque.
	cfg := valid
	cfg.Endpoint = "https://localhost:9000"
	if _, err := NewS3(cfg); err == nil || !strings.Contains(err.Error(), "BLOB_USE_TLS") {
		t.Errorf("the error does not point at BLOB_USE_TLS: %v", err)
	}
}

func TestUnconfigured_RefusesEverything(t *testing.T) {
	// Refusing, unlike email.Discard which reports success: an upload is something
	// an operator is doing and watching, so it must fail with a message rather than
	// appear to work.
	var s Storage = Unconfigured{}

	if _, err := s.Put(t.Context(), "k", strings.NewReader("x"), 1, "image/jpeg"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Put = %v, want ErrNotConfigured", err)
	}
	if err := s.Delete(t.Context(), "k"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("Delete = %v, want ErrNotConfigured", err)
	}
	if got := s.URL("k"); got != "" {
		t.Errorf("URL = %q, want empty", got)
	}
}

func TestFake(t *testing.T) {
	f := NewFake()

	url, err := f.Put(t.Context(), "products/a/1.jpg", bytes.NewReader(jpegBytes), int64(len(jpegBytes)), "image/jpeg")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if url != "https://images.example/products/a/1.jpg" {
		t.Errorf("Put returned %q", url)
	}

	// The bytes stored are the bytes sent, which is what lets a handler test assert
	// an upload arrived intact.
	obj, ok := f.Get("products/a/1.jpg")
	if !ok {
		t.Fatal("the object was not stored")
	}
	if !bytes.Equal(obj.Body, jpegBytes) {
		t.Error("the stored bytes differ from what was sent")
	}
	if obj.ContentType != "image/jpeg" {
		t.Errorf("ContentType = %q", obj.ContentType)
	}

	if err := f.Delete(t.Context(), "products/a/1.jpg"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := f.Get("products/a/1.jpg"); ok {
		t.Error("the object survived Delete")
	}
	// Deletions are recorded separately from the map, so a test can assert a
	// particular key was deleted rather than merely that it is absent — which it
	// would also be if it had never existed.
	if got := f.Deleted(); len(got) != 1 || got[0] != "products/a/1.jpg" {
		t.Errorf("Deleted() = %v", got)
	}

	f.Err = errors.New("bucket is unreachable")
	if _, err := f.Put(t.Context(), "k", strings.NewReader("x"), 1, "image/jpeg"); err == nil {
		t.Error("Put succeeded with Err set")
	}
	if err := f.Delete(t.Context(), "k"); err == nil {
		t.Error("Delete succeeded with Err set")
	}
}
