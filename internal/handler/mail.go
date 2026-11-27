package handler

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/17xande-dev/goevent/internal/db/gen"
	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/17xande-dev/goevent/internal/ticket"
	"github.com/17xande-dev/mailer"
	"github.com/17xande-dev/outbox"
	"github.com/jackc/pgx/v5"
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

const (
	kindConfirmation = "confirmation"
	kindReceived     = "received"
	kindNotify       = "notify"
	kindResend       = "resend"

	registrationSubjectPrefix = "registration:"
)

func registrationSubject(id string) string { return registrationSubjectPrefix + id }

func (h *Handler) newMailQueue(store outbox.Store[pgx.Tx], opts outbox.Options) (*outbox.Queue[pgx.Tx], error) {
	// A transport or template error can include credentials or ticket codes.
	opts.Describe = func(error) string {
		return "Email delivery failed; check mail configuration and retry."
	}
	email := outbox.Kind[pgx.Tx]{
		Run:     h.deliver,
		Timeout: func(context.Context, outbox.Job) time.Duration { return 30 * time.Second },
	}
	confirmation := email
	confirmation.OnDone = func(ctx context.Context, tx pgx.Tx, job outbox.Job, _ string) error {
		return gen.New(tx).MarkRegistrationEmailed(ctx, strings.TrimPrefix(job.Subject, registrationSubjectPrefix))
	}
	return outbox.New(store, map[string]outbox.Kind[pgx.Tx]{
		kindConfirmation: confirmation,
		kindReceived:     email,
		kindNotify:       email,
		kindResend:       email,
	}, opts)
}

// MailQueue is the server's queue, constructed with the mail handlers above.
func (h *Handler) MailQueue() *outbox.Queue[pgx.Tx] { return h.outbox }

// hooks is what the registrations store runs inside a checkout.
func (h *Handler) hooks() registrations.Hooks {
	return registrations.Hooks{Confirmed: h.onConfirm(), AwaitingPayment: h.onAwaitingPayment}
}

// onConfirm queues the registrant's confirmation and, when an organiser address
// is configured, their copy.
func (h *Handler) onConfirm() registrations.OnConfirm {
	return func(ctx context.Context, tx pgx.Tx, c registrations.Confirmation) error {
		if err := h.enqueueOnce(ctx, tx, c.Registration.ID, kindConfirmation); err != nil {
			return err
		}
		if h.cfg.NotifyEmail != "" {
			return h.enqueueOnce(ctx, tx, c.Registration.ID, kindNotify)
		}
		return nil
	}
}

// onAwaitingPayment queues the acknowledgement a pay-later registrant gets:
// how to pay, and no tickets yet.
func (h *Handler) onAwaitingPayment(ctx context.Context, tx pgx.Tx, r registrations.Registration) error {
	return h.enqueueOnce(ctx, tx, r.ID, kindReceived)
}

func (h *Handler) enqueueOnce(ctx context.Context, tx pgx.Tx, id, kind string) error {
	if h.outbox == nil {
		return errors.New("handler: the email queue is not configured")
	}
	subject := registrationSubject(id)
	_, _, err := h.outbox.Enqueue(ctx, tx, outbox.Message{Kind: kind, Subject: subject, UniqueKey: subject + ":" + kind})
	return mailQueueError("enqueue "+kind+" email", err)
}

func mailQueueError(operation string, err error) error {
	if err == nil {
		return nil
	}
	// A PostgreSQL diagnostic can include the row containing the sealed payload.
	return fmt.Errorf("%s: %T", operation, err)
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
	id, ok := strings.CutPrefix(job.Subject, registrationSubjectPrefix)
	if !ok || id == "" {
		return outbox.Permanent(errors.New("email job has no registration subject"))
	}
	reg, err := h.regs.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("load registration for email: %w", err)
	}
	e, err := h.events.Get(ctx, reg.EventID)
	if err != nil {
		return fmt.Errorf("load event for email: %w", err)
	}
	data := mailData{
		SiteName: h.cfg.SiteName, Event: e, Registration: reg,
		ManageURL: h.cfg.BaseURL + h.managePath(reg),
		AdminURL:  h.cfg.BaseURL + "/admin/registrations/" + reg.ID,
	}

	switch job.Kind {
	case kindNotify:
		text, err := h.tmpl.Text("email_notify.txt", data)
		if err != nil {
			return err
		}
		subject := "New registration " + reg.Reference + ": " + e.Title
		if reg.Oversold {
			subject = "OVERSOLD — " + subject
		}
		return h.mail.Send(ctx, mailer.Message{To: []string{h.cfg.NotifyEmail}, Subject: oneLine(subject), Text: text})

	case kindReceived:
		return h.sendRendered(ctx, reg, "email_received", data,
			"Registration received for "+e.Title+" — payment needed", nil)

	case kindConfirmation, kindResend:
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
		To: []string{reg.ContactEmail}, Subject: oneLine(subject), Text: text, HTML: html, Inline: inline,
	})
}

// oneLine flattens a subject. The mailer refuses a line break in one, rightly,
// and an event title is typed by a person: a title that somehow carries one
// would otherwise fail every email for its event, on every retry.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
