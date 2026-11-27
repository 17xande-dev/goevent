package db_test

import (
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/17xande-dev/goevent/internal/db"
	"github.com/17xande-dev/goevent/internal/dbtest"
	"github.com/jackc/pgx/v5/pgxpool"
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
		WHERE schemaname = current_schema()
		AND tablename NOT IN ('goose_db_version', 'outbox_goose_version', 'outbox_jobs')`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count < 10 {
		t.Fatalf("application tables = %d, want at least 10 after removing email_jobs", count)
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
	if _, err := pool.Exec(t.Context(), "SELECT count(*) FROM registrations"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "SELECT count(*) FROM outbox_jobs"); err != nil {
		t.Fatal(err)
	}
}

func TestMigrate_OutboxWaitsForApplicationSchema(t *testing.T) {
	pool := dbtest.EmptyPool(t)
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)
	// A conflicting application table makes the baseline fail. The outbox
	// must still be untouched, including its independent migration history.
	if _, err := pool.Exec(ctx, "CREATE TABLE events (id INT)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool, log); err == nil {
		t.Fatal("conflicting application baseline succeeded")
	}
	var installed bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('outbox_goose_version') IS NOT NULL").Scan(&installed); err != nil {
		t.Fatal(err)
	}
	if installed {
		t.Fatal("outbox migration ran before the application migration succeeded")
	}
}

func TestMigrate_DropsLegacyQueueWithoutLosingRegistrations(t *testing.T) {
	pool := dbtest.EmptyPool(t)
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)
	if err := db.MigrateFS(ctx, pool, previousSchema(t), "m", log); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		WITH e AS (
			INSERT INTO events (slug, title, starts_at, ends_at)
			VALUES ('camp', 'Camp', now(), now() + interval '1 hour')
			RETURNING id
		), r AS (
			INSERT INTO registrations (event_id, reference, contact_first_name, contact_last_name,
				contact_email, total_cents, currency, hold_expires_at, checkout_key)
			SELECT id, 'REF123', 'Ada', 'Lovelace', 'ada@example.com', 0, 'ZAR', now(), 'k' FROM e
			RETURNING id
		)
		INSERT INTO email_jobs (registration_id, kind, payload)
		SELECT id, 'confirmation', '\x01' FROM r
	`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool, log); err != nil {
		t.Fatal(err)
	}
	var legacy bool
	var registrations, jobs int
	err := pool.QueryRow(ctx, `SELECT to_regclass('email_jobs') IS NOT NULL,
		(SELECT count(*) FROM registrations WHERE reference = 'REF123'), (SELECT count(*) FROM outbox_jobs)`).
		Scan(&legacy, &registrations, &jobs)
	if err != nil || legacy || registrations != 1 || jobs != 0 {
		t.Fatalf("legacy table = %v, registrations = %d, imported jobs = %d, error = %v", legacy, registrations, jobs, err)
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
	// The application's foreign-key order must allow every application Down.
	// The outbox keeps its own schema and separate migration history.
	if _, err := provider.DownTo(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(t.Context(), pool, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
}

// previousSchema is the database as the binary before the last_seen migration
// left it: migration 0001 alone, as that binary embeds it.
func previousSchema(t *testing.T) fstest.MapFS {
	t.Helper()
	data, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{"m/0001_init.sql": {Data: data}}
}

// tableCounts is the row count of every application table, so a migration that
// loses or invents a row anywhere is caught, not only in the table it alters.
func tableCounts(t *testing.T, pool *pgxpool.Pool) map[string]int {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT tablename FROM pg_catalog.pg_tables
		WHERE schemaname = current_schema() AND tablename <> 'goose_db_version' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	counts := map[string]int{}
	for _, name := range tables {
		var n int
		if err := pool.QueryRow(t.Context(), fmt.Sprintf("SELECT count(*) FROM %s", name)).Scan(&n); err != nil {
			t.Fatal(err)
		}
		counts[name] = n
	}
	return counts
}

// A realistic database from the previous schema (a row in every table, live and
// idle sessions) takes the last_seen migration without losing anything, signs
// nobody out, rolls back to the previous schema, and migrates again.
func TestMigrate_AdminSessionsLastSeen(t *testing.T) {
	pool := dbtest.EmptyPool(t)
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)
	if err := db.MigrateFS(ctx, pool, previousSchema(t), "m", log); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`INSERT INTO admin_users (id, email, name, password_hash, role) VALUES
			('00000000-0000-0000-0000-0000000000a1', 'owner@example.com', 'Owner', 'x', 'owner')`,
		// One session an hour old with a day left, and one issued a minute ago.
		`INSERT INTO admin_sessions (token_hash, user_id, created_at, expires_at) VALUES
			('\x01', '00000000-0000-0000-0000-0000000000a1', now() - interval '1 hour', now() + interval '23 hours'),
			('\x02', '00000000-0000-0000-0000-0000000000a1', now() - interval '1 minute', now() + interval '23 hours 59 minutes')`,
		`INSERT INTO admin_setup (token_hash, consumed_at) VALUES ('\x03', now())`,
		`INSERT INTO events (id, slug, title, starts_at, ends_at) VALUES
			('00000000-0000-0000-0000-0000000000e1', 'camp', 'Camp', now(), now() + interval '1 day')`,
		`INSERT INTO ticket_types (id, event_id, name, price_cents) VALUES
			('00000000-0000-0000-0000-0000000000f1', '00000000-0000-0000-0000-0000000000e1', 'Adult', 5000)`,
		`INSERT INTO questions (id, event_id, scope, kind, label) VALUES
			('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000e1', 'registration', 'text', 'Diet?')`,
		`INSERT INTO registrations (id, event_id, reference, contact_first_name, contact_last_name, contact_email,
			total_cents, currency, hold_expires_at, checkout_key) VALUES
			('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-0000000000e1', 'ABC123', 'A', 'B',
			 'a@example.com', 5000, 'ZAR', now() + interval '1 hour', 'k1')`,
		`INSERT INTO attendees (id, registration_id, ticket_type_id, first_name, last_name, ticket_name, unit_price_cents) VALUES
			('00000000-0000-0000-0000-0000000000d1', '00000000-0000-0000-0000-0000000000c1',
			 '00000000-0000-0000-0000-0000000000f1', 'A', 'B', 'Adult', 5000)`,
		`INSERT INTO answers (registration_id, attendee_id, question_id, label, value) VALUES
			('00000000-0000-0000-0000-0000000000c1', '00000000-0000-0000-0000-0000000000d1',
			 '00000000-0000-0000-0000-0000000000b1', 'Diet?', 'none')`,
		`INSERT INTO payments (registration_id, method, amount_cents, currency) VALUES
			('00000000-0000-0000-0000-0000000000c1', 'eft', 5000, 'ZAR')`,
		`INSERT INTO email_jobs (registration_id, kind, payload) VALUES
			('00000000-0000-0000-0000-0000000000c1', 'confirmation', '\x04')`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	before := tableCounts(t, pool)
	for name, n := range before {
		if n == 0 {
			t.Fatalf("fixture has no rows in %s", name)
		}
	}

	// Exercise this exact upgrade, even when the current binary has newer
	// migrations that deliberately change other tables.
	lastSeenSQL, err := os.ReadFile("migrations/0002_admin_sessions_last_seen.sql")
	if err != nil {
		t.Fatal(err)
	}
	throughLastSeen := previousSchema(t)
	throughLastSeen["m/0002_admin_sessions_last_seen.sql"] = &fstest.MapFile{Data: lastSeenSQL}
	started := time.Now()
	if err := db.MigrateFS(ctx, pool, throughLastSeen, "m", log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if after := tableCounts(t, pool); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("row counts changed by the migration: before %v, after %v", before, after)
	}
	// Existing sessions survive with last_seen backfilled to the migration time
	// (so each starts its idle clock then), and created_at/expires_at untouched.
	rows, err := pool.Query(ctx, `SELECT last_seen, expires_at - created_at FROM admin_sessions ORDER BY token_hash`)
	if err != nil {
		t.Fatal(err)
	}
	var seen []time.Time
	for rows.Next() {
		var at time.Time
		var span time.Duration
		if err := rows.Scan(&at, &span); err != nil {
			t.Fatal(err)
		}
		if at.Before(started.Add(-time.Second)) || at.After(time.Now().Add(time.Second)) {
			t.Errorf("last_seen %s is not the migration time (%s..%s)", at, started, time.Now())
		}
		if span != 24*time.Hour {
			t.Errorf("session lifetime span %s, want the stored 24h", span)
		}
		seen = append(seen, at)
	}
	rows.Close()
	if len(seen) != 2 {
		t.Fatalf("sessions after migration = %d, want 2", len(seen))
	}

	// A second boot is a no-op.
	if err := db.MigrateFS(ctx, pool, throughLastSeen, "m", log); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	// The previous binary (which embeds only 0001) against the migrated database:
	// goose tolerates a database ahead of its embedded migrations, and its INSERT
	// (no last_seen) still works because the column has a default.
	if err := db.MigrateFS(ctx, pool, previousSchema(t), "m", log); err != nil {
		t.Errorf("previous binary refused the migrated database: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO admin_sessions (token_hash, user_id, expires_at)
		VALUES ('\x05', '00000000-0000-0000-0000-0000000000a1', now() + interval '1 hour')`); err != nil {
		t.Errorf("previous binary's session insert failed on the new schema: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM admin_sessions WHERE token_hash = '\x05'`); err != nil {
		t.Fatal(err)
	}

	// Down returns to the previous goose version and the previous schema, with
	// every session still there.
	adapter := stdlib.OpenDBFromPool(pool)
	defer adapter.Close()
	migrations, err := fs.Sub(throughLastSeen, "m")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, adapter, migrations)
	if err != nil {
		t.Fatal(err)
	}
	status, err := provider.Status(ctx)
	if err != nil || len(status) != 2 || status[1].State != "applied" {
		t.Fatalf("status after two boots: %+v %v", status, err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatalf("down: %v", err)
	}
	if v, err := provider.GetDBVersion(ctx); err != nil || v != 1 {
		t.Fatalf("version after down = %d, %v; want 1", v, err)
	}
	var cols string
	if err := pool.QueryRow(ctx, `SELECT string_agg(column_name, ',' ORDER BY ordinal_position)
		FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = 'admin_sessions'`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != "token_hash,user_id,created_at,expires_at" {
		t.Errorf("admin_sessions columns after down = %s", cols)
	}
	var indexes string
	if err := pool.QueryRow(ctx, `SELECT string_agg(indexname, ',' ORDER BY indexname) FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename = 'admin_sessions'`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(indexes, "last_seen") {
		t.Errorf("down left the last_seen index behind: %s", indexes)
	}
	if after := tableCounts(t, pool); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("row counts changed by the rollback: before %v, after %v", before, after)
	}
	// And forward again.
	if err := db.MigrateFS(ctx, pool, throughLastSeen, "m", log); err != nil {
		t.Fatalf("migrate after down: %v", err)
	}
}

func TestOutboxStatus_ReportsTheLibrariesOwnMigrationAndCreatesNothing(t *testing.T) {
	pool := dbtest.EmptyPool(t)
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)

	before, err := db.OutboxStatus(ctx, pool)
	if err != nil || len(before) != 1 || before[0].Version != 1 || before[0].Applied {
		t.Fatalf("fresh database: %+v %v", before, err)
	}
	var created bool
	if err := pool.QueryRow(ctx, "SELECT to_regclass('outbox_goose_version') IS NOT NULL").Scan(&created); err != nil || created {
		t.Fatalf("asking for the status created the history table: %v %v", created, err)
	}

	if err := db.Migrate(ctx, pool, log); err != nil {
		t.Fatal(err)
	}
	// What the library really installed, read from its own table: this fails
	// when an outbox upgrade ships a migration the status code does not know.
	var latest int64
	if err := pool.QueryRow(ctx, "SELECT max(version_id) FROM outbox_goose_version").Scan(&latest); err != nil {
		t.Fatal(err)
	}
	after, err := db.OutboxStatus(ctx, pool)
	if err != nil || len(after) != 1 || !after[0].Applied || after[0].AppliedAt.IsZero() || after[0].Version != latest {
		t.Fatalf("after migrate (library latest %d): %+v %v", latest, after, err)
	}
}

func TestOutboxStatus_RefusesAHistoryNewerThanTheBuild(t *testing.T) {
	pool := dbtest.EmptyPool(t)
	ctx, log := t.Context(), slog.New(slog.DiscardHandler)
	if err := db.Migrate(ctx, pool, log); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO outbox_goose_version (version_id, is_applied) VALUES (2, true)"); err != nil {
		t.Fatal(err)
	}
	if s, err := db.OutboxStatus(ctx, pool); err == nil {
		t.Fatalf("a newer outbox history was reported as %+v", s)
	}
}
