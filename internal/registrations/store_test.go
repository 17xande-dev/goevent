package registrations_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/db/gen"
	"github.com/17xande-dev/goevent/internal/dbtest"
	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fixture struct {
	pool   *pgxpool.Pool
	events *events.Store
	regs   *registrations.Store
	event  events.Event
	adult  events.TicketType
	free   events.TicketType
}

var now = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

// open makes a published event a fortnight out with a paid ticket type capped at
// adultCap and an uncapped free one.
func open(t *testing.T, adultCap int, edit ...func(*events.Event)) fixture {
	t.Helper()
	pool := dbtest.Pool(t)
	es := events.NewStore(pool)
	start := now.Add(14 * 24 * time.Hour)
	e := events.Event{Slug: "camp", Title: "Camp", StartsAt: start, EndsAt: start.Add(48 * time.Hour),
		Timezone: events.DefaultTimezone, Listed: true}
	for _, f := range edit {
		f(&e)
	}
	e, err := es.Create(t.Context(), e)
	if err != nil {
		t.Fatal(err)
	}
	adult, err := es.CreateTicketType(t.Context(), events.TicketType{EventID: e.ID, Name: "Adult",
		PriceCents: 45000, Capacity: &adultCap, MaxPerRegistration: 10})
	if err != nil {
		t.Fatal(err)
	}
	free, err := es.CreateTicketType(t.Context(), events.TicketType{EventID: e.ID, Name: "Child",
		MaxPerRegistration: 10})
	if err != nil {
		t.Fatal(err)
	}
	if e, err = es.Transition(t.Context(), e.ID, events.StatusDraft, events.StatusPublished); err != nil {
		t.Fatal(err)
	}
	return fixture{pool: pool, events: es, regs: registrations.NewStore(pool), event: e, adult: adult, free: free}
}

func (f fixture) order(key string, method registrations.Method, types ...events.TicketType) registrations.Order {
	o := registrations.Order{
		EventID: f.event.ID, CheckoutKey: key, Method: method, Currency: "ZAR",
		Contact: registrations.Contact{FirstName: "Ada", LastName: "Lovelace", Email: "ada@example.com"},
	}
	for i, tt := range types {
		o.Attendees = append(o.Attendees, registrations.AttendeeOrder{
			TicketTypeID: tt.ID, FirstName: fmt.Sprintf("Person %d", i), LastName: "Lovelace",
		})
	}
	return o
}

func TestCheckout_FreeIsConfirmedAtOnce(t *testing.T) {
	f := open(t, 10)
	res, err := f.regs.Checkout(t.Context(), f.order("k1", "", f.free, f.free), now)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if res.Registration.Status != registrations.StatusConfirmed || res.Payment != nil {
		t.Errorf("free registration: status %q, payment %v", res.Registration.Status, res.Payment)
	}
	if len(res.Attendees) != 2 || res.Attendees[0].TicketName != "Child" {
		t.Errorf("attendees = %+v", res.Attendees)
	}
	if len(res.Registration.Reference) != 7 || res.Registration.Reference[3] != '-' {
		t.Errorf("reference %q is not XXX-XXX", res.Registration.Reference)
	}
}

func TestCheckout_PaidHoldsSeatsWithAPendingPayment(t *testing.T) {
	f := open(t, 10)
	res, err := f.regs.Checkout(t.Context(), f.order("k1", "fake", f.adult, f.adult, f.free), now)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	r := res.Registration
	if r.Status != registrations.StatusPending || r.TotalCents != 90000 {
		t.Errorf("status %q total %d, want pending 90000", r.Status, r.TotalCents)
	}
	if !r.HoldExpiresAt.Equal(now.Add(registrations.HoldOnline)) {
		t.Errorf("hold until %s", r.HoldExpiresAt)
	}
	if res.Payment == nil || res.Payment.AmountCents != 90000 || res.Payment.Method != "fake" {
		t.Errorf("payment = %+v", res.Payment)
	}
}

func TestCheckout_ResubmittingIsTheSameRegistration(t *testing.T) {
	f := open(t, 10)
	first, err := f.regs.Checkout(t.Context(), f.order("same", "fake", f.adult), now)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.regs.Checkout(t.Context(), f.order("same", "fake", f.adult, f.adult), now)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Reused || again.Registration.ID != first.Registration.ID || again.Payment.ID != first.Payment.ID {
		t.Errorf("a resubmission booked again: %+v", again)
	}
	if h, _ := f.regs.Held(t.Context(), f.event.ID); h.Total != 1 {
		t.Errorf("held = %d, want 1", h.Total)
	}
}

// The property the event lock exists for. Twenty people press Register for the
// last three seats at the same moment; exactly three get one.
func TestCheckout_ConcurrentCheckoutsCannotOversell(t *testing.T) {
	f := open(t, 3)
	var wg sync.WaitGroup
	var mu sync.Mutex
	booked, soldOut := 0, 0
	for i := range 20 {
		wg.Go(func() {
			_, err := f.regs.Checkout(context.Background(), f.order(fmt.Sprint("k", i), "fake", f.adult), now)
			mu.Lock()
			defer mu.Unlock()
			var so *registrations.SoldOutError
			switch {
			case err == nil:
				booked++
			case errors.As(err, &so):
				soldOut++
			default:
				t.Errorf("checkout %d: %v", i, err)
			}
		})
	}
	wg.Wait()
	if booked != 3 || soldOut != 17 {
		t.Errorf("booked %d, sold out %d; want 3 and 17", booked, soldOut)
	}
}

func TestCheckout_EventCapacityAndSoldOutNames(t *testing.T) {
	two := 2
	f := open(t, 10, func(e *events.Event) { e.Capacity = &two })

	_, err := f.regs.Checkout(t.Context(), f.order("k1", "fake", f.adult, f.free, f.free), now)
	var so *registrations.SoldOutError
	if !errors.As(err, &so) || !so.Event {
		t.Fatalf("three into an event of two: err = %v", err)
	}

	f2 := open(t, 1)
	_, err = f2.regs.Checkout(t.Context(), f2.order("k1", "fake", f2.adult, f2.adult), now)
	if !errors.As(err, &so) || len(so.Tickets) != 1 || so.Tickets[0] != "Adult" {
		t.Fatalf("two adults into one seat: err = %v", err)
	}
}

func TestCheckout_ExpiredHoldsFreeTheirSeats(t *testing.T) {
	f := open(t, 1)
	if _, err := f.regs.Checkout(t.Context(), f.order("first", "fake", f.adult), now); err != nil {
		t.Fatal(err)
	}
	// While held, nobody else gets it.
	var so *registrations.SoldOutError
	if _, err := f.regs.Checkout(t.Context(), f.order("second", "fake", f.adult), now); !errors.As(err, &so) {
		t.Fatalf("a held seat was sold again: %v", err)
	}
	// The hold lapses — the database's clock, not the caller's, decides.
	if _, err := f.pool.Exec(t.Context(), `UPDATE registrations SET hold_expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.regs.Checkout(t.Context(), f.order("third", "fake", f.adult), now); err != nil {
		t.Errorf("a lapsed hold still blocks the seat: %v", err)
	}
}

func TestCheckout_RefusesWhatCannotBeHad(t *testing.T) {
	f := open(t, 10)
	tooMany := f.order("k1", "fake")
	for range 11 {
		tooMany.Attendees = append(tooMany.Attendees, registrations.AttendeeOrder{TicketTypeID: f.adult.ID, FirstName: "A", LastName: "B"})
	}
	var ue *registrations.UnavailableError
	if _, err := f.regs.Checkout(t.Context(), tooMany, now); !errors.As(err, &ue) {
		t.Errorf("eleven of a max-ten ticket: err = %v", err)
	}
	if _, err := f.regs.Checkout(t.Context(), f.order("k2", ""), now); !errors.Is(err, registrations.ErrEmpty) {
		t.Errorf("nobody: err = %v", err)
	}
	if _, err := f.regs.Checkout(t.Context(), f.order("k3", "", f.adult), now); !errors.Is(err, registrations.ErrNoMethod) {
		t.Errorf("paid with no method: err = %v", err)
	}
	if _, err := f.regs.Checkout(t.Context(), f.order("k4", registrations.MethodLater, f.adult), now); !errors.Is(err, registrations.ErrPayLater) {
		t.Errorf("pay later where it is not offered: err = %v", err)
	}
	// After the event ends, registration is closed whatever its status says.
	if _, err := f.regs.Checkout(t.Context(), f.order("k5", "", f.free), f.event.EndsAt); !errors.Is(err, registrations.ErrClosed) {
		t.Errorf("after the event: err = %v", err)
	}
}

func TestCheckout_PayLaterHoldsUntilRegistrationCloses(t *testing.T) {
	f := open(t, 10, func(e *events.Event) { e.PayLater, e.PayLaterInstructions = true, "EFT to …" })
	res, err := f.regs.Checkout(t.Context(), f.order("k1", registrations.MethodLater, f.adult), now)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Registration.PayLater || res.Payment != nil || !res.Registration.HoldExpiresAt.Equal(f.event.StartsAt) {
		t.Errorf("pay later: %+v payment %v", res.Registration, res.Payment)
	}
}

func notice(p *registrations.Payment, ref string) registrations.Notice {
	return registrations.Notice{PaymentID: p.ID, Gateway: "fake", Ref: ref, Status: "COMPLETE",
		AmountCents: p.AmountCents, Amount: "450.00", Raw: "payment_id=x"}
}

func TestMarkPaid_ConfirmsOnceAndRunsOnConfirmInTheTransaction(t *testing.T) {
	f := open(t, 10)
	res, err := f.regs.Checkout(t.Context(), f.order("k1", "fake", f.adult), now)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	onConfirm := func(_ context.Context, _ *gen.Queries, c registrations.Confirmation) error {
		calls++
		if len(c.Attendees) != 1 || c.Registration.Status != registrations.StatusConfirmed {
			t.Errorf("confirmation = %+v", c)
		}
		return nil
	}
	if out, err := f.regs.MarkPaid(t.Context(), notice(res.Payment, "pf-1"), onConfirm); err != nil || out != registrations.Confirmed {
		t.Fatalf("MarkPaid = %v, %v", out, err)
	}
	// The gateway retries; the replay changes nothing and sends nothing.
	if out, err := f.regs.MarkPaid(t.Context(), notice(res.Payment, "pf-1"), onConfirm); err != nil || out != registrations.AlreadyPaid {
		t.Fatalf("replay = %v, %v", out, err)
	}
	if calls != 1 {
		t.Errorf("onConfirm ran %d times", calls)
	}
	got, _ := f.regs.Get(t.Context(), res.Registration.ID)
	if got.Status != registrations.StatusConfirmed || got.Oversold {
		t.Errorf("registration = %+v", got)
	}
}

func TestMarkPaid_OnConfirmFailureRollsBackThePayment(t *testing.T) {
	f := open(t, 10)
	res, _ := f.regs.Checkout(t.Context(), f.order("k1", "fake", f.adult), now)
	boom := errors.New("queue unavailable")
	if _, err := f.regs.MarkPaid(t.Context(), notice(res.Payment, "pf-1"),
		func(context.Context, *gen.Queries, registrations.Confirmation) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	// Nothing committed, so the gateway's retry gets a second chance at all of it.
	if p, _ := f.regs.Payment(t.Context(), res.Payment.ID); p.Status != "pending" {
		t.Errorf("payment status = %q after a rolled-back confirmation", p.Status)
	}
}

func TestMarkPaid_RefusesTheWrongAmountAndTheWrongGateway(t *testing.T) {
	f := open(t, 10)
	res, _ := f.regs.Checkout(t.Context(), f.order("k1", "fake", f.adult), now)

	n := notice(res.Payment, "pf-1")
	n.Gateway = "payfast"
	if _, err := f.regs.MarkPaid(t.Context(), n, nil); !errors.Is(err, registrations.ErrWrongGateway) {
		t.Errorf("wrong gateway: err = %v", err)
	}
	n = notice(res.Payment, "pf-1")
	n.AmountCents = 100
	if _, err := f.regs.MarkPaid(t.Context(), n, nil); !errors.Is(err, registrations.ErrAmountMismatch) {
		t.Errorf("wrong amount: err = %v", err)
	}
	if got, _ := f.regs.Get(t.Context(), res.Registration.ID); got.Status != registrations.StatusPending {
		t.Errorf("a short payment confirmed the registration: %q", got.Status)
	}
	if p, _ := f.regs.Payment(t.Context(), res.Payment.ID); p.Status != "pending" || p.GatewayRef != "pf-1" {
		t.Errorf("the mismatched notice was not recorded: %+v", p)
	}
}

// The hold lapses while the registrant is still on the gateway's page, somebody
// else takes the seat, and then the first payment arrives. The money is real,
// so the registration is confirmed — and flagged.
func TestMarkPaid_LatePaymentIntoAFullEventIsConfirmedAndFlagged(t *testing.T) {
	f := open(t, 1)
	late, _ := f.regs.Checkout(t.Context(), f.order("late", "fake", f.adult), now)
	if _, err := f.pool.Exec(t.Context(), `UPDATE registrations SET hold_expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if n, err := f.regs.ExpireHolds(t.Context()); err != nil || n != 1 {
		t.Fatalf("ExpireHolds = %d, %v", n, err)
	}
	if _, err := f.regs.Checkout(t.Context(), f.order("prompt", "", f.free), now); err != nil {
		t.Fatal(err)
	}
	took, err := f.regs.Checkout(t.Context(), f.order("took", "fake", f.adult), now)
	if err != nil {
		t.Fatal(err)
	}
	f.regs.MarkPaid(t.Context(), notice(took.Payment, "pf-2"), nil)

	out, err := f.regs.MarkPaid(t.Context(), notice(late.Payment, "pf-1"), nil)
	if err != nil || out != registrations.Confirmed {
		t.Fatalf("late payment = %v, %v", out, err)
	}
	got, _ := f.regs.Get(t.Context(), late.Registration.ID)
	if got.Status != registrations.StatusConfirmed || !got.Oversold {
		t.Errorf("late registration = %+v, want confirmed and oversold", got)
	}
}

func TestMarkPaid_LatePaymentWithRoomIsNotFlagged(t *testing.T) {
	f := open(t, 5)
	late, _ := f.regs.Checkout(t.Context(), f.order("late", "fake", f.adult), now)
	f.pool.Exec(t.Context(), `UPDATE registrations SET hold_expires_at = now() - interval '1 second'`)
	f.regs.ExpireHolds(t.Context())

	if _, err := f.regs.MarkPaid(t.Context(), notice(late.Payment, "pf-1"), nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.regs.Get(t.Context(), late.Registration.ID); got.Oversold || got.Status != registrations.StatusConfirmed {
		t.Errorf("late registration with room = %+v", got)
	}
}

func TestResume_GivesAFailedCheckoutAFreshPayment(t *testing.T) {
	f := open(t, 10)
	first, _ := f.regs.Checkout(t.Context(), f.order("k1", "fake", f.adult), time.Now())
	if err := f.regs.RecordUnpaid(t.Context(), notice(first.Payment, "pf-1"), "failed"); err != nil {
		t.Fatal(err)
	}
	again, err := f.regs.Checkout(t.Context(), f.order("k1", "fake", f.adult), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if again.Payment == nil || again.Payment.ID == first.Payment.ID {
		t.Errorf("a retry after a failed payment did not get a new one: %+v", again.Payment)
	}
}
