package main

import (
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/auth"
)

func TestReadPassword(t *testing.T) {
	// A piped `printf %s` sends no newline; a typed line does. Both are the
	// documented way to use this, so both must work.
	for _, in := range []string{"correct horse", "correct horse\n", "correct horse\r\n"} {
		got, err := readPassword(strings.NewReader(in))
		if err != nil {
			t.Errorf("readPassword(%q): %v", in, err)
			continue
		}
		if got != "correct horse" {
			t.Errorf("readPassword(%q) = %q", in, got)
		}
	}

	// Spaces are legitimate in a passphrase and must survive.
	if got, _ := readPassword(strings.NewReader("  padded  ")); got != "  padded  " {
		t.Errorf("readPassword trimmed a passphrase to %q", got)
	}

	for _, in := range []string{"", "\n"} {
		if _, err := readPassword(strings.NewReader(in)); err == nil {
			t.Errorf("readPassword(%q) accepted an empty password", in)
		}
	}
}

func TestHashedPasswordVerifies(t *testing.T) {
	// The hash this command prints must be one the server accepts, which is the
	// whole point of the command existing.
	// Cheap parameters: this asserts the round trip, not how expensive the hash is.
	cheap := auth.Params{Memory: 64, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
	hash, err := auth.HashPassword("correct horse battery staple", cheap)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !auth.CheckPassword(hash, "correct horse battery staple") {
		t.Error("the generated hash does not verify the password it was made from")
	}
	// And it is a hash the server will accept at boot rather than refuse.
	if err := auth.ParsePasswordHash(hash); err != nil {
		t.Errorf("the generated hash would fail the boot-time check: %v", err)
	}
}
