package registrations_test

import (
	"context"
	"errors"
	"testing"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/goevent/internal/db/gen"
	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/registrations"
)

// admin makes an administrator for recorded_by to point at.
func admin(t *testing.T, f fixture) string {
	t.Helper()
	u, err := auth.NewStore(f.pool).Create(t.Context(), "office@example.com", "Office", "$argon2id$v=19$m=64,t=1,p=1$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA", auth.RoleManager, false)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func TestRecordPayment_PartsThenFullConfirmsOnce(t *testing.T) {
	f := open(t, 10, func(e *events.Event) { e.PayLater, e.PayLaterInstructions = true, "EFT" })
	who := admin(t, f)
	res, err := f.regs.Checkout(t.Context(), f.order("k1", registrations.MethodLater, f.adult, f.adult), now)
	if err != nil {
		t.Fatal(err)
	}
	id := res.Registration.ID // 900.00 owed

	calls := 0
	hook := func(context.Context, *gen.Queries, registrations.Confirmation) error { calls++; return nil }
	m := registrations.Manual{RegistrationID: id, Method: registrations.MethodEFT, AmountCents: 50000, RecordedBy: who}
	if ok, err := f.regs.RecordPayment(t.Context(), m, hook); err != nil || ok {
		t.Fatalf("first part = %v, %v; want recorded, not confirmed", ok, err)
	}
	// More than is still owed is a typo, not a payment.
	m.AmountCents = 50000
	if _, err := f.regs.RecordPayment(t.Context(), m, hook); !errors.Is(err, registrations.ErrAmount) {
		t.Errorf("overpayment: err = %v", err)
	}
	m.AmountCents = 40000
	m.Method = registrations.MethodCash
	if ok, err := f.regs.RecordPayment(t.Context(), m, hook); err != nil || !ok {
		t.Fatalf("the rest = %v, %v; want confirmed", ok, err)
	}
	if calls != 1 {
		t.Errorf("onConfirm ran %d times", calls)
	}
	got, _ := f.regs.Get(t.Context(), id)
	ps, _ := f.regs.Payments(t.Context(), id)
	if got.Status != registrations.StatusConfirmed || len(ps) != 2 || ps[1].Method != "cash" {
		t.Errorf("after paying: %q, payments %+v", got.Status, ps)
	}
	// Nothing more is owed now.
	m.AmountCents = 1
	if _, err := f.regs.RecordPayment(t.Context(), m, hook); !errors.Is(err, registrations.ErrAmount) {
		t.Errorf("payment on a settled registration: err = %v", err)
	}
}

func TestCancel_ReleasesSeatsAndRefusesLaterPayment(t *testing.T) {
	f := open(t, 1, func(e *events.Event) { e.PayLater, e.PayLaterInstructions = true, "EFT" })
	who := admin(t, f)
	res, err := f.regs.Checkout(t.Context(), f.order("k1", registrations.MethodLater, f.adult), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.regs.Cancel(t.Context(), res.Registration.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := f.regs.Cancel(t.Context(), res.Registration.ID); !errors.Is(err, registrations.ErrNotCancellable) {
		t.Errorf("cancelling twice: err = %v", err)
	}
	// The one seat is free again.
	if _, err := f.regs.Checkout(t.Context(), f.order("k2", registrations.MethodLater, f.adult), now); err != nil {
		t.Errorf("the cancelled seat was not released: %v", err)
	}
	_, err = f.regs.RecordPayment(t.Context(), registrations.Manual{RegistrationID: res.Registration.ID,
		Method: registrations.MethodCash, AmountCents: 45000, RecordedBy: who}, nil)
	if !errors.Is(err, registrations.ErrCancelled) {
		t.Errorf("paying a cancelled registration: err = %v", err)
	}
}

func TestCancelAttendee_ReleasesOneSeat(t *testing.T) {
	f := open(t, 2)
	res, err := f.regs.Checkout(t.Context(), f.order("k1", "fake", f.adult, f.adult), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.regs.CancelAttendee(t.Context(), res.Registration.ID, res.Attendees[1].ID); err != nil {
		t.Fatal(err)
	}
	if err := f.regs.CancelAttendee(t.Context(), res.Registration.ID, res.Attendees[1].ID); !errors.Is(err, registrations.ErrNotFound) {
		t.Errorf("cancelling twice: err = %v", err)
	}
	// Another registration's attendee, through this one: refused.
	other, _ := f.regs.Checkout(t.Context(), f.order("k2", "", f.free), now)
	if err := f.regs.CancelAttendee(t.Context(), res.Registration.ID, other.Attendees[0].ID); !errors.Is(err, registrations.ErrNotFound) {
		t.Errorf("cross-registration cancel: err = %v", err)
	}
	if h, _ := f.regs.Held(t.Context(), f.event.ID); h.ByType[f.adult.ID] != 1 {
		t.Errorf("adult seats held = %d, want 1", h.ByType[f.adult.ID])
	}
}

func TestEventStatsAndExport(t *testing.T) {
	f := open(t, 10)
	q, err := f.events.CreateQuestion(t.Context(), events.Question{EventID: f.event.ID, Scope: events.ScopeAttendee,
		Kind: events.KindText, Label: "Diet"})
	if err != nil {
		t.Fatal(err)
	}
	o := f.order("free", "", f.free, f.free)
	o.Attendees[0].Answers = []registrations.Answer{{QuestionID: q.ID, Label: q.Label, Value: "Vegetarian"}}
	if _, err := f.regs.Checkout(t.Context(), o, now); err != nil {
		t.Fatal(err)
	}
	paid, _ := f.regs.Checkout(t.Context(), f.order("paid", "fake", f.adult), now)
	f.regs.MarkPaid(t.Context(), notice(paid.Payment, "pf-1"), nil)
	if _, err := f.regs.Checkout(t.Context(), f.order("held", "fake", f.adult), now); err != nil {
		t.Fatal(err)
	}

	st, err := f.regs.EventStats(t.Context(), f.event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Confirmed != 3 || st.Held != 1 || st.PaidCents != 45000 || st.ByType[f.adult.ID] != (registrations.TicketCount{Confirmed: 1, Held: 1}) {
		t.Errorf("stats = %+v", st)
	}

	rows, err := f.regs.Export(t.Context(), f.event.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("export rows = %d, want 4", len(rows))
	}
	if rows[0].Answers[q.ID] != "Vegetarian" || rows[1].Answers[q.ID] != "" {
		t.Errorf("answers went to the wrong attendee: %v / %v", rows[0].Answers, rows[1].Answers)
	}
}
