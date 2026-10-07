package handler

import (
	"encoding/csv"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/registrations"
)

// payLater registers one adult to pay by EFT and returns the registration.
func payLater(t *testing.T, s *app) (events.Event, registrations.Registration) {
	t.Helper()
	e, adult, _ := published(t, s, 10, func(e *events.Event) {
		e.PayLater, e.PayLaterInstructions = true, "Example Bank"
	})
	form := registerForm(t, s, e, map[string]int{adult.ID: 1})
	form.Set("method", string(registrations.MethodLater))
	form.Set("contact_phone", "+27 82 555 0101")
	if res, body := submit(t, s, e, form); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("pay-later registration = %d %s", res.StatusCode, excerpt(body))
	}
	return e, onlyRegistration(t, s)
}

func TestAdminRegistrations_RecordingTheEFTConfirmsAndSendsTickets(t *testing.T) {
	s := newApp(t)
	_, reg := payLater(t, s)
	signIn(t, s.srv)
	s.handler.ProcessMail(t.Context()) // the "received" acknowledgement

	res, _ := post(t, s.srv, registrationPath(reg.ID)+"/payments", url.Values{
		"method": {"eft"}, "amount": {"450.00"}, "note": {"FNB ref 1234"},
	})
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "notice=confirmed") {
		t.Fatalf("record payment went to %q", loc)
	}
	got, _ := s.regs.Get(t.Context(), reg.ID)
	if got.Status != registrations.StatusConfirmed {
		t.Fatalf("status = %q after recording the whole amount", got.Status)
	}
	s.handler.ProcessMail(t.Context())
	var tickets int
	for _, m := range s.mail.To("ada@example.com") {
		if strings.HasPrefix(m.Subject, "Your tickets") {
			tickets++
			if len(m.Inline) != 1 {
				t.Errorf("the tickets email has %d codes", len(m.Inline))
			}
		}
	}
	if tickets != 1 {
		t.Errorf("tickets emails = %d, want 1", tickets)
	}
	_, page := get(t, s.srv, registrationPath(reg.ID))
	if !strings.Contains(page, "FNB ref 1234") {
		t.Errorf("the payment's note is not on the page:\n%s", excerpt(page))
	}
}

func TestAdminRegistrations_PaymentFormIsReadStrictly(t *testing.T) {
	s := newApp(t)
	_, reg := payLater(t, s)
	signIn(t, s.srv)

	if res, _ := post(t, s.srv, registrationPath(reg.ID)+"/payments", url.Values{"amount": {"450"}}); res.StatusCode != http.StatusBadRequest {
		t.Errorf("no method = %d, want 400", res.StatusCode)
	}
	for _, amount := range []string{"451", "0", "abc", ""} {
		res, _ := post(t, s.srv, registrationPath(reg.ID)+"/payments", url.Values{"method": {"cash"}, "amount": {amount}})
		if loc := res.Header.Get("Location"); !strings.Contains(loc, "problem=amount") {
			t.Errorf("amount %q went to %q", amount, loc)
		}
	}
	if got, _ := s.regs.Get(t.Context(), reg.ID); got.Status != registrations.StatusPending {
		t.Errorf("a refused payment changed the status to %q", got.Status)
	}
}

func TestAdminRegistrations_CancelReleasesAndSaysSo(t *testing.T) {
	s := newApp(t)
	_, reg := payLater(t, s)
	signIn(t, s.srv)

	res, _ := post(t, s.srv, registrationPath(reg.ID)+"/cancel", url.Values{})
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "notice=cancelled") {
		t.Fatalf("cancel went to %q", loc)
	}
	res, _ = post(t, s.srv, registrationPath(reg.ID)+"/cancel", url.Values{})
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "problem=already_cancelled") {
		t.Errorf("second cancel went to %q", loc)
	}
	// The registrant's own page says so too.
	_, page := get(t, s.srv, s.handler.managePath(reg))
	if !strings.Contains(page, "has been cancelled") {
		t.Errorf("the registrant's page does not say it was cancelled:\n%s", excerpt(page))
	}
}

func TestAdminRegistrations_ResendOnlyForConfirmedAndSkipsCancelledPeople(t *testing.T) {
	s := newApp(t)
	e, _, child := published(t, s, 10)
	submit(t, s, e, registerForm(t, s, e, map[string]int{child.ID: 2}))
	reg := onlyRegistration(t, s)
	signIn(t, s.srv)
	s.handler.ProcessMail(t.Context())

	people, _ := s.regs.Attendees(t.Context(), reg.ID)
	post(t, s.srv, registrationPath(reg.ID)+"/attendees/"+people[1].ID+"/cancel", url.Values{})
	res, _ := post(t, s.srv, registrationPath(reg.ID)+"/resend", url.Values{})
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "notice=resent") {
		t.Fatalf("resend went to %q", loc)
	}
	s.handler.ProcessMail(t.Context())
	sent := s.mail.To("ada@example.com")
	if len(sent) != 2 || len(sent[1].Inline) != 1 {
		t.Fatalf("after resend: %d emails, the last with %d codes", len(sent), len(sent[len(sent)-1].Inline))
	}
	// The cancelled person's ticket is not sent again. (Their old code still
	// verifies — it is an HMAC of their id — so check-in refuses it by the
	// attendee's status.)
	if strings.Contains(sent[1].Text, people[1].ID) {
		t.Error("the cancelled attendee's ticket was sent again")
	}

	s2 := newApp(t)
	_, pending := payLater(t, s2)
	signIn(t, s2.srv)
	res, _ = post(t, s2.srv, registrationPath(pending.ID)+"/resend", url.Values{})
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "problem=not_confirmed") {
		t.Errorf("resend for a pending registration went to %q", loc)
	}
}

func TestAdminRegistrations_ListFilters(t *testing.T) {
	s := newApp(t)
	e, reg := payLater(t, s)
	signIn(t, s.srv)

	for query, want := range map[string]bool{
		"":                    true,
		"?q=" + reg.Reference: true,
		"?q=ada%40example":    true,
		"?q=nobody":           false,
		"?status=pending":     true,
		"?status=confirmed":   false,
		"?event=" + e.ID:      true,
		"?event=3f2504e0-4f89-41d3-9a0c-0305e82c3301": false,
	} {
		res, page := get(t, s.srv, "/admin/registrations"+query)
		if res.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", query, res.StatusCode)
			continue
		}
		if got := strings.Contains(page, reg.Reference); got != want {
			t.Errorf("GET %s lists the registration = %v, want %v", query, got, want)
		}
	}
}

func TestAdminRegistrations_ViewerSeesNoWriteControls(t *testing.T) {
	s := newApp(t)
	_, reg := payLater(t, s)
	mustAccount(t, s, "viewer@example.com", testPassword, auth.RoleViewer)
	signInAs(t, s.srv, "viewer@example.com", testPassword)

	res, page := get(t, s.srv, registrationPath(reg.ID))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("viewer GET = %d", res.StatusCode)
	}
	for _, absent := range []string{"Record payment", "Cancel this registration", "Send the tickets again"} {
		if strings.Contains(page, absent) {
			t.Errorf("a viewer is offered %q", absent)
		}
	}
}

// The export is opened in a spreadsheet, so every cell a public form filled is
// neutralised: a phone number starting + or a name starting = would otherwise
// be evaluated as a formula on a staff machine.
func TestAdminEventExport_EscapesFormulasAndHasEveryQuestion(t *testing.T) {
	s := newApp(t)
	e, adult, _ := published(t, s, 10, func(e *events.Event) {
		e.PayLater, e.PayLaterInstructions = true, "EFT"
	})
	q, err := s.events.CreateQuestion(t.Context(), events.Question{EventID: e.ID, Scope: events.ScopeAttendee,
		Kind: events.KindText, Label: "Diet"})
	if err != nil {
		t.Fatal(err)
	}
	form := registerForm(t, s, e, map[string]int{adult.ID: 1})
	form.Set("method", string(registrations.MethodLater))
	form.Set("a0.first", "=HYPERLINK(\"http://evil.example\")")
	form.Set("a0.q."+q.ID, "@SUM(1)")
	form.Set("contact_phone", "+27 82 555 0101")
	if res, body := submit(t, s, e, form); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("register = %d %s", res.StatusCode, excerpt(body))
	}
	signIn(t, s.srv)

	res, body := get(t, s.srv, "/admin/events/"+e.ID+"/attendees.csv")
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("export = %d %q", res.StatusCode, res.Header.Get("Content-Type"))
	}
	if !strings.Contains(res.Header.Get("Content-Disposition"), "attachment") {
		t.Error("the export is not a download")
	}
	recs, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil || len(recs) != 2 {
		t.Fatalf("csv = %d records, %v:\n%s", len(recs), err, body)
	}
	if recs[0][len(recs[0])-1] != "Diet" {
		t.Errorf("header = %v, want a column for the question", recs[0])
	}
	row := strings.Join(recs[1], "|")
	for _, want := range []string{`'=HYPERLINK`, `'@SUM(1)`, `'+27 82 555 0101`} {
		if !strings.Contains(row, want) {
			t.Errorf("row is missing escaped %q: %s", want, row)
		}
	}
}
