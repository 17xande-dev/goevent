package auth

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// cheapParams keeps the tests fast. DefaultParams costs 64 MiB and about a tenth
// of a second per call, which is right for one admin logging in and wrong for a
// test file that hashes dozens of times — the same job bcrypt.MinCost did.
var cheapParams = Params{Memory: 64, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}

const testPhrase = "correct horse battery staple"

func TestHashPassword_RoundTrips(t *testing.T) {
	hash, err := HashPassword(testPhrase, cheapParams)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(hash, "correct horse") {
		t.Fatal("the hash contains the password")
	}

	if !CheckPassword(hash, testPhrase) {
		t.Error("the correct password was rejected")
	}
	for _, wrong := range []string{
		"", "correct horse battery stapl", "Correct Horse Battery Staple",
		testPhrase + " ", " " + testPhrase,
	} {
		if CheckPassword(hash, wrong) {
			t.Errorf("password %q was accepted", wrong)
		}
	}

	if _, err := HashPassword("", cheapParams); err == nil {
		t.Error("HashPassword accepted an empty password")
	}
	// Incomplete parameters are an error rather than being silently defaulted: a
	// zero-value Params would otherwise produce a hash with no work behind it.
	if _, err := HashPassword(testPhrase, Params{}); err == nil {
		t.Error("HashPassword accepted zero-value parameters")
	}
}

func TestHashPassword_ProducesAPHCString(t *testing.T) {
	// The standard PHC encoding is the point of writing this by hand: it is what
	// makes the hash readable by any other argon2 implementation, which is the
	// property bcrypt's format is valued for.
	hash, err := HashPassword(testPhrase, cheapParams)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	if !strings.HasPrefix(hash, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Errorf("hash = %q, want the argon2id PHC prefix with the parameters in it", hash)
	}
	if parts := strings.Split(hash, "$"); len(parts) != 6 {
		t.Errorf("hash has %d $-separated parts, want 6: %q", len(parts), hash)
	}
	// No padding, as the format specifies.
	if strings.Contains(hash, "=") && !strings.Contains(hash, "v=") {
		t.Errorf("hash contains base64 padding: %q", hash)
	}
}

func TestHashPassword_IsSalted(t *testing.T) {
	a, err := HashPassword("same", cheapParams)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	b, err := HashPassword("same", cheapParams)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if a == b {
		t.Error("two hashes of the same password are identical; the salt is not doing its job")
	}
	// And each still verifies, which is what proves the salt is stored rather than
	// merely generated.
	if !CheckPassword(a, "same") || !CheckPassword(b, "same") {
		t.Error("a salted hash does not verify")
	}
}

func TestCheckPassword_StillAcceptsBcrypt(t *testing.T) {
	// The reason this exists: an existing deployment's ADMIN_PASSWORD_HASH must not
	// stop working the day the algorithm changed. The operator moves to argon2id
	// when they next run `make hashpw`, not on a flag day.
	legacy, err := bcrypt.GenerateFromPassword([]byte(testPhrase), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}

	if !CheckPassword(string(legacy), testPhrase) {
		t.Error("an existing bcrypt hash was rejected")
	}
	if CheckPassword(string(legacy), "wrong") {
		t.Error("a bcrypt hash accepted the wrong password")
	}
	if err := ParsePasswordHash(string(legacy)); err != nil {
		t.Errorf("ParsePasswordHash rejected a valid bcrypt hash: %v", err)
	}

	// New hashes are argon2id only — accepting bcrypt is a compatibility path, not
	// a choice on offer.
	fresh, err := HashPassword(testPhrase, cheapParams)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(fresh, "$argon2id$") {
		t.Errorf("HashPassword produced %q, want argon2id", fresh)
	}
}

func TestCheckPassword_FailsClosed(t *testing.T) {
	// Every one of these is a plain false rather than a panic or an accept. A login
	// form has nothing useful to do with the distinction, and a malformed hash must
	// never be a way in.
	bad := []string{
		"",
		"not-a-hash",
		"$argon2id$",
		"$argon2id$v=19$m=64,t=1,p=1$",
		"$argon2id$v=19$m=64,t=1,p=1$c2FsdA$", // empty key
		"$argon2id$v=19$m=64,t=1,p=1$$aGFzaA", // empty salt
		"$argon2id$v=18$m=64,t=1,p=1$c2FsdA$aGFzaA",           // wrong version
		"$argon2id$v=19$m=0,t=1,p=1$c2FsdA$aGFzaA",            // zero memory
		"$argon2id$v=19$m=64,t=0,p=1$c2FsdA$aGFzaA",           // zero passes
		"$argon2id$v=19$m=64,t=1,p=0$c2FsdA$aGFzaA",           // zero lanes
		"$argon2id$v=19$m=nonsense,t=1,p=1$c2FsdA$aGFzaA",     // unparseable
		"$argon2id$v=19$m=64,t=1,p=1$!!!not-base64!!!$aGFzaA", // bad salt
		"$argon2id$v=19$m=64,t=1,p=1$c2FsdA$!!!not-base64!!!", // bad key
		"$argon2i$v=19$m=64,t=1,p=1$c2FsdA$aGFzaA",            // the wrong variant
		"$2a$10$tooshort",
		"$scrypt$whatever",
		"plaintextpassword",
	}
	for _, hash := range bad {
		if CheckPassword(hash, "anything") {
			t.Errorf("hash %q accepted a password", hash)
		}
		if CheckPassword(hash, "") {
			t.Errorf("hash %q accepted an empty password", hash)
		}
	}
}

func TestCheckPassword_RefusesAbsurdMemory(t *testing.T) {
	// A mistyped ADMIN_PASSWORD_HASH claiming four gibibytes would otherwise try to
	// allocate it on the first login attempt and take the server down — a denial of
	// service delivered by a typo.
	hash := "$argon2id$v=19$m=4194304,t=3,p=4$c2FsdHNhbHRzYWx0c2E$aGFzaGhhc2hoYXNoaGFzaA"

	if CheckPassword(hash, "anything") {
		t.Error("a hash asking for 4 GiB was accepted")
	}
	err := ParsePasswordHash(hash)
	if !errors.Is(err, ErrPasswordFormat) {
		t.Errorf("ParsePasswordHash = %v, want ErrPasswordFormat", err)
	}
	if !strings.Contains(err.Error(), "memory") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

func TestParsePasswordHash(t *testing.T) {
	// This is what turns a mistyped hash into a boot failure with an explanation,
	// rather than an admin who can never sign in and no clue why.
	good, err := HashPassword(testPhrase, cheapParams)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := ParsePasswordHash(good); err != nil {
		t.Errorf("ParsePasswordHash rejected a hash it produced: %v", err)
	}

	for _, hash := range []string{"", "nonsense", "$argon2id$broken", "$2a$notacost$x"} {
		if err := ParsePasswordHash(hash); err == nil {
			t.Errorf("ParsePasswordHash accepted %q", hash)
		}
	}
	// The message has to tell the operator how to produce a good one.
	if err := ParsePasswordHash("nonsense"); !strings.Contains(err.Error(), "make hashpw") {
		t.Errorf("the error does not say how to generate a hash: %v", err)
	}
}

func TestDefaultParams_MeetOWASPMinimum(t *testing.T) {
	// OWASP's floor for argon2id is 19 MiB, two passes, one lane. Pinning it here
	// means lowering the parameters is a deliberate act with a failing test to
	// explain itself, not a quiet edit.
	if DefaultParams.Memory < 19*1024 {
		t.Errorf("Memory = %d KiB, below OWASP's 19 MiB minimum", DefaultParams.Memory)
	}
	if DefaultParams.Time < 2 {
		t.Errorf("Time = %d, below OWASP's minimum of 2", DefaultParams.Time)
	}
	if DefaultParams.Parallelism < 1 {
		t.Errorf("Parallelism = %d", DefaultParams.Parallelism)
	}
	if DefaultParams.SaltLength < 16 {
		t.Errorf("SaltLength = %d, want at least 16", DefaultParams.SaltLength)
	}
	if DefaultParams.KeyLength < 32 {
		t.Errorf("KeyLength = %d, want at least 32", DefaultParams.KeyLength)
	}
	// And the parameters are readable out of the hash, which is what lets them be
	// raised later without a migration.
	if DefaultParams.Memory > maxVerifyMemory {
		t.Errorf("DefaultParams asks for more memory than CheckPassword will allow")
	}
}
