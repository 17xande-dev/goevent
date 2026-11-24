package auth

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	websession "github.com/17xande-dev/websec/session"
	"github.com/17xande-dev/websec/session/sessiontest"
)

// sessionLifetime is what the server runs: the fixed idle limit and the
// SESSION_TTL_HOURS default (24 h) as the absolute one. IssueSession takes the
// absolute limit per call, so the suite's value is passed to it below rather than
// read from configuration.
var sessionLifetime = websession.Lifetime{Idle: IdleTimeout, Absolute: 24 * time.Hour}

// TestSessionStoreConformance runs the shared session-store suite against the
// real admin_sessions table and the real Store.
//
// Time is moved by backdating rows, never by a fake clock: the absolute limit is
// compared with the database's own now(), so a Go clock could not move it.
func TestSessionStoreConformance(t *testing.T) {
	s, pool, ctx := newStore(t)
	var users atomic.Int64

	sessiontest.Run(t, sessiontest.Harness{
		Lifetime: sessionLifetime,
		NewUser: func(t *testing.T) string {
			t.Helper()
			// A manager, not an owner: disabling the last enabled owner is refused.
			u := mustCreate(t, s, ctx, fmt.Sprintf("user%d@example.com", users.Add(1)), "a long passphrase here", RoleManager)
			return u.ID
		},
		Create: func(t *testing.T, user string) string {
			t.Helper()
			token, _, err := s.IssueSession(ctx, user, sessionLifetime.Absolute)
			if err != nil {
				t.Fatalf("IssueSession: %v", err)
			}
			return token
		},
		// The request-time lookup is what RequireAdmin does: resolve the token (which
		// applies the idle limit and records the use), then refuse a disabled account.
		Lookup: func(t *testing.T, token string) (string, bool, error) {
			t.Helper()
			_, user, err := s.Session(ctx, token)
			switch {
			case err == nil && !user.Disabled:
				return user.ID, true, nil
			case err == nil, errors.Is(err, ErrNotFound):
				return "", false, nil
			default:
				return "", false, err
			}
		},
		// expires_at is derived at issue time as created + the absolute limit, so it
		// moves with created_at; leaving it alone would make "absolute" pass or fail
		// for the wrong reason.
		Backdate: func(t *testing.T, token string, created, lastSeen time.Time) {
			t.Helper()
			tag, err := pool.Exec(ctx,
				`UPDATE admin_sessions SET created_at = $2, last_seen = $3, expires_at = $4 WHERE token_hash = $1`,
				hashToken(token), created, lastSeen, created.Add(sessionLifetime.Absolute))
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("backdate: %v (rows %d)", err, tag.RowsAffected())
			}
		},
		LastSeen: func(t *testing.T, token string) time.Time {
			t.Helper()
			var at time.Time
			if err := pool.QueryRow(ctx, `SELECT last_seen FROM admin_sessions WHERE token_hash = $1`, hashToken(token)).Scan(&at); err != nil {
				t.Fatalf("last_seen: %v", err)
			}
			return at
		},
		Sweep: func(t *testing.T) int {
			t.Helper()
			n, err := s.DeleteExpiredSessions(ctx)
			if err != nil {
				t.Fatalf("DeleteExpiredSessions: %v", err)
			}
			return int(n)
		},
		Count: func(t *testing.T) int {
			t.Helper()
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_sessions`).Scan(&n); err != nil {
				t.Fatalf("count: %v", err)
			}
			return n
		},
		RawRows: func(t *testing.T) []string {
			t.Helper()
			rows, err := pool.Query(ctx, `SELECT concat_ws('|', encode(token_hash, 'hex'), user_id::text,
				created_at::text, expires_at::text, last_seen::text) FROM admin_sessions`)
			if err != nil {
				t.Fatalf("raw rows: %v", err)
			}
			defer rows.Close()
			var out []string
			for rows.Next() {
				var row string
				if err := rows.Scan(&row); err != nil {
					t.Fatal(err)
				}
				out = append(out, strings.ToLower(row))
			}
			return out
		},
		Disable: func(t *testing.T, user string) {
			t.Helper()
			if err := s.SetDisabled(ctx, user, true); err != nil {
				t.Fatalf("SetDisabled: %v", err)
			}
		},
		// The store's own password change ends every session of the account,
		// including the one that made the change: the handler clears the cookie
		// and sends the person to sign in again.
		ChangePassword: func(t *testing.T, user, _ string) string {
			t.Helper()
			if err := s.SetPassword(ctx, user, hash(t, "another long passphrase"), false); err != nil {
				t.Fatalf("SetPassword: %v", err)
			}
			return ""
		},
		// Administrative revocation: ending a user's sessions without touching the
		// account (what an operator's SQL or a future "sign out everywhere" does).
		RevokeUser: func(t *testing.T, user string) {
			t.Helper()
			if err := s.DeleteSessionsForUser(ctx, user); err != nil {
				t.Fatalf("DeleteSessionsForUser: %v", err)
			}
		},
	})
}
