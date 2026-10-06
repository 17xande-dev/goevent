package main

import (
	"log/slog"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/goevent/internal/dbtest"
)

func TestStartSessionCleanup_SweepsExpiredSessionsOnBoot(t *testing.T) {
	pool := dbtest.Pool(t)
	users := auth.NewStore(pool)
	ctx := t.Context()

	owner, err := users.Create(ctx, "owner@example.com", "The Owner",
		mustHash(t, "correct horse battery"), auth.RoleOwner, false)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	live, _, err := users.IssueSession(ctx, owner.ID, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	if _, _, err := users.IssueSession(ctx, owner.ID, time.Nanosecond); err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	startSessionCleanup(ctx, users, slog.New(slog.DiscardHandler))

	// The sweep runs immediately, so a restart after a busy month does the work
	// rather than waiting an hour for the first tick.
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := users.CountSessionsForUser(ctx, owner.ID)
		if err != nil {
			t.Fatalf("CountSessionsForUser: %v", err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sessions = %d after the boot sweep, want only the live one", n)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Housekeeping, not a security control: the live session is untouched, and the
	// expired one was already refused on read before this ran.
	if _, _, err := users.Session(ctx, live); err != nil {
		t.Errorf("the sweep took the live session too: %v", err)
	}
}

// mustHash is argon2id at its cheapest; this test is about the sweep.
func mustHash(t *testing.T, password string) string {
	t.Helper()
	h, err := auth.HashPassword(password,
		auth.Params{Memory: 64, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return h
}
