package registrations_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/registrations"
)

func TestCheckIn_OnceThenAlreadyThenUndo(t *testing.T) {
	f := open(t, 10)
	who := admin(t, f)
	res, err := f.regs.Checkout(t.Context(), f.order("k1", "", f.free, f.free), now)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Attendees[0]

	in, err := f.regs.CheckIn(t.Context(), f.event.ID, a.ID, who)
	if err != nil || in.Refused != "" || in.CheckedInAt == nil || in.Name != "Person 0 Lovelace" {
		t.Fatalf("first scan = %+v, %v", in, err)
	}
	again, err := f.regs.CheckIn(t.Context(), f.event.ID, a.ID, who)
	if err != nil || again.Refused != registrations.RefusedAlready || again.CheckedInAt == nil ||
		!again.CheckedInAt.Equal(*in.CheckedInAt) {
		t.Errorf("second scan = %+v, %v; want already, at the first time", again, err)
	}
	if c, _ := f.regs.DoorCounts(t.Context(), f.event.ID); c != (registrations.DoorCounts{Expected: 2, CheckedIn: 1}) {
		t.Errorf("counts = %+v", c)
	}

	if err := f.regs.UndoCheckIn(t.Context(), f.event.ID, a.ID); err != nil {
		t.Fatalf("undo: %v", err)
	}
	if err := f.regs.UndoCheckIn(t.Context(), f.event.ID, a.ID); !errors.Is(err, registrations.ErrNotFound) {
		t.Errorf("undoing twice: err = %v", err)
	}
	if in, _ := f.regs.CheckIn(t.Context(), f.event.ID, a.ID, ""); in.Refused != "" {
		t.Errorf("checking in after an undo = %+v", in)
	}
}

func TestCheckIn_ConcurrentScansAdmitOnce(t *testing.T) {
	f := open(t, 10)
	res, err := f.regs.Checkout(t.Context(), f.order("k1", "", f.free), now)
	if err != nil {
		t.Fatal(err)
	}
	const doors = 10
	var wg sync.WaitGroup
	results := make([]registrations.CheckInResult, doors)
	errs := make([]error, doors)
	for i := range doors {
		wg.Go(func() {
			results[i], errs[i] = f.regs.CheckIn(t.Context(), f.event.ID, res.Attendees[0].ID, "")
		})
	}
	wg.Wait()
	admitted := 0
	for i, r := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		switch r.Refused {
		case "":
			admitted++
		case registrations.RefusedAlready:
		default:
			t.Errorf("scan %d refused for %q", i, r.Refused)
		}
	}
	if admitted != 1 {
		t.Errorf("%d scans admitted the same ticket", admitted)
	}
}

func TestCheckIn_SaysWhyNot(t *testing.T) {
	f := open(t, 10, func(e *events.Event) { e.PayLater, e.PayLaterInstructions = true, "EFT" })
	unpaid, err := f.regs.Checkout(t.Context(), f.order("k1", registrations.MethodLater, f.adult), now)
	if err != nil {
		t.Fatal(err)
	}
	free, err := f.regs.Checkout(t.Context(), f.order("k2", "", f.free, f.free), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.regs.CancelAttendee(t.Context(), free.Registration.ID, free.Attendees[1].ID); err != nil {
		t.Fatal(err)
	}
	other := open(t, 10) // some other event: its id is not this attendee's event

	for _, c := range []struct {
		name, event, attendee string
		want                  registrations.Refusal
	}{
		{"awaiting payment", f.event.ID, unpaid.Attendees[0].ID, registrations.RefusedUnpaid},
		{"cancelled attendee", f.event.ID, free.Attendees[1].ID, registrations.RefusedCancelled},
		{"another event", other.event.ID, free.Attendees[0].ID, registrations.RefusedUnknown},
		{"no such attendee", f.event.ID, "00000000-0000-0000-0000-000000000000", registrations.RefusedUnknown},
		{"not a uuid", f.event.ID, "nonsense", registrations.RefusedUnknown},
	} {
		got, err := f.regs.CheckIn(t.Context(), c.event, c.attendee, "")
		if err != nil || got.Refused != c.want {
			t.Errorf("%s: %+v, %v; want %q", c.name, got, err, c.want)
		}
	}
	// A whole registration cancelled after the fact.
	if _, err := f.regs.Cancel(t.Context(), free.Registration.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.regs.CheckIn(t.Context(), f.event.ID, free.Attendees[0].ID, ""); got.Refused != registrations.RefusedCancelled {
		t.Errorf("cancelled registration: %+v", got)
	}
}

func TestSearchDoor(t *testing.T) {
	f := open(t, 10)
	res, err := f.regs.Checkout(t.Context(), f.order("k1", "", f.free), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"person 0", "LOVELACE", res.Registration.Reference, "ada@example"} {
		got, err := f.regs.SearchDoor(t.Context(), f.event.ID, q)
		if err != nil || len(got) != 1 || got[0].AttendeeID != res.Attendees[0].ID || !got[0].CanEnter() {
			t.Errorf("search %q = %+v, %v", q, got, err)
		}
	}
	if got, _ := f.regs.SearchDoor(t.Context(), f.event.ID, "   "); got != nil {
		t.Errorf("blank search = %+v", got)
	}
	if got, _ := f.regs.SearchDoor(t.Context(), f.event.ID, "%"); len(got) != 0 {
		t.Errorf("a LIKE wildcard matched %d", len(got))
	}
}
