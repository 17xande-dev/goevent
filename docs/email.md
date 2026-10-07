# Email

Mail is required: a confirmation carries the attendee's tickets, and the tickets exist
nowhere else a registrant would look. The server refuses to start without SMTP or Microsoft
Graph configured; see [Configuration](configuration.md#mail-is-required).

Sending lives in [`github.com/17xande-dev/mailer`](https://github.com/17xande-dev/mailer),
shared with gostore. `newMailer` in [`main.go`](../main.go) builds one sender at startup:
Graph when it is configured, SMTP otherwise. The two are not layered.

## What gets sent

Every email is a job in the `email_jobs` table naming **a registration and a kind**. There
are four kinds ([`internal/outbox`](../internal/outbox/outbox.go)):

| Kind | To | When | Carries |
|---|---|---|---|
| `confirmation` | the person who registered | the registration is confirmed: at checkout for a free one, or when a payment pays it in full (a gateway's or one an admin records) | a QR code per active attendee, and the manage link |
| `received` | the person who registered | a pay-later registration is made | how much to pay, the event's pay-later instructions, the reference to quote, how long places are held, and the manage link. No tickets |
| `notify` | `NOTIFY_EMAIL` | alongside every `confirmation`, when `NOTIFY_EMAIL` is set | the reference, contact, total and an admin link. Plain text. Subject and body start with `OVERSOLD` when the registration was [oversold](payments.md#late-payments-and-overselling) |
| `resend` | the person who registered | an admin presses **Send the tickets again** on a confirmed registration | the same as `confirmation` |

`confirmation`, `received` and `notify` are unique per registration (a partial unique index
on `(registration_id, kind)`), so a replayed payment notification cannot queue them twice.
`resend` is not, and can be sent as often as an admin asks.

The `confirmation` and `received` mails have a plain-text part and an HTML part. The
`notify` mail is plain text only.

### Rendered at send time

A job stores no message. The worker reads the registration, its event and its attendees
from the database when it sends, and renders the templates then
(`deliver` in [`internal/handler/mail.go`](../internal/handler/mail.go)). Manage links and
ticket codes are HMACs of database ids under `SECRET_KEY`, so they are recomputed rather
than stored; a confirmation sent days after a pay-later checkout carries the same manage
link the registrant already has, and a resend carries the same ticket codes as the first.

An attendee cancelled before the mail goes out gets no ticket in it: only attendees whose
status is `active` are included.

### Ticket QR codes are inline images

Each ticket's QR code is rendered as a PNG ([`internal/ticket`](../internal/ticket/qr.go))
and attached as an inline part with a `Content-ID` (`ticket-1`, `ticket-2`, …); the HTML
refers to it as `<img src="cid:ticket-1">`. Nothing is fetched from a server when the mail
is opened, so the code shows in clients that block remote images. The registrant's manage
page (`/r/{reference}?t=…`) shows the same codes as inline SVG once the registration is
confirmed.

## The outbox

Jobs are written **inside the transaction that makes them true**: the checkout that
confirms a free registration, the payment that confirms a paid one, the checkout that
creates a pay-later one. If the job cannot be written, the transaction rolls back; for a
gateway notification that means a `503`, so the gateway retries.

A worker in the server process ([`StartMailWorker`](../internal/handler/mail.go)) checks
for due jobs every five seconds and sends up to twenty per pass. Each job is claimed with
`SELECT … FOR UPDATE SKIP LOCKED` and its row lock is held during a send bounded to 30
seconds, so several instances never send the same job. No request ever waits for a mail
server.

- **Encrypted.** Each job has a payload sealed with AES-256-GCM under a key derived from
  `SECRET_KEY`, bound to its registration id and kind so a payload moved to another row
  fails to decrypt. The payloads written today are empty, since emails are rendered at send
  time; the encryption is there so a future job carrying something sensitive needs no
  extra care. A successful send clears the payload.
- **Retried.** A failure records the attempt and a generic reason, and the job is retried
  with exponential backoff from 30 seconds up to one hour. Unsent jobs survive a restart.
  The worker never logs payloads or the transport's error text, since either could carry a
  ticket code or a credential; the real cause is visible to whoever runs the mail relay.
- **At least once.** If the relay accepts a message and the process dies before recording
  success, the job is sent again. Payment and confirmation are idempotent; a duplicate
  email is the worst case.
- **A key change strands the queue.** A job sealed under a previous `SECRET_KEY` fails with
  "Unable to decrypt queued email. Check SECRET_KEY." Drain the queue before rotating the
  key, and remember that rotating it also invalidates every manage link and ticket already
  sent ([Configuration](configuration.md#secret_key)).

### Seeing and retrying

The registration's admin page (`/admin/registrations/{id}`) lists its email jobs under
**Emails**: kind, attempts, when it was sent, and the last error. **Retry failed emails**
makes every unsent job for that registration due now; it does not resend what was already
sent. **Send the tickets again**, shown on a confirmed registration, queues a `resend`.
Both need the `registrations.write` permission.

## SMTP

```sh
SMTP_HOST=smtp.example.com
SMTP_PORT=587
SMTP_TLS=starttls          # or tls (implicit, port 465), or none (development only)
SMTP_USERNAME=...
SMTP_PASSWORD=...
EMAIL_FROM=events@example.com
EMAIL_REPLY_TO=office@example.com   # optional
```

Omit the username and password for a relay that authenticates by address, which is how
the development stack reaches mailpit. A relay usually rejects a `From` on a domain it is
not responsible for.

## Microsoft Exchange Online

Basic Auth for SMTP client submission is going away, so an Exchange mailbox is reached
either with **XOAUTH2** over SMTP or through **Microsoft Graph**. Tokens are fetched with
the app registration's client credentials and cached until shortly before they expire.

### SMTP with XOAUTH2

```sh
SMTP_HOST=smtp.office365.com
SMTP_PORT=587
SMTP_TLS=starttls
SMTP_USERNAME=events@example.com   # the mailbox; XOAUTH2 authenticates as a named one
EMAIL_FROM=events@example.com
SMTP_OAUTH_TENANT_ID=...
SMTP_OAUTH_CLIENT_ID=...
SMTP_OAUTH_CLIENT_SECRET=...
# and no SMTP_PASSWORD; setting both refuses to boot
```

The tenant-side setup is the part that fails silently: a tenant that has not been set up
issues a token happily and the mail server then refuses it. You need an Entra ID app
registration with the **`SMTP.SendAsApp`** application permission (Office 365 Exchange
Online) and admin consent, a service principal for it registered in Exchange Online,
**Send As** on the mailbox, and **SMTP AUTH enabled for that mailbox**, which is off
tenant-wide by default. Check each against Microsoft's current documentation.

### Microsoft Graph

The alternative to the above, not an addition to it. Set the three Graph variables and
Graph is used instead of SMTP; `SMTP_HOST` need not be set, and `EMAIL_FROM` names the
mailbox sent as:

```sh
EMAIL_FROM=events@example.com
GRAPH_TENANT_ID=...
GRAPH_CLIENT_ID=...
GRAPH_CLIENT_SECRET=...
```

It needs a **different** permission: **`Mail.Send`** (application, admin-consented), not
`SMTP.SendAsApp`, and no SMTP AUTH setting at all. On its own `Mail.Send` lets the app send
as any mailbox in the tenant; restrict it to the one it sends as with Exchange Online's
RBAC for Applications. The [tunnel deployment](deploy/tunnel.md) uses Graph.

Exchange Online is a mailbox service rather than a transactional relay, and throttles
accordingly. For a large event, a transactional provider over ordinary SMTP is the
lower-risk choice.

## Email templates

In [`internal/handler/templates/mail`](../internal/handler/templates/mail), overridable from
`TEMPLATE_DIR` like any other template:

| File | Is |
|---|---|
| `email_confirmation.gohtml`, `email_confirmation.txt` | The confirmation and the resend |
| `email_received.gohtml`, `email_received.txt` | The pay-later acknowledgement |
| `email_notify.txt` | The organiser's copy |

The `.txt` files go through `text/template` and the rest through `html/template`; running
a plain-text body through the HTML escaper would put `&amp;` in front of a registrant. No
layout wraps them. The HTML is deliberately primitive: table layout and inline styles,
since mail clients lag browsers by years and no CSP applies to email. The subjects are set
in code, not in the templates.

The data each template gets is `mailData` in `mail.go`: `.SiteName`, `.Event`,
`.Registration`, `.Tickets` (each with `.Name`, `.TicketName`, `.Code` and `.CID`),
`.ManageURL` and `.AdminURL`.
