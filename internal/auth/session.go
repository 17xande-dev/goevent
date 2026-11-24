// Package auth holds the administrator accounts, their roles, and the passwords
// and sessions that get them in.
//
// A session is a row in admin_sessions and a cookie carrying 32 random bytes.
// The row is what makes it revocable, which is the whole reason the signed,
// self-describing cookie this package started with is gone: a cookie that
// verifies on its own cannot be taken back, and "end every session belonging to
// this account" is what a password change, a disable and a demotion all need.
//
// Only sha256(token) is stored. There is nothing else to store — the token
// carries no claims — so a leaked database backup hands over no live session,
// and the plain token exists only in the cookie the browser holds.
//
// The pieces: model.go has the roles, permissions and the User row; store.go has
// the accounts and the one-shot setup token; password.go hashes passwords; this
// file issues and verifies sessions.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/17xande-dev/goevent/internal/db/gen"
	"github.com/17xande-dev/websec/cookie"
	websession "github.com/17xande-dev/websec/session"
	"github.com/jackc/pgx/v5"
)

// CookieName is the admin session cookie's bare name; the name on the wire gets
// a prefix from websec/cookie (__Secure- here, because of the /admin path).
const CookieName = "admin_session"

// IdleTimeout is how long a session may go unused before it ends, whatever the
// absolute limit (SESSION_TTL_HOURS, 24 hours by default) still allows. Fixed
// rather than configurable: the absolute limit is the knob an operator already
// has, and one more would only be a second way to get a session length wrong.
const IdleTimeout = 2 * time.Hour

// lifetime is the policy for a session row: the idle limit above and the
// absolute limit the row itself recorded when it was issued (expires_at less
// created_at). Reading the absolute limit from the row rather than from
// configuration is what keeps a session issued under an older SESSION_TTL_HOURS
// on the limit it was given.
func lifetime(created, expires time.Time) websession.Lifetime {
	return websession.Lifetime{Idle: IdleTimeout, Absolute: expires.Sub(created)}
}

// NewSessionCookie builds the admin session cookie policy. Path is /admin, so
// the cookie is never sent with a request for the public event pages. insecure
// is true only for a plain-HTTP deployment (BASE_URL not https), where a prefix
// and Secure would make the browser drop the cookie.
func NewSessionCookie(insecure bool) (*cookie.Cookie, error) {
	return cookie.New(cookie.Config{Name: CookieName, Path: "/admin", Insecure: insecure})
}

// TokenBytes is how much randomness a session token carries. 32 bytes is the
// size below which nothing is gained by arguing about it and above which nothing
// is either: 256 bits of uniform randomness is not going to be guessed.
const TokenBytes = 32

// NewToken returns a fresh session token, URL-safe and unpadded so it needs no
// escaping in a cookie value.
//
// It is exported because the setup flow mints a token of its own with the same
// properties, and because a caller assembling their own bootstrap should not have
// to reimplement "32 bytes from crypto/rand".
func NewToken() (string, error) {
	b := make([]byte, TokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// IssueSession records a new session for a user and returns the token to put in
// the cookie, alongside the row it created.
//
// The token is returned once and never again: the store keeps only its hash, so
// nothing can reconstruct it afterwards — including this package.
func (s *Store) IssueSession(ctx context.Context, userID string, ttl time.Duration) (string, Session, error) {
	if ttl <= 0 {
		return "", Session{}, fmt.Errorf("auth: session ttl must be positive, got %s", ttl)
	}
	token, err := NewToken()
	if err != nil {
		return "", Session{}, err
	}

	expires := time.Now().Add(ttl)
	err = s.q.CreateAdminSession(ctx, gen.CreateAdminSessionParams{
		TokenHash: hashToken(token),
		UserID:    userID,
		ExpiresAt: expires,
	})
	if err != nil {
		return "", Session{}, translate(fmt.Errorf("auth: create session: %w", err))
	}
	now := time.Now()
	return token, Session{UserID: userID, CreatedAt: now, LastSeen: now, ExpiresAt: expires}, nil
}

// Session looks a token up, returning the session and the account it belongs to.
//
// ErrNotFound is the answer for a token that was never valid, has expired, or has
// been revoked — one answer for the three, because a request holding any of them
// is equally not signed in and telling them apart would only invite a caller to
// treat one of them as nearly authenticated.
//
// The absolute limit is enforced in the query's own predicate rather than by
// comparing ExpiresAt here, so a caller cannot forget to; the idle limit is
// applied here, by websec/session, from the row's last_seen. The periodic sweep
// that deletes expired rows is housekeeping and explicitly not what makes this
// correct.
//
// A lookup also records that the session was used, at most once a minute (see
// TouchAdminSession), which is why a stored last_seen may be up to a minute
// stale: an idle session can end up to a minute early, never late.
func (s *Store) Session(ctx context.Context, token string) (Session, User, error) {
	// A short-circuit, not an optimisation: without it an empty cookie would
	// hash to a perfectly well-formed lookup for sha256(""), which is a value an
	// attacker knows and could in principle insert against.
	if token == "" {
		return Session{}, User{}, ErrNotFound
	}

	row, err := s.q.GetAdminSession(ctx, hashToken(token))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, User{}, ErrNotFound
		}
		return Session{}, User{}, translate(fmt.Errorf("auth: session: %w", err))
	}
	sess := Session{
		UserID:    row.AdminSession.UserID,
		CreatedAt: row.AdminSession.CreatedAt,
		LastSeen:  row.AdminSession.LastSeen,
		ExpiresAt: row.AdminSession.ExpiresAt,
	}
	now := time.Now()
	if lifetime(sess.CreatedAt, sess.ExpiresAt).Expired(sess.CreatedAt, sess.LastSeen, now) {
		return Session{}, User{}, ErrNotFound
	}
	// NeedsTouch also reports a last_seen in the future (a clock that stepped
	// back), which the statement rewrites to now: it extends a session by at most
	// one request.
	if websession.NeedsTouch(sess.LastSeen, now) {
		err := s.q.TouchAdminSession(ctx, gen.TouchAdminSessionParams{
			TokenHash:    hashToken(token),
			EverySeconds: websession.TouchEvery.Seconds(),
		})
		if err != nil {
			return Session{}, User{}, translate(fmt.Errorf("auth: touch session: %w", err))
		}
	}
	return sess, userOf(row.AdminUser), nil
}

// DeleteSession ends one session, by the token that holds it. Signing out is this
// and clearing the cookie, in that order of importance: the cookie is a copy, the
// row is the session.
//
// Deleting a token that is not there is not an error. A double-submitted sign-out
// and a sign-out with an already-expired cookie both arrive here, and neither is
// a problem worth reporting to somebody who has just left.
func (s *Store) DeleteSession(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if err := s.q.DeleteAdminSession(ctx, hashToken(token)); err != nil {
		return translate(fmt.Errorf("auth: delete session: %w", err))
	}
	return nil
}

// hashToken is sha256 and deliberately not a KDF. A session or setup token is 32
// bytes of uniform randomness with nothing to guess, so a slow hash would buy no
// resistance to an offline attack that cannot succeed anyway — and would be paid
// on every request that carries one.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// revokeAccess ends everything that lets an account in without its password:
// its sessions. It runs inside the caller's transaction, so there is no moment at
// which a changed password, a demotion or a disable has happened but a session
// issued before it still works.
func revokeAccess(ctx context.Context, q *gen.Queries, userID string) error {
	if _, err := q.DeleteAdminSessionsForUser(ctx, userID); err != nil {
		return translate(fmt.Errorf("auth: end sessions: %w", err))
	}
	return nil
}
