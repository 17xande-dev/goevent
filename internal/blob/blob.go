// Package blob stores product images in object storage.
//
// The Storage interface is deliberately three methods wide. The store needs to put
// an image somewhere public, remove it again, and know its URL — nothing else. No
// listing, no copying, no multipart uploads: a shop with a few hundred product
// photos does not need them, and every method on an interface is a method every
// future implementation has to provide.
//
// # Reads never come through here
//
// An uploaded image's URL points straight at the bucket's public hostname, so the
// bytes are served by whatever CDN sits in front of it and never pass through Go.
// That is why there is a URL method and no Get: the storefront links to images, it
// does not proxy them.
//
// The trade is that the bucket has to be publicly readable, which is why Put
// refuses anything it cannot prove is an image — see Validate. An attacker who can
// put HTML on a hostname you own has something worth having.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Storage puts and removes objects, and knows their public URLs.
type Storage interface {
	// Put stores size bytes read from r under key, with the given content type,
	// and returns the public URL to fetch it from.
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (string, error)

	// Delete removes an object. Deleting something that is not there is not an
	// error: the caller's intent — "this should not exist" — is satisfied either
	// way, and a retry should not fail.
	Delete(ctx context.Context, key string) error

	// URL is the public URL for a key, computed without touching the network.
	URL(key string) string
}

// MaxUploadBytes is the default cap on an uploaded image. Product photographs are
// hundreds of kilobytes; the limit exists so an authenticated but careless
// operator cannot post a 200 MB TIFF and have the server buffer it.
const MaxUploadBytes int64 = 5 << 20

// ErrUnsupportedType is returned for an upload that is not an image this store
// will serve.
var ErrUnsupportedType = errors.New("blob: not a supported image type")

// allowedTypes maps a sniffed content type to the extension it is stored under.
//
// The extension comes from the *sniffed* type and never from the uploaded
// filename, which is the whole point of this map. A bucket that is publicly
// readable and will happily serve `evil.html` because somebody named their upload
// that is a cross-site scripting hole on a hostname you own.
var allowedTypes = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// Validate sniffs the leading bytes of an upload and returns the content type to
// store it as, together with the file extension for its key.
//
// It ignores both the browser's Content-Type header and the filename: the first is
// client-controlled and the second is worse. http.DetectContentType reads magic
// bytes, which is the only part of an upload that is evidence of anything.
func Validate(head []byte) (contentType, ext string, err error) {
	sniffed := http.DetectContentType(head)
	// DetectContentType may append parameters, e.g. "text/plain; charset=utf-8".
	base, _, _ := strings.Cut(sniffed, ";")
	base = strings.TrimSpace(base)

	ext, ok := allowedTypes[base]
	if !ok {
		return "", "", fmt.Errorf("%w: %s", ErrUnsupportedType, base)
	}
	return base, ext, nil
}

// SupportedTypes lists the acceptable types, for a form's accept attribute and for
// the message shown when an upload is refused.
func SupportedTypes() []string {
	return []string{"image/jpeg", "image/png", "image/gif", "image/webp"}
}
