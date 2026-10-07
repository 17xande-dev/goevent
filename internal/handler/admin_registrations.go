package handler

import (
	"bytes"
	"encoding/csv"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/outbox"
	"github.com/17xande-dev/goevent/internal/registrations"
)

// The registrations admin: the list, one registration, the money recorded by
// hand, cancellations, resending tickets, and the export.
//
// A payment recorded here confirms a registration exactly as a gateway's does —
// same lock, same confirmation email queued in the same transaction — so a
// pay-later registration paid at the office ends up indistinguishable from one
// paid online.

var registrationNotices = map[string]string{
	"paid":               "Payment recorded.",
	"confirmed":          "Payment recorded. The registration is confirmed and the tickets are on their way.",
	"cancelled":          "Registration cancelled and its places released. Refund any money taken by hand.",
	"attendee_cancelled": "Attendee cancelled and their place released. Their ticket no longer works.",
	"resent":             "The tickets are being sent again.",
	"retrying":           "Failed emails will be tried again in a moment.",
}

var registrationProblems = map[string]string{
	"amount":            "That amount is not owed. Enter no more than what is left to pay.",
	"cancelled":         "This registration is cancelled, so no payment can be recorded against it.",
	"not_confirmed":     "Only a confirmed registration has tickets to send.",
	"already_cancelled": "This registration is already cancelled.",
}

type registrationsPage struct {
	page
	Registrations []registrations.Listed
	Events        []events.Event
	Filter        registrations.Filter
	Statuses      []registrations.Status
}

func (h *Handler) adminRegistrationList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := registrations.Filter{EventID: q.Get("event"), Status: registrations.Status(q.Get("status")), Search: q.Get("q")}
	// An unknown status filters to nothing rather than being ignored, which
	// would show everything under a heading that claims a filter.
	list, err := h.regs.List(r.Context(), f)
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	evs, err := h.events.List(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "admin_registrations", registrationsPage{
		page: h.newPage(r, "Registrations"), Registrations: list, Events: evs, Filter: f,
		Statuses: []registrations.Status{registrations.StatusConfirmed, registrations.StatusPending,
			registrations.StatusExpired, registrations.StatusCancelled},
	})
}

type registrationPage struct {
	page
	Event           events.Event
	Registration    registrations.Registration
	Attendees       []registrations.Attendee
	Answers         []registrations.Answer
	AttendeeAnswers map[string][]registrations.Answer
	Payments        []registrations.Payment
	Emails          []outbox.Status
	PaidCents       int64
	ManageURL       string
	Notice, Problem string
}

// Owed is what is left to pay.
func (p registrationPage) Owed() int64 { return max(0, p.Registration.TotalCents-p.PaidCents) }

func (h *Handler) adminRegistrationShow(w http.ResponseWriter, r *http.Request) {
	reg, err := h.regs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	e, err := h.events.Get(r.Context(), reg.EventID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	data := registrationPage{
		page: h.newPage(r, reg.Reference), Event: e, Registration: reg,
		AttendeeAnswers: map[string][]registrations.Answer{},
		ManageURL:       h.cfg.BaseURL + h.managePath(reg),
		Notice:          noticeFor(r, registrationNotices),
		Problem:         registrationProblems[r.URL.Query().Get("problem")],
	}
	if data.Attendees, err = h.regs.Attendees(r.Context(), reg.ID); err != nil {
		h.serverError(w, r, err)
		return
	}
	answers, err := h.regs.Answers(r.Context(), reg.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	for _, a := range answers {
		if a.AttendeeID == "" {
			data.Answers = append(data.Answers, a)
		} else {
			data.AttendeeAnswers[a.AttendeeID] = append(data.AttendeeAnswers[a.AttendeeID], a)
		}
	}
	if data.Payments, err = h.regs.Payments(r.Context(), reg.ID); err != nil {
		h.serverError(w, r, err)
		return
	}
	for _, p := range data.Payments {
		if p.Status == "paid" {
			data.PaidCents += p.AmountCents
		}
	}
	if data.Emails, err = h.outbox.ForRegistration(r.Context(), reg.ID); err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "admin_registration", data)
}

// adminRegistrationPay records cash or EFT. The method is read strictly — an
// absent or unknown one is a 400, never a default — and the amount must be
// owed: more is a typo, not a payment.
func (h *Handler) adminRegistrationPay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	method := registrations.Method(r.PostFormValue("method"))
	if method != registrations.MethodCash && method != registrations.MethodEFT {
		h.clientError(w, r, http.StatusBadRequest, "Unknown payment method",
			"That form asked for a payment method this server does not record.")
		return
	}
	cents, err := events.ParsePrice(r.PostFormValue("amount"))
	if err != nil {
		h.registrationProblem(w, r, id, "amount")
		return
	}
	confirmed, err := h.regs.RecordPayment(r.Context(), registrations.Manual{
		RegistrationID: id, Method: method, AmountCents: cents, RecordedBy: h.actorID(r),
		Note: r.PostFormValue("note"),
	}, h.onConfirm())
	switch {
	case errors.Is(err, registrations.ErrAmount):
		h.registrationProblem(w, r, id, "amount")
		return
	case errors.Is(err, registrations.ErrCancelled):
		h.registrationProblem(w, r, id, "cancelled")
		return
	case err != nil:
		h.storeError(w, r, err)
		return
	}
	h.logger(r).Info("manual payment recorded", "registration", id, "method", method,
		"amount_cents", cents, "confirmed", confirmed, "by", h.actorID(r))
	notice := "paid"
	if confirmed {
		notice = "confirmed"
	}
	http.Redirect(w, r, registrationPath(id)+"?notice="+notice, http.StatusSeeOther)
}

func (h *Handler) adminRegistrationCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, err := h.regs.Cancel(r.Context(), id)
	switch {
	case errors.Is(err, registrations.ErrNotCancellable):
		h.registrationProblem(w, r, id, "already_cancelled")
		return
	case err != nil:
		h.storeError(w, r, err)
		return
	}
	h.logger(r).Info("registration cancelled", "registration", id, "by", h.actorID(r))
	http.Redirect(w, r, registrationPath(id)+"?notice=cancelled", http.StatusSeeOther)
}

func (h *Handler) adminAttendeeCancel(w http.ResponseWriter, r *http.Request) {
	id, attendee := r.PathValue("id"), r.PathValue("attendeeID")
	if err := h.regs.CancelAttendee(r.Context(), id, attendee); err != nil {
		h.storeError(w, r, err)
		return
	}
	h.logger(r).Info("attendee cancelled", "registration", id, "attendee", attendee, "by", h.actorID(r))
	http.Redirect(w, r, registrationPath(id)+"?notice=attendee_cancelled", http.StatusSeeOther)
}

// adminRegistrationResend queues the tickets again. Only for a confirmed
// registration: anything else has no tickets.
func (h *Handler) adminRegistrationResend(w http.ResponseWriter, r *http.Request) {
	reg, err := h.regs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	if reg.Status != registrations.StatusConfirmed {
		h.registrationProblem(w, r, reg.ID, "not_confirmed")
		return
	}
	if err := h.outbox.Queue(r.Context(), reg.ID, outbox.KindResend, nil); err != nil {
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, registrationPath(reg.ID)+"?notice=resent", http.StatusSeeOther)
}

func (h *Handler) adminRegistrationRetryEmail(w http.ResponseWriter, r *http.Request) {
	reg, err := h.regs.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	if err := h.outbox.Retry(r.Context(), reg.ID); err != nil {
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, registrationPath(reg.ID)+"?notice=retrying", http.StatusSeeOther)
}

func (h *Handler) registrationProblem(w http.ResponseWriter, r *http.Request, id, code string) {
	http.Redirect(w, r, registrationPath(id)+"?problem="+code, http.StatusSeeOther)
}

func registrationPath(id string) string { return "/admin/registrations/" + url.PathEscape(id) }

// adminEventExport is every active attendee of an event as CSV, one row each,
// with a column per question.
//
// Built in a buffer and only then written: streaming would commit a 200 before
// the query could fail, and hand somebody a truncated file that looks complete.
func (h *Handler) adminEventExport(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	qs, err := h.events.Questions(r.Context(), e.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	rows, err := h.regs.Export(r.Context(), e.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}

	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	header := []string{"Reference", "Status", "First name", "Last name", "Email", "Ticket", "Price",
		"Checked in", "Registered by", "Contact email", "Contact phone", "Registered"}
	for _, q := range qs {
		header = append(header, q.Label)
	}
	cw.Write(cells(header))
	for _, row := range rows {
		checkedIn := ""
		if row.CheckedInAt != nil {
			checkedIn = e.Local(*row.CheckedInAt).Format("2006-01-02 15:04")
		}
		rec := []string{row.Reference, row.RegistrationStatus.Label(), row.FirstName, row.LastName,
			row.Email, row.TicketName, events.FormatPrice(row.UnitPriceCents), checkedIn, row.Contact,
			row.ContactEmail, row.ContactPhone, e.Local(row.RegisteredAt).Format("2006-01-02 15:04")}
		for _, q := range qs {
			rec = append(rec, row.Answers[q.ID])
		}
		cw.Write(cells(rec))
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		h.serverError(w, r, err)
		return
	}

	name := e.Slug + "-attendees-" + time.Now().Format("2006-01-02") + ".csv"
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Write(buf.Bytes())
}

// cells escapes every cell a spreadsheet would read as a formula. Excel and
// Sheets evaluate a cell beginning =, +, - or @ — so a name typed into a public
// form becomes a live formula on a staff machine, and a +27… phone number is
// the everyday case that trips it. A leading apostrophe makes it text.
func cells(rec []string) []string {
	out := make([]string, len(rec))
	for i, c := range rec {
		if c != "" && strings.ContainsRune("=+-@\t\r", rune(c[0])) {
			c = "'" + c
		}
		out[i] = c
	}
	return out
}
