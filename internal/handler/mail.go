package handler

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/17xande-dev/goevent/internal/db/gen"
	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/outbox"
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/17xande-dev/goevent/internal/ticket"
	"github.com/17xande-dev/mailer"
)

// The emails, from queueing to sending.
//
// Queueing happens inside whichever transaction makes the email true — the
// checkout that confirmed a free registration, the payment that confirmed a
// paid one — through the registrations store's hooks. A job is only "send this
// kind of email about this registration"; the worker renders it from the
// database when it sends, which is what lets a confirmation days after
// checkout carry the same manage link and ticket codes the registrant may
// already have: both are derived from the registration, not stored.

// hooks is what the registrations store runs inside a checkout.
func (h *Handler) hooks() registrations.Hooks {
	return registrations.Hooks{Confirmed: h.onConfirm(), AwaitingPayment: h.onAwaitingPayment}
}

// onConfirm queues the registrant's confirmation and, when an organiser address
// is configured, their copy.
func (h *Handler) onConfirm() registrations.OnConfirm {
	return func(ctx context.Context, q *gen.Queries, c registrations.Confirmation) error {
		if h.outbox == nil {
			return errors.New("handler: the email queue is not configured")
		}
		if err := h.outbox.Enqueue(ctx, q, c.Registration.ID, outbox.KindConfirmation, nil); err != nil {
			return err
		}
		if h.cfg.NotifyEmail != "" {
			return h.outbox.Enqueue(ctx, q, c.Registration.ID, outbox.KindNotify, nil)
		}
		return nil
	}
}

// onAwaitingPayment queues the acknowledgement a pay-later registrant gets:
// how to pay, and no tickets yet.
func (h *Handler) onAwaitingPayment(ctx context.Context, q *gen.Queries, r registrations.Registration) error {
	if h.outbox == nil {
		return errors.New("handler: the email queue is not configured")
	}
	return h.outbox.Enqueue(ctx, q, r.ID, outbox.KindReceived, nil)
}

// ProcessMail drains a bounded batch. Tests call it directly; the server calls
// it on a timer, so no request ever waits for a mail server.
func (h *Handler) ProcessMail(ctx context.Context) {
	for range 20 {
		found, err := h.outbox.DeliverOne(ctx, h.deliver)
		if err != nil {
			h.log.Error("email queue", "error", err)
		}
		if !found || ctx.Err() != nil {
			return
		}
	}
}

// StartMailWorker runs ProcessMail every few seconds until ctx is cancelled,
// and returns a wait function so shutdown joins the worker before closing the
// pool. Unsent jobs survive a restart and are retried.
func (h *Handler) StartMailWorker(ctx context.Context) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			h.ProcessMail(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { <-done }
}

// mailTicket is one ticket as an email shows it.
type mailTicket struct {
	Name, TicketName string
	Code             string
	// CID is the inline image's Content-ID; the HTML refers to cid:CID.
	CID string
}

type mailData struct {
	SiteName     string
	Event        events.Event
	Registration registrations.Registration
	Tickets      []mailTicket
	ManageURL    string
	AdminURL     string
}

// deliver renders one queued email from the database and sends it.
func (h *Handler) deliver(ctx context.Context, job outbox.Job) error {
	reg, err := h.regs.Get(ctx, job.RegistrationID)
	if err != nil {
		return fmt.Errorf("email for registration %s: %w", job.RegistrationID, err)
	}
	e, err := h.events.Get(ctx, reg.EventID)
	if err != nil {
		return fmt.Errorf("email for registration %s: event: %w", reg.ID, err)
	}
	data := mailData{
		SiteName: h.cfg.SiteName, Event: e, Registration: reg,
		ManageURL: h.cfg.BaseURL + h.managePath(reg),
		AdminURL:  h.cfg.BaseURL + "/admin/registrations/" + reg.ID,
	}

	switch job.Kind {
	case outbox.KindNotify:
		text, err := h.tmpl.Text("email_notify.txt", data)
		if err != nil {
			return err
		}
		subject := "New registration " + reg.Reference + ": " + e.Title
		if reg.Oversold {
			subject = "OVERSOLD — " + subject
		}
		return h.mail.Send(ctx, mailer.Message{To: []string{h.cfg.NotifyEmail}, Subject: subject, Text: text})

	case outbox.KindReceived:
		return h.sendRendered(ctx, reg, "email_received", data,
			"Registration received for "+e.Title+" — payment needed", nil)

	case outbox.KindConfirmation, outbox.KindResend:
		attendees, err := h.regs.Attendees(ctx, reg.ID)
		if err != nil {
			return err
		}
		var inline []mailer.Inline
		for _, a := range attendees {
			if a.Status != "active" {
				continue
			}
			t := mailTicket{Name: a.Name(), TicketName: a.TicketName, Code: h.signer.TicketCode(a.ID),
				CID: "ticket-" + strconv.Itoa(len(data.Tickets)+1)}
			img, err := ticket.PNG(t.Code, 6)
			if err != nil {
				return err
			}
			inline = append(inline, mailer.Inline{ContentID: t.CID, ContentType: "image/png", Data: img})
			data.Tickets = append(data.Tickets, t)
		}
		return h.sendRendered(ctx, reg, "email_confirmation", data,
			"Your tickets for "+e.Title+" ("+reg.Reference+")", inline)

	default:
		return fmt.Errorf("unknown email kind %q", job.Kind)
	}
}

// sendRendered renders an email's two halves — name.txt and name — and sends
// it to the person who registered.
func (h *Handler) sendRendered(ctx context.Context, reg registrations.Registration, name string, data mailData, subject string, inline []mailer.Inline) error {
	text, err := h.tmpl.Text(name+".txt", data)
	if err != nil {
		return err
	}
	html, err := h.tmpl.String(name, data)
	if err != nil {
		return err
	}
	return h.mail.Send(ctx, mailer.Message{
		To: []string{reg.ContactEmail}, Subject: subject, Text: text, HTML: html, Inline: inline,
	})
}
