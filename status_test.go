package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/db"
	"github.com/17xande-dev/goevent/internal/dbtest"
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
