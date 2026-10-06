package handler

import (
	"net/http"
	"time"

	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/registrations"
)

// RegisterPath is the registration form's route, which the caller mounts on the
// first-party (CSRF) handler: its POST changes state.
const RegisterPath = "/events/{slug}/register"

// RegisterPublic wires the routes anybody may reach without a session: the
// events, the checkout's return pages, a registrant's manage page, and the
// bundled assets. None of them changes anything — the registration form, which
// does, is in FirstPartyHandler — so none carries a CSRF cookie.
func (h *Handler) RegisterPublic(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/events", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /events", h.publicEvents)
	mux.HandleFunc("GET /events/{slug}", h.publicEvent)

	// Where a gateway sends the browser back. Informational only: only an
	// authenticated notification to the callback can make a payment paid.
	mux.HandleFunc("GET /checkout/success", h.checkoutReturn)
	mux.HandleFunc("GET /checkout/cancel", h.checkoutReturn)
	mux.Handle("GET /checkout/status", h.limits.status(http.HandlerFunc(h.checkoutStatus)))

	// A registrant's own page, reached from their confirmation email.
	mux.HandleFunc("GET /r/{reference}", h.manageRegistration)

	// "/" — the bare subtree, which matches every path no other pattern claimed —
	// is how a custom 404 page is installed, since ServeMux has no
	// NotFoundHandler to set.
	//
	// One consequence worth knowing: a request to a *known* path under an
	// unregistered method lands here as a 404 rather than getting a 405, because
	// a pattern that matches beats one that would only have matched with a
	// different method.
	mux.HandleFunc("/", h.notFoundFor(mux))

	// Vendored htmx and the theme, served from the binary so no page needs a CDN.
	mux.Handle("GET /static/", http.HandlerFunc(h.static))
}

// isHTMX reports whether htmx made this request.
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

type publicEventsPage struct {
	page
	Events []events.Event
}

func (h *Handler) publicEvents(w http.ResponseWriter, r *http.Request) {
	list, err := h.events.ListPublic(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "events", publicEventsPage{page: h.newPage(r, "Events"), Events: list})
}

// offer is a ticket type as the event page shows it: what it costs and how many
// one registration could take right now.
type offer struct {
	events.TicketType
	// Choices is the quantities to offer, 0 first; just [0] when none are left.
	Choices []int
	SoldOut bool
	// Left is how many remain, when that is few enough to be worth saying.
	Left int
}

type publicEventPage struct {
	page
	Event  events.Event
	Offers []offer
	// Open is whether registration is open now; Closed says why not when it is
	// not.
	Open   bool
	Closed string
	// Problem is a refusal from the register step, by code.
	Problem string
}

// publicEvent is an event's page, with the first step of registering: how
// many of each ticket. That step is a GET form — choosing quantities changes
// nothing — and leads to the registration form proper.
func (h *Handler) publicEvent(w http.ResponseWriter, r *http.Request) {
	e, ok := h.publicEventBySlug(w, r)
	if !ok {
		return
	}
	data, err := h.eventPage(r, e)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "event", data)
}

func (h *Handler) eventPage(r *http.Request, e events.Event) (publicEventPage, error) {
	now := time.Now()
	data := publicEventPage{
		page: h.newPage(r, e.Title), Event: e, Open: e.RegistrationOpen(now),
		Problem: registerProblems[r.URL.Query().Get("problem")],
	}
	switch {
	case e.Status == events.StatusCancelled:
		data.Closed = "This event has been cancelled."
	case e.Status == events.StatusClosed:
		data.Closed = "Registration for this event has closed."
	case !now.Before(e.EndsAt):
		data.Closed = "This event has finished."
	case e.RegistrationOpensAt != nil && now.Before(*e.RegistrationOpensAt):
		data.Closed = "Registration opens " + e.Local(*e.RegistrationOpensAt).Format("Monday 2 January at 15:04") + "."
	case !data.Open:
		data.Closed = "Registration for this event has closed."
	}
	if !data.Open {
		return data, nil
	}

	tts, err := h.events.TicketTypes(r.Context(), e.ID)
	if err != nil {
		return data, err
	}
	held, err := h.regs.Held(r.Context(), e.ID)
	if err != nil {
		return data, err
	}
	for _, t := range tts {
		if !t.OnSale(now) {
			continue
		}
		o := offer{TicketType: t}
		most := t.MaxPerRegistration
		if left, limited := registrations.Remaining(e, t, held); limited {
			most = min(most, left)
			if left <= 10 {
				o.Left = left
			}
		}
		o.SoldOut = most == 0
		for n := 0; n <= most; n++ {
			o.Choices = append(o.Choices, n)
		}
		data.Offers = append(data.Offers, o)
	}
	return data, nil
}

// publicEventBySlug loads a visible event: a draft is a 404, exactly as if it
// did not exist, so drafts cannot be discovered by guessing their address.
func (h *Handler) publicEventBySlug(w http.ResponseWriter, r *http.Request) (events.Event, bool) {
	e, err := h.events.GetBySlug(r.Context(), r.PathValue("slug"))
	if err == nil && !e.Status.Public() {
		err = events.ErrNotFound
	}
	if err != nil {
		h.storeError(w, r, err)
		return events.Event{}, false
	}
	return e, true
}
