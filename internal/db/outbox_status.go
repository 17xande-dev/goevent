package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3/database"
)

// OutboxHistory is the table the outbox module records its own migrations in,
// apart from the application's goose_db_version.
const OutboxHistory = "outbox_goose_version"

// OutboxMigration is the state of the one outbox migration this build knows.
type OutboxMigration struct {
	Version   int64
	Applied   bool
	AppliedAt time.Time
}

// outboxLatest is the newest outbox migration shipped by the outbox version in
// go.mod (v0.1.0 has only 0001). The module has no status API, so this reads
// its named history with goose's store, the way the gowebkit template does. A
// bump of outbox that adds a migration has to raise this, and
// TestOutboxStatus_ReportsTheLibrariesOwnMigration fails until it does.
const outboxLatest int64 = 1

// OutboxStatus reads the outbox migration history without creating it, so
// asking never initialises an empty database. A history newer than this build
// knows is an error, not a guess.
func OutboxStatus(ctx context.Context, pool *pgxpool.Pool) ([]OutboxMigration, error) {
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer sqlDB.Close() // Closes the adapter, not the pgx pool.
	store, err := database.NewStore(database.DialectPostgres, OutboxHistory)
	if err != nil {
		return nil, fmt.Errorf("db: outbox migration history: %w", err)
	}
	pending := []OutboxMigration{{Version: outboxLatest}}
	exists, err := store.(database.StoreExtender).TableExists(ctx, sqlDB)
	if err != nil {
		return nil, fmt.Errorf("db: inspect outbox migration history: %w", err)
	}
	if !exists {
		return pending, nil
	}
	current, err := store.GetLatestVersion(ctx, sqlDB)
	if err != nil && !errors.Is(err, database.ErrVersionNotFound) {
		return nil, fmt.Errorf("db: read outbox migration version: %w", err)
	}
	if current > outboxLatest {
		return nil, fmt.Errorf("db: outbox migration %d is newer than this build's latest outbox migration %d", current, outboxLatest)
	}
	if current == outboxLatest {
		v, err := store.GetMigration(ctx, sqlDB, outboxLatest)
		if err != nil {
			return nil, fmt.Errorf("db: read outbox migration status: %w", err)
		}
		pending[0].Applied, pending[0].AppliedAt = v.IsApplied, v.Timestamp
	}
	return pending, nil
}
