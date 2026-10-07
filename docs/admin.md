# Admin and accounts

The admin lives at `/admin`, and lands on the events list.

| Area | Routes | Permission to change |
|---|---|---|
| Events, ticket types, questions, images | `/admin/events`, `/admin/events/{id}/…` | `events.write` |
| An event's attendees as CSV | `GET /admin/events/{id}/attendees.csv` | `read` |
| The door | `/admin/events/{id}/checkin`; see [Check-in](checkin.md) | `checkin` |
| Registrations: record cash or EFT, cancel a registration or one attendee, resend tickets, retry email | `/admin/registrations`, `/admin/registrations/{id}/…` | `registrations.write` |
| Administrator accounts | `/admin/users` | `users.write` |
| Your own profile and password | `/admin/account` | any signed-in account |

Payments and cancellations are covered in [Payments](payments.md), emails in
[Email](email.md).

## First run

Accounts are rows in `admin_users`, not a credential in the environment. There is no
default password.

With no administrator in the database, the server generates a one-time setup token on
startup and logs it:

```sh
docker compose logs server | grep setup_token
```

`/admin` redirects to `/admin/setup`, which exchanges the token for the first account, an
`owner`. The token is spent by the claim, and both setup routes answer `404` from then on,
for good: the claim is recorded in `admin_setup` and never cleared, so neither a restart
nor disabling every account reopens it.

A deploy that nobody reads the logs of can supply `SETUP_TOKEN` (at least 32 characters;
`openssl rand -base64 32`) instead. Then nothing is printed.

If a token was issued and lost before anyone claimed it, the server says so at startup.
The token is stored only as a hash and cannot be recovered; `DELETE FROM admin_setup` makes
the next start issue a new one (or use `SETUP_TOKEN`).

## Sessions

A sign-in inserts a row into `admin_sessions`; the cookie carries 32 random bytes and only
their SHA-256 is stored. Sessions last `SESSION_TTL_HOURS` (default 24). Changing an
account's password, role or disabled state deletes every session it holds in the same
transaction, so the next request from that browser is signed out. An hourly sweep deletes
expired rows; expiry itself is enforced in the lookup query.

- A failed session lookup (a database outage) is a `500`, not a redirect to the login form.
- An htmx request that has lost its session gets `401` with `HX-Refresh: true`, so the page
  reloads into the login form instead of swapping it into a fragment.
- A browser signed in to the admin sees an account menu in the public site's header too.
  It is fetched after the page loads from `/admin/account/menu`, so public pages are the
  same for every visitor and the `/admin`-scoped cookie never has to reach them.

See [Security](security.md#administrators) for the cookie's attributes.

## Roles

Five roles. `owner` and `admin` can do everything; the others are narrower:

| Role | For |
|---|---|
| `owner` | Everything. The role the last-owner guard protects |
| `admin` | Everything, including managing accounts |
| `manager` | Running events and registrations, and the door. Cannot reach `/admin/users` |
| `checkin` | Door volunteers: the check-in page, plus read access to the admin |
| `viewer` | Reads every admin page except the door, changes nothing |

The full permission matrix is in [Security](security.md#roles-and-permissions). Give each
account the least role that covers the job. `checkin` still reads registrations and the
attendee export, so a volunteer who should see nothing else is not yet expressible.

## Managing accounts

`/admin/users` needs `users.write`. What it will not do matters as much as what it will:

- **Accounts are disabled, never deleted.** The record of who did what stays. Disabling
  ends the account's sessions.
- **Nobody may change their own role, disable themselves, or reset their own password
  there.** Those controls are absent for your own row, and the routes answer `409`.
- **The last enabled owner cannot be disabled or demoted.** Both guards take the same
  advisory lock, so two administrators removing two different owners at once cannot both
  succeed.
- **A password somebody else set is temporary.** Creating an account or resetting its
  password sets `must_change_password`, and every admin page then redirects to the change
  form until a new password is chosen.
- **Changing your own password** (`/admin/account`, under the profile icon) asks for the
  current one, ends every session including the current one, and lands on the login form.

## Locked out

If nobody can sign in at all (every owner disabled, or the only password lost), set a
password by hand. `make hashpw` reads a password from the terminal without echoing it and
prints an argon2id hash:

```sh
make hashpw
```

Then, in the database (`make psql`, or `docker compose exec postgres psql -U goevent goevent`
on a server):

```sql
UPDATE admin_users SET password_hash = '<hash>', disabled = false,
    must_change_password = true WHERE lower(email) = 'you@example.com';
DELETE FROM admin_sessions WHERE user_id = (SELECT id FROM admin_users
    WHERE lower(email) = 'you@example.com');
```

The `DELETE` matters: changing the hash by hand skips what the admin does around a
password change, and a session issued under the old password would otherwise stay live.
See [`cmd/hashpw`](../cmd/hashpw/main.go).
