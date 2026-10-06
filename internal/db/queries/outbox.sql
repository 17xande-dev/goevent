-- name: EnqueueEmail :exec
INSERT INTO email_jobs (registration_id, kind, payload) VALUES ($1, $2, $3);

-- A transaction holds this row while sending (with a bounded context). Multiple
-- instances skip each other's work; a crash releases the lock for retry.
-- name: NextEmail :one
SELECT * FROM email_jobs
WHERE sent_at IS NULL AND next_attempt_at <= now()
ORDER BY next_attempt_at, id LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: CompleteEmail :exec
UPDATE email_jobs SET sent_at = now(), attempts = attempts + 1,
    payload = ''::bytea, last_error = '' WHERE id = $1;

-- name: FailEmail :exec
UPDATE email_jobs SET attempts = attempts + 1, last_error = $2,
    next_attempt_at = now() + make_interval(secs => LEAST(3600, 30 * power(2, LEAST(attempts, 7)))::double precision)
WHERE id = $1;

-- name: MarkRegistrationEmailed :exec
UPDATE registrations SET emailed = TRUE WHERE id = $1;

-- name: ListRegistrationEmails :many
SELECT id, kind, attempts, next_attempt_at, last_error, sent_at
FROM email_jobs WHERE registration_id = $1 ORDER BY id;

-- name: RetryRegistrationEmails :execrows
UPDATE email_jobs SET next_attempt_at = now()
WHERE registration_id = $1 AND sent_at IS NULL;
