package db_test

import (
	"log/slog"
	"os"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/17xande-dev/goevent/internal/db"
	"github.com/17xande-dev/goevent/internal/dbtest"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func TestMigrate_BaselineAndRestart(t *testing.T) {
	pool := dbtest.EmptyPool(t)
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)
	before, err := db.Status(ctx, pool, log)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 || before[0].Source.Version != 1 {
		t.Fatalf("initial baseline missing: %+v", before)
	}
	for _, migration := range before {
		if migration.State == "applied" {
			t.Fatal("fresh database reports an applied migration")
		}
	}
	if err := db.Migrate(ctx, pool, log); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO events (slug, title, starts_at, ends_at)
		VALUES ('first-event', 'First event', now(), now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool, log); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE slug = 'first-event'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatal("restart erased data")
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_tables
		WHERE schemaname = current_schema() AND tablename <> 'goose_db_version'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count < 11 {
		t.Fatalf("application tables = %d, want at least 11", count)
	}
	after, err := db.Status(ctx, pool, log)
	if err != nil || len(after) != len(before) {
		t.Fatalf("migration status: %+v %v", after, err)
	}
	for _, migration := range after {
		if migration.State != "applied" {
			t.Fatalf("migration %d not applied", migration.Source.Version)
		}
	}
}

func TestMigrate_AppliesFutureChanges(t *testing.T) {
	pool := dbtest.EmptyPool(t)
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)
	files := fstest.MapFS{"m/0001_first.sql": {Data: []byte("-- +goose Up\nCREATE TABLE a (id INT);\n")}}
	if err := db.MigrateFS(ctx, pool, files, "m", log); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO a VALUES (7)"); err != nil {
		t.Fatal(err)
	}
	files["m/0002_later.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nALTER TABLE a ADD COLUMN label TEXT NOT NULL DEFAULT 'new';\n")}
	if err := db.MigrateFS(ctx, pool, files, "m", log); err != nil {
		t.Fatal(err)
	}
	var label string
	if err := pool.QueryRow(ctx, "SELECT label FROM a WHERE id = 7").Scan(&label); err != nil {
		t.Fatal(err)
	}
	if label != "new" {
		t.Fatal("future migration did not preserve and update existing data")
	}
}

func TestMigrate_RejectsOutOfOrder(t *testing.T) {
	pool := dbtest.EmptyPool(t)
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)
	files := fstest.MapFS{"m/0005_first.sql": {Data: []byte("-- +goose Up\nCREATE TABLE a (id INT);\n")}}
	if err := db.MigrateFS(ctx, pool, files, "m", log); err != nil {
		t.Fatal(err)
	}
	files["m/0003_earlier.sql"] = &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE b (id INT);\n")}
	if err := db.MigrateFS(ctx, pool, files, "m", log); err == nil {
		t.Fatal("out-of-order migration accepted")
	}
}

func TestMigrate_RollsBackFailedChange(t *testing.T) {
	pool := dbtest.EmptyPool(t)
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)
	files := fstest.MapFS{
		"m/0001_first.sql":  {Data: []byte("-- +goose Up\nCREATE TABLE a (id INT);\n")},
		"m/0002_broken.sql": {Data: []byte("-- +goose Up\nCREATE TABLE b (id INT);\nTHIS IS NOT SQL;\n")},
	}
	if err := db.MigrateFS(ctx, pool, files, "m", log); err == nil {
		t.Fatal("broken migration accepted")
	}
	var first, broken bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('a') IS NOT NULL, to_regclass('b') IS NOT NULL").Scan(&first, &broken); err != nil {
		t.Fatal(err)
	}
	if !first || broken {
		t.Fatalf("first applied=%v, broken applied=%v", first, broken)
	}
	var recorded int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM goose_db_version WHERE version_id = 2").Scan(&recorded); err != nil {
		t.Fatal(err)
	}
	if recorded != 0 {
		t.Fatal("failed migration recorded as applied")
	}
}

func TestMigrate_ConcurrentFirstBoots(t *testing.T) {
	pool := dbtest.EmptyPool(t)
	start := make(chan struct{})
	results := make(chan error, 4)
	var wg sync.WaitGroup
	for range cap(results) {
		wg.Go(func() { <-start; results <- db.Migrate(t.Context(), pool, slog.New(slog.DiscardHandler)) })
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), "SELECT count(*) FROM email_jobs"); err != nil {
		t.Fatal(err)
	}
}

func TestMigrate_BaselineDownAndUp(t *testing.T) {
	pool := dbtest.Pool(t)
	adapter := stdlib.OpenDBFromPool(pool)
	defer adapter.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, adapter, os.DirFS("migrations"))
	if err != nil {
		t.Fatal(err)
	}
	// The initial schema's foreign-key order must allow a complete local reset.
	if _, err := provider.Down(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), pool, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
}
