package registrations

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/17xande-dev/goevent/internal/db/gen"
	"github.com/jackc/pgx/v5"
)

// What an administrator does to registrations: find them, record money that
// arrived outside a gateway, and call them off.

var (
	// ErrNotCancellable is a registration that is already cancelled.
	ErrNotCancellable = errors.New("registrations: already cancelled")
	// ErrCancelled refuses recording a payment against a cancelled registration:
	// the seats were released, and taking money for them is a decision to undo
	// the cancellation first, not a side effect of typing an amount.
	ErrCancelled = errors.New("registrations: the registration is cancelled")
	// ErrAmount is a manual payment of nothing, or of more than is owed.
	ErrAmount = errors.New("registrations: that amount is not owed")
)

// Filter narrows the admin's list. Zero values mean "any".
type Filter struct {
	EventID string
	Status  Status
	Search  string
}

// Listed is one row of the admin's list.
type Listed struct {
	Registration
	EventTitle    string
	AttendeeCount int
	PaidCents     int64
}

// Owed is what is left to pay.
func (l Listed) Owed() int64 { return max(0, l.TotalCents-l.PaidCents) }

// List returns registrations for the admin, newest first, at most 500: past
// that, a person filters rather than scrolls.
func (s *Store) List(ctx context.Context, f Filter) ([]Listed, error) {
	p := gen.ListRegistrationsParams{}
	if f.EventID != "" {
		p.EventID = &f.EventID
	}
	if f.Status != "" {
		st := string(f.Status)
		p.Status = &st
	}
	if q := strings.TrimSpace(f.Search); q != "" {
		p.Search = &q
	}
	rows, err := s.q.ListRegistrations(ctx, p)
	if err != nil {
		return nil, translate(fmt.Errorf("registrations: list: %w", err))
	}
	out := make([]Listed, len(rows))
	for i, r := range rows {
		out[i] = Listed{
			Registration: registration(gen.Registration{
				ID: r.ID, EventID: r.EventID, Reference: r.Reference,
				ContactFirstName: r.ContactFirstName, ContactLastName: r.ContactLastName,
				ContactEmail: r.ContactEmail, ContactPhone: r.ContactPhone, Status: r.Status,
				TotalCents: r.TotalCents, Currency: r.Currency, HoldExpiresAt: r.HoldExpiresAt,
				PayLater: r.PayLater, CheckoutKey: r.CheckoutKey, Oversold: r.Oversold,
				Emailed: r.Emailed, CreatedAt: r.CreatedAt, ConfirmedAt: r.ConfirmedAt,
				CancelledAt: r.CancelledAt,
			}),
			EventTitle: r.EventTitle, AttendeeCount: r.AttendeeCount, PaidCents: r.PaidCents,
		}
	}
	return out, nil
}

// Payments returns a registration's payments, oldest first.
func (s *Store) Payments(ctx context.Context, regID string) ([]Payment, error) {
	rows, err := s.q.ListPayments(ctx, regID)
	if err != nil {
		return nil, translate(fmt.Errorf("registrations: payments: %w", err))
	}
	out := make([]Payment, len(rows))
	for i, r := range rows {
		out[i] = payment(r)
	}
	return out, nil
}

// Manual is money an administrator says arrived: cash at the office, an EFT on
// the bank statement.
type Manual struct {
	RegistrationID string
	Method         Method // MethodCash or MethodEFT
	AmountCents    int64
	RecordedBy     string // the administrator's id
	Note           string
}

// RecordPayment records a manual payment and, when it pays the registration in
// full, confirms it and runs onConfirm — in one transaction, under the
// registration's lock, exactly as a gateway's payment does. The amount may be
// less than what is owed, for somebody paying in parts, but not more.
func (s *Store) RecordPayment(ctx context.Context, m Manual, onConfirm OnConfirm) (confirmed bool, err error) {
	if m.Method != MethodCash && m.Method != MethodEFT {
		return false, fmt.Errorf("registrations: %q is not a manual payment method", m.Method)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("registrations: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := s.q.WithTx(tx)

	reg, err := q.LockRegistration(ctx, m.RegistrationID)
	if err != nil {
		return false, translate(fmt.Errorf("registrations: lock: %w", err))
	}
	if reg.Status == string(StatusCancelled) {
		return false, ErrCancelled
	}
	paid, err := q.PaidTotal(ctx, reg.ID)
	if err != nil {
		return false, fmt.Errorf("registrations: paid total: %w", err)
	}
	if m.AmountCents <= 0 || m.AmountCents > reg.TotalCents-paid {
		return false, ErrAmount
	}
	now := time.Now()
	by := m.RecordedBy
	if _, err := q.CreatePayment(ctx, gen.CreatePaymentParams{
		RegistrationID: reg.ID, Method: string(m.Method), AmountCents: m.AmountCents,
		Currency: reg.Currency, RecordedBy: &by, Note: strings.TrimSpace(m.Note),
		Status: "paid", PaidAt: &now,
	}); err != nil {
		return false, fmt.Errorf("registrations: record payment: %w", err)
	}

	conf, ok, err := confirmIfPaid(ctx, q, reg.ID)
	if err != nil {
		return false, err
	}
	if ok && onConfirm != nil {
		if err := onConfirm(ctx, q, conf); err != nil {
			return false, err
		}
	}
	return ok, tx.Commit(ctx)
}

// Cancel calls a registration off, releasing its seats. Money already taken is
// refunded by hand, in the gateway's dashboard or the bank's: a cancellation
// records a decision, it does not move money.
func (s *Store) Cancel(ctx context.Context, regID string) (Registration, error) {
	r, err := s.q.CancelRegistration(ctx, regID)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, getErr := s.Get(ctx, regID); getErr != nil {
			return Registration{}, getErr
		}
		return Registration{}, ErrNotCancellable
	}
	if err != nil {
		return Registration{}, translate(fmt.Errorf("registrations: cancel: %w", err))
	}
	return registration(r), nil
}

// CancelAttendee cancels one person on a registration, releasing their seat.
// Their ticket stops working at the door from then on.
func (s *Store) CancelAttendee(ctx context.Context, regID, attendeeID string) error {
	n, err := s.q.CancelAttendee(ctx, gen.CancelAttendeeParams{ID: attendeeID, RegistrationID: regID})
	if err != nil {
		return translate(fmt.Errorf("registrations: cancel attendee: %w", err))
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// TicketCount is one ticket type's seats.
type TicketCount struct{ Confirmed, Held int }

// Stats is an event's figures for its admin page.
type Stats struct {
	ByType    map[string]TicketCount
	Confirmed int
	Held      int
	PaidCents int64
	Oversold  int
}

// EventStats counts an event's seats and the money received for it.
func (s *Store) EventStats(ctx context.Context, eventID string) (Stats, error) {
	rows, err := s.q.EventTicketCounts(ctx, eventID)
	if err != nil {
		return Stats{}, translate(fmt.Errorf("registrations: counts: %w", err))
	}
	st := Stats{ByType: make(map[string]TicketCount, len(rows))}
	for _, r := range rows {
		st.ByType[r.TicketTypeID] = TicketCount{Confirmed: r.Confirmed, Held: r.Held}
		st.Confirmed += r.Confirmed
		st.Held += r.Held
	}
	money, err := s.q.EventMoney(ctx, eventID)
	if err != nil {
		return Stats{}, translate(fmt.Errorf("registrations: money: %w", err))
	}
	st.PaidCents, st.Oversold = money.PaidCents, money.Oversold
	return st, nil
}

// ExportRow is one attendee in an event's export.
type ExportRow struct {
	Attendee
	Reference          string
	RegistrationStatus Status
	Contact            string
	ContactEmail       string
	ContactPhone       string
	RegisteredAt       time.Time
	// Answers by question id: the attendee's own and their registration's.
	Answers map[string]string
}

// Export returns every active attendee of an event, with their answers, in
// registration order.
func (s *Store) Export(ctx context.Context, eventID string) ([]ExportRow, error) {
	rows, err := s.q.ExportAttendees(ctx, eventID)
	if err != nil {
		return nil, translate(fmt.Errorf("registrations: export: %w", err))
	}
	ans, err := s.q.ExportAnswers(ctx, eventID)
	if err != nil {
		return nil, translate(fmt.Errorf("registrations: export answers: %w", err))
	}
	byAttendee := map[string]map[string]string{}
	byRegistration := map[string]map[string]string{}
	for _, a := range ans {
		target := byRegistration
		key := a.RegistrationID
		if a.AttendeeID != nil {
			target, key = byAttendee, *a.AttendeeID
		}
		if target[key] == nil {
			target[key] = map[string]string{}
		}
		target[key][a.QuestionID] = a.Value
	}
	out := make([]ExportRow, len(rows))
	for i, r := range rows {
		answers := map[string]string{}
		maps.Copy(answers, byRegistration[r.RegistrationID])
		maps.Copy(answers, byAttendee[r.AttendeeID])
		out[i] = ExportRow{
			Attendee: Attendee{
				ID: r.AttendeeID, RegistrationID: r.RegistrationID, FirstName: r.FirstName,
				LastName: r.LastName, Email: r.Email, TicketName: r.TicketName,
				UnitPriceCents: r.UnitPriceCents, CheckedInAt: r.CheckedInAt,
			},
			Reference: r.Reference, RegistrationStatus: Status(r.RegistrationStatus),
			Contact:      strings.TrimSpace(r.ContactFirstName + " " + r.ContactLastName),
			ContactEmail: r.ContactEmail, ContactPhone: r.ContactPhone, RegisteredAt: r.CreatedAt,
			Answers: answers,
		}
	}
	return out, nil
}
