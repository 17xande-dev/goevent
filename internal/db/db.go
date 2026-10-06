// Package db owns the Postgres connection pool and runs embedded goose migrations.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Concurrent boots queue rather than applying the same migration twice.
const migrationLockID int64 = 8_675_309_001

// Connect opens a pool and verifies it can reach the database.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("db: parse DATABASE_URL: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// Migrate applies embedded migrations in version order, skipping completed ones.
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	return MigrateFS(ctx, pool, migrationsFS, "migrations", log)
}

// MigrateFS lets tests exercise future schema changes and their failure modes.
func MigrateFS(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, dir string, log *slog.Logger) error {
	provider, closeDB, err := newProvider(pool, fsys, dir, log)
	if err != nil {
		return err
	}
	defer closeDB()
	results, err := provider.Up(ctx)
	if err != nil {
		return fmt.Errorf("db: apply migrations: %w", err)
	}
	for _, r := range results {
		log.Info("applied migration", "version", r.Source.Version, "name", r.Source.Path, "duration", r.Duration)
	}
	return nil
}

// Status reports applied and pending migrations without starting the server.
func Status(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) ([]*goose.MigrationStatus, error) {
	provider, closeDB, err := newProvider(pool, migrationsFS, "migrations", log)
	if err != nil {
		return nil, err
	}
	defer closeDB()
	status, err := provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: migration status: %w", err)
	}
	return status, nil
}

// The database/sql adapter belongs to goose; closing it must not close the pgx pool.
func newProvider(pool *pgxpool.Pool, fsys fs.FS, dir string, log *slog.Logger) (*goose.Provider, func(), error) {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		return nil, nil, fmt.Errorf("db: migrations dir %q: %w", dir, err)
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(migrationLockID))
	if err != nil {
		sqlDB.Close()
		return nil, nil, fmt.Errorf("db: create migration locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, sub,
		goose.WithSessionLocker(locker),
		goose.WithSlog(log),
		goose.WithAllowOutofOrder(false),
	)
	if err != nil {
		sqlDB.Close()
		return nil, nil, fmt.Errorf("db: create migration provider: %w", err)
	}
	return provider, func() { closeQuietly(sqlDB, log) }, nil
}

func closeQuietly(db *sql.DB, log *slog.Logger) {
	if err := db.Close(); err != nil {
		log.Warn("close migration database handle", "error", err)
	}
}
