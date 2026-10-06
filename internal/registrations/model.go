// Package registrations is what people did: who registered for what, the
// seats they hold, and the money paid for them. Events are the other half,
// in internal/events.
package registrations

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/17xande-dev/goevent/internal/db/gen"
)

// Status is where a registration is.
type Status string

const (
	// StatusPending holds seats until HoldExpiresAt while payment is awaited.
	StatusPending Status = "pending"
	// StatusConfirmed is paid in full, or free. Its attendees hold tickets.
	StatusConfirmed Status = "confirmed"
	// StatusCancelled was called off by an administrator.
	StatusCancelled Status = "cancelled"
	// StatusExpired is a hold that lapsed unpaid.
	StatusExpired Status = "expired"
)

// Label is the status for a person.
func (s Status) Label() string {
	switch s {
	case StatusPending:
		return "Awaiting payment"
	case StatusConfirmed:
		return "Confirmed"
	case StatusCancelled:
		return "Cancelled"
	case StatusExpired:
		return "Expired"
	default:
		return string(s)
	}
}

// Method is how a registration is paid for.
type Method string

const (
	// MethodLater is EFT or cash, recorded by an administrator when it arrives.
	MethodLater Method = "later"
	MethodCash  Method = "cash"
	MethodEFT   Method = "eft"
)

// HoldOnline is how long a pending online payment keeps its seats: long enough
// to get through a gateway's page, short enough that an abandoned one does not
// keep the last seat from somebody else for the afternoon.
const HoldOnline = 15 * time.Minute

// Registration is one checkout: a contact person and the attendees they
// registered.
type Registration struct {
	ID            string
	EventID       string
	Reference     string
	ContactFirst  string
	ContactLast   string
	ContactEmail  string
	ContactPhone  string
	Status        Status
	TotalCents    int64
	Currency      string
	HoldExpiresAt time.Time
	PayLater      bool
	// Oversold is set on a registration confirmed by a payment that arrived
	// after its hold lapsed and found the event full. The money is real, so it
	// is never refused — but somebody has to decide what happens.
	Oversold    bool
	Emailed     bool
	CreatedAt   time.Time
	ConfirmedAt *time.Time
	CancelledAt *time.Time
}

// ContactName is the registrant's whole name.
func (r Registration) ContactName() string {
	return strings.TrimSpace(r.ContactFirst + " " + r.ContactLast)
}

// Free reports whether nothing was owed.
func (r Registration) Free() bool { return r.TotalCents == 0 }

// Attendee is one person holding one ticket.
type Attendee struct {
	ID             string
	RegistrationID string
	TicketTypeID   string
	FirstName      string
	LastName       string
	Email          string
	TicketName     string
	UnitPriceCents int64
	Status         string
	CheckedInAt    *time.Time
	Position       int
}

// Name is the attendee's whole name.
func (a Attendee) Name() string { return strings.TrimSpace(a.FirstName + " " + a.LastName) }

// Answer is one answer to one question, with the question as it was asked.
type Answer struct {
	AttendeeID string // empty for a registration-level answer
	QuestionID string
	Label      string
	Value      string
}

// Payment is money received, or attempted, against a registration.
type Payment struct {
	ID             string
	RegistrationID string
	Method         string
	AmountCents    int64
	Currency       string
	Status         string
	GatewayRef     string
	GatewayStatus  string
	CreatedAt      time.Time
	PaidAt         *time.Time
}

// Order is a registration form, submitted: what the checkout is asked to book.
type Order struct {
	EventID     string
	CheckoutKey string
	Contact     Contact
	Attendees   []AttendeeOrder
	// Answers are the registration-level answers.
	Answers []Answer
	// Method is a gateway's name, MethodLater, or empty for a free order.
	Method Method
	// Currency is what every price is in: the deployment's, from configuration.
	Currency string
}

// Contact is the person registering.
type Contact struct {
	FirstName, LastName, Email, Phone string
}

// AttendeeOrder is one attendee on the form.
type AttendeeOrder struct {
	TicketTypeID        string
	FirstName, LastName string
	Email               string
	Answers             []Answer
}

// Result is what the checkout booked.
type Result struct {
	Registration Registration
	Attendees    []Attendee
	// Payment is the pending online payment to hand over to a gateway; nil for
	// a free registration or one paying later.
	Payment *Payment
	// Reused reports that this was a resubmission of a checkout already made.
	Reused bool
}

// Held is the seats currently taken for an event.
type Held struct {
	ByType map[string]int
	Total  int
}

var (
	// ErrNotFound is a registration or payment that does not exist.
	ErrNotFound = errors.New("registrations: not found")
	// ErrClosed means the event is not taking registrations now.
	ErrClosed = errors.New("registrations: registration is not open")
	// ErrEmpty is an order with nobody on it.
	ErrEmpty = errors.New("registrations: choose at least one ticket")
	// ErrPayLater is a pay-later order for an event that does not offer it.
	ErrPayLater = errors.New("registrations: this event does not offer paying later")
	// ErrNoMethod is a paid order with no way to pay chosen.
	ErrNoMethod = errors.New("registrations: choose how to pay")
	// ErrWrongGateway is a notification from a gateway the payment was not made with.
	ErrWrongGateway = errors.New("registrations: notification from the wrong gateway")
	// ErrAmountMismatch is a notification for a different amount than was asked.
	ErrAmountMismatch = errors.New("registrations: paid amount does not match")
)

// UnavailableError is an order for tickets that cannot be had: not on sale, or
// more than one registration may take.
type UnavailableError struct{ Ticket, Reason string }

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("registrations: %s %s", e.Ticket, e.Reason)
}

// SoldOutError is an order that does not fit in what is left. Tickets names
// the ticket types without room; Event is set when the event as a whole is full.
type SoldOutError struct {
	Tickets []string
	Event   bool
}

func (e *SoldOutError) Error() string {
	if e.Event {
		return "registrations: the event is full"
	}
	return "registrations: sold out: " + strings.Join(e.Tickets, ", ")
}

// PaidOutcome is what a payment notification did.
type PaidOutcome int

const (
	// Recorded: the payment is now paid but the registration is not yet paid in full.
	Recorded PaidOutcome = iota
	// Confirmed: the registration is paid in full and now confirmed.
	Confirmed
	// AlreadyPaid: the payment was paid before; a replay.
	AlreadyPaid
)

// Confirmation is a registration that has just been confirmed, with what its
// confirmation email needs.
type Confirmation struct {
	Registration Registration
	Attendees    []Attendee
}

func registration(r gen.Registration) Registration {
	return Registration{
		ID: r.ID, EventID: r.EventID, Reference: r.Reference,
		ContactFirst: r.ContactFirstName, ContactLast: r.ContactLastName,
		ContactEmail: r.ContactEmail, ContactPhone: r.ContactPhone,
		Status: Status(r.Status), TotalCents: r.TotalCents, Currency: r.Currency,
		HoldExpiresAt: r.HoldExpiresAt, PayLater: r.PayLater, Oversold: r.Oversold,
		Emailed: r.Emailed, CreatedAt: r.CreatedAt, ConfirmedAt: r.ConfirmedAt,
		CancelledAt: r.CancelledAt,
	}
}

func attendee(r gen.Attendee) Attendee {
	return Attendee{
		ID: r.ID, RegistrationID: r.RegistrationID, TicketTypeID: r.TicketTypeID,
		FirstName: r.FirstName, LastName: r.LastName, Email: r.Email,
		TicketName: r.TicketName, UnitPriceCents: r.UnitPriceCents, Status: r.Status,
		CheckedInAt: r.CheckedInAt, Position: r.Position,
	}
}

func payment(r gen.Payment) Payment {
	p := Payment{
		ID: r.ID, RegistrationID: r.RegistrationID, Method: r.Method,
		AmountCents: r.AmountCents, Currency: r.Currency, Status: r.Status,
		GatewayStatus: r.GatewayStatus, CreatedAt: r.CreatedAt, PaidAt: r.PaidAt,
	}
	if r.GatewayRef != nil {
		p.GatewayRef = *r.GatewayRef
	}
	return p
}
