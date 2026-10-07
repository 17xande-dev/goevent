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

-- The admin's list: newest first, optionally one event, one status, and a
-- search over the reference and the person who registered. The attendee count
-- and the amount paid come with each row, so the list needs no second query.
-- name: ListRegistrations :many
SELECT r.*, e.title AS event_title,
    (SELECT count(*) FROM attendees a WHERE a.registration_id = r.id AND a.status = 'active')::int AS attendee_count,
    (SELECT COALESCE(sum(p.amount_cents), 0) FROM payments p WHERE p.registration_id = r.id AND p.status = 'paid')::bigint AS paid_cents
FROM registrations r
JOIN events e ON e.id = r.event_id
WHERE (sqlc.narg(event_id)::uuid IS NULL OR r.event_id = sqlc.narg(event_id))
  AND (sqlc.narg(status)::text IS NULL OR r.status = sqlc.narg(status))
  AND (sqlc.narg(search)::text IS NULL
       OR r.reference ILIKE '%' || sqlc.narg(search) || '%'
       OR r.contact_email ILIKE '%' || sqlc.narg(search) || '%'
       OR (r.contact_first_name || ' ' || r.contact_last_name) ILIKE '%' || sqlc.narg(search) || '%')
ORDER BY r.created_at DESC, r.id DESC
LIMIT 500;

-- One statement, so a cancellation cannot race a payment confirming the same
-- row: whichever commits second sees the other's status.
-- name: CancelRegistration :one
UPDATE registrations SET status = 'cancelled', cancelled_at = now()
WHERE id = $1 AND status IN ('pending', 'confirmed', 'expired')
RETURNING *;

-- name: CancelAttendee :execrows
UPDATE attendees SET status = 'cancelled'
WHERE id = $1 AND registration_id = $2 AND status = 'active';

-- Seats per ticket type, split the way the event page wants them: confirmed,
-- and pending holds that still stand.
-- name: EventTicketCounts :many
SELECT a.ticket_type_id,
    count(*) FILTER (WHERE r.status = 'confirmed')::int AS confirmed,
    count(*) FILTER (WHERE r.status = 'pending' AND r.hold_expires_at > now())::int AS held
FROM attendees a
JOIN registrations r ON r.id = a.registration_id
WHERE r.event_id = $1 AND a.status = 'active'
GROUP BY a.ticket_type_id;

-- name: EventMoney :one
SELECT COALESCE(sum(p.amount_cents) FILTER (WHERE p.status = 'paid'), 0)::bigint AS paid_cents,
    (SELECT count(*) FROM registrations r2 WHERE r2.event_id = $1 AND r2.oversold)::int AS oversold
FROM payments p
JOIN registrations r ON r.id = p.registration_id
WHERE r.event_id = $1;

-- Every active attendee of an event with their registration, for the export.
-- name: ExportAttendees :many
SELECT a.id AS attendee_id, a.first_name, a.last_name, a.email, a.ticket_name, a.unit_price_cents,
    a.checked_in_at, r.id AS registration_id, r.reference, r.status AS registration_status,
    r.contact_first_name, r.contact_last_name, r.contact_email, r.contact_phone, r.created_at
FROM attendees a
JOIN registrations r ON r.id = a.registration_id
WHERE r.event_id = $1 AND a.status = 'active' AND r.status IN ('confirmed', 'pending')
ORDER BY r.created_at, a.position;

-- name: ExportAnswers :many
SELECT ans.registration_id, ans.attendee_id, ans.question_id, ans.value
FROM answers ans
JOIN registrations r ON r.id = ans.registration_id
WHERE r.event_id = $1;
