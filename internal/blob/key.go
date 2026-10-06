package blob

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// ImageKey returns the object key for a product image: a directory per product and
// a random name within it, e.g.
//
//	products/3f2504e0-4f89-41d3-9a0c-0305e82c3301/9f86d081b1e2.jpg
//
// The random component is what makes replacing an image work with a CDN in front of
// the bucket. A stable key would need a cache purge on every replacement — an
// operation this store has no credentials for and no way to verify — and until the
// purge happened, the old photograph would keep being served. A new key is a new
// URL, so the new image is visible immediately and the old one is deleted rather
// than invalidated.
//
// The product id in the path is for the human looking at a bucket listing, not for
// the code: nothing parses this key back apart.
func ImageKey(productID, ext string) (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("blob: generate object name: %w", err)
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return "products/" + productID + "/" + hex.EncodeToString(b) + ext, nil
}

// DownloadKey returns the object key for a purchasable file, e.g.
//
//	downloads/3f2504e0-4f89-41d3-9a0c-0305e82c3301/2b7e1516a0c1f2e3d4b5.mp4
//
// Sixteen random bytes rather than ImageKey's six. An image key is unguessable
// only as a convenience — the bucket is public, so guessing one wins nothing that
// a product page does not already give away. A download key names bytes somebody
// paid for, and although the bucket is private and every read is authorised, the
// key should not be the weak link if a bucket is ever misconfigured. This is
// defence in depth on the one thing that would be unrecoverable.
//
// The extension comes from the sniffed content type, never the uploaded filename,
// for the same reason it does for images: a filename is client-controlled.
func DownloadKey(productID, ext string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("blob: generate download name: %w", err)
	}
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return "downloads/" + productID + "/" + hex.EncodeToString(b) + ext, nil
}
