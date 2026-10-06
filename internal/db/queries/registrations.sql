-- Checkouts for one event are serialised on its row: capacity is counted and
-- spent under this lock, so two people cannot both take the last seat.
-- name: LockEvent :one
SELECT * FROM events WHERE id = $1 FOR UPDATE;

-- Seats held per ticket type: every active attendee on a confirmed
-- registration, and on a pending one whose hold has not lapsed. exclude_id
-- leaves one registration out, for asking whether it would still fit.
-- name: HeldByTicketType :many
SELECT a.ticket_type_id, count(*)::int AS held
FROM attendees a
JOIN registrations r ON r.id = a.registration_id
WHERE r.event_id = @event_id
  AND r.id <> @exclude_id
  AND a.status = 'active'
  AND (r.status = 'confirmed' OR (r.status = 'pending' AND r.hold_expires_at > now()))
GROUP BY a.ticket_type_id;

-- name: RegistrationByCheckoutKey :one
SELECT * FROM registrations WHERE event_id = $1 AND checkout_key = $2;

-- name: CreateRegistration :one
INSERT INTO registrations (
    event_id, reference, contact_first_name, contact_last_name, contact_email, contact_phone,
    status, total_cents, currency, hold_expires_at, pay_later, checkout_key, confirmed_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13
) RETURNING *;

-- name: CreateAttendee :one
INSERT INTO attendees (
    registration_id, ticket_type_id, first_name, last_name, email, ticket_name,
    unit_price_cents, position
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8
) RETURNING *;

-- name: CreateAnswer :exec
INSERT INTO answers (registration_id, attendee_id, question_id, label, value)
VALUES ($1, $2, $3, $4, $5);

-- name: CreatePayment :one
INSERT INTO payments (registration_id, method, amount_cents, currency, recorded_by, note, status, paid_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: PendingPayment :one
SELECT * FROM payments
WHERE registration_id = $1 AND status = 'pending' AND method = $2
ORDER BY created_at DESC LIMIT 1;

-- name: GetPayment :one
SELECT * FROM payments WHERE id = $1;

-- name: LockPayment :one
SELECT * FROM payments WHERE id = $1 FOR UPDATE;

-- name: ListPayments :many
SELECT * FROM payments WHERE registration_id = $1 ORDER BY created_at;

-- What a gateway said, kept whether or not it was "paid". A paid payment is
-- never moved back: a late "pending" or "failed" notification after a success
-- is the gateway's history, not ours.
-- name: RecordPaymentNotice :exec
UPDATE payments SET
    gateway_ref = COALESCE(gateway_ref, sqlc.narg(gateway_ref)),
    gateway_status = @gateway_status,
    gateway_amount = @gateway_amount,
    gateway_payload = @gateway_payload,
    status = CASE WHEN status = 'paid' THEN status ELSE @status END
WHERE id = @id;

-- name: MarkPaymentPaid :exec
UPDATE payments SET status = 'paid', paid_at = now() WHERE id = $1;

-- name: PaidTotal :one
SELECT COALESCE(sum(amount_cents), 0)::bigint AS paid
FROM payments WHERE registration_id = $1 AND status = 'paid';

-- name: LockRegistration :one
SELECT * FROM registrations WHERE id = $1 FOR UPDATE;

-- name: ConfirmRegistration :one
UPDATE registrations SET status = 'confirmed', confirmed_at = now(), oversold = $2
WHERE id = $1
RETURNING *;

-- name: GetRegistration :one
SELECT * FROM registrations WHERE id = $1;

-- name: GetRegistrationByReference :one
SELECT * FROM registrations WHERE reference = $1;

-- name: ListAttendees :many
SELECT * FROM attendees WHERE registration_id = $1 ORDER BY position, id;

-- name: CountAttendeesByType :many
SELECT ticket_type_id, count(*)::int AS n
FROM attendees WHERE registration_id = $1 AND status = 'active'
GROUP BY ticket_type_id;

-- name: ListAnswers :many
SELECT * FROM answers WHERE registration_id = $1 ORDER BY id;

-- Holds that lapsed without being paid for. Their seats were already free —
-- availability only counts unexpired holds — so this is bookkeeping: the row
-- says what happened. A payment that arrives later still confirms it.
-- name: ExpireHolds :execrows
UPDATE registrations SET status = 'expired'
WHERE status = 'pending' AND hold_expires_at <= now();
