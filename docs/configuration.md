# Configuration

Everything comes from the environment. The source of truth is
[`internal/config/config.go`](../internal/config/config.go); [`.env.example`](../.env.example)
is the annotated template for local development, and each directory in
[`deploy/`](../deploy) has an `.env.example` holding only what that deployment needs.

`goevent -check-config` loads and validates the whole configuration, prints `config: ok`,
and exits without touching the database. `make check-config` runs it with the development
defaults.

## Where configuration and secrets live

Copy `.env.example` to `.env` and edit it. The tracked template contains only PayFast's
published sandbox credentials, local service defaults and a published development
`SECRET_KEY`, none of which belong on a server anyone else can reach. A real `.env` holds
passwords and keys: it is gitignored, and should be readable only by its owner
(`chmod 600 .env`).

A deployment works the same way: one `.env` at `0600` beside its `compose.yaml`. See
[Deploying](deploy/README.md).

**Any credential can instead arrive as a file.** For each secret the server takes,
`KEY_FILE` names a file whose contents are the value:

```sh
SECRET_KEY_FILE=/run/secrets/secret_key
DATABASE_URL_FILE=/run/secrets/database_url
```

That keeps the value out of the process environment, which `docker inspect` shows, and
lets a deployment mount it as a Compose or Kubernetes secret. Setting both `KEY` and
`KEY_FILE` is refused at boot, and so is a file that cannot be read. Trailing newlines are
stripped, so a file written by `echo` or `pass show` works as it is.

The keys that accept `_FILE` are `SECRET_KEY`, `DATABASE_URL`, `SETUP_TOKEN`,
`PAYFAST_MERCHANT_KEY`, `PAYFAST_PASSPHRASE`, `SNAPSCAN_API_KEY`,
`SNAPSCAN_WEBHOOK_AUTH_KEY`, `SNAPSCAN_VALIDATION_KEY`, `SMTP_PASSWORD`,
`SMTP_OAUTH_CLIENT_SECRET`, `GRAPH_CLIENT_SECRET` and `BLOB_SECRET_ACCESS_KEY`
(`secretKeys` in `config.go`). Identifiers that sit beside them, such as a merchant id, an
access key id or a snap code, are not secret and do not.

## `SECRET_KEY`

**Required.** 32 random bytes as 64 hex characters:

```sh
openssl rand -hex 32
```

It is the server's one secret. `deriveKeys` in [`main.go`](../main.go) derives three
separate keys from it, each an HMAC of the secret under its own label:

- the AES-256-GCM key that encrypts queued email (`goevent/outbox/v1`);
- the key that signs registrants' manage links (`goevent/manage-link/v1`);
- the key that signs ticket codes (`goevent/ticket/v1`).

Manage links and ticket codes are not stored anywhere; they are recomputed from the key
whenever they are needed. So **changing `SECRET_KEY` invalidates every manage link and
every ticket QR code already emailed**, and any email still queued becomes undecryptable
(the job fails with "Unable to decrypt queued email. Check SECRET_KEY."). Keep it, and back
it up with the database. See [Security](security.md#manage-links-and-ticket-codes).

## Every variable

| Var | Required | Default | Purpose |
|---|---|---|---|
| `DATABASE_URL` | **yes** | — | Postgres connection string |
| `SECRET_KEY` | **yes** | — | 64 hex characters; see above |
| `PORT` | no | `8080` | Listen port |
| `BASE_URL` | no | `http://localhost:8080` | Public origin, no trailing slash. Builds absolute URLs (emails, gateway return and notify URLs). An `https://` value also turns on `Secure` cookies and HSTS, tells the CSRF check the browser's origin is https, and hides error detail |
| `SITE_NAME` | no | `goevent` | The organisation's name, in the header, page titles and emails |
| `CURRENCY` | no | `ZAR` | Currency code. Both gateways settle in `ZAR` only, and the server refuses to start with a gateway configured and any other value |
| `NOTIFY_EMAIL` | no | — | Where the organiser's copy of each confirmed registration goes. Empty sends only the registrant's mail |
| `SETUP_TOKEN` | no | generated | The one-time token that claims the first administrator. At least 32 characters (`openssl rand -base64 32`). Generated and logged on first boot if unset |
| `SESSION_TTL_HOURS` | no | `24` | How long an admin sign-in lasts. Positive integer |
| `SHUTDOWN_TIMEOUT_SECONDS` | no | `15` | Grace period for in-flight requests on shutdown |
| `LOG_LEVEL` | no | `info` | `debug`, `info`, `warn` or `error` |
| `LOG_FORMAT` | no | `json` | `gcp` renames `level`/`msg` to `severity`/`message` for Google Cloud Logging. See [Logging](operations.md#logging) |
| `CLIENT_IP_SOURCE` | no | `remote` | Where the client address is read from: `remote` (the connection), `forwarded` (leftmost `X-Forwarded-For`, only behind a proxy that replaces it, such as Caddy) or `cloudflare` (`CF-Connecting-IP`, only where Cloudflare is the only way in). The rate limits and PayFast's source-IP check depend on it |
| **Payments** | | | Optional; see the note below |
| `PAYFAST_MERCHANT_ID` | no | — | Its presence switches PayFast on |
| `PAYFAST_MERCHANT_KEY` | with PayFast | — | From the PayFast dashboard |
| `PAYFAST_PASSPHRASE` | no | — | The account's salt passphrase; must match the dashboard exactly |
| `PAYFAST_SANDBOX` | no | `true` | `false` takes real money. Refused with PayFast's published sandbox merchant id (`10000100`). See [Going live](payments.md#going-live) |
| `PAYFAST_NOTIFY_URL` | no | `BASE_URL` + `/payments/payfast/callback` | Override when PayFast cannot reach `BASE_URL`, such as a tunnel during development. Must be an absolute URL |
| `PAYFAST_ALLOWED_CIDRS` | no | PayFast's published ranges | Comma-separated CIDRs a notification may come from. `any` disables the check (sandbox testing only) |
| `SNAPSCAN_SNAP_CODE` | no | — | Its presence switches SnapScan on. **No sandbox: this takes real money** |
| `SNAPSCAN_API_KEY` | with SnapScan | — | Reads payments back from SnapScan's API to confirm a notification |
| `SNAPSCAN_WEBHOOK_AUTH_KEY` | with SnapScan | — | The shared secret a notification's HMAC is computed with |
| `SNAPSCAN_VALIDATION_KEY` | no | — | Secure QR Payload key; signs the amount and payment id in the payment URL |
| **Mail** | | | Required; see the note below |
| `SMTP_HOST` | one transport | — | Mail relay |
| `EMAIL_FROM` | **yes** | — | Sender address, for SMTP and Graph alike |
| `SMTP_PORT` | no | `587` | `465` with `SMTP_TLS=tls`; the dev stack's mailpit is `1026` from the host, `1025` inside Compose |
| `SMTP_TLS` | no | `starttls` | `starttls`, `tls` (implicit) or `none` (development only; logged as a warning) |
| `SMTP_USERNAME` / `SMTP_PASSWORD` | no | — | Omit both for a relay that authenticates by address |
| `SMTP_OAUTH_TENANT_ID` / `SMTP_OAUTH_CLIENT_ID` / `SMTP_OAUTH_CLIENT_SECRET` | no | — | XOAUTH2 instead of a password. All three or none; `SMTP_USERNAME` becomes required and `SMTP_PASSWORD` must be unset. See [Microsoft Exchange Online](email.md#microsoft-exchange-online) |
| `GRAPH_TENANT_ID` / `GRAPH_CLIENT_ID` / `GRAPH_CLIENT_SECRET` | one transport | — | Send through Microsoft Graph instead of SMTP. All three (plus `EMAIL_FROM`) or none; wins over SMTP when set |
| `EMAIL_REPLY_TO` | no | — | When replies should not go to `EMAIL_FROM` |
| **Event images** | | | One backend required; see the note below |
| `IMAGE_DIR` | one backend | — | Store images in this directory, served by this server at `/images/` |
| `BLOB_ENDPOINT` | one backend | — | S3-compatible host[:port], no scheme (R2, GCS interoperability, MinIO) |
| `BLOB_BUCKET` | with `BLOB_ENDPOINT` | — | Bucket name |
| `BLOB_ACCESS_KEY_ID` / `BLOB_SECRET_ACCESS_KEY` | with `BLOB_ENDPOINT` | — | Credentials |
| `BLOB_PUBLIC_BASE_URL` | with `BLOB_ENDPOINT` | — | Where browsers **read** images from; the bucket must be publicly readable there. Absolute URL |
| `BLOB_REGION` | no | `auto` | What R2 wants; GCS and MinIO ignore it |
| `BLOB_USE_TLS` | no | `true` | `false` only for a MinIO on the same machine (logged as a warning) |
| **Theming** | | | See [Theming](theming.md) |
| `TEMPLATE_DIR` | no | — | Directory of templates that override the embedded ones by path. Must exist |
| `STATIC_DIR` | no | — | Directory of assets that override the bundled ones by name. Must exist |
| `THEME_RELOAD` | no | `false` | Re-read both directories on every request. Development only |
| `FONT_ORIGINS` | no | — | Origins a web font may load from. Widens the CSP's `font-src` **and** `style-src`. No `*` |
| `FONT_CSS_URL` | no | — | A hosted font service's stylesheet, linked from every page's `<head>`. Its origin must be in `FONT_ORIGINS` |
| `EMBED_ORIGINS` | no | — | Origins allowed to frame these pages (the CSP's `frame-ancestors`). Empty means `'none'`. `scheme://host[:port]`, no path |
| **Rate limits** | | | Per client IP, per minute; `0` disables one |
| `RATE_LIMIT_LOGIN_PER_MINUTE` | no | `10` | Admin login, the setup claim, and changing your own password |
| `RATE_LIMIT_CHECKOUT_PER_MINUTE` | no | `20` | `POST /events/{slug}/register` |
| `RATE_LIMIT_CALLBACK_PER_MINUTE` | no | `120` | `POST /payments/{gateway}/callback` |
| `RATE_LIMIT_STATUS_PER_MINUTE` | no | `30` | `GET /checkout/status`, the QR hand-over's poll |

`TEST_DATABASE_URL` is read only by the test suite; see [Development](development.md#tests).

`TRUST_PROXY_IP` is refused at boot with a message pointing at `CLIENT_IP_SOURCE`, which
replaced it.

### Payment gateways: zero, one or both

Each gateway's credentials are required only when that gateway is switched on:
`PAYFAST_MERCHANT_ID` needs `PAYFAST_MERCHANT_KEY`, and `SNAPSCAN_SNAP_CODE` needs
`SNAPSCAN_API_KEY` and `SNAPSCAN_WEBHOOK_AUTH_KEY`. With both, the registration form asks
which to use.

**Zero gateways is a valid configuration.** The server logs a warning and starts. Free
events work, and so do paid events that allow paying later: the registrant gets payment
instructions and an administrator records the cash or EFT when it arrives. Paying online
is simply not offered. See [Payments](payments.md).

### Mail is required

A ticket reaches its attendee as a QR code in the confirmation email, so the server
refuses to start without a mail transport: either `SMTP_HOST` and `EMAIL_FROM`, or all of
`GRAPH_TENANT_ID`, `GRAPH_CLIENT_ID`, `GRAPH_CLIENT_SECRET` and `EMAIL_FROM`. `SMTP_HOST`
without `EMAIL_FROM` is refused; `EMAIL_FROM` alone is what a Graph-only deployment looks
like. A partly configured Graph or XOAUTH2 registration is refused rather than left to
fail at the first send. See [Email](email.md).

### One image backend is required

`IMAGE_DIR` and `BLOB_ENDPOINT` are mutually exclusive, and the server refuses to start
with both or neither. `IMAGE_DIR` is the low bar: one path, nothing else running. It suits
a single instance with a persistent volume, not anything behind a load balancer. The
`BLOB_*` set is all or nothing: `BLOB_ENDPOINT` with any of `BLOB_BUCKET`,
`BLOB_ACCESS_KEY_ID`, `BLOB_SECRET_ACCESS_KEY` or `BLOB_PUBLIC_BASE_URL` missing is
refused, and so are any of those set without `BLOB_ENDPOINT`.

## Command-line flags

| Flag | Does |
|---|---|
| `-migrate` | Apply pending migrations and exit. Reads only `DATABASE_URL` (and the log settings) |
| `-migrate-status` | Print each migration's state and exit. Also reads only `DATABASE_URL` |
| `-check-config` | Validate the full configuration and exit, touching nothing. Takes precedence over the two above |

Without a flag the server applies pending migrations, then starts.
