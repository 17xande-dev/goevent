// Package events holds what people register for: the event itself, the ticket
// types it offers, and the questions it asks. Registrations — what people
// actually did — are another package's business.
package events

import (
	"slices"
	"time"

	// The IANA database, embedded. The production image is distroless with no
	// /usr/share/zoneinfo, and an event's times are kept in its own zone: without
	// this every LoadLocation would fail there and only there.
	_ "time/tzdata"
)

// DefaultTimezone is where an event happens when nobody said otherwise. The
// project exists for South African organisations, so this is the honest default
// rather than UTC, which nobody's event is in.
const DefaultTimezone = "Africa/Johannesburg"

// Status is where an event is in its life.
type Status string

const (
	// StatusDraft is being prepared. Nobody but an administrator sees it.
	StatusDraft Status = "draft"
	// StatusPublished is visible and, inside its registration window, open.
	StatusPublished Status = "published"
	// StatusClosed is still visible but takes no more registrations.
	StatusClosed Status = "closed"
	// StatusCancelled is not happening. Visible, so a link somebody already has
	// says so rather than 404ing.
	StatusCancelled Status = "cancelled"
)

// Statuses is every status, in life order.
var Statuses = []Status{StatusDraft, StatusPublished, StatusClosed, StatusCancelled}

// Valid reports whether s is one of them.
func (s Status) Valid() bool { return slices.Contains(Statuses, s) }

// Label is the status for a person.
func (s Status) Label() string {
	switch s {
	case StatusDraft:
		return "Draft"
	case StatusPublished:
		return "Published"
	case StatusClosed:
		return "Closed"
	case StatusCancelled:
		return "Cancelled"
	default:
		return string(s)
	}
}

// Public reports whether visitors may see an event at all. A draft is the only
// state that is private.
func (s Status) Public() bool { return s != StatusDraft && s.Valid() }

// Next is where an event may go from s, in the order the admin offers the
// buttons. Cancelled is not a dead end — an event called off by mistake goes
// back to draft and is published again deliberately — but there is no direct
// route from cancelled to published, so reopening is two decisions, not one
// misclick.
func (s Status) Next() []Status {
	switch s {
	case StatusDraft:
		return []Status{StatusPublished, StatusCancelled}
	case StatusPublished:
		return []Status{StatusClosed, StatusDraft, StatusCancelled}
	case StatusClosed:
		return []Status{StatusPublished, StatusCancelled}
	case StatusCancelled:
		return []Status{StatusDraft}
	default:
		return nil
	}
}

// CanBecome reports whether s may change to next.
func (s Status) CanBecome(next Status) bool { return slices.Contains(s.Next(), next) }

// Action is the button that moves an event to s.
func (s Status) Action() string {
	switch s {
	case StatusPublished:
		return "Publish"
	case StatusDraft:
		return "Return to draft"
	case StatusClosed:
		return "Close registration"
	case StatusCancelled:
		return "Cancel event"
	default:
		return string(s)
	}
}

// Event is one thing people register for. It is a single occurrence: a
// multi-date event is an open decision, not something this type pretends to
// model.
type Event struct {
	ID          string
	Slug        string
	Title       string
	Summary     string
	Description string
	Venue       string
	Address     string
	StartsAt    time.Time
	EndsAt      time.Time
	// Timezone is an IANA name. Times are stored as instants and shown in this
	// zone, so an event in Cape Town reads the same on a server in Frankfurt.
	Timezone string
	ImageKey string
	// Capacity is the cap across every ticket type together; nil is unlimited.
	Capacity *int
	Status   Status
	// Listed events appear in the public list; unlisted ones only by link.
	Listed               bool
	RegistrationOpensAt  *time.Time
	RegistrationClosesAt *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// Location is the event's zone, or the default if the stored name is somehow
// not one this binary knows — a display fallback, never a reason to fail a page.
func (e Event) Location() *time.Location {
	if loc, err := time.LoadLocation(e.Timezone); err == nil {
		return loc
	}
	loc, _ := time.LoadLocation(DefaultTimezone)
	return loc
}

// Local returns t in the event's zone, for display.
func (e Event) Local(t time.Time) time.Time { return t.In(e.Location()) }

// When is the event's dates and times for a person, in its own zone: one date
// when it starts and ends on the same day, both when it does not — a weekend
// camp that read "17:00 – 14:00" would look like it ended before it began.
func (e Event) When() string {
	start, end := e.Local(e.StartsAt), e.Local(e.EndsAt)
	const day, clock = "Mon 2 Jan 2006", "15:04"
	if start.Format(day) == end.Format(day) {
		return start.Format(day+", "+clock) + "–" + end.Format(clock)
	}
	return start.Format(day+", "+clock) + " – " + end.Format(day+", "+clock)
}

// RegistrationOpen reports whether a visitor may register at now: published,
// inside the window if one is set, and not yet over.
func (e Event) RegistrationOpen(now time.Time) bool {
	if e.Status != StatusPublished || !now.Before(e.EndsAt) {
		return false
	}
	if e.RegistrationOpensAt != nil && now.Before(*e.RegistrationOpensAt) {
		return false
	}
	if e.RegistrationClosesAt != nil && !now.Before(*e.RegistrationClosesAt) {
		return false
	}
	return true
}

// TicketType is what a person can register as: Planning Center's "selection
// type". A price of zero is a free ticket.
type TicketType struct {
	ID          string
	EventID     string
	Name        string
	Description string
	PriceCents  int64
	// Capacity is how many of this type may be held; nil is unlimited.
	Capacity           *int
	MaxPerRegistration int
	SalesStartAt       *time.Time
	SalesEndAt         *time.Time
	// Hidden types are never offered publicly; an administrator can still book one.
	Hidden bool
	// Archived types are retired: kept because somebody holds one, never offered.
	Archived  bool
	Position  int
	CreatedAt time.Time
}

// Free reports whether the ticket costs nothing.
func (t TicketType) Free() bool { return t.PriceCents == 0 }

// OnSale reports whether the ticket may be offered publicly at now.
func (t TicketType) OnSale(now time.Time) bool {
	if t.Hidden || t.Archived {
		return false
	}
	if t.SalesStartAt != nil && now.Before(*t.SalesStartAt) {
		return false
	}
	if t.SalesEndAt != nil && !now.Before(*t.SalesEndAt) {
		return false
	}
	return true
}

// Scope is who a question is asked of.
type Scope string

const (
	// ScopeRegistration is asked once, of the person registering.
	ScopeRegistration Scope = "registration"
	// ScopeAttendee is asked once per attendee.
	ScopeAttendee Scope = "attendee"
)

// Scopes is every scope.
var Scopes = []Scope{ScopeAttendee, ScopeRegistration}

// Valid reports whether s is one of them.
func (s Scope) Valid() bool { return slices.Contains(Scopes, s) }

// Label is the scope for a person.
func (s Scope) Label() string {
	if s == ScopeRegistration {
		return "Once per registration"
	}
	return "For each attendee"
}

// Kind is the input a question renders as.
type Kind string

const (
	KindText     Kind = "text"
	KindTextarea Kind = "textarea"
	KindSelect   Kind = "select"
	KindCheckbox Kind = "checkbox"
	KindDate     Kind = "date"
	KindPhone    Kind = "phone"
)

// Kinds is every kind, in the order a select offers them.
var Kinds = []Kind{KindText, KindTextarea, KindSelect, KindCheckbox, KindDate, KindPhone}

// Valid reports whether k is one of them.
func (k Kind) Valid() bool { return slices.Contains(Kinds, k) }

// Label is the kind for a person.
func (k Kind) Label() string {
	switch k {
	case KindText:
		return "Short text"
	case KindTextarea:
		return "Long text"
	case KindSelect:
		return "Choose one"
	case KindCheckbox:
		return "Tick box"
	case KindDate:
		return "Date"
	case KindPhone:
		return "Phone number"
	default:
		return string(k)
	}
}

// Question is a custom field on the registration form.
type Question struct {
	ID       string
	EventID  string
	Scope    Scope
	Kind     Kind
	Label    string
	Help     string
	Options  []string
	Required bool
	// TicketTypeIDs limits an attendee question to some ticket types; nil means
	// every one.
	TicketTypeIDs []string
	Position      int
	Archived      bool
	CreatedAt     time.Time
}

// AppliesTo reports whether an attendee holding ticketTypeID is asked this
// question.
func (q Question) AppliesTo(ticketTypeID string) bool {
	if q.Scope != ScopeAttendee {
		return false
	}
	return q.TicketTypeIDs == nil || slices.Contains(q.TicketTypeIDs, ticketTypeID)
}

// LimitedTo reports whether the question names ticketTypeID — for ticking a box
// on the admin form, where nil means none are ticked rather than all.
func (q Question) LimitedTo(ticketTypeID string) bool {
	return slices.Contains(q.TicketTypeIDs, ticketTypeID)
}
