package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

// The admin password is hashed with argon2id, which OWASP puts first and bcrypt
// second. The KDF is x/crypto's — already a dependency — so this costs no new
// module; what is written here is the PHC string encoding, which is string
// formatting rather than cryptography, and whose failure mode is "the hash does
// not verify" rather than a quiet weakness.
//
// # Both formats verify
//
// CheckPassword accepts a bcrypt hash as well as an argon2id one, dispatching on
// the prefix. That is not indecision: it means an existing deployment's
// ADMIN_PASSWORD_HASH keeps working across this change, and an operator moves to
// argon2id when they next run `make hashpw` rather than on a flag day. New hashes
// are only ever argon2id.

// Params are argon2id's cost parameters.
//
// They are exported because tests need cheap ones — the same job bcrypt.MinCost
// used to do. Production code passes DefaultParams.
type Params struct {
	// Memory in KiB, and the parameter that actually does the work: argon2's
	// resistance to custom hardware comes from needing this much memory per
	// guess.
	Memory uint32
	// Time is the number of passes over that memory.
	Time uint32
	// Parallelism is the number of lanes. More lanes on fewer cores simply
	// serialise; they do not weaken the hash.
	Parallelism uint8

	SaltLength uint32
	KeyLength  uint32
}

// DefaultParams is RFC 9106's second recommended configuration: 64 MiB, three
// passes, four lanes. It costs on the order of a tenth of a second and 64 MiB per
// verification, which is affordable because this store has one admin, logs in
// rarely, and — since the hardening phase — rate-limits the attempt.
//
// The parameters are encoded in the hash, so raising them later needs no
// migration: old hashes keep verifying at their own settings and the next
// `make hashpw` writes the new ones.
var DefaultParams = Params{
	Memory:      64 * 1024,
	Time:        3,
	Parallelism: 4,
	SaltLength:  16,
	KeyLength:   32,
}

// maxVerifyMemory caps the memory a *stored* hash may ask for at verification
// time. Without it a mistyped ADMIN_PASSWORD_HASH claiming m=4194304 would try to
// allocate four gibibytes on the first login attempt and take the server with it.
const maxVerifyMemory = 1 << 20 // 1 GiB in KiB

// ErrPasswordFormat means a stored hash is not in a format this package can read.
var ErrPasswordFormat = errors.New("auth: unrecognised password hash format")

// HashPassword returns an argon2id PHC string for storing in
// ADMIN_PASSWORD_HASH.
func HashPassword(password string, p Params) (string, error) {
	if password == "" {
		return "", errors.New("auth: password must not be empty")
	}
	if p.Memory == 0 || p.Time == 0 || p.Parallelism == 0 || p.SaltLength == 0 || p.KeyLength == 0 {
		return "", fmt.Errorf("auth: argon2id parameters are incomplete: %+v", p)
	}

	salt := make([]byte, p.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}

	key := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Parallelism, p.KeyLength)

	// The standard PHC encoding, which is what makes this hash readable by any
	// other argon2 implementation — the property bcrypt's format is valued for.
	// base64 without padding, as the format specifies.
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword reports whether password matches the stored hash, in either
// format.
//
// Every failure — wrong password, malformed hash, absurd parameters — is a plain
// false. A login form has nothing useful to do with the distinction, and telling
// an attacker which of the two went wrong is worse than useless. A malformed hash
// is worth an operator's attention, so ParsePasswordHash exists for the boot-time
// check that provides it.
func CheckPassword(hash, password string) bool {
	switch {
	case strings.HasPrefix(hash, "$argon2id$"):
		p, salt, want, err := parseArgon2id(hash)
		if err != nil {
			return false
		}
		got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Parallelism, p.KeyLength)
		// Constant time, because the comparison is against a secret-derived value
		// and an early return would leak how much of it matched.
		return subtle.ConstantTimeCompare(got, want) == 1

	case strings.HasPrefix(hash, "$2"):
		// bcrypt: $2a$, $2b$ or $2y$. Kept so an existing deployment's hash does
		// not stop working the day this package changed.
		return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil

	default:
		return false
	}
}

// ParsePasswordHash checks that a stored hash is one this package can verify, so
// a mistyped ADMIN_PASSWORD_HASH is a boot failure rather than an admin who can
// never sign in and no explanation of why.
func ParsePasswordHash(hash string) error {
	switch {
	case strings.HasPrefix(hash, "$argon2id$"):
		_, _, _, err := parseArgon2id(hash)
		return err
	case strings.HasPrefix(hash, "$2"):
		if _, err := bcrypt.Cost([]byte(hash)); err != nil {
			return fmt.Errorf("auth: bcrypt hash is malformed: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("%w: expected a $argon2id$ or $2a$ hash (generate one with `make hashpw`)",
			ErrPasswordFormat)
	}
}

// parseArgon2id reads a PHC string back into its parts. It is deliberately strict:
// anything unexpected is an error rather than a default, because a hash is not a
// place to be forgiving.
func parseArgon2id(hash string) (p Params, salt, key []byte, err error) {
	// "$argon2id$v=19$m=65536,t=3,p=4$<salt>$<key>" splits into six, the first
	// empty.
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, fmt.Errorf("%w: not a well-formed argon2id string", ErrPasswordFormat)
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return p, nil, nil, fmt.Errorf("%w: unreadable version %q", ErrPasswordFormat, parts[2])
	}
	if version != argon2.Version {
		return p, nil, nil, fmt.Errorf("%w: argon2 version %d, want %d",
			ErrPasswordFormat, version, argon2.Version)
	}

	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Parallelism); err != nil {
		return p, nil, nil, fmt.Errorf("%w: unreadable parameters %q", ErrPasswordFormat, parts[3])
	}
	if p.Memory == 0 || p.Time == 0 || p.Parallelism == 0 {
		return p, nil, nil, fmt.Errorf("%w: zero parameter in %q", ErrPasswordFormat, parts[3])
	}
	if p.Memory > maxVerifyMemory {
		return p, nil, nil, fmt.Errorf("%w: hash asks for %d KiB of memory, more than the %d KiB limit",
			ErrPasswordFormat, p.Memory, maxVerifyMemory)
	}

	if salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil {
		return p, nil, nil, fmt.Errorf("%w: undecodable salt", ErrPasswordFormat)
	}
	if key, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil {
		return p, nil, nil, fmt.Errorf("%w: undecodable hash", ErrPasswordFormat)
	}
	if len(salt) == 0 || len(key) == 0 {
		return p, nil, nil, fmt.Errorf("%w: empty salt or hash", ErrPasswordFormat)
	}

	p.SaltLength, p.KeyLength = uint32(len(salt)), uint32(len(key))
	return p, salt, key, nil
}
