package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/17xande-dev/password"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	legacyPassword = "fixture password — correct horse"
	legacyHash     = "$2a$10$twjQA8BMDdFuHP3afXoWe.PWv/NGS/A2zpHSrgDmWAVc/fzuPmJ66"
)

func TestLoginUpgradeDoesNotOverwriteConcurrentPasswordChange(t *testing.T) {
	s, pool, ctx := newStore(t)
	defer password.SetParamsForTesting(password.Params{Memory: 64, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})()
	u, err := s.Create(ctx, "owner@example.com", "", legacyHash, RoleOwner, false)
	if err != nil {
		t.Fatal(err)
	}
	const newPassword = "a brand new passphrase"
	newHash := hash(t, newPassword)

	// Interpose immediately before the real SQL runs. That places the competing
	// write after verification without relying on a sleep or scheduler order.
	attempted := false
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = beforePasswordUpgrade(func(ctx context.Context) {
		tag, err := pool.Exec(ctx, `UPDATE admin_users SET password_hash = $1 WHERE id = $2`, newHash, u.ID)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("concurrent password change: %v, %v", tag, err)
		}
		attempted = true
	})
	loginPool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer loginPool.Close()
	if got, err := NewStore(loginPool).Authenticate(ctx, u.Email, legacyPassword); !attempted || err != nil || got.ID != u.ID {
		t.Fatalf("login after lost CAS: upgrade attempted=%t, user=%q, err=%v", attempted, got.ID, err)
	}
	if _, err := s.Authenticate(ctx, u.Email, newPassword); err != nil {
		t.Fatalf("new password was overwritten: %v", err)
	}
	if _, err := s.Authenticate(ctx, u.Email, legacyPassword); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("old password must fail, got %v", err)
	}
}

func TestLoginSucceedsIfHashUpgradeWriteFails(t *testing.T) {
	s, pool, ctx := newStore(t)
	defer password.SetParamsForTesting(password.Params{Memory: 64, Time: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32})()
	u, err := s.Create(ctx, "owner@example.com", "", legacyHash, RoleOwner, false)
	if err != nil {
		t.Fatal(err)
	}
	// Permit bookkeeping, but have Postgres reject the replacement hash.
	if _, err := pool.Exec(ctx, `ALTER TABLE admin_users ADD CHECK (password_hash LIKE '$2a$%')`); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Authenticate(ctx, u.Email, legacyPassword); err != nil || got.ID != u.ID {
		t.Fatalf("login after failed upgrade: user=%q, err=%v", got.ID, err)
	}
}

type beforePasswordUpgrade func(context.Context)

func (f beforePasswordUpgrade) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(q.SQL, "-- name: UpgradeAdminPasswordHash ") {
		f(ctx)
	}
	return ctx
}

func (beforePasswordUpgrade) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
