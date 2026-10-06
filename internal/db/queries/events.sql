-- name: ListEvents :many
SELECT * FROM events ORDER BY starts_at DESC, id;

-- What the public list shows: listed events that are not drafts and have not
-- ended. A closed or cancelled one stays listed until it is over, saying so,
-- rather than vanishing from under somebody who was about to register.
-- name: ListPublicEvents :many
SELECT * FROM events
WHERE listed AND status <> 'draft' AND ends_at > now()
ORDER BY starts_at, id;

-- name: GetEvent :one
SELECT * FROM events WHERE id = $1;

-- name: GetEventBySlug :one
SELECT * FROM events WHERE slug = $1;

-- name: CreateEvent :one
INSERT INTO events (
    slug, title, summary, description, venue, address, starts_at, ends_at,
    timezone, capacity, listed, registration_opens_at, registration_closes_at,
    pay_later, pay_later_instructions
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15
) RETURNING *;

-- Status and image are not written here: each has its own action, so saving the
-- details form can never publish an event or drop its picture by accident.
-- name: UpdateEvent :one
UPDATE events SET
    slug = $2, title = $3, summary = $4, description = $5, venue = $6, address = $7,
    starts_at = $8, ends_at = $9, timezone = $10, capacity = $11, listed = $12,
    registration_opens_at = $13, registration_closes_at = $14,
    pay_later = $15, pay_later_instructions = $16, updated_at = now()
WHERE id = $1
RETURNING *;

-- A status change in one statement: the move is checked against the status the
-- row has *now*, and publishing additionally needs a ticket type somebody could
-- register for. Reading either first and then writing would let two
-- administrators act on what each saw a moment ago.
-- name: TransitionEvent :one
UPDATE events SET status = @next_status::text, updated_at = now()
WHERE events.id = @id AND events.status = @from_status::text
  AND (@next_status::text <> 'published' OR EXISTS (
      SELECT 1 FROM ticket_types t
      WHERE t.event_id = events.id AND NOT t.archived AND NOT t.hidden))
RETURNING *;

-- name: SetEventImage :one
UPDATE events SET image_key = $2, updated_at = now() WHERE id = $1 RETURNING *;

-- name: DeleteEvent :execrows
DELETE FROM events WHERE id = $1;

-- name: ListTicketTypes :many
SELECT * FROM ticket_types WHERE event_id = $1 ORDER BY archived, position, created_at;

-- name: GetTicketType :one
SELECT * FROM ticket_types WHERE event_id = $1 AND id = $2;

-- name: CreateTicketType :one
INSERT INTO ticket_types (
    event_id, name, description, price_cents, capacity, max_per_registration,
    sales_start_at, sales_end_at, hidden, position
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
) RETURNING *;

-- name: UpdateTicketType :one
UPDATE ticket_types SET
    name = $3, description = $4, price_cents = $5, capacity = $6,
    max_per_registration = $7, sales_start_at = $8, sales_end_at = $9,
    hidden = $10, position = $11, archived = $12
WHERE event_id = $1 AND id = $2
RETURNING *;

-- name: DeleteTicketType :execrows
DELETE FROM ticket_types WHERE event_id = $1 AND id = $2;

-- name: ArchiveTicketType :execrows
UPDATE ticket_types SET archived = TRUE WHERE event_id = $1 AND id = $2;

-- name: ListQuestions :many
SELECT * FROM questions WHERE event_id = $1 ORDER BY archived, position, created_at;

-- name: GetQuestion :one
SELECT * FROM questions WHERE event_id = $1 AND id = $2;

-- name: CreateQuestion :one
INSERT INTO questions (
    event_id, scope, kind, label, help, options, required, ticket_type_ids, position
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
) RETURNING *;

-- name: UpdateQuestion :one
UPDATE questions SET
    scope = $3, kind = $4, label = $5, help = $6, options = $7, required = $8,
    ticket_type_ids = $9, position = $10, archived = $11
WHERE event_id = $1 AND id = $2
RETURNING *;

-- name: DeleteQuestion :execrows
DELETE FROM questions WHERE event_id = $1 AND id = $2;

-- name: ArchiveQuestion :execrows
UPDATE questions SET archived = TRUE WHERE event_id = $1 AND id = $2;
