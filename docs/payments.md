# Payments

Two gateways ship, **PayFast** and **SnapScan**, both settling in rand, behind a small
`payment.Gateway` interface in [`internal/payment`](../internal/payment/gateway.go). A
deployment enables either, both, or neither. Alongside them, an event can let people
**pay later** by EFT or cash, which an administrator records by hand.

| | PayFast | SnapScan | Pay later |
|---|---|---|---|
| Hand-over | A cross-origin **POST** of signed fields, submitted on load | A **link**, shown with its QR code | Instructions on screen and by email |
| Needs in the CSP | `form-action` | `img-src` | nothing |
| Confirmed by | An authenticated ITN | An authenticated webhook | An administrator recording the money |
| Sandbox | Yes, and it is the default | **None. Any configuration takes real money** | — |
| Refunds | The PayFast dashboard | The SnapScan merchant portal | By EFT or cash |

With no gateway configured the server still starts, and runs free events and pay-later
events only. See [Configuration](configuration.md#payment-gateways-zero-one-or-both).

## From checkout to confirmation

`registrations.CheckoutWith` ([`internal/registrations/store.go`](../internal/registrations/store.go))
books a registration in one transaction holding the event's row lock. Whether
registration is open, what each ticket costs, and how many seats are held are all read
inside it, so two checkouts for the last seat run one after the other and the second sees
the first's hold. Prices come from the database, never from the form.

What happens next depends on the total and the method chosen:

| Order | Status | Seats held until | Then |
|---|---|---|---|
| Free | `confirmed` at once | — | The confirmation with tickets is queued in the same transaction |
| Paid online (PayFast or SnapScan) | `pending` | **15 minutes** from checkout (`HoldOnline`) | A `pending` payment row is created and the registrant is handed to the gateway |
| Pay later | `pending`, flagged paying later | **When registration closes** (the event's close time, or its start if it has none), and never less than 15 minutes | The payment-instructions email is queued; no payment row yet |

Pay later is offered only when the event allows it, and is the only way to pay a paid
event on a deployment with no gateway.

A hold is what keeps seats: availability counts confirmed registrations and unexpired
holds. A lapsed hold stops counting the instant it lapses. Every minute a sweep marks such
registrations `expired`, which is bookkeeping only; see [Operations](operations.md#background-work).

The registration form carries a `checkout_key`, so a resubmitted form finds the
registration it already made instead of booking twice. Resubmitting a pending online
registration whose payment failed or was abandoned gets a fresh payment row to try with,
as long as the hold still stands.

### Payments are keyed by payment id

The id a gateway is given, and echoes back on its notification, is the **payment's** id,
not the registration's: PayFast's `m_payment_id`, SnapScan's `id`. A registration can have
several payments (a failed attempt and a retry, or several manual part-payments), and each
notification settles exactly the payment it names. The return pages
(`/checkout/success?payment=…`, `/checkout/cancel?payment=…`) and the QR page's status poll
(`/checkout/status?payment=…`) are found the same way, and show only whether that payment
went through. The link to the registration itself is shown only to the browser holding the
cookie the checkout set.

A browser returning to a success URL proves nothing. Only an authenticated notification
can mark a payment paid.

## The callback

`POST /payments/{gateway}/callback` ([`internal/handler/webhook.go`](../internal/handler/webhook.go))
is unauthenticated by definition, and is the only thing that can decide money has changed
hands. So:

- It is mounted outside the CSRF group: a gateway cannot carry a token. It is rate limited
  (`RATE_LIMIT_CALLBACK_PER_MINUTE`, default 120) and reads at most 64 KiB.
- `{gateway}` must name a configured gateway; anything else is ignored.
- The gateway authenticates the notification (below). Nothing is acted on until it has.
- Then `registrations.MarkPaid` does what only this side can, in one transaction:
  1. **Lock the payment row** named by the notification. Not found: logged and answered
     `200`. Most likely two deployments share a merchant account.
  2. **Check the gateway.** A payment made through PayFast cannot be settled by a SnapScan
     notification, or the reverse.
  3. **Replay guard.** A payment already `paid` is left alone and reported as a replay.
     Because the row is locked, a duplicate arriving at the same moment waits for the
     first and then finds it paid.
  4. **Check the amount** against the payment's own amount, in integer cents. A mismatch
     records what the gateway said on the payment row for whoever reconciles it, confirms
     nothing, and is logged at error level.
  5. Mark the payment `paid`. If the registration's paid payments now cover its total, it
     is **confirmed** and its confirmation (and `NOTIFY_EMAIL` copy) is queued in the same
     transaction.

A notification that is not a payment (pending, failed, cancelled) is recorded on the
payment row and never moves a `paid` payment back.

**Answers.** Completed processing and permanent rejections answer `200`, since no retry
will change them. A temporary failure, such as the gateway's verification endpoint being
unreachable or a database error, answers `503` with `Retry-After: 30`, so the gateway
retries; the whole operation is idempotent. A throttled request gets `429`.

A cancelled registration stays cancelled when a payment for it arrives: the payment is
recorded, and cancellation is a decision a payment does not overturn.

### Late payments and overselling

A gateway may confirm a payment after its 15-minute hold lapsed, for instance when someone
finishes an Instant EFT slowly. **The payment is always accepted**: the money is real, and
refusing to record it would lose it. The registration is confirmed. If, in the meantime,
the event or a ticket type filled up with other registrations, it is also flagged
**oversold**, decided under the event's lock so the count is settled.

An oversold registration shows an *Oversold* badge in the admin list, a notice on its own
page and a count on the event's page, and the organiser's `NOTIFY_EMAIL` copy says
`OVERSOLD` in its subject. Deciding what to do, make room or refund, is the organiser's
call.

## Pay later: EFT and cash

When an event allows it, the registration form offers **Pay later by EFT or cash**. The
registrant gets a `received` email with the amount, the reference to quote, the event's
payment instructions and how long their places are held. They get no tickets yet.

When the money arrives, an administrator with `registrations.write` records it on the
registration's admin page: method (cash or EFT), amount, and an optional note.
`registrations.RecordPayment` takes the same locks and confirms the same way a gateway
payment does, so recording the final amount confirms the registration and emails the
tickets. A part-payment is allowed; more than is owed is refused, and so is any payment
against a cancelled registration. The payment row records which administrator entered it.

A payment can be recorded against a registration whose hold has expired. It is confirmed,
and flagged oversold if it no longer fits, exactly as a late gateway payment is.

## Refunds and cancellation

**Refunds are done by hand**, in the PayFast dashboard, the SnapScan merchant portal, or by
EFT. goevent records forward payments only and moves no money back.

Cancelling a registration or a single attendee in the admin releases their seats and stops
their tickets working at the door. It does not refund anything; the admin says so beside
the button.

## SnapScan

The registrant opens one URL. On a phone it opens the SnapScan app; on a desktop the same
URL is rendered as a QR code to scan with a phone. The hand-over page shows both and polls
`GET /checkout/status` every five seconds until the payment is confirmed, then moves to the
success page. Without JavaScript it shows the waiting message and the confirmation still
arrives by email.

### Setting it up

1. Ask SnapScan merchant support for your **snap code**, an **API key** and a **webhook
   authentication key**, and give them the webhook address: `BASE_URL` +
   `/payments/snapscan/callback`. Unlike PayFast's, it is configured on the merchant account
   rather than sent with each payment.
2. Set `SNAPSCAN_SNAP_CODE`, `SNAPSCAN_API_KEY` and `SNAPSCAN_WEBHOOK_AUTH_KEY`. The server
   refuses to start with the first and not the others.
3. Optionally ask them to enable **Secure QR Payload** and set `SNAPSCAN_VALIDATION_KEY`. It
   signs the amount and payment id in the URL so neither can be edited between this page
   and the scan. Without it, `strict=true` (always sent) still refuses an amount below the
   one asked for and a second payment on the same id.

**The QR image URL carries no redirect URLs.** SnapScan's image endpoint answers `403` with
an HTML body when `s_url` or `f_url` is present, and the browser then refuses it as an
image (Chrome logs `ERR_BLOCKED_BY_ORB`). Only the link carries them.

**There is no sandbox.** The first real test is the smallest payment you are willing to
make, and it needs the webhook to reach the server from the internet.

### How a SnapScan notification is authenticated

1. **The body is signed with the webhook key.** `Authorization: SnapScan signature=<hash>`
   is an HMAC-SHA256 of the raw body, compared in constant time.
2. **SnapScan's API agrees.** The payment is read back from
   `/merchant/api/v1/payments/{id}` with the API key, and the status and amount acted on come
   from that response, never from the notification body.
3. **The snap code is ours.**

The amount used is `requiredAmount`, not `totalAmount`, because a tip on a tipping-enabled
account makes the total larger than the figure asked for. A refund arriving down the
webhook is rejected rather than read as a payment.

## PayFast

### Setting it up

1. Get a merchant id and key from the [PayFast dashboard](https://sandbox.payfast.co.za).
   Sandbox credentials have no relationship to a live account's.
2. Set a **salt passphrase** in the dashboard and the same value in `PAYFAST_PASSPHRASE`.
   Set on one side only, every signature fails.
3. Leave `PAYFAST_SANDBOX=true` until a sandbox payment has worked end to end.
4. Make sure PayFast's servers can reach the notify URL, `BASE_URL` +
   `/payments/payfast/callback`. On a laptop that means a tunnel, with its address in
   `PAYFAST_NOTIFY_URL`, and `PAYFAST_ALLOWED_CIDRS=any` for the sandbox (the development
   `compose.yaml` sets this).

Then: register for a paid event, pay on the sandbox, and check that the registration is
confirmed and the tickets arrive in mailpit. Replaying the captured notification with
`curl` must log a replay and change nothing.

### Going live

**`PAYFAST_SANDBOX` defaults to `true`**, so nobody's first afternoon with this project
charges a real card. The mirror mistake is a deployment that never sets it, takes no money,
and looks fine; so both [Compose deployments](deploy/README.md) refuse to start until it is
set explicitly in `.env`.

Switching it off is two changes: your own merchant id, key and passphrase, and
`PAYFAST_SANDBOX=false`. The server **refuses to start** with `PAYFAST_SANDBOX=false` and
PayFast's published sandbox merchant id (`10000100`).

Behind any proxy, **`CLIENT_IP_SOURCE` must describe it**, or the source-IP check compares
PayFast's ranges against the proxy's address and rejects every genuine notification: money
taken, nothing recorded. The deployments set it for you: `forwarded` behind Caddy,
`cloudflare` behind the tunnel. Use `remote` only when nothing sits in front.

### How a PayFast notification is authenticated

The **ITN**, PayFast's form-encoded POST to the notify URL, passes four checks, in this
order, before anything happens ([`internal/payment/payfast/itn.go`](../internal/payment/payfast/itn.go)):

1. **The signature recomputes** over the fields exactly as received, in the order received.
2. **The source IP** is in PayFast's published ranges, or `PAYFAST_ALLOWED_CIDRS`.
3. **PayFast confirms it** when the exact bytes received are posted back to its validate
   endpoint.
4. **The merchant id** is ours.

None is sufficient alone. Only `payment_status=COMPLETE` means the money is taken.

### The PayFast signature

Three details account for nearly every PayFast integration failure, and the package
comment in [`internal/payment/payfast`](../internal/payment/payfast/payfast.go) spells them
out:

- **Field order is submission order, not alphabetical.** This is why `payment.Field` is a
  slice and never a map near a signature.
- **`urlencode` is PHP's**, which differs from Go's `url.QueryEscape` over `~`.
- **Blank fields** are left out of the outgoing form and included when verifying a
  notification, because that is what PayFast does in each direction.

`TestPayFast_SignatureMatchesKnownVector` pins a parameter string and its digest. Put that
string through PayFast's signature tool before taking real money.

## Money

Integer cents everywhere in Go and in the database; a decimal string only at a gateway
boundary and in rendered pages. SnapScan takes integer cents directly.
