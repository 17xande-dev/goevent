// Package dbtest gives database-backed tests a migrated, private schema.
//
// Postgres has no t.TempDir() equivalent, so isolation is a schema per test:
// create one, point search_path at it, migrate into it, drop it on cleanup.
// When TEST_DATABASE_URL is unset the test skips, so `go test ./...` works for
// a drive-by contributor with no infrastructure running.
package dbtest

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/17xande-dev/goevent/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

// calls counts how many pools each test has asked for, so a second call gets a
// schema of its own.
//
// Without this, two Pool calls in one test derive the same schema name and the
// second silently drops and recreates the first one's tables — the first pool
// stays open and working, against an empty database, and whatever it had written
// is gone. That is a genuinely baffling failure to debug, and it has happened
// once already.
var calls sync.Map // test name -> *atomic.Int64

// Pool returns a private schema with all embedded migrations applied.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := EmptyPool(t)
	if err := db.Migrate(t.Context(), pool, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// EmptyPool provides an empty private schema for testing the migration runner.
func EmptyPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping database tests")
	}

	ctx := t.Context()
	schema := "test_" + sanitize(t.Name())
	counter, _ := calls.LoadOrStore(t.Name(), new(atomic.Int64))
	if n := counter.(*atomic.Int64).Add(1); n > 1 {
		schema = fmt.Sprintf("%s_%d", schema, n)
	}

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()
	if _, err := admin.Exec(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE; CREATE SCHEMA %s", schema, schema)); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	// public is on the path as well as the test's own schema, because pg_trgm's
	// operators live there: the initial migration installs the extension into public
	// explicitly, and an extension *name* is database-global, so only the first
	// test schema to run would otherwise own it and every other one would fail
	// with "operator does not exist: text <% text".
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ", public"

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect with search_path: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		// The test's context is already cancelled by now, so cleanup gets its
		// own — otherwise the schema leaks and the next run collides with it.
		ctx := context.WithoutCancel(ctx)
		cleanup, err := pgxpool.New(ctx, url)
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer cleanup.Close()
		if _, err := cleanup.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})

	return pool
}

func sanitize(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out = append(out, r)
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
