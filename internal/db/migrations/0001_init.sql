-- +goose Up
-- The complete greenfield baseline. Also the schema input to sqlc.

CREATE TABLE admin_users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT NOT NULL,
    name          TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'manager', 'checkin', 'viewer')),
    disabled      BOOLEAN NOT NULL DEFAULT FALSE,
    must_change_password BOOLEAN NOT NULL DEFAULT FALSE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ
);
-- Email identity is case-insensitive; the application also normalises addr-spec.
CREATE UNIQUE INDEX admin_users_email_key ON admin_users (lower(email));

CREATE TABLE admin_sessions (
    token_hash BYTEA PRIMARY KEY,
    user_id    UUID NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX admin_sessions_user_idx ON admin_sessions (user_id);
CREATE INDEX admin_sessions_expires_idx ON admin_sessions (expires_at);

-- Single-use claim of the first administrator, with no default password.
CREATE TABLE admin_setup (
    id          BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    token_hash  BYTEA NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    consumed_at TIMESTAMPTZ
);

-- An event people register for. One occurrence: a multi-date event is an open
-- decision (an event_times table), not something this row pretends to model.
CREATE TABLE events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        TEXT NOT NULL UNIQUE CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$'),
    title       TEXT NOT NULL CHECK (length(title) BETWEEN 1 AND 200),
    summary     TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    venue       TEXT NOT NULL DEFAULT '',
    address     TEXT NOT NULL DEFAULT '',
    starts_at   TIMESTAMPTZ NOT NULL,
    ends_at     TIMESTAMPTZ NOT NULL,
    -- The IANA zone the event happens in, so times render as the attendee
    -- will experience them rather than in the server's zone.
    timezone    TEXT NOT NULL DEFAULT 'Africa/Johannesburg',
    image_key   TEXT,
    -- Event-wide cap across every ticket type; NULL is unlimited. Checked on top
    -- of each ticket type's own capacity, never instead of it.
    capacity    INTEGER CHECK (capacity >= 0),
    status      TEXT NOT NULL DEFAULT 'draft'
                CHECK (status IN ('draft', 'published', 'closed', 'cancelled')),
    -- false = reachable by direct link only, never in the public list.
    listed      BOOLEAN NOT NULL DEFAULT TRUE,
    registration_opens_at  TIMESTAMPTZ,
    registration_closes_at TIMESTAMPTZ,
    -- Paying later — EFT or cash, recorded by an administrator — is offered
    -- beside any online gateway. The instructions (bank details, a reference to
    -- quote) are shown and emailed to whoever chooses it.
    pay_later   BOOLEAN NOT NULL DEFAULT FALSE,
    pay_later_instructions TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (ends_at >= starts_at)
);
CREATE INDEX events_public_idx ON events (starts_at) WHERE status = 'published' AND listed;

-- What a person can register as: Planning Center's "selection type", pretix's
-- "item". price_cents = 0 is a free ticket.
CREATE TABLE ticket_types (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id    UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    name        TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    description TEXT NOT NULL DEFAULT '',
    price_cents BIGINT NOT NULL CHECK (price_cents >= 0),
    capacity    INTEGER CHECK (capacity >= 0),
    max_per_registration INTEGER NOT NULL DEFAULT 10 CHECK (max_per_registration BETWEEN 1 AND 100),
    sales_start_at TIMESTAMPTZ,
    sales_end_at   TIMESTAMPTZ,
    -- Hidden ticket types are bookable only by an admin, never offered publicly.
    hidden      BOOLEAN NOT NULL DEFAULT FALSE,
    -- Archived rather than deleted once anybody holds one, so history keeps its
    -- foreign key.
    archived    BOOLEAN NOT NULL DEFAULT FALSE,
    position    INTEGER NOT NULL DEFAULT 0,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON ticket_types (event_id, position);

-- Custom questions. Registration-scope questions are asked once; attendee-scope
-- ones once per attendee, optionally only for some ticket types.
CREATE TABLE questions (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id    UUID NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    scope       TEXT NOT NULL CHECK (scope IN ('registration', 'attendee')),
    kind        TEXT NOT NULL CHECK (kind IN ('text', 'textarea', 'select', 'checkbox', 'date', 'phone')),
    label       TEXT NOT NULL CHECK (length(label) BETWEEN 1 AND 300),
    help        TEXT NOT NULL DEFAULT '',
    options     TEXT[] NOT NULL DEFAULT '{}',
    required    BOOLEAN NOT NULL DEFAULT FALSE,
    -- NULL means every ticket type. Only meaningful for attendee questions.
    ticket_type_ids UUID[],
    position    INTEGER NOT NULL DEFAULT 0,
    archived    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX ON questions (event_id, position);

-- One checkout: the transaction a contact person makes for one or more
-- attendees. "pending" holds capacity until hold_expires_at.
CREATE TABLE registrations (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id        UUID NOT NULL REFERENCES events(id),
    -- A short human code, read over the phone and typed at check-in.
    reference       TEXT NOT NULL UNIQUE,
    contact_first_name TEXT NOT NULL,
    contact_last_name  TEXT NOT NULL,
    contact_email   TEXT NOT NULL,
    contact_phone   TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'confirmed', 'cancelled', 'expired')),
    total_cents     BIGINT NOT NULL CHECK (total_cents >= 0),
    -- ISO 4217. Checked because an empty currency once got through a handler
    -- that forgot to set it, and a total with no currency is not a price.
    currency        TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    hold_expires_at TIMESTAMPTZ NOT NULL,
    -- Chose to pay by EFT or cash. Held until registration closes rather than
    -- for minutes, because the money arrives on a bank's schedule.
    pay_later       BOOLEAN NOT NULL DEFAULT FALSE,
    -- The form's idempotency key: a resubmitted form finds this row again rather
    -- than booking twice.
    checkout_key    TEXT NOT NULL,
    -- Confirmed by a payment that arrived after its hold lapsed and found the
    -- event full. Never rejected — the money is real — but flagged for a person.
    oversold        BOOLEAN NOT NULL DEFAULT FALSE,
    emailed         BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    confirmed_at    TIMESTAMPTZ,
    cancelled_at    TIMESTAMPTZ,
    UNIQUE (event_id, checkout_key)
);
CREATE INDEX ON registrations (event_id, status);
CREATE INDEX registrations_holds_idx ON registrations (hold_expires_at) WHERE status = 'pending';
CREATE INDEX registrations_created_idx ON registrations (created_at DESC, id DESC);

-- One person holding one ticket: pretix's "order position".
CREATE TABLE attendees (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    registration_id UUID NOT NULL REFERENCES registrations(id) ON DELETE CASCADE,
    ticket_type_id  UUID NOT NULL REFERENCES ticket_types(id),
    first_name      TEXT NOT NULL,
    last_name       TEXT NOT NULL,
    email           TEXT NOT NULL DEFAULT '',
    -- Snapshots: what was bought, at the price it was bought for.
    ticket_name     TEXT NOT NULL,
    unit_price_cents BIGINT NOT NULL CHECK (unit_price_cents >= 0),
    -- No ticket secret is stored: a ticket's code is the attendee id and an
    -- HMAC of it under the server's key (internal/registrations/token.go), so
    -- it can be re-sent at any time and checked at the door without a lookup.
    status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'cancelled')),
    checked_in_at   TIMESTAMPTZ,
    checked_in_by   UUID REFERENCES admin_users(id),
    position        INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX ON attendees (registration_id, position);
CREATE INDEX ON attendees (ticket_type_id);

CREATE TABLE answers (
    id              BIGSERIAL PRIMARY KEY,
    registration_id UUID NOT NULL REFERENCES registrations(id) ON DELETE CASCADE,
    attendee_id     UUID REFERENCES attendees(id) ON DELETE CASCADE,
    question_id     UUID NOT NULL REFERENCES questions(id),
    -- The question as it was asked, so editing a label later does not rewrite
    -- what somebody answered.
    label           TEXT NOT NULL,
    value           TEXT NOT NULL
);
CREATE INDEX ON answers (registration_id);

-- Money received, or attempted, against a registration. A list rather than
-- columns on the registration so deposits and balances need no schema change.
-- A payment's id is what the gateway carries as its reference to us.
CREATE TABLE payments (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    registration_id UUID NOT NULL REFERENCES registrations(id),
    -- A gateway's name (payfast, snapscan, …) or cash / eft. Checked for shape
    -- rather than listed: gateways are defined in code, and adding one should
    -- not need a migration.
    method          TEXT NOT NULL CHECK (method ~ '^[a-z][a-z0-9_]*$'),
    amount_cents    BIGINT NOT NULL CHECK (amount_cents > 0),
    currency        TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    status          TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'paid', 'failed', 'cancelled')),
    gateway_ref     TEXT,
    gateway_status  TEXT NOT NULL DEFAULT '',
    gateway_amount  TEXT NOT NULL DEFAULT '',
    -- The notification as received, for an operator reconciling with the
    -- gateway's dashboard. Text, not JSON: PayFast posts a form, SnapScan JSON.
    gateway_payload TEXT NOT NULL DEFAULT '',
    -- Set for cash and EFT: the administrator who said the money arrived.
    recorded_by     UUID REFERENCES admin_users(id),
    note            TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at         TIMESTAMPTZ
);
CREATE INDEX ON payments (registration_id);
CREATE UNIQUE INDEX ON payments (method, gateway_ref) WHERE gateway_ref IS NOT NULL;

-- The encrypted outbox. A confirmation and an organiser notice are sent once
-- per registration; ticket resends may be queued as often as they are asked
-- for.
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

-- +goose Down
-- Local development only.
DROP TABLE email_jobs;
DROP TABLE payments;
DROP TABLE answers;
DROP TABLE attendees;
DROP TABLE registrations;
DROP TABLE questions;
DROP TABLE ticket_types;
DROP TABLE events;
DROP TABLE admin_setup;
DROP TABLE admin_sessions;
DROP TABLE admin_users;
