package registrations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/17xande-dev/goevent/internal/db/gen"
	"github.com/jackc/pgx/v5"
)

// Check-in at the door.
//
// A scan is checked in one UPDATE whose WHERE clause is the whole rule — this
// event's ticket, active, on a confirmed registration, not already in — so two
// volunteers scanning the same code at once admit it once. When the UPDATE
// touches nothing, the row is read again only to say why.

// Refusal is why a ticket was not checked in, for the volunteer to read out.
type Refusal string

const (
	// RefusedUnknown is a code for no attendee at all, or another event's.
	RefusedUnknown Refusal = "unknown"
	// RefusedAlready is a ticket already checked in; CheckedInAt says when.
	RefusedAlready Refusal = "already"
	// RefusedCancelled is an attendee, or a registration, that was cancelled.
	RefusedCancelled Refusal = "cancelled"
	// RefusedUnpaid is a registration not yet confirmed — still awaiting payment.
	RefusedUnpaid Refusal = "unpaid"
)

// Door is one attendee as the door sees them.
type Door struct {
	AttendeeID         string
	Name               string
	TicketName         string
	Reference          string
	RegistrationID     string
	RegistrationStatus Status
	AttendeeStatus     string
	CheckedInAt        *time.Time
}

// CanEnter reports whether this attendee may be checked in now.
func (d Door) CanEnter() bool {
	return d.CheckedInAt == nil && d.AttendeeStatus == "active" && d.RegistrationStatus == StatusConfirmed
}

// CheckInResult is what a scan did.
type CheckInResult struct {
	Door
	// Refused is empty when the attendee was checked in now.
	Refused Refusal
}

// CheckIn admits an attendee of eventID, recording who let them in.
func (s *Store) CheckIn(ctx context.Context, eventID, attendeeID, by string) (CheckInResult, error) {
	var who *string
	if by != "" {
		who = &by
	}
	at, err := s.q.CheckIn(ctx, gen.CheckInParams{AttendeeID: attendeeID, EventID: eventID, By: who})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		if errors.Is(translate(err), ErrNotFound) {
			return CheckInResult{Refused: RefusedUnknown}, nil
		}
		return CheckInResult{}, fmt.Errorf("registrations: check in: %w", err)
	}

	row, getErr := s.q.AttendeeForEvent(ctx, attendeeID)
	if getErr != nil || row.EventID != eventID {
		if getErr != nil && !errors.Is(translate(getErr), ErrNotFound) {
			return CheckInResult{}, fmt.Errorf("registrations: read attendee: %w", getErr)
		}
		return CheckInResult{Refused: RefusedUnknown}, nil
	}
	res := CheckInResult{Door: door(row)}
	if err == nil {
		res.CheckedInAt = at
		return res, nil
	}
	switch {
	case row.CheckedInAt != nil:
		res.Refused = RefusedAlready
	case row.Status != "active" || row.RegistrationStatus == string(StatusCancelled):
		res.Refused = RefusedCancelled
	default:
		res.Refused = RefusedUnpaid
	}
	return res, nil
}

// UndoCheckIn reverses a check-in made by mistake.
func (s *Store) UndoCheckIn(ctx context.Context, eventID, attendeeID string) error {
	n, err := s.q.UndoCheckIn(ctx, gen.UndoCheckInParams{AttendeeID: attendeeID, EventID: eventID})
	if err != nil {
		return translate(fmt.Errorf("registrations: undo check-in: %w", err))
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SearchDoor finds an event's attendees by name, reference, or the name or
// email of whoever registered them. A lapsed hold is nobody the door expects.
func (s *Store) SearchDoor(ctx context.Context, eventID, q string) ([]Door, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	rows, err := s.q.SearchAttendees(ctx, gen.SearchAttendeesParams{EventID: eventID, Search: likeEscape(q)})
	if err != nil {
		return nil, translate(fmt.Errorf("registrations: search: %w", err))
	}
	out := make([]Door, len(rows))
	for i, r := range rows {
		out[i] = door(gen.AttendeeForEventRow(r))
	}
	return out, nil
}

// DoorCounts is how many are expected and how many are in.
type DoorCounts struct{ Expected, CheckedIn int }

func (s *Store) DoorCounts(ctx context.Context, eventID string) (DoorCounts, error) {
	r, err := s.q.CheckInCounts(ctx, eventID)
	if err != nil {
		return DoorCounts{}, translate(fmt.Errorf("registrations: door counts: %w", err))
	}
	return DoorCounts{Expected: r.Expected, CheckedIn: r.CheckedIn}, nil
}

func door(r gen.AttendeeForEventRow) Door {
	return Door{
		AttendeeID: r.ID, Name: strings.TrimSpace(r.FirstName + " " + r.LastName),
		TicketName: r.TicketName, Reference: r.Reference, RegistrationID: r.RegistrationID,
		RegistrationStatus: Status(r.RegistrationStatus), AttendeeStatus: r.Status,
		CheckedInAt: r.CheckedInAt,
	}
}

// DoorAttendee is one attendee of eventID as the door sees them; an attendee of
// another event is ErrNotFound.
func (s *Store) DoorAttendee(ctx context.Context, eventID, attendeeID string) (Door, error) {
	row, err := s.q.AttendeeForEvent(ctx, attendeeID)
	if err != nil {
		return Door{}, translate(fmt.Errorf("registrations: read attendee: %w", err))
	}
	if row.EventID != eventID {
		return Door{}, ErrNotFound
	}
	return door(row), nil
}

// likeEscape makes text match itself in a LIKE pattern: a search for "50%" is
// for those three characters, not for everything starting "50".
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
