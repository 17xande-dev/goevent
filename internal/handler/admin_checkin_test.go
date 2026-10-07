package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/registrations"
)

// freeRegistration registers two children, which confirms at once, and returns
// the event's door path and the attendees.
func freeRegistration(t *testing.T, s *app) (string, []registrations.Attendee) {
	t.Helper()
	e, _, child := published(t, s, 10)
	if res, body := submit(t, s, e, registerForm(t, s, e, map[string]int{child.ID: 2})); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("free registration = %d %s", res.StatusCode, excerpt(body))
	}
	as, err := s.regs.Attendees(t.Context(), onlyRegistration(t, s).ID)
	if err != nil {
		t.Fatal(err)
	}
	return checkinPath(e.ID), as
}

// scan posts what a scanner typed and follows the redirect back to the door.
func scan(t *testing.T, s *app, door string, form url.Values) (string, string) {
	t.Helper()
	res, body := post(t, s.srv, door, form)
	loc := res.Header.Get("Location")
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, door) {
		t.Fatalf("POST %s = %d to %q: %s", door, res.StatusCode, loc, excerpt(body))
	}
	_, page := get(t, s.srv, loc)
	return loc, page
}

func TestCheckin_ScanAdmitsOnceThenSaysAlready(t *testing.T) {
	s := newApp(t)
	door, as := freeRegistration(t, s)
	signIn(t, s.srv)
	code := s.signer.TicketCode(as[0].ID)

	loc, page := scan(t, s, door, url.Values{"code": {code}})
	if !strings.Contains(loc, "result=in") || !strings.Contains(page, "is checked in") {
		t.Fatalf("first scan went to %q:\n%s", loc, excerpt(page))
	}
	if !strings.Contains(page, "<strong>1</strong> of 2 in") {
		t.Errorf("the counts did not move:\n%s", excerpt(page))
	}
	// The scanner's trailing whitespace is not part of the code.
	loc, page = scan(t, s, door, url.Values{"code": {code + "\n"}})
	if !strings.Contains(loc, "result=already") || !strings.Contains(page, "was already checked in at") {
		t.Errorf("second scan went to %q:\n%s", loc, excerpt(page))
	}

	// And undone, it can be scanned again.
	res, _ := post(t, s.srv, door+"/"+as[0].ID+"/undo", url.Values{})
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "result=undone") {
		t.Fatalf("undo went to %q", loc)
	}
	if loc, _ := scan(t, s, door, url.Values{"code": {code}}); !strings.Contains(loc, "result=in") {
		t.Errorf("scan after undo went to %q", loc)
	}
}

func TestCheckin_TextIsASearchAndAForgedCodeIsNot(t *testing.T) {
	s := newApp(t)
	door, as := freeRegistration(t, s)
	signIn(t, s.srv)

	loc, page := scan(t, s, door, url.Values{"code": {"lovelace"}})
	if !strings.Contains(loc, "q=lovelace") || strings.Count(page, `name="attendee"`) != 2 {
		t.Fatalf("a typed name went to %q and offered %d check-ins:\n%s", loc, strings.Count(page, `name="attendee"`), excerpt(page))
	}
	// The button beside a result checks that one person in and keeps the search.
	loc, page = scan(t, s, door, url.Values{"attendee": {as[1].ID}, "q": {"lovelace"}})
	if !strings.Contains(loc, "result=in") || !strings.Contains(loc, "q=lovelace") || strings.Count(page, `name="attendee"`) != 1 {
		t.Errorf("checking in from the results went to %q:\n%s", loc, excerpt(page))
	}

	// A ticket-shaped code this server did not sign.
	forged := as[0].ID + ".AAAAAAAAAAAAAAAAAAAAAA"
	if loc, page := scan(t, s, door, url.Values{"code": {forged}}); !strings.Contains(loc, "result=invalid") ||
		!strings.Contains(page, "not a ticket this system made") {
		t.Errorf("a forged code went to %q", loc)
	}
	if got, _ := s.regs.Attendees(t.Context(), as[0].RegistrationID); got[0].CheckedInAt != nil {
		t.Error("a forged code checked somebody in")
	}
}

func TestCheckin_RefusesAnotherEventsTicketAndAnUnpaidOne(t *testing.T) {
	s := newApp(t)
	e, reg := payLater(t, s)
	signIn(t, s.srv)
	as, err := s.regs.Attendees(t.Context(), reg.ID)
	if err != nil {
		t.Fatal(err)
	}
	door := checkinPath(e.ID)
	if loc, page := scan(t, s, door, url.Values{"code": {s.signer.TicketCode(as[0].ID)}}); !strings.Contains(loc, "result=unpaid") ||
		!strings.Contains(page, "still awaiting payment") {
		t.Errorf("an unpaid ticket went to %q:\n%s", loc, excerpt(page))
	}
	// A real ticket, signed by this server, for an attendee of no event here.
	stranger := s.signer.TicketCode("3f2504e0-4f89-41d3-9a0c-0305e82c3301")
	if loc, page := scan(t, s, door, url.Values{"code": {stranger}}); !strings.Contains(loc, "result=unknown") ||
		!strings.Contains(page, "not for this event") {
		t.Errorf("a stranger's ticket went to %q", loc)
	}
}

func TestCheckin_CountsFragmentIsJustTheCounts(t *testing.T) {
	s := newApp(t)
	door, _ := freeRegistration(t, s)
	signIn(t, s.srv)
	res, body := get(t, s.srv, door+"/counts")
	if res.StatusCode != http.StatusOK || strings.Contains(body, "<html") || !strings.Contains(body, "of 2 in") {
		t.Errorf("counts = %d:\n%s", res.StatusCode, excerpt(body))
	}
}
