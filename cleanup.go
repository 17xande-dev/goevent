package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/17xande-dev/goevent/internal/auth"
)

// sessionCleanupInterval is how often expired admin sessions are swept. Hourly
// rather than daily, because the table is written to on every sign-in and read on
// every admin request, and an hour of expired rows is a smaller table to keep in
// cache than a day of them.
const sessionCleanupInterval = time.Hour

// startSessionCleanup deletes expired admin sessions, on boot and then hourly,
// until ctx is cancelled.
//
// This is housekeeping and not a security control, which is worth being explicit
// about: expiry is enforced in the lookup query's own predicate, so an expired row
// authenticates nobody in the hour before it is deleted. What the sweep buys is a
// bounded table.
//
// It runs in every instance rather than being elected to one: the work is one
// idempotent DELETE, so two instances doing it at once end in the state one would
// have reached.
//
// The sweep is deliberately not a database trigger or a cron container. It is a
// goroutine in the process that already has a pool and a logger, which keeps the
// deployment story — one binary, one container — intact.
func startSessionCleanup(ctx context.Context, users *auth.Store, log *slog.Logger) {
	sweep := func() {
		sweepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()

		removed, err := users.DeleteExpiredSessions(sweepCtx)
		if err != nil {
			log.Error("admin session cleanup failed", "error", err)
			return
		}
		if removed > 0 {
			log.Info("admin session cleanup removed expired sessions", "sessions", removed)
		}
	}

	go func() {
		sweep()

		ticker := time.NewTicker(sessionCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Debug("admin session cleanup stopping")
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
}
