package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/db"
	"github.com/17xande-dev/goevent/internal/dbtest"
	"github.com/17xande-dev/outbox/pgxstore"
)

// -migrate-status must show the outbox history too: it is a separate table, and
// otherwise a failed or missing outbox migration is invisible to an operator.
func TestPrintMigrationStatus_ListsTheOutboxHistoryAfterTheApplicationOnes(t *testing.T) {
	pool := dbtest.EmptyPool(t)
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)

	var out bytes.Buffer
	if err := printMigrationStatus(ctx, &out, pool, log); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	last := lines[len(lines)-1]
	if !strings.HasPrefix(last, "1 ") || !strings.Contains(last, "pending") || !strings.Contains(last, "outbox outbox_goose_version") {
		t.Errorf("fresh database: last line %q; full output:\n%s", last, out.String())
	}
	if strings.Contains(out.String(), "applied") {
		t.Errorf("a fresh database reports an applied migration:\n%s", out.String())
	}
	var created bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('outbox_goose_version') IS NOT NULL").Scan(&created); err != nil || created {
		t.Fatalf("asking for the status created the history table: %v %v", created, err)
	}

	if err := db.Migrate(ctx, pool, log); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := printMigrationStatus(ctx, &out, pool, log); err != nil {
		t.Fatal(err)
	}
	lines = strings.Split(strings.TrimSpace(out.String()), "\n")
	last = lines[len(lines)-1]
	if !strings.Contains(last, "applied") || !strings.Contains(last, "outbox outbox_goose_version") {
		t.Errorf("migrated database: last line %q; full output:\n%s", last, out.String())
	}
	for _, l := range lines[:len(lines)-1] {
		if strings.Contains(l, "outbox") || !strings.Contains(l, "applied") {
			t.Errorf("application line %q", l)
		}
	}
}

func TestPrintMigrationStatus_UsesTheOutboxHistoryTimestamp(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx := t.Context()
	before, err := pgxstore.MigrationStatus(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	appliedAt := time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC)
	// Goose's Provider counts any row as applied. The false flag distinguishes
	// the library's status from the former app helper, which read the flag.
	if _, err := pool.Exec(ctx, `UPDATE outbox_goose_version SET is_applied = false, tstamp = $2::timestamptz WHERE version_id = $1`, before.Latest, appliedAt); err != nil {
		t.Fatal(err)
	}
	status, err := pgxstore.MigrationStatus(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	latest := status.Migrations[len(status.Migrations)-1]
	if latest.Version != before.Latest || !latest.Applied || !latest.AppliedAt.Equal(appliedAt) {
		t.Fatalf("outbox migration from fixture: got %+v, want version %d applied at %s", latest, before.Latest, appliedAt)
	}

	var out bytes.Buffer
	if err := printMigrationStatus(ctx, &out, pool, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%-6d %-10s %-25s outbox outbox_goose_version\n", latest.Version, "applied", appliedAt.Format(time.RFC3339))
	if !strings.HasSuffix(out.String(), want) {
		t.Errorf("status must end with %q; full output:\n%s", want, out.String())
	}
}

func TestPrintMigrationStatus_RefusesInvalidOutboxHistory(t *testing.T) {
	tests := []struct {
		name, change, wantError string
	}{
		{"newer", "INSERT INTO outbox_goose_version (version_id, is_applied) SELECT max(version_id) + 1, true FROM outbox_goose_version", "newer than this build"},
		{"empty", "DELETE FROM outbox_goose_version", "missing zero version migration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := dbtest.Pool(t)
			ctx := t.Context()
			if _, err := pool.Exec(ctx, tt.change); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			err := printMigrationStatus(ctx, &out, pool, slog.New(slog.DiscardHandler))
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Errorf("status error = %v, want %q; output:\n%s", err, tt.wantError, out.String())
			}
		})
	}
}
