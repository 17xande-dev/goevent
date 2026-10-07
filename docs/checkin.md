# Check-in

Each event has a door page at `/admin/events/{id}/checkin`, linked from the event's admin
page and from `/admin/checkin`, which lists the events on now or coming up. Both need the
`checkin` permission, which every role except `viewer` holds. The `checkin` role exists
for door volunteers: it reaches these pages and its own account and nothing else, so a
volunteer sees who is expected but not their registrations, and a reference on the door
page is plain text rather than a link. See [Admin and accounts](admin.md#roles).

## Scanning

Every attendee's ticket is a QR code in their confirmation email and on their manage page.
It holds `{attendeeID}.{mac}`, signed with a key derived from `SECRET_KEY`; see
[Security](security.md#manage-links-and-ticket-codes).

The page is built for a **USB or Bluetooth QR scanner in keyboard mode**: the scanner types
the code and presses Enter. The scan box is an ordinary form field with `autofocus`, and
every action redirects back to the page, so the box has focus again for the next ticket
and nobody needs to touch the screen. A phone or laptop with such a scanner paired works
the same way. There is no camera scanning in the browser.

What a scan says:

| Result | Shown |
|---|---|
| Admitted | "*Name* is checked in", with the ticket type and reference |
| Already in | "*Name* was already checked in at HH:MM" (event's time zone). Nothing changes, so scanning twice is harmless |
| Cancelled | The attendee or their registration was cancelled: send them to the registration desk |
| Unpaid | The registration is still awaiting payment, with a link to it |
| Not for this event | A valid ticket for a different event |
| Not a ticket | Text shaped like a ticket code whose signature does not match: a misread, or someone else's ticket |

A check-in is one `UPDATE` whose condition is the whole rule (this event's attendee,
active, on a confirmed registration, not already in), so two volunteers scanning the same
ticket at once admit it once. Who checked each person in is recorded.

## Searching

Anything typed into the box that is not shaped like a ticket code is a search. It matches
attendees of this event by **attendee name**, **registration reference**, or the **name or
email of whoever registered** them, up to 50 results. Registrations whose hold expired are
left out. Each result has a **Check in** button, or shows when they came in with an
**Undo** button, or says why they cannot enter.

## Undo

A mistaken check-in is undone with **Undo**, beside the person in search results or below
the confirmation after a scan. It clears the check-in time; the next scan admits them again.

## Counts

The header shows "*N* of *M* in": active attendees on confirmed registrations, and how many
of them are checked in. It polls `/admin/events/{id}/checkin/counts` every 10 seconds, so
volunteers at several doors each see the whole room.

The attendee CSV export (`/admin/events/{id}/attendees.csv`) includes a *Checked in*
column with the time.

## What it is not

The door page is deliberately simple: one event, scan or search, check in or undo. It
works only online, against the server. A separate, fuller check-in app (rooms, labels,
check-out, the things children's check-in needs) is planned for later and would read
goevent's data; it does not exist yet.
