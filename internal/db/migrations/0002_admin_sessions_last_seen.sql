-- +goose Up
-- The last time a session was used, so an idle session can end before its
-- absolute limit (expires_at). It is written at most once a minute per session
-- (websec/session's touch throttle), not on every request.
--
-- DEFAULT now() backfills every existing row to the moment of this migration, so
-- the column's arrival signs nobody out: a session that was live a second ago is
-- live a second later, and starts its idle clock now. It stays as the default so
-- that a binary from before this migration, which does not know the column, can
-- still insert a session.
ALTER TABLE admin_sessions ADD COLUMN last_seen TIMESTAMPTZ NOT NULL DEFAULT now();
-- The idle sweep filters on it.
CREATE INDEX admin_sessions_last_seen_idx ON admin_sessions (last_seen);

-- +goose Down
-- Returns the table to the 0001 shape. Sessions stay valid: only the idle limit
-- goes, and the previous binary enforces expires_at alone.
DROP INDEX admin_sessions_last_seen_idx;
ALTER TABLE admin_sessions DROP COLUMN last_seen;
