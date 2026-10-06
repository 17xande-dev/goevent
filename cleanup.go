package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/goevent/internal/registrations"
)

// The in-process sweeps. Each is a goroutine in the process that already has a
// pool and a logger, rather than a database trigger or a cron container, which
// keeps the deployment story — one binary, one container — intact.
//
// They run in every instance rather than being elected to one: each is one
// idempotent statement, so two instances doing it at once end in the state one
// would have reached.

// sessionCleanupInterval is how often expired admin sessions are swept. Hourly,
// because the table is read on every admin request and an hour of expired rows
// is a smaller table to keep in cache than a day of them.
const sessionCleanupInterval = time.Hour

// holdExpiryInterval is how often lapsed registration holds are marked expired.
// A minute, because the status shows on the registrant's own page and in the
// admin; the seats themselves were freed the instant the hold lapsed.
const holdExpiryInterval = time.Minute

// startSessionCleanup deletes expired admin sessions, on boot and then hourly.
//
// Housekeeping, not a security control: expiry is enforced in the lookup query's
// own predicate, so an expired row authenticates nobody before it is deleted.
// What the sweep buys is a bounded table.
func startSessionCleanup(ctx context.Context, users *auth.Store, log *slog.Logger) {
	every(ctx, sessionCleanupInterval, "admin session cleanup", log, func(ctx context.Context) error {
		removed, err := users.DeleteExpiredSessions(ctx)
		if removed > 0 {
			log.Info("admin session cleanup removed expired sessions", "sessions", removed)
		}
		return err
	})
}

// startHoldExpiry marks registrations whose hold lapsed unpaid as expired, on
// boot and then every minute. Bookkeeping too: availability counts only
// unexpired holds, so a lapsed one stopped holding its seats before this runs.
func startHoldExpiry(ctx context.Context, regs *registrations.Store, log *slog.Logger) {
	every(ctx, holdExpiryInterval, "hold expiry", log, func(ctx context.Context) error {
		n, err := regs.ExpireHolds(ctx)
		if n > 0 {
			log.Info("expired lapsed registration holds", "registrations", n)
		}
		return err
	})
}

// every runs sweep once now and then every interval until ctx is cancelled.
// Each run gets its own timeout, detached from the shutdown context, so a sweep
// running when the server stops finishes or gives up on its own terms rather
// than leaving a half-executed statement. A failure is logged and left for the
// next tick: a sweep that fails is a table a little longer, not worth waking
// anybody.
func every(ctx context.Context, interval time.Duration, name string, log *slog.Logger, sweep func(context.Context) error) {
	run := func() {
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := sweep(runCtx); err != nil {
			log.Error(name+" failed", "error", err)
		}
	}
	go func() {
		run()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				log.Debug(name + " stopping")
				return
			case <-ticker.C:
				run()
			}
		}
	}()
}
