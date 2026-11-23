package handler

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// maxLoginKeyBytes keeps a login key inside the limiter's per-key size limit
// (256 bytes by default; a limiter refuses a longer key rather than tracking it).
const maxLoginKeyBytes = 200

// loginEmailKey is the key failed sign-ins are charged under: the address the
// attempt named, lower-cased. An address longer than any real one (the form
// accepts up to 1 MiB) is hashed, so each still gets its own bucket instead of
// being refused outright by the limiter.
func loginEmailKey(email string) string {
	key := "email:" + strings.ToLower(email)
	if len(key) <= maxLoginKeyBytes {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return "email-sha256:" + hex.EncodeToString(sum[:])
}
