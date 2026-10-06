# goevent

Self-hostable event registration and ticketing, written in Go, with South African
payment processors. It does a small part of what Planning Center Registrations and
Check-Ins do: you publish an event, people register and pay for themselves and the
people they bring, and each attendee gets a QR ticket to check in with at the door.

Server-rendered `html/template` with a little htmx, PostgreSQL, and
[PayFast](https://payfast.io) and [SnapScan](https://www.snapscan.co.za) for
payment. Stdlib-first, with a deliberately small dependency surface.

> **Status: early.** The foundation works: configuration, migrations, the container
> stack, administrator accounts with roles, the security baseline (CSP, CSRF, rate
> limits) and the payment gateway code. Events, registration, checkout, tickets
> and check-in are being built in that order.
>
> There is no default admin password. On first start the server logs a one-time
> setup token, and `/admin/setup` exchanges it for the first administrator account.
> The compose stack ships PayFast's **published sandbox credentials** and a
> **published development email key** so it works out of the box; replace both
> before anybody else can reach a deployment.

## Why

Planning Center does not support South African payment processors. Churches and
other organisations here end up running events through it and taking money
somewhere else by hand. goevent takes the money in rand, through processors that
settle in South Africa, and keeps the rest small.

It is a sibling of [gostore](https://github.com/17xande-dev/gostore), and its
payment, admin and security code started as a copy of gostore's.

## Quickstart

```sh
git clone https://github.com/17xande-dev/goevent
cd goevent
make up
curl localhost:8081/healthz   # -> ok
docker compose logs server | grep setup_token   # the one-time token for the first admin
open http://localhost:8081/admin      # redirects to /admin/setup; paste the token there
```

`make up` starts Postgres, [mailpit](http://localhost:8026) (captures outgoing email)
and the server. The host ports are shifted by one (5433, 1026, 8026, 8081), so this
stack runs beside another local Postgres. Event images live in `.local/images`.

To run the Go server on your host instead, against the compose Postgres and mailpit:

```sh
make run
```

## Development

```sh
make test          # every test, including the database-backed ones (needs make up or make run's postgres)
make sqlc          # regenerate internal/db/gen after editing internal/db/queries or migrations
make sqlc-check    # what CI runs: fail if the generated code is stale
```

The gate before a commit:

```sh
gofmt -l . ; go vet ./... && make sqlc-check && make test
```

## Decisions

| Decision | Choice | What would change it |
|---|---|---|
| Database | Postgres via pgx, sqlc, goose | — (parity with gostore; the check-in app may read the same database) |
| People | Contact and attendee details live on each registration; no accounts, no people database | Check-in needing household lookup by phone |
| Payments | Pay in full online (PayFast or SnapScan), free events, cash or EFT recorded by an admin | Deposits and balances — the schema already records payments as a list |
| No gateway configured | Allowed: free and manually-settled events only | — |
| Capacity | A pending registration holds its seats for 15 minutes; a payment arriving after the hold lapsed is never refused, but flagged *oversold* | — |
| Refunds | In the gateway's own dashboard | A gateway with a refund API worth wiring up |

### Decisions still open

| Decision | Candidates | Trigger |
|---|---|---|
| QR code generation | `rsc.io/qr`, `skip2/go-qrcode` | Tickets (next phases) |
| Multi-date events | An `event_times` table | The first recurring or multi-session event |
| Check-in app | A separate app reading goevent through an API | Kids' check-in: rooms, security labels, check-out |
| More gateways | Yoco, Ozow, Paystack, Peach | Demand from an adopter |
| Shared payment module | Extract `internal/payment` from gostore and goevent | Both apps stable |
| Discount codes, waitlists, reminders | — | After the MVP |

## Licence

MIT. See [LICENSE](LICENSE).
