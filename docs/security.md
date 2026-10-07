# Security

## CSRF

Every state-changing request from a browser, in the admin or on the registration form,
needs a [nosurf](https://github.com/justinas/nosurf) token: a `csrf_token` form field (the
`{{template "csrf" .CSRFToken}}` partial) or an `X-CSRF-Token` header. Without one the
request gets `403`; an htmx request gets `HX-Refresh: true` as well, since reloading is both
the explanation and the fix.

CSRF covers a group of routes rather than the whole server (`FirstPartyHandler` in
[`internal/handler/admin.go`](../internal/handler/admin.go)): everything under `/admin/`,
and `/events/{slug}/register`. nosurf sets a cookie on every response it handles, and the
event pages that only read stay cookie-free. Grouping is also what exempts the payment
callback: it is not in the group at all, rather than excused by a path string that has to
keep matching the route.

Two traps if you extend it:

- **A page rendered outside the group gets an empty `.CSRFToken`**, and every form on it is
  refused with `403`. A new page with a form must be inside the group.
- nosurf requires an unsafe request to identify its origin (`Sec-Fetch-Site`, `Origin` or
  `Referer`). Browsers send one; **`curl` does not**, so manual testing needs
  `-H "Origin: http://localhost:8080"`.

nosurf compares the origin against one built from the `Host` header and a scheme taken from
**`BASE_URL`**, not the connection: behind a TLS-terminating proxy the connection is plain
HTTP while the browser's origin is https. A wrong `BASE_URL` breaks every form with `403`.

## Administrators

Accounts are rows in `admin_users`; there is no credential in the environment and no
default password. The first account is claimed at `/admin/setup` with a one-time token. See
[Admin and accounts](admin.md) for the flow.

**Sessions are rows, not signed cookies.** The cookie carries 32 random bytes; only their
SHA-256 is stored, so a leaked backup hands over no live session. Changing an account's
password, role or disabled state deletes its sessions in the same transaction. The cookie
is `HttpOnly`, `SameSite=Lax`, scoped to `Path=/admin`, and `Secure` when `BASE_URL` is
https. Signing in replaces any session the browser arrived with (no session fixation). A
`?next=` redirect is accepted only as a clean path under `/admin/`.

### Roles and permissions

Every admin route names the permission it needs on the line that registers it
(`RegisterAdmin` in `admin.go`); a route cannot be added without one. Permissions are a
static map in [`internal/auth/model.go`](../internal/auth/model.go), not a table.

| Permission | Covers | `owner` | `admin` | `manager` | `checkin` | `viewer` |
|---|---|---|---|---|---|---|
| `account` | your own profile and password, and the `/admin/` front door | yes | yes | yes | yes | yes |
| `read` | the event and registration pages that only display, including the attendee CSV export | yes | yes | yes | — | yes |
| `events.write` | events, ticket types, questions, event images | yes | yes | yes | — | — |
| `registrations.write` | recording cash and EFT payments, cancelling, resending tickets, retrying email | yes | yes | yes | — | — |
| `checkin` | the door page: checking in and undoing it | yes | yes | yes | yes | — |
| `users.write` | `/admin/users`: creating accounts, changing roles, disabling, resetting passwords | yes | yes | — | — | — |

`checkin` is the role for door volunteers. It does not hold `read`: a check-in account
reaches the door pages and its own account, and the door shows names, ticket types and
references but never contact details, answers or payments. A `viewer` cannot reach the
door. A disabled account holds no permissions at all, and an
unknown role fails closed.

Templates hide controls a role cannot use, but that is presentation: the enforcement is
`requirePerm` on the route, which answers `403`.

## Registrants

Registering needs no account. What a registrant holds instead are two credentials, both
derived from `SECRET_KEY`.

### Manage links and ticket codes

[`internal/registrations/token.go`](../internal/registrations/token.go):

- **Manage link:** `/r/{reference}?t={token}`, where the token is an HMAC-SHA256 of the
  registration's id, truncated to 128 bits. It opens the registrant's own page: their
  details, answers, status and, once confirmed, their tickets. A wrong reference or token
  is a plain `404`, with no hint which half was wrong. The page is sent with
  `Cache-Control: private, no-store` and `Referrer-Policy: no-referrer`.
- **Ticket code:** `{attendeeID}.{mac}`, the attendee's id and an HMAC of it under a
  separate key, also 128 bits. This is what the QR code holds.

The two keys are derived from `SECRET_KEY` under different labels, so a manage token can
never be presented as a ticket or the reverse, and both are compared in constant time.

They are HMACs rather than random secrets stored as hashes so they can be produced again:
a confirmation sent days after a pay-later checkout carries the same link, and resending
tickets does not replace them. **The cost is that neither is revocable.** Nothing short of
rotating `SECRET_KEY` invalidates one, and rotating it invalidates every link and ticket at
once (and strands queued email; see [Configuration](configuration.md#secret_key)). Treat a
forwarded confirmation email as a forwarded ticket.

**A cancelled ticket is refused at the door by status, not by code.** Check-in admits an
attendee only if the attendee is active, the registration is confirmed, and they are not
already in; a cancelled attendee or registration is refused with "cancelled", an unpaid one
with "awaiting payment". See [Check-in](checkin.md).

After checkout, the registering browser also gets a `goevent_reg` cookie (24 hours,
`HttpOnly`, `SameSite=Lax`) holding the registration id and its manage token, so the
gateway's return page can link back to the registration for that browser only. Anyone
else opening a return URL sees only whether that payment went through.

### Attendee data in exports

`GET /admin/events/{id}/attendees.csv` contains what people typed into a public form.
Every cell beginning `=`, `+`, `-`, `@`, a tab or a carriage return is prefixed with `'`,
so a spreadsheet treats it as text rather than a formula (`cells` in
[`admin_registrations.go`](../internal/handler/admin_registrations.go)). A South African
phone number starting `+27` is the everyday case. The response is `private, no-store`.

## The payment callback

`POST /payments/{gateway}/callback` is unauthenticated by definition and is the only route
that can mark money paid. Each gateway authenticates its own notification (signature,
source IP, a server-to-server check and merchant id for PayFast; an HMAC, an API read-back
and snap code for SnapScan), and `registrations.MarkPaid` then checks the payment, gateway
and amount and guards against replays. See [Payments](payments.md#the-callback).

## Rate limits

Per client IP, a token bucket from
[`golang.org/x/time/rate`](https://pkg.go.dev/golang.org/x/time/rate), applied on the line
that registers each route rather than around a prefix:

| Route | Default | Why |
|---|---|---|
| `POST /admin/login`, `POST /admin/setup`, `POST /admin/account` | 10/min, one shared allowance | Each verifies a secret. argon2id makes an attempt expensive, but cost is not a limit |
| `POST /events/{slug}/register` | 20/min | Registration spam, loose enough that a double-click never trips it |
| `POST /payments/{gateway}/callback` | 120/min | Unauthenticated, and an accepted PayFast or SnapScan notification makes the server call the gateway back: an amplifier |
| `GET /checkout/status` | 30/min | The QR hand-over page's poll |

The burst is a third of the allowance, minimum two, so `10/min` is three attempts at once
and then one every six seconds. A refusal is `429` with `Retry-After`: a page for people,
a bare status for the gateway. Setting a limit to `0` disables it and logs a warning. Only
the POST on `/admin/login` is limited, so an operator can always read the page explaining
why. Idle buckets are evicted lazily during ordinary requests, so the map cannot grow
without bound as an attacker cycles addresses.

The limits key on the client address, so `CLIENT_IP_SOURCE` must describe what is in front
of the server. Naming a header nothing sets lets a client choose its own address.

## Password hashing

argon2id via `x/crypto/argon2`, with RFC 9106's second recommended parameters: 64 MiB,
three passes, four lanes, in a standard PHC string:

```
$argon2id$v=19$m=65536,t=3,p=4$<salt>$<hash>
```

The parameters live in the hash, so raising them needs no migration. A bcrypt hash still
verifies. Verification caps the memory a stored hash may request at 1 GiB, so a hand-edited
hash cannot take the server down at the next login.

## Response headers

Every response, including `404`s and `/healthz`
([`internal/middleware/securityheaders.go`](../internal/middleware/securityheaders.go)):

```
Content-Security-Policy: default-src 'self'; img-src 'self' <bucket> <qr gateway>;
  font-src 'self' <font origins>; style-src 'self' <font origins>; script-src 'self';
  form-action 'self' <form gateway>; base-uri 'none'; object-src 'none';
  frame-ancestors <EMBED_ORIGINS, or 'none'>
X-Content-Type-Options: nosniff
Referrer-Policy: strict-origin-when-cross-origin
Permissions-Policy: geolocation=(), camera=(), microphone=(), payment=()
Strict-Transport-Security: max-age=63072000; includeSubDomains   (https deployments only)
```

The placeholders are the only external origins any directive gets, each named by the
deployment: the image bucket's public origin, PayFast's form target, SnapScan's QR image
host, embedders, and a font service. There is no `'unsafe-inline'` anywhere:

- **No inline styles or scripts in templates.** A `style` attribute, a `<style>` block, an
  `on…=` attribute or an inline `<script>` is silently blocked. CSS goes in a stylesheet
  and JavaScript in a `.js` file, both from `STATIC_DIR` if a theme needs its own. A test
  enforces this for the shipped templates; see
  [Development](development.md#no-inline-styles-or-event-handlers-in-templates).
- **`FONT_ORIGINS` opens two directives**, `font-src` and `style-src`, because a hosted font
  is a stylesheet and then the files it names. See [Web fonts](theming.md#web-fonts).
- Ticket QR codes on the manage page are inline SVG with no style or external reference.
  Email bodies use inline styles, because no CSP applies to them.

HSTS is sent only when `BASE_URL` is https. There is no `preload`; that is the operator's
decision.

## Error detail

On an `http://` `BASE_URL` an error page shows the underlying Go error, which names tables
and constraints; on `https://` it shows only a reference to find in the logs. See
[Operations](operations.md#when-something-goes-wrong).
