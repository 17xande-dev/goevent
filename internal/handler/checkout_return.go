package handler

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"

	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/17xande-dev/goevent/internal/ticket"
)

// After the gateway: the page its browser return lands on, the status poll a
// QR hand-over asks, and the registrant's own page.
//
// None of these decides anything. A payment is paid when an authenticated
// notification reached the callback, and these only report what that did.
//
// The return pages are found by a payment id, which the gateway knows too, so a
// payment id alone shows only whether it was paid. The link to the
// registration itself — names, tickets — is offered when the browser also holds
// the cookie the checkout set, which only the browser that registered does.

type checkoutReturnPage struct {
	page
	Event        events.Event
	Registration registrations.Registration
	Payment      registrations.Payment
	Found        bool
	Cancelled    bool
	// Manage is the registration's page, when this browser made it.
	Manage string
}

// Paid reports whether the payment went through.
func (p checkoutReturnPage) Paid() bool { return p.Payment.Status == "paid" }

// Failed reports whether the gateway said it did not.
func (p checkoutReturnPage) Failed() bool {
	return p.Payment.Status == "failed" || p.Payment.Status == "cancelled"
}

func (h *Handler) checkoutReturn(w http.ResponseWriter, r *http.Request) {
	data := h.returnData(r)
	data.Cancelled = strings.HasSuffix(r.URL.Path, "/cancel")
	if data.Cancelled {
		data.page = h.newPage(r, "Payment cancelled")
	}
	h.render(w, r, http.StatusOK, "checkout_return", data)
}

// checkoutStatus is the QR hand-over's poll: the status block alone for htmx,
// a whole page for anybody who opens the URL.
func (h *Handler) checkoutStatus(w http.ResponseWriter, r *http.Request) {
	data := h.returnData(r)
	if data.Paid() && isHTMX(r) {
		w.Header().Set("HX-Redirect", "/checkout/success?payment="+url.QueryEscape(data.Payment.ID))
	}
	name := "checkout_status"
	if isHTMX(r) && r.Header.Get("HX-Boosted") != "true" {
		name = "checkout_status_fragment"
	}
	h.render(w, r, http.StatusOK, name, data)
}

func (h *Handler) returnData(r *http.Request) checkoutReturnPage {
	data := checkoutReturnPage{page: h.newPage(r, "Thank you")}
	p, err := h.regs.Payment(r.Context(), r.URL.Query().Get("payment"))
	if err != nil {
		return data
	}
	reg, err := h.regs.Get(r.Context(), p.RegistrationID)
	if err != nil {
		h.logger(r).Error("payment without its registration", "payment", p.ID, "error", err)
		return data
	}
	e, err := h.events.Get(r.Context(), reg.EventID)
	if err != nil {
		h.logger(r).Error("registration without its event", "registration", reg.ID, "error", err)
		return data
	}
	data.Event, data.Registration, data.Payment, data.Found = e, reg, p, true
	if c, err := r.Cookie(regCookie); err == nil {
		if id, token, ok := strings.Cut(c.Value, "."); ok && id == reg.ID && h.signer.CheckManage(reg.ID, token) {
			data.Manage = h.managePath(reg)
		}
	}
	return data
}

type managePage struct {
	page
	Event        events.Event
	Registration registrations.Registration
	Attendees    []registrations.Attendee
	Answers      []registrations.Answer
	// AttendeeAnswers groups the per-attendee answers by attendee id.
	AttendeeAnswers map[string][]registrations.Answer
	// Tickets is each active attendee's QR code, by attendee id — present only
	// once the registration is confirmed: a held seat is not a ticket.
	Tickets map[string]template.HTML
}

// manageRegistration is a registrant's own page: what they registered, its
// status, and — once confirmed — their tickets. The reference names it and the
// token proves the visitor was sent the link. Wrong either way is a 404, never
// a hint about which half was wrong.
func (h *Handler) manageRegistration(w http.ResponseWriter, r *http.Request) {
	reg, err := h.regs.ByReference(r.Context(), r.PathValue("reference"))
	if err != nil || !h.signer.CheckManage(reg.ID, r.URL.Query().Get("t")) {
		h.notFound(w, r)
		return
	}
	e, err := h.events.Get(r.Context(), reg.EventID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	attendees, err := h.regs.Attendees(r.Context(), reg.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	answers, err := h.regs.Answers(r.Context(), reg.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	data := managePage{
		page: h.newPage(r, e.Title), Event: e, Registration: reg, Attendees: attendees,
		AttendeeAnswers: map[string][]registrations.Answer{},
	}
	for _, a := range answers {
		if a.AttendeeID == "" {
			data.Answers = append(data.Answers, a)
		} else {
			data.AttendeeAnswers[a.AttendeeID] = append(data.AttendeeAnswers[a.AttendeeID], a)
		}
	}
	if reg.Status == registrations.StatusConfirmed {
		data.Tickets = map[string]template.HTML{}
		for _, a := range attendees {
			if a.Status != "active" {
				continue
			}
			svg, err := ticket.SVG(h.signer.TicketCode(a.ID), "Ticket code for "+a.Name())
			if err != nil {
				h.serverError(w, r, err)
				return
			}
			data.Tickets[a.ID] = svg
		}
	}
	// The page carries the registrant's details and, soon, their tickets: it is
	// theirs alone, and no shared cache should keep a copy.
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	h.render(w, r, http.StatusOK, "registration", data)
}
