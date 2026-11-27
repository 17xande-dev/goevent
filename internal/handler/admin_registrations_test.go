package handler

import (
	"encoding/csv"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/17xande-dev/outbox"
	"github.com/17xande-dev/outbox/outboxtest"
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
	outboxtest.Drain(t, s.outbox) // the "received" acknowledgement

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
	outboxtest.Drain(t, s.outbox)
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
	outboxtest.Drain(t, s.outbox)

	people, _ := s.regs.Attendees(t.Context(), reg.ID)
	post(t, s.srv, registrationPath(reg.ID)+"/attendees/"+people[1].ID+"/cancel", url.Values{})
	res, _ := post(t, s.srv, registrationPath(reg.ID)+"/resend", url.Values{})
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "notice=resent") {
		t.Fatalf("resend went to %q", loc)
	}
	outboxtest.Drain(t, s.outbox)
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

func TestAdminRegistrations_RetryDeadAndPendingEmailsForOnlyThisRegistration(t *testing.T) {
	s := newApp(t)
	e, _, free := published(t, s, 10)
	submit(t, s, e, registerForm(t, s, e, map[string]int{free.ID: 1}))
	reg := onlyRegistration(t, s)
	subject := "registration:" + reg.ID
	ctx := t.Context()
	signIn(t, s.srv)

	s.mail.Err = errors.New("relay unavailable")
	for range 6 {
		if n := outboxtest.Drain(t, s.outbox); n != 1 {
			t.Fatalf("ran %d jobs, want one due attempt", n)
		}
		s.clock.Advance(7 * time.Hour)
	}
	jobs := outboxtest.Enqueued(t, s.outbox, outbox.Filter{Subject: subject})
	if len(jobs) != 1 || jobs[0].State != outbox.StateDead || jobs[0].Attempts != 6 {
		t.Fatalf("after six failures: %+v", jobs)
	}
	dead := jobs[0].ID

	getRes, page := get(t, s.srv, registrationPath(reg.ID))
	if getRes.StatusCode != http.StatusOK || !strings.Contains(page, ">Dead</td>") || !strings.Contains(page, ">6/6</td>") {
		t.Fatalf("the registration page did not display the dead job: %d", getRes.StatusCode)
	}
	runAt := s.clock.Now().Add(time.Hour)
	pending, _, err := s.outbox.EnqueueNow(ctx, outbox.Message{Kind: "resend", Subject: subject, RunAt: runAt})
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := s.outbox.EnqueueNow(ctx, outbox.Message{Kind: "resend", Subject: "registration:other", RunAt: runAt})
	if err != nil {
		t.Fatal(err)
	}
	res, _ := post(t, s.srv, registrationPath(reg.ID)+"/emails/retry", url.Values{})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != registrationPath(reg.ID)+"?notice=retrying" {
		t.Fatalf("retry: status %d, redirect %q", res.StatusCode, res.Header.Get("Location"))
	}
	got, err := s.outbox.Get(ctx, dead)
	if err != nil || got.State != outbox.StatePending || got.Attempts != 0 || got.RetryCount != 1 || got.RetriedBy != s.owner.ID {
		t.Fatalf("dead job reset and audit: %+v %v", got, err)
	}
	untouched, err := s.outbox.Get(ctx, other)
	if err != nil || !untouched.RunAt.After(s.clock.Now()) {
		t.Fatalf("another registration's pending job was rushed: %+v %v", untouched, err)
	}
	s.mail.Err = nil
	if n := outboxtest.Drain(t, s.outbox); n != 2 || len(s.mail.To("ada@example.com")) != 2 {
		t.Fatalf("ran %d jobs; want the retried confirmation and the rushed resend", n)
	}
	if got, err := s.outbox.Get(ctx, pending); err != nil || got.State != outbox.StateDone {
		t.Fatalf("scheduled resend after retry: %+v %v", got, err)
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
	// Labels become the header row, which is escaped like any other. These two
	// are the spellings a plain first-byte check misses: a leading space, and a
	// BOM that spreadsheet importers discard before they look for a formula.
	for _, label := range []string{" =1", "\ufeff=HYPERLINK(1)"} {
		if _, err := s.events.CreateQuestion(t.Context(), events.Question{EventID: e.ID, Scope: events.ScopeAttendee,
			Kind: events.KindText, Label: label}); err != nil {
			t.Fatal(err)
		}
	}
	form := registerForm(t, s, e, map[string]int{adult.ID: 1})
	form.Set("method", string(registrations.MethodLater))
	form.Set("a0.first", "=HYPERLINK(\"http://evil.example\")")
	form.Set("a0.q."+q.ID, "@SUM(1)")
	form.Set("contact_phone", "+27 82 555 0101")
	// The form trims spaces but not a BOM, so this one reaches the stored row.
	form.Set("a0.last", "\ufeff=HYPERLINK(1)")
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
	header := strings.Join(recs[0], "|")
	if !strings.Contains(header, "|Diet|") {
		t.Errorf("header = %v, want a column for the question", recs[0])
	}
	for _, want := range []string{"|'=1|", "|'=HYPERLINK(1)"} {
		if !strings.Contains(header, want) {
			t.Errorf("header is missing escaped %q: %s", want, header)
		}
	}
	row := strings.Join(recs[1], "|")
	for _, want := range []string{`'=HYPERLINK`, `'@SUM(1)`, `'+27 82 555 0101`, "|'=HYPERLINK(1)|"} {
		if !strings.Contains(row, want) {
			t.Errorf("row is missing escaped %q: %s", want, row)
		}
	}
}

// The test above fills the columns a public form can reach in one pass, and a
// substring check on the joined row cannot say which column an escape came
// from. This one checks each column on its own: an export that escapes every
// cell except one (a column pulled out of the CSVRow call, say) must fail on
// that column's name. The hidden-prefix spelling (a leading space, a BOM) is
// used because it is the one a first-byte check misses.
//
// Email and the registered-by name are written straight to the rows, not
// through the form, because the form's own validation would refuse some of
// them, and the export must not depend on that. The price column is formatted
// from integer cents under a CHECK >= 0, so no stored value can begin with
// anything but a digit; it has no hostile source to test.
func TestAdminEventExport_EscapesEachColumnOnItsOwn(t *testing.T) {
	s := newApp(t)
	e, adult, _ := published(t, s, 10, func(e *events.Event) {
		e.PayLater, e.PayLaterInstructions = true, "EFT"
	})
	form := registerForm(t, s, e, map[string]int{adult.ID: 1})
	form.Set("method", string(registrations.MethodLater))
	if res, body := submit(t, s, e, form); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("register = %d %s", res.StatusCode, excerpt(body))
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE attendees SET email = ' =cmd|email'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(),
		"UPDATE registrations SET contact_first_name = E'\\ufeff=cmd|by', contact_last_name = ''"); err != nil {
		t.Fatal(err)
	}
	signIn(t, s.srv)

	res, body := get(t, s.srv, "/admin/events/"+e.ID+"/attendees.csv")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("export = %d", res.StatusCode)
	}
	recs, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil || len(recs) != 2 {
		t.Fatalf("csv = %d records, %v:\n%s", len(recs), err, body)
	}
	col := func(name string) string {
		t.Helper()
		for i, h := range recs[0] {
			if h == name {
				return recs[1][i]
			}
		}
		t.Fatalf("no %q column in %v", name, recs[0])
		return ""
	}
	// Each cell leads with an apostrophe, so the spreadsheet shows text rather
	// than evaluating it (websec drops the hidden prefix while escaping).
	for name, want := range map[string]string{
		"Email":         "'=cmd|email",
		"Registered by": "'=cmd|by",
	} {
		if got := col(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// The registration page lists every email job of the registration, however many
// resends there have been: an old failure must not scroll off a page limit and
// leave an organiser believing the tickets went.
func TestAdminRegistrations_EmailListHasNoPageCap(t *testing.T) {
	s := newApp(t)
	e, _, free := published(t, s, 10)
	submit(t, s, e, registerForm(t, s, e, map[string]int{free.ID: 1}))
	reg := onlyRegistration(t, s)
	signIn(t, s.srv)

	// The confirmation is already queued; add more than the queue's default page
	// of 100, all for this registration (resend has no unique key).
	const resends = 104
	for range resends {
		if _, _, err := s.outbox.EnqueueNow(t.Context(), outbox.Message{Kind: kindResend, Subject: registrationSubject(reg.ID)}); err != nil {
			t.Fatal(err)
		}
	}
	res, page := get(t, s.srv, registrationPath(reg.ID))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d", res.StatusCode)
	}
	if rows := strings.Count(page, `<td class="num">0/`); rows != resends+1 {
		t.Errorf("the page lists %d email jobs, want all %d", rows, resends+1)
	}
}
