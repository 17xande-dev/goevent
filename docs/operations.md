# Operations

goevent is one process: the HTTP server, plus a few goroutines doing background work on
the same database pool. There is no cron container, no queue broker and no separate
worker.

## Health

`GET /healthz` pings the database with a two-second timeout. It answers `200 ok` when the
database is reachable and `503 database unreachable` (logged at error level) when it is
not, so a container platform or load balancer can gate traffic on it.

Migrations run on boot, before the server listens, under a Postgres advisory lock so
several instances starting at once cannot race. To migrate as a separate step, run the
same binary with `-migrate` first; it needs only `DATABASE_URL`. `-check-config` validates
the full environment without touching anything, so a deploy can fail on a missing setting
before the schema moves:

```sh
goevent -check-config && goevent -migrate && exec goevent
```

On `SIGTERM` or `SIGINT` the server stops accepting connections, waits up to
`SHUTDOWN_TIMEOUT_SECONDS` (default 15) for in-flight requests, and stops the mail worker
before closing the pool.

## Background work

All of it runs in every instance rather than being elected to one. Each sweep is a single
idempotent statement, and the mail worker locks the jobs it sends, so two instances end in
the state one would have reached.

| Work | Interval | Does |
|---|---|---|
| Hold expiry | on boot, then every minute | Marks `pending` registrations whose hold has lapsed as `expired` ([`cleanup.go`](../cleanup.go)) |
| Session cleanup | on boot, then hourly | Deletes expired admin sessions |
| Mail worker | every 5 seconds | Sends up to 20 due email jobs per pass; see [Email](email.md#the-outbox) |

**Hold expiry is bookkeeping.** Availability already counts only confirmed registrations
and unexpired holds, so a lapsed hold's seats were free the instant it lapsed. The sweep
makes the registration's status say so, on the registrant's page and in the admin. A
payment that arrives after expiry still confirms the registration; see
[Late payments](payments.md#late-payments-and-overselling).

**Session cleanup is housekeeping, not security.** Expiry is enforced in the session
lookup's own query; the sweep only keeps the table small.

Each sweep run has its own one-minute timeout, detached from shutdown, so it finishes or
gives up on its own terms. A failed sweep is logged at error level and retried at the next
tick. A sweep that removes something logs how many at info level.

The mail worker's failures are visible per registration in the admin, and retried with
backoff from 30 seconds to an hour; a failed send is logged as `email queue` at error level
without the message or the transport's error text.

## When something goes wrong

Every HTML surface answers a failure with a rendered page: an unknown URL, an expired
form, a request that came too fast, a fault on the server. Three templates cover it:
`pages/not_found.gohtml` for 404, `pages/error_client.gohtml` for the rest of the 4xx range,
`pages/error_server.gohtml` for 5xx, sharing `partials/error_reference.gohtml`. All are
overridable from `TEMPLATE_DIR`.

**In development the page says what broke; in production it does not.** An `https://`
`BASE_URL` shows only a reference; anything else shows the Go error as well. That is the
same signal `Secure` cookies and HSTS use. The error string names tables and constraints,
which is useful to whoever is writing the code and reconnaissance to anyone else.

**Every response carries a request id**, echoed as `X-Request-Id` and shown on the error
page as the reference. The same id is on every log line for that request. An id arriving in
`X-Cloud-Trace-Context` (set by Cloud Run) or `X-Request-Id` is adopted, cleaned and
truncated, rather than replaced.

Two exceptions:

- **Byte endpoints stay plain.** A missing `/static/…` or `/images/…` gets Go's one-line
  404.
- **A broken theme still answers correctly.** If an error template will not render, plain
  text is sent with the same status.

### Errors and htmx

htmx does not swap `4xx` or `5xx` responses by default, which would silently drop every
refusal sent as a fragment. `partials/document.gohtml` configures `responseHandling` to swap
errors too. So an error response either fills the element that asked for it, or, for a
whole error page, sends `HX-Retarget: body` and replaces the document. `HX-Refresh` is used
where a reload is the fix: an expired CSRF token, or an admin session that has ended.

## Logging

**JSON to stdout, and nothing else.** No log file, no log table. That is what every
container platform reads: `docker compose logs server`, `make logs`, or Cloud Logging.

`LOG_FORMAT=gcp` renames `level` and `msg` to `severity` and `message`, which Google Cloud
Logging needs for severity filters and alerts to work. `LOG_LEVEL` is `debug`, `info`,
`warn` or `error`.

Errors are not written to the database: it is the most likely thing to be broken during an
incident. What the schema keeps is facts that need acting on: a registration's `oversold`
flag, each payment's gateway status, amount and raw notification, and each email job's
attempts and last error.

Lines worth watching for:

| Level | Message | Means |
|---|---|---|
| error | `payment amount does not match; NOT confirming the registration` | A genuine notification for a different amount. Reconcile by hand |
| error | `failed to apply a payment notification` | Money may be taken and not recorded. The gateway retries; check the database |
| warn | `rejected payment callback` | A notification failed authentication. Its `error` says which check: a signature mismatch is usually a passphrase, an IP rejection usually `CLIENT_IP_SOURCE` |
| warn | `payment callback names an unknown payment` | Probably another deployment sharing the merchant account |
| error | `email queue` | A send failed; see the registration's email list |
| warn | `no administrator exists…` | Unclaimed; see [Admin](admin.md#first-run) |
| warn | `THEME_RELOAD is on…` | Development setting left on in a deployment |
| warn | `no payment gateway is configured…` | Expected when running free and pay-later events only |

Metrics, tracing and alerting are not built in. A healthy server logs a handful of lines
at boot and then per-event lines; the `error` stream is high-signal.
