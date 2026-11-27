-- +goose Up
-- Development databases lose queued jobs because the module uses a new schema and seal format.
DROP TABLE email_jobs;

-- +goose Down
-- The old schema is recreated without its former rows.
CREATE TABLE email_jobs (
    id              BIGSERIAL PRIMARY KEY,
    registration_id UUID NOT NULL REFERENCES registrations(id) ON DELETE CASCADE,
    kind            TEXT NOT NULL CHECK (kind IN ('confirmation', 'received', 'notify', 'resend')),
    payload         BYTEA NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT NOT NULL DEFAULT '',
    sent_at         TIMESTAMPTZ
);
CREATE UNIQUE INDEX email_jobs_once ON email_jobs (registration_id, kind) WHERE kind IN ('confirmation', 'received', 'notify');
CREATE INDEX email_jobs_pending ON email_jobs (next_attempt_at, id) WHERE sent_at IS NULL;
CREATE INDEX ON email_jobs (registration_id);
