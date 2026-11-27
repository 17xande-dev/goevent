# Email

Mail is required: a confirmation carries the attendee's tickets, and the tickets exist
nowhere else a registrant would look. The server refuses to start without SMTP or Microsoft
Graph configured; see [Configuration](configuration.md#mail-is-required).

Sending lives in [`github.com/17xande-dev/mailer`](https://github.com/17xande-dev/mailer),
shared with gostore. `newMailer` in [`main.go`](../main.go) builds one sender at startup:
Graph when it is configured, SMTP otherwise. The two are not layered.

## What gets sent

Every email is a job in `outbox_jobs`, managed by
[`github.com/17xande-dev/outbox`](https://github.com/17xande-dev/outbox). It names a
registration as `registration:<id>` and one of four kinds
([`internal/handler/mail.go`](../internal/handler/mail.go)):

| Kind | To | When | Carries |
|---|---|---|---|
| `confirmation` | the person who registered | the registration is confirmed: at checkout for a free one, or when a payment pays it in full (a gateway's or one an admin records) | a QR code per active attendee, and the manage link |
| `received` | the person who registered | a pay-later registration is made | how much to pay, the event's pay-later instructions, the reference to quote, how long places are held, and the manage link. No tickets |
| `notify` | `NOTIFY_EMAIL` | alongside every `confirmation`, when `NOTIFY_EMAIL` is set | the reference, contact, total and an admin link. Plain text. Subject and body start with `OVERSOLD` when the registration was [oversold](payments.md#late-payments-and-overselling) |
| `resend` | the person who registered | an admin presses **Send the tickets again** on a confirmed registration | the same as `confirmation` |

`confirmation`, `received` and `notify` each get the unique key `registration:<id>:<kind>`,
so a repeated enqueue cannot add another job of that kind. `resend` has no unique key
and can be sent as often as an admin asks. The application keeps completed jobs so their
unique keys remain reserved.

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

A worker in each server process checks for due jobs every three seconds and is woken
after a successful enqueue. It claims a job with a two-minute lease and commits the claim
before sending, so no row lock or connection is held across the send. Each delivery has a
30-second timeout. A successful confirmation marks the registration emailed in the same
transaction that marks the job done. No request waits for a mail server.

- **Encrypted.** Each payload is sealed with AES-256-GCM under key ID `1`, derived from
  `SECRET_KEY` and authenticated with its kind, subject and unique key. The application
  writes an empty payload and renders the email from the database at send time. A
  successful send clears the stored ciphertext.
- **Retried.** A failure records a generic reason, then waits about 1 minute, 5 minutes,
  30 minutes, 2 hours and 6 hours before succeeding or reaching **Dead** on the sixth
  failed attempt. Unsent jobs survive restarts. Worker failures are logged by error class;
  panics are contained and their values are omitted. Payloads and transport error text
  can carry ticket codes or credentials and are not logged.
- **At least once.** If the relay accepts a message and the process dies before recording
  success, a later attempt can send it again. This can also happen if the confirmation's
  database hook fails. A cancelled delivery during graceful shutdown can be retried
  without spending an attempt.
- **A key change strands the queue.** A job sealed under a previous `SECRET_KEY` goes
  straight to **Dead** with "cannot open payload: check the key"; it never reaches the
  mail transport. Drain the queue before rotating the key. Rotating it also invalidates
  every manage link and ticket already sent ([Configuration](configuration.md#secret_key)).

### Upgrading a development database

Migration `0003_drop_email_jobs` removes the old `email_jobs` table and its indexes.
**Existing development databases lose pending jobs and old email history.** Drain the old
queue first if those messages need to go out. The migration leaves registrations and
tickets intact. `-migrate` and normal startup apply the application's migrations first,
then the outbox migrations, which are recorded in `outbox_goose_version`. Each migration
provider takes its own PostgreSQL advisory lock.

### Seeing and retrying

The registration's admin page (`/admin/registrations/{id}`) lists its email jobs under
**Emails**: kind, state (including **Dead**), attempts, when it was sent, and the last
error. **Retry failed emails** brings pending jobs forward and resets dead jobs to
pending with a fresh attempt budget and the administrator recorded on the retry.
Already-sent and running jobs are unaffected. **Send the tickets again**, shown on a
confirmed registration, queues a `resend`. Both need the `registrations.write` permission.

After an outage, use the queue page (`/admin/mail`, **Email** in the admin menu) instead
of opening registrations one by one. It shows the pending, running, done and dead counts
and lists the dead emails with their kind, attempts, the fixed failure reason and a link to
the registration they belong to. Each row has a **Retry** button (`registrations.write`,
CSRF-protected); it resets that one job to pending with a fresh attempt budget and records
who did it. If the job is no longer dead (someone else retried it, or the page is old) the
page says so and changes nothing. The list shows the newest 100 dead emails and says when
there are more; retrying some makes the rest appear. The events page shows a link with the
dead count whenever it is not zero.

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
