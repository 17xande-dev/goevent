package blob

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// ImageKey returns the object key for an event image: a directory per event and
// a random name within it, e.g.
//
//	events/3f2504e0-4f89-41d3-9a0c-0305e82c3301/9f86d081b1e2.jpg
//
// The random component is what makes replacing an image work with a CDN in front of
// the bucket. A stable key would need a cache purge on every replacement — an
// operation this server has no credentials for and no way to verify — and until the
// purge happened, the old picture would keep being served. A new key is a new URL,
// so the new image is visible immediately and the old one is deleted rather than
// invalidated.
//
// The event id in the path is for the human looking at a bucket listing, not for
// the code: nothing parses this key back apart.
func ImageKey(eventID, ext string) (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("blob: generate object name: %w", err)
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return "events/" + eventID + "/" + hex.EncodeToString(b) + ext, nil
}
