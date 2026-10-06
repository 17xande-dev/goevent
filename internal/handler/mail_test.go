package handler

import (
	"bytes"
	"errors"
	"image/png"
	"regexp"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/config"
	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/registrations"
)

func withNotify(c *config.Config) {
	c.NotifyEmail = "organiser@example.com"
	c.BaseURL = "https://events.example"
}

func TestMail_FreeConfirmationCarriesScannableTickets(t *testing.T) {
	s := newApp(t, withNotify)
	e, _, child := published(t, s, 10)
	submit(t, s, e, registerForm(t, s, e, map[string]int{child.ID: 2}))
	s.handler.ProcessMail(t.Context())

	got := s.mail.To("ada@example.com")
	if len(got) != 1 {
		t.Fatalf("the registrant got %d emails, want 1", len(got))
	}
	m := got[0]
	if !strings.Contains(m.Subject, "Your tickets for Family Camp") {
		t.Errorf("subject = %q", m.Subject)
	}
	// One inline QR per attendee, each a real PNG the HTML refers to.
	if len(m.Inline) != 2 {
		t.Fatalf("inline images = %d, want 2", len(m.Inline))
	}
	for _, in := range m.Inline {
		if !strings.Contains(m.HTML, `src="cid:`+in.ContentID+`"`) {
			t.Errorf("the HTML does not show %s", in.ContentID)
		}
		if _, err := png.Decode(bytes.NewReader(in.Data)); err != nil {
			t.Errorf("%s is not a PNG: %v", in.ContentID, err)
		}
	}
	// The plain text carries the codes themselves, and they are this server's.
	codes := regexp.MustCompile(`[0-9a-f-]{36}\.[A-Za-z0-9_-]{22}`).FindAllString(m.Text, -1)
	if len(codes) != 2 {
		t.Fatalf("ticket codes in the text = %d, want 2:\n%s", len(codes), m.Text)
	}
	for _, c := range codes {
		if _, ok := s.signer.ParseTicket(c); !ok {
			t.Errorf("code %q does not verify", c)
		}
	}
	if !strings.Contains(m.Text, "https://events.example/r/") {
		t.Errorf("no manage link in:\n%s", m.Text)
	}

	// The organiser's copy, without the tickets.
	org := s.mail.To("organiser@example.com")
	if len(org) != 1 || len(org[0].Inline) != 0 || !strings.Contains(org[0].Text, "/admin/registrations/") {
		t.Errorf("organiser copy = %+v", org)
	}
	if got := onlyRegistration(t, s); !got.Emailed {
		t.Error("the registration is not marked emailed")
	}
}

func TestMail_PaidConfirmationGoesOnceAfterThePayment(t *testing.T) {
	s := newApp(t)
	e, adult, _ := published(t, s, 10)
	submit(t, s, e, registerForm(t, s, e, map[string]int{adult.ID: 1}))
	s.handler.ProcessMail(t.Context())
	if n := len(s.mail.Sent()); n != 0 {
		t.Fatalf("%d emails before anything was paid", n)
	}

	paymentID := s.gateway.Requests()[0].PaymentID
	callback(t, s, paymentID, "COMPLETE", 45000)
	callback(t, s, paymentID, "COMPLETE", 45000) // the gateway retries
	s.handler.ProcessMail(t.Context())

	got := s.mail.To("ada@example.com")
	if len(got) != 1 || len(got[0].Inline) != 1 {
		t.Fatalf("after payment and a replay: %d emails", len(got))
	}
}

func TestMail_PayLaterIsAcknowledgedWithoutTickets(t *testing.T) {
	s := newApp(t)
	e, adult, _ := published(t, s, 10, func(e *events.Event) {
		e.PayLater, e.PayLaterInstructions = true, "Bank: Example Bank & Co"
	})
	form := registerForm(t, s, e, map[string]int{adult.ID: 1})
	form.Set("method", string(registrations.MethodLater))
	submit(t, s, e, form)
	s.handler.ProcessMail(t.Context())

	got := s.mail.To("ada@example.com")
	if len(got) != 1 {
		t.Fatalf("emails = %d", len(got))
	}
	m := got[0]
	if !strings.Contains(m.Subject, "payment needed") || len(m.Inline) != 0 {
		t.Errorf("acknowledgement = %q with %d images", m.Subject, len(m.Inline))
	}
	// Plain text is not HTML-escaped: the ampersand arrives as written.
	if !strings.Contains(m.Text, "Example Bank & Co") || !strings.Contains(m.Text, "ZAR 450.00") {
		t.Errorf("instructions or amount missing:\n%s", m.Text)
	}
}

func TestMail_AFailedSendIsKeptForTheNextTry(t *testing.T) {
	s := newApp(t)
	e, _, child := published(t, s, 10)
	submit(t, s, e, registerForm(t, s, e, map[string]int{child.ID: 1}))

	s.mail.Err = errors.New("relay unavailable")
	s.handler.ProcessMail(t.Context())
	reg := onlyRegistration(t, s)
	jobs, err := s.outbox.ForRegistration(t.Context(), reg.ID)
	if err != nil || len(jobs) != 1 || jobs[0].SentAt != nil || jobs[0].Attempts != 1 {
		t.Fatalf("after a failed send: %+v %v", jobs, err)
	}
	if reg.Emailed {
		t.Error("marked emailed after a failed send")
	}
}

func TestManage_ConfirmedRegistrationShowsTicketCodes(t *testing.T) {
	s := newApp(t)
	e, _, child := published(t, s, 10)
	res, _ := submit(t, s, e, registerForm(t, s, e, map[string]int{child.ID: 2}))
	_, page := get(t, s.srv, res.Header.Get("Location"))
	if n := strings.Count(page, `<svg class="qr"`); n != 2 {
		t.Errorf("confirmed page shows %d QR codes, want 2", n)
	}

	// A pending one shows none: a held seat is not a ticket.
	s2 := newApp(t)
	e2, adult2, _ := published(t, s2, 10)
	submit(t, s2, e2, registerForm(t, s2, e2, map[string]int{adult2.ID: 1}))
	reg := onlyRegistration(t, s2)
	_, page = get(t, s2.srv, s2.handler.managePath(reg))
	if strings.Contains(page, `<svg class="qr"`) {
		t.Error("a pending registration's page shows ticket codes")
	}
}
