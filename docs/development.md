# Developing goevent

## The local stack

[`compose.yaml`](../compose.yaml) runs Postgres, [mailpit](https://mailpit.axllent.org) and
the server. Its host ports are shifted by one from the usual, so it runs beside gostore's
stack or any other local Postgres and mailpit:

| Service | Host address | Inside Compose |
|---|---|---|
| Postgres | `localhost:5433` | `postgres:5432` |
| mailpit SMTP | `localhost:1026` | `mailpit:1025` |
| mailpit web UI | <http://localhost:8026> | — |
| server | <http://localhost:8081> | `server:8080` |

All four are bound to `127.0.0.1`. The credentials in `compose.yaml` (`goevent`/`goevent`,
PayFast's sandbox merchant, the `abab…` `SECRET_KEY`) are published development values.

```sh
make up                                         # build and start everything
curl localhost:8081/healthz                     # -> ok
docker compose logs server | grep setup_token   # the one-time token for /admin/setup
```

Every email the server sends lands in mailpit, never at a real address. Event images are
written to `.local/images`, bind-mounted into the container at `/images`; `make up` creates
the directory and runs the container as your own UID and GID so both sides can write it.

To run the Go server on the host instead, against the Compose Postgres and mailpit:

```sh
make run
```

`make run` starts `postgres` and `mailpit`, then runs `go run .` with development defaults
for everything required (database, `SECRET_KEY`, mail, `IMAGE_DIR=.local/images`, PayFast's
sandbox), so it works on a clean checkout with no `.env`. Override any of them by exporting
it. It uses the same database URL as the tests, `TEST_DATABASE_URL`.

Both `make up` and `make run` serve [`theme/`](../theme) with `THEME_RELOAD=true`, so a
template or stylesheet edit there needs a refresh, not a restart. See [Theming](theming.md).

PayFast's sandbox cannot notify `localhost`. To see a sandbox payment confirm, expose the
server through a tunnel and set `PAYFAST_NOTIFY_URL` to the tunnel's address plus
`/payments/payfast/callback`. SnapScan has no sandbox at all; see [Payments](payments.md).

## Make targets

| Target | Does |
|---|---|
| `make up` | Build and start the whole stack |
| `make down` | Stop it. `make down ARGS=-v` also deletes the database volume |
| `make logs` | Follow the server's logs |
| `make run` | Run the server on the host against the Compose Postgres and mailpit |
| `make test` | Every test, including the database-backed ones, with `-count=1` |
| `make vet`, `make fmt`, `make tidy`, `make build` | `go vet`, `go fmt`, `go mod tidy`, `go build` over `./...` |
| `make psql` | A `psql` shell in the Compose database |
| `make migrate` | Apply pending migrations without starting the server |
| `make migrate-status` | Show which migrations have been applied |
| `make check-config` | Validate the full configuration without starting anything |
| `make sqlc` | Regenerate `internal/db/gen` from the queries and migrations |
| `make sqlc-check` | Fail if the checked-in generated code is stale (what CI runs) |
| `make sqlc-install` | Install the pinned sqlc (`SQLC_VERSION`) |
| `make hashpw` | Read a password from the terminal and print an argon2id hash (lockout recovery; see [Admin](admin.md#locked-out)) |
| `make image` | Build the production image locally, tagged `TAG` |
| `make publish` | Build and push `TAG` and `latest` to GHCR; see [Publishing an image](deploy/README.md#publishing-an-image) |

The gate before a commit:

```sh
gofmt -l . ; go vet ./... && make sqlc-check && make test
```

CI runs `make sqlc-check`, `go vet`, `go build` and `go test -race ./...` against a
Postgres service, and builds the Docker image.

## Tests

```sh
make test          # TEST_DATABASE_URL defaults to the Compose database on port 5433
go test ./...      # database-backed tests skip when TEST_DATABASE_URL is unset
```

Database tests get a private schema each ([`internal/dbtest`](../internal/dbtest/dbtest.go)):
it is created, put on the `search_path`, migrated, and dropped on cleanup. They never
interfere with each other or with development data, which is why `make run` and the tests
can share one database.

**The QR decode test needs `zbarimg`.** `TestPNG_DecodesBackToTheCode` in
[`internal/ticket`](../internal/ticket/qr_test.go) draws a ticket's QR code and reads it
back with a real decoder, because drawing the right number of squares in the wrong places
would pass every other test and fail at the door. It is skipped when `zbarimg` is not on
the `PATH`. Install the `zbar` package (`zbar-tools` on Debian and Ubuntu) to run it.

### No inline styles or event handlers in templates

The Content-Security-Policy has no `'unsafe-inline'` in `style-src` or `script-src`. A
`style="…"` attribute or an `onclick=` in a served page renders, returns `200`, passes
every handler test, and does nothing in a browser. So templates use classes in
`styles.css` for styling and `.js` files for behaviour, never inline attributes.

`TestAssets_EveryServedPageIsFreeOfInlineStylesAndHandlers` in
[`internal/handler/static_test.go`](../internal/handler/static_test.go) enforces it: it
renders the admin, public, registration, hand-over, manage and check-in pages and fails on
any `style="` or `on…=` attribute. A new page should be added to its list. Email bodies are
exempt, because no CSP applies to them.

## The store layer

Queries live in `internal/db/queries/*.sql`; [sqlc](https://sqlc.dev) reads them against
the migrations and generates `internal/db/gen`. The stores in `internal/auth`,
`internal/events`, `internal/registrations` and `internal/outbox` call the generated
methods and map rows onto domain types.

```
internal/db/migrations/*.sql   the schema (goose runs these; sqlc reads them)
internal/db/queries/*.sql      the queries, annotated with -- name: X :one
internal/db/gen/               generated. Do not edit; `make sqlc` rewrites it
internal/{auth,events,registrations,outbox}
                               the stores: mapping, error translation, transactions
```

**Add or change a query:** edit the `.sql` file, run `make sqlc`, then use the new method.
CI fails if the generated code is stale.

Transactions are orchestrated in Go, not in SQL: `registrations.CheckoutWith`,
`registrations.MarkPaid` and `registrations.RecordPayment` lock, check, write and queue
email in one transaction each, and that control flow is the logic. Error translation is
hand-written too.

Things worth knowing before touching [`sqlc.yaml`](../sqlc.yaml):

- **`uuid` is overridden to `string`**, so ids are one type everywhere. A malformed id
  reaches Postgres as error `22P02`, which each store's `translate()` maps to `ErrNotFound`.
- **`timestamptz` is `time.Time`**, and a nullable one `*time.Time`.
- **`int4` is `int`**, and a nullable one `*int`: an optional capacity where `nil` is
  "unlimited" and `0` is "none left".

## Migrations

Numbered `.sql` files in `internal/db/migrations`, managed by
[goose](https://github.com/pressly/goose), embedded into the binary, and applied on boot
under a Postgres advisory lock so several instances starting at once cannot race. The
migrations are also sqlc's idea of the schema, so a schema change and the generated code
change together.

**Before 1.0 there is one migration, `0001_init.sql`, and it is edited in place.** It is
the complete greenfield baseline. To change the schema, edit it, run `make sqlc`, and
recreate the development database, since goose records applied versions and will not
re-run an edited file:

```sh
make down ARGS=-v && make up
```

That deletes the Compose database volume. There is no seed command.

Once there is deployed data to preserve, the rules change:

- **Never edit a migration that has been applied anywhere.** goose records versions, not
  checksums, so an edited file is silently skipped. Add a new one numbered above every
  existing file, with a `-- +goose Up` section.
- **Down sections are for local development.** Production is forward-only.
- **Create extensions in a named schema** (`CREATE EXTENSION … SCHEMA public`). The
  database tests run each in their own schema, and the bare form installs into whichever
  test ran first.
- Statements that cannot run in a transaction, such as `CREATE INDEX CONCURRENTLY`, need
  `-- +goose NO TRANSACTION` at the top of the file.

The files are ordinary goose migrations, so the `goose` CLI works against the directory
when one needs inspecting or applying by hand.

## Dependencies

The objection is to **frameworks**, not libraries: something that owns the shape of the
application would defeat a stdlib-shaped `net/http` and `html/template` design. A small,
single-purpose, widely reviewed library is preferred over hand-rolling anything
security-sensitive. The deciding question is the depth of the problem, not the size of the
dependency.

| Dependency | For |
|---|---|
| [`jackc/pgx/v5`](https://github.com/jackc/pgx) | Postgres driver and pool. No cgo, so the binary stays static |
| [`pressly/goose/v3`](https://github.com/pressly/goose) | Migrations, with advisory locking |
| [`justinas/nosurf`](https://github.com/justinas/nosurf) | CSRF tokens and origin checks |
| [`golang.org/x/crypto`](https://pkg.go.dev/golang.org/x/crypto) | `argon2` for admin passwords; `bcrypt` so older hashes still verify |
| [`golang.org/x/time`](https://pkg.go.dev/golang.org/x/time/rate) | The token bucket behind the rate limits |
| [`17xande-dev/mailer`](https://github.com/17xande-dev/mailer) | Sending email behind one `Sender` interface: SMTP, XOAUTH2 for Exchange, Microsoft Graph, inline images. Shared with gostore. Pulls in [`wneessen/go-mail`](https://github.com/wneessen/go-mail) for MIME |
| [`minio/minio-go/v7`](https://github.com/minio/minio-go) | Event images in S3-compatible storage |
| [`rsc.io/qr`](https://pkg.go.dev/rsc.io/qr) | Encoding ticket codes as QR codes; [`internal/ticket`](../internal/ticket/qr.go) draws them as PNG and SVG |

Everything else is the standard library. htmx is vendored into
[`internal/handler/static`](../internal/handler/static/README.md), not loaded from a CDN.

### Build-time tools

| Tool | For |
|---|---|
| [`sqlc`](https://sqlc.dev) | Generates the stores' row structs and scan code from the SQL |

sqlc is pinned in the Makefile (`SQLC_VERSION`), not as a `go tool` directive in `go.mod`.
`go tool` would put about forty indirect modules that never reach the binary into the file
that is meant to state the binary's dependencies. `make sqlc-install` installs the pinned
version; CI runs it before anything else.
