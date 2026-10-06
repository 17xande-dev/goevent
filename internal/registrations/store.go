package registrations

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/17xande-dev/goevent/internal/db/gen"
	"github.com/17xande-dev/goevent/internal/events"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// noRegistration is an id no registration has, for HeldByTicketType when no
// registration is to be left out of the count.
const noRegistration = "00000000-0000-0000-0000-000000000000"

// Store is the persistence for registrations, their attendees and payments.
type Store struct {
	pool *pgxpool.Pool
	q    *gen.Queries
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, q: gen.New(pool)}
}

// Held returns the seats currently taken for an event, for showing what is
// left. It is a snapshot: the checkout counts again under the event's lock, and
// that count is the one that decides.
func (s *Store) Held(ctx context.Context, eventID string) (Held, error) {
	return held(ctx, s.q, eventID, noRegistration)
}

func held(ctx context.Context, q *gen.Queries, eventID, exclude string) (Held, error) {
	rows, err := q.HeldByTicketType(ctx, gen.HeldByTicketTypeParams{EventID: eventID, ExcludeID: exclude})
	if err != nil {
		return Held{}, translate(fmt.Errorf("registrations: count held: %w", err))
	}
	h := Held{ByType: make(map[string]int, len(rows))}
	for _, r := range rows {
		h.ByType[r.TicketTypeID] = r.Held
		h.Total += r.Held
	}
	return h, nil
}

// Remaining is how many more of t could be booked given h, and whether there
// is a limit at all. The smaller of the ticket type's and the event's room.
func Remaining(e events.Event, t events.TicketType, h Held) (n int, limited bool) {
	n = -1
	if t.Capacity != nil {
		n = max(0, *t.Capacity-h.ByType[t.ID])
	}
	if e.Capacity != nil {
		left := max(0, *e.Capacity-h.Total)
		if n < 0 || left < n {
			n = left
		}
	}
	return n, n >= 0
}

// Checkout books an order.
//
// Everything that decides whether the order fits is read inside one
// transaction holding the event's row lock: whether registration is open, what
// each ticket costs, and how many seats are held. Two checkouts for the last
// seat therefore run one after the other, and the second sees the first's
// hold. Prices come from the rows read here, never from the form.
//
// A free order is confirmed at once. A paid one is pending, holding its seats
// for HoldOnline — or, paying later, until registration closes — with a pending
// payment row for the gateway to be handed.
//
// The checkout key makes it idempotent: a resubmitted form finds the
// registration it already made instead of booking twice.
func (s *Store) Checkout(ctx context.Context, o Order, now time.Time) (Result, error) {
	return s.CheckoutWith(ctx, o, now, Hooks{})
}

// Hooks run inside the checkout's transaction, so what they write — queued
// emails — commits with the registration or not at all.
type Hooks struct {
	// Confirmed runs for a registration confirmed at checkout: a free one.
	Confirmed OnConfirm
	// AwaitingPayment runs for one that will be paid later, by EFT or cash.
	AwaitingPayment func(ctx context.Context, q *gen.Queries, r Registration) error
}

// CheckoutWith is Checkout with hooks. It is a separate method rather than a
// parameter so the many callers that want none — tests about seats and
// prices — need not spell out an empty value.
func (s *Store) CheckoutWith(ctx context.Context, o Order, now time.Time, hooks Hooks) (Result, error) {
	if len(o.Attendees) == 0 {
		return Result{}, ErrEmpty
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("registrations: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := s.q.WithTx(tx)

	erow, err := q.LockEvent(ctx, o.EventID)
	if err != nil {
		return Result{}, translate(fmt.Errorf("registrations: lock event: %w", err))
	}
	e := events.FromRow(erow)

	if existing, err := q.RegistrationByCheckoutKey(ctx, gen.RegistrationByCheckoutKeyParams{
		EventID: o.EventID, CheckoutKey: o.CheckoutKey,
	}); err == nil {
		res, err := s.resume(ctx, q, registration(existing), o.Method)
		if err != nil {
			return Result{}, err
		}
		return res, tx.Commit(ctx)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Result{}, fmt.Errorf("registrations: find checkout: %w", err)
	}

	if !e.RegistrationOpen(now) {
		return Result{}, ErrClosed
	}

	ttRows, err := q.ListTicketTypes(ctx, o.EventID)
	if err != nil {
		return Result{}, fmt.Errorf("registrations: ticket types: %w", err)
	}
	types := make(map[string]events.TicketType, len(ttRows))
	for _, r := range ttRows {
		types[r.ID] = events.TicketTypeFromRow(r)
	}

	// What was asked for, per ticket type, in the order the form listed it.
	wanted := map[string]int{}
	var order []string
	var total int64
	for _, a := range o.Attendees {
		t, ok := types[a.TicketTypeID]
		if !ok || !t.OnSale(now) {
			name := "that ticket"
			if ok {
				name = t.Name
			}
			return Result{}, &UnavailableError{Ticket: name, Reason: "is not on sale"}
		}
		if wanted[t.ID] == 0 {
			order = append(order, t.ID)
		}
		wanted[t.ID]++
		if wanted[t.ID] > t.MaxPerRegistration {
			return Result{}, &UnavailableError{Ticket: t.Name,
				Reason: fmt.Sprintf("is limited to %d per registration", t.MaxPerRegistration)}
		}
		total += t.PriceCents
	}

	h, err := held(ctx, q, o.EventID, noRegistration)
	if err != nil {
		return Result{}, err
	}
	if err := fits(e, types, order, wanted, h); err != nil {
		return Result{}, err
	}

	// How it will be paid for, and so how long its seats are held.
	status, hold, payLater := StatusConfirmed, now, false
	var confirmedAt *time.Time
	switch {
	case total == 0:
		confirmedAt = &now
	case o.Method == MethodLater:
		if !e.PayLater {
			return Result{}, ErrPayLater
		}
		status, payLater = StatusPending, true
		hold = e.RegistrationDeadline()
		if floor := now.Add(HoldOnline); hold.Before(floor) {
			hold = floor
		}
	case o.Method == "":
		return Result{}, ErrNoMethod
	default:
		status, hold = StatusPending, now.Add(HoldOnline)
	}

	reg, err := createRegistration(ctx, tx, gen.CreateRegistrationParams{
		EventID: o.EventID, ContactFirstName: o.Contact.FirstName, ContactLastName: o.Contact.LastName,
		ContactEmail: o.Contact.Email, ContactPhone: o.Contact.Phone, Status: string(status),
		TotalCents: total, Currency: o.Currency, HoldExpiresAt: hold, PayLater: payLater,
		CheckoutKey: o.CheckoutKey, ConfirmedAt: confirmedAt,
	})
	if err != nil {
		return Result{}, err
	}

	res := Result{Registration: registration(reg)}
	for i, a := range o.Attendees {
		t := types[a.TicketTypeID]
		row, err := q.CreateAttendee(ctx, gen.CreateAttendeeParams{
			RegistrationID: reg.ID, TicketTypeID: t.ID, FirstName: a.FirstName, LastName: a.LastName,
			Email: a.Email, TicketName: t.Name, UnitPriceCents: t.PriceCents, Position: i,
		})
		if err != nil {
			return Result{}, fmt.Errorf("registrations: add attendee: %w", err)
		}
		res.Attendees = append(res.Attendees, attendee(row))
		for _, ans := range a.Answers {
			if err := saveAnswer(ctx, q, reg.ID, &row.ID, ans); err != nil {
				return Result{}, err
			}
		}
	}
	for _, ans := range o.Answers {
		if err := saveAnswer(ctx, q, reg.ID, nil, ans); err != nil {
			return Result{}, err
		}
	}

	if status == StatusPending && !payLater {
		p, err := q.CreatePayment(ctx, gen.CreatePaymentParams{
			RegistrationID: reg.ID, Method: string(o.Method), AmountCents: total,
			Currency: reg.Currency, Status: "pending",
		})
		if err != nil {
			return Result{}, fmt.Errorf("registrations: create payment: %w", err)
		}
		pp := payment(p)
		res.Payment = &pp
	}

	switch {
	case status == StatusConfirmed && hooks.Confirmed != nil:
		if err := hooks.Confirmed(ctx, q, Confirmation{Registration: res.Registration, Attendees: res.Attendees}); err != nil {
			return Result{}, err
		}
	case payLater && hooks.AwaitingPayment != nil:
		if err := hooks.AwaitingPayment(ctx, q, res.Registration); err != nil {
			return Result{}, err
		}
	}
	return res, tx.Commit(ctx)
}

// resume answers a resubmitted checkout with what the first submission made.
// A pending online registration whose payment failed or was abandoned gets a
// fresh payment to try again with, as long as its hold still stands.
func (s *Store) resume(ctx context.Context, q *gen.Queries, reg Registration, method Method) (Result, error) {
	res := Result{Registration: reg, Reused: true}
	rows, err := q.ListAttendees(ctx, reg.ID)
	if err != nil {
		return Result{}, fmt.Errorf("registrations: attendees: %w", err)
	}
	for _, r := range rows {
		res.Attendees = append(res.Attendees, attendee(r))
	}
	if reg.Status != StatusPending || reg.PayLater || method == "" || method == MethodLater {
		return res, nil
	}
	p, err := q.PendingPayment(ctx, gen.PendingPaymentParams{RegistrationID: reg.ID, Method: string(method)})
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		if !reg.HoldExpiresAt.After(time.Now()) {
			return res, nil
		}
		p, err = q.CreatePayment(ctx, gen.CreatePaymentParams{
			RegistrationID: reg.ID, Method: string(method), AmountCents: reg.TotalCents,
			Currency: reg.Currency, Status: "pending",
		})
		if err != nil {
			return Result{}, fmt.Errorf("registrations: retry payment: %w", err)
		}
	default:
		return Result{}, fmt.Errorf("registrations: pending payment: %w", err)
	}
	pp := payment(p)
	res.Payment = &pp
	return res, nil
}

// fits reports whether wanted fits in what h leaves, naming every ticket type
// that is short rather than the first, so the form can say all of it at once.
func fits(e events.Event, types map[string]events.TicketType, order []string, wanted map[string]int, h Held) error {
	var short []string
	n := 0
	for _, id := range order {
		t := types[id]
		n += wanted[id]
		if t.Capacity != nil && h.ByType[id]+wanted[id] > *t.Capacity {
			short = append(short, t.Name)
		}
	}
	if len(short) > 0 {
		return &SoldOutError{Tickets: short}
	}
	if e.Capacity != nil && h.Total+n > *e.Capacity {
		return &SoldOutError{Event: true}
	}
	return nil
}

// createRegistration inserts with a fresh reference, trying again under a
// savepoint in the unlikely event the reference is taken.
func createRegistration(ctx context.Context, tx pgx.Tx, p gen.CreateRegistrationParams) (gen.Registration, error) {
	for range 5 {
		ref, err := newReference()
		if err != nil {
			return gen.Registration{}, err
		}
		p.Reference = ref
		sp, err := tx.Begin(ctx)
		if err != nil {
			return gen.Registration{}, fmt.Errorf("registrations: savepoint: %w", err)
		}
		r, err := gen.New(sp).CreateRegistration(ctx, p)
		if err == nil {
			return r, sp.Commit(ctx)
		}
		sp.Rollback(ctx)
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); !ok || pgErr.Code != "23505" ||
			!strings.Contains(pgErr.ConstraintName, "reference") {
			return gen.Registration{}, fmt.Errorf("registrations: create: %w", err)
		}
	}
	return gen.Registration{}, errors.New("registrations: could not find a free reference")
}

// referenceAlphabet has no 0/O, 1/I/L: a reference is read over the phone and
// typed at a door.
const referenceAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// newReference is six characters as XXX-XXX, about 887 million of them.
func newReference() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("registrations: reference: %w", err)
	}
	out := make([]byte, 0, 7)
	for i, c := range b {
		if i == 3 {
			out = append(out, '-')
		}
		out = append(out, referenceAlphabet[int(c)%len(referenceAlphabet)])
	}
	return string(out), nil
}

func saveAnswer(ctx context.Context, q *gen.Queries, regID string, attendeeID *string, a Answer) error {
	if err := q.CreateAnswer(ctx, gen.CreateAnswerParams{
		RegistrationID: regID, AttendeeID: attendeeID, QuestionID: a.QuestionID,
		Label: a.Label, Value: a.Value,
	}); err != nil {
		return fmt.Errorf("registrations: save answer: %w", err)
	}
	return nil
}

// Notice is an authenticated gateway notification, in this package's terms.
type Notice struct {
	PaymentID   string
	Gateway     string
	Ref         string
	Status      string
	AmountCents int64
	Amount      string
	Raw         string
}

// OnConfirm runs inside the transaction that confirms a registration, so
// whatever it writes — the confirmation emails, queued — commits with the
// confirmation or not at all.
type OnConfirm func(ctx context.Context, q *gen.Queries, c Confirmation) error

// MarkPaid applies a "paid" notification.
//
// In one transaction: the payment row is locked, so a replayed notification
// waits for the first and then finds it paid; the amount is checked against
// what was asked; the payment is marked paid; and if that pays the registration
// in full it is confirmed and onConfirm runs. A payment for a registration
// whose hold lapsed is still taken — the money is real — and the registration
// is confirmed, flagged oversold if the event filled in the meantime.
func (s *Store) MarkPaid(ctx context.Context, n Notice, onConfirm OnConfirm) (PaidOutcome, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("registrations: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	q := s.q.WithTx(tx)

	p, err := q.LockPayment(ctx, n.PaymentID)
	if err != nil {
		return 0, translate(fmt.Errorf("registrations: lock payment: %w", err))
	}
	if p.Method != n.Gateway {
		return 0, ErrWrongGateway
	}
	if p.Status == "paid" {
		return AlreadyPaid, nil
	}
	if n.AmountCents != p.AmountCents {
		// Recorded so an operator can see it, but the payment stays as it was.
		if err := recordNotice(ctx, q, n, p.Status); err != nil {
			return 0, err
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, err
		}
		return 0, ErrAmountMismatch
	}
	if err := recordNotice(ctx, q, n, "paid"); err != nil {
		return 0, err
	}
	if err := q.MarkPaymentPaid(ctx, p.ID); err != nil {
		return 0, fmt.Errorf("registrations: mark paid: %w", err)
	}

	conf, ok, err := confirmIfPaid(ctx, q, p.RegistrationID)
	if err != nil {
		return 0, err
	}
	outcome := Recorded
	if ok {
		outcome = Confirmed
		if onConfirm != nil {
			if err := onConfirm(ctx, q, conf); err != nil {
				return 0, err
			}
		}
	}
	return outcome, tx.Commit(ctx)
}

// RecordUnpaid applies a notification that is not a payment — pending,
// failed, cancelled — to the payment's record. It never moves a paid payment
// back; see RecordPaymentNotice.
func (s *Store) RecordUnpaid(ctx context.Context, n Notice, status string) error {
	p, err := s.q.GetPayment(ctx, n.PaymentID)
	if err != nil {
		return translate(fmt.Errorf("registrations: payment: %w", err))
	}
	if p.Method != n.Gateway {
		return ErrWrongGateway
	}
	return recordNotice(ctx, s.q, n, status)
}

func recordNotice(ctx context.Context, q *gen.Queries, n Notice, status string) error {
	var ref *string
	if n.Ref != "" {
		ref = &n.Ref
	}
	err := q.RecordPaymentNotice(ctx, gen.RecordPaymentNoticeParams{
		ID: n.PaymentID, GatewayRef: ref, GatewayStatus: n.Status,
		GatewayAmount: n.Amount, GatewayPayload: n.Raw, Status: status,
	})
	if err != nil {
		return translate(fmt.Errorf("registrations: record notice: %w", err))
	}
	return nil
}

// confirmIfPaid confirms a registration whose payments now cover its total.
// It holds the registration's lock, and the event's too when the hold has
// lapsed, so the oversold check counts against a settled picture.
func confirmIfPaid(ctx context.Context, q *gen.Queries, regID string) (Confirmation, bool, error) {
	reg, err := q.LockRegistration(ctx, regID)
	if err != nil {
		return Confirmation{}, false, translate(fmt.Errorf("registrations: lock registration: %w", err))
	}
	// Confirmed already, or cancelled by an administrator: a cancellation is a
	// decision a payment does not overturn. The payment is still recorded.
	if reg.Status != string(StatusPending) && reg.Status != string(StatusExpired) {
		return Confirmation{}, false, nil
	}
	paid, err := q.PaidTotal(ctx, regID)
	if err != nil {
		return Confirmation{}, false, fmt.Errorf("registrations: paid total: %w", err)
	}
	if paid < reg.TotalCents {
		return Confirmation{}, false, nil
	}

	oversold := false
	if reg.Status == string(StatusExpired) || !reg.HoldExpiresAt.After(time.Now()) {
		if oversold, err = overflows(ctx, q, reg); err != nil {
			return Confirmation{}, false, err
		}
	}
	confirmed, err := q.ConfirmRegistration(ctx, gen.ConfirmRegistrationParams{ID: regID, Oversold: oversold})
	if err != nil {
		return Confirmation{}, false, fmt.Errorf("registrations: confirm: %w", err)
	}
	rows, err := q.ListAttendees(ctx, regID)
	if err != nil {
		return Confirmation{}, false, fmt.Errorf("registrations: attendees: %w", err)
	}
	c := Confirmation{Registration: registration(confirmed)}
	for _, r := range rows {
		c.Attendees = append(c.Attendees, attendee(r))
	}
	return c, true, nil
}

// overflows reports whether a registration whose hold lapsed no longer fits:
// its attendees on top of everybody else's would exceed a ticket type's or the
// event's capacity.
func overflows(ctx context.Context, q *gen.Queries, reg gen.Registration) (bool, error) {
	erow, err := q.LockEvent(ctx, reg.EventID)
	if err != nil {
		return false, translate(fmt.Errorf("registrations: lock event: %w", err))
	}
	e := events.FromRow(erow)
	h, err := held(ctx, q, reg.EventID, reg.ID)
	if err != nil {
		return false, err
	}
	mine, err := q.CountAttendeesByType(ctx, reg.ID)
	if err != nil {
		return false, fmt.Errorf("registrations: count own: %w", err)
	}
	ttRows, err := q.ListTicketTypes(ctx, reg.EventID)
	if err != nil {
		return false, fmt.Errorf("registrations: ticket types: %w", err)
	}
	types := make(map[string]events.TicketType, len(ttRows))
	for _, r := range ttRows {
		types[r.ID] = events.TicketTypeFromRow(r)
	}
	wanted := map[string]int{}
	var order []string
	for _, m := range mine {
		wanted[m.TicketTypeID] = m.N
		order = append(order, m.TicketTypeID)
	}
	return fits(e, types, order, wanted, h) != nil, nil
}

// ExpireHolds marks lapsed holds expired. Bookkeeping only: see the query.
func (s *Store) ExpireHolds(ctx context.Context) (int64, error) {
	n, err := s.q.ExpireHolds(ctx)
	if err != nil {
		return 0, fmt.Errorf("registrations: expire holds: %w", err)
	}
	return n, nil
}

// Get returns a registration by id.
func (s *Store) Get(ctx context.Context, id string) (Registration, error) {
	r, err := s.q.GetRegistration(ctx, id)
	if err != nil {
		return Registration{}, translate(fmt.Errorf("registrations: get: %w", err))
	}
	return registration(r), nil
}

// ByReference returns a registration by its human reference.
func (s *Store) ByReference(ctx context.Context, ref string) (Registration, error) {
	r, err := s.q.GetRegistrationByReference(ctx, strings.ToUpper(strings.TrimSpace(ref)))
	if err != nil {
		return Registration{}, translate(fmt.Errorf("registrations: by reference: %w", err))
	}
	return registration(r), nil
}

// Payment returns one payment.
func (s *Store) Payment(ctx context.Context, id string) (Payment, error) {
	r, err := s.q.GetPayment(ctx, id)
	if err != nil {
		return Payment{}, translate(fmt.Errorf("registrations: payment: %w", err))
	}
	return payment(r), nil
}

// Attendees returns a registration's attendees in form order.
func (s *Store) Attendees(ctx context.Context, regID string) ([]Attendee, error) {
	rows, err := s.q.ListAttendees(ctx, regID)
	if err != nil {
		return nil, translate(fmt.Errorf("registrations: attendees: %w", err))
	}
	out := make([]Attendee, len(rows))
	for i, r := range rows {
		out[i] = attendee(r)
	}
	return out, nil
}

// Answers returns a registration's answers.
func (s *Store) Answers(ctx context.Context, regID string) ([]Answer, error) {
	rows, err := s.q.ListAnswers(ctx, regID)
	if err != nil {
		return nil, translate(fmt.Errorf("registrations: answers: %w", err))
	}
	out := make([]Answer, len(rows))
	for i, r := range rows {
		out[i] = Answer{QuestionID: r.QuestionID, Label: r.Label, Value: r.Value}
		if r.AttendeeID != nil {
			out[i].AttendeeID = *r.AttendeeID
		}
	}
	return out, nil
}

func translate(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.Code == "22P02" {
		return ErrNotFound
	}
	return err
}
