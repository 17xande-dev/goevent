package handler

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/17xande-dev/goevent/internal/blob"
	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/17xande-dev/goevent/internal/validate"
)

// The events admin: the list, the details form, status changes and the event
// page that gathers the ticket types and questions under it.
//
// An event is created as a draft and published by its own button, never by the
// details form, so saving a typo fix can never put a half-made event in front of
// the public.

// eventNotices are the notices the event pages show, by code. Looked up, never
// echoed — see noticeFor.
var eventNotices = map[string]string{
	"created":           "Event created as a draft. Add a ticket type, then publish it.",
	"saved":             "Saved.",
	"published":         "Published. The event is visible and, inside its registration window, open.",
	"draft":             "Returned to draft. Only administrators can see it now.",
	"closed":            "Registration closed. The event page stays up and says so.",
	"cancelled":         "Cancelled. The event page stays up and says so.",
	"deleted":           "Event deleted.",
	"image_saved":       "Image uploaded.",
	"image_removed":     "Image removed.",
	"ticket_saved":      "Ticket type saved.",
	"ticket_deleted":    "Ticket type deleted.",
	"ticket_archived":   "People already hold this ticket type, so it was archived rather than deleted. It is no longer offered.",
	"question_saved":    "Question saved.",
	"question_deleted":  "Question deleted.",
	"question_archived": "People have already answered this question, so it was archived rather than deleted. It is no longer asked.",
}

// eventProblems are the refusals the event page explains, by code.
var eventProblems = map[string]string{
	"needs_ticket": "Add a ticket type before publishing — an event with nothing to register for cannot take registrations.",
	"in_use":       "People have registered for this event, so it cannot be deleted. Cancel it instead.",
	"status":       "That change is not available from the event's current status.",
}

type eventsPage struct {
	page
	Events []events.Event
	Notice string
	Now    time.Time
}

// eventFormPage is the details form, alone for a new event and as part of the
// event page for an existing one.
type eventFormPage struct {
	page
	Event  events.Event
	Form   formValues
	Errors validate.FormErrors
}

// eventPage is everything about one event.
type eventPage struct {
	eventFormPage
	Notice      string
	Problem     string
	TicketTypes []events.TicketType
	Questions   []events.Question
	Next        []events.Status
	// Stats is the seats taken and the money received.
	Stats       registrations.Stats
	ImageError  string
	AcceptTypes string
	MaxUploadMB int64
}

func (h *Handler) adminEventList(w http.ResponseWriter, r *http.Request) {
	list, err := h.events.List(r.Context())
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "admin_events", eventsPage{
		page:   h.newPage(r, "Events"),
		Events: list,
		Notice: noticeFor(r, eventNotices),
		Now:    time.Now(),
	})
}

func (h *Handler) adminEventNew(w http.ResponseWriter, r *http.Request) {
	e := events.Event{Timezone: events.DefaultTimezone, Listed: true}
	h.render(w, r, http.StatusOK, "admin_event_new", eventFormPage{
		page:  h.newPage(r, "New event"),
		Event: e,
		Form:  eventValues(e),
	})
}

func (h *Handler) adminEventCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return
	}
	e, form, errs := parseEvent(r)
	if !errs.Any() {
		created, err := h.events.Create(r.Context(), e)
		switch {
		case err == nil:
			h.logger(r).Info("event created", "event", created.ID, "slug", created.Slug, "by", h.actorID(r))
			http.Redirect(w, r, eventPath(created.ID)+"?notice=created", http.StatusSeeOther)
			return
		case errors.Is(err, events.ErrSlugTaken):
			errs.Add("slug", "Another event already uses this address.")
		default:
			h.serverError(w, r, err)
			return
		}
	}
	h.render(w, r, http.StatusUnprocessableEntity, "admin_event_new", eventFormPage{
		page: h.newPage(r, "New event"), Event: e, Form: form, Errors: errs,
	})
}

func (h *Handler) adminEventShow(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	h.renderEvent(w, r, http.StatusOK, e, eventValues(e), nil, "")
}

func (h *Handler) adminEventUpdate(w http.ResponseWriter, r *http.Request) {
	stored, ok := h.event(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return
	}
	e, form, errs := parseEvent(r)
	e.ID = stored.ID
	if !errs.Any() {
		_, err := h.events.Update(r.Context(), e)
		switch {
		case err == nil:
			http.Redirect(w, r, eventPath(e.ID)+"?notice=saved", http.StatusSeeOther)
			return
		case errors.Is(err, events.ErrSlugTaken):
			errs.Add("slug", "Another event already uses this address.")
		default:
			h.storeError(w, r, err)
			return
		}
	}
	// The stored event, not the half-parsed one: the rest of the page — status,
	// image, ticket types — describes what is saved, and only the form shows what
	// was typed.
	h.renderEvent(w, r, http.StatusUnprocessableEntity, stored, form, errs, "")
}

// adminEventStatus moves an event through its life. The target is read strictly
// from the button that was pressed and checked against where the event may go
// from here, so a hand-made request cannot jump a cancelled event straight back
// to published.
func (h *Handler) adminEventStatus(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	next := events.Status(r.PostFormValue("status"))
	if !next.Valid() {
		h.clientError(w, r, http.StatusBadRequest, "Unknown status",
			"That form asked for a status this server does not have.")
		return
	}
	_, err := h.events.Transition(r.Context(), e.ID, e.Status, next)
	switch {
	case errors.Is(err, events.ErrStatusMove):
		http.Redirect(w, r, eventPath(e.ID)+"?problem=status", http.StatusSeeOther)
		return
	case errors.Is(err, events.ErrNothingOffered):
		http.Redirect(w, r, eventPath(e.ID)+"?problem=needs_ticket", http.StatusSeeOther)
		return
	case err != nil:
		h.storeError(w, r, err)
		return
	}
	h.logger(r).Info("event status changed", "event", e.ID, "from", e.Status, "to", next, "by", h.actorID(r))
	http.Redirect(w, r, eventPath(e.ID)+"?notice="+string(next), http.StatusSeeOther)
}

func (h *Handler) adminEventDelete(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	err := h.events.Delete(r.Context(), e.ID)
	switch {
	case errors.Is(err, events.ErrInUse):
		http.Redirect(w, r, eventPath(e.ID)+"?problem=in_use", http.StatusSeeOther)
		return
	case err != nil:
		h.storeError(w, r, err)
		return
	}
	// The row is gone, so the image it owned is an orphan now: remove it.
	h.deleteImageObject(r, e.ImageKey, e.ID)
	h.logger(r).Info("event deleted", "event", e.ID, "slug", e.Slug, "by", h.actorID(r))
	http.Redirect(w, r, "/admin/events?notice=deleted", http.StatusSeeOther)
}

// renderEvent renders the event page around a details form in whatever state
// the caller has it.
func (h *Handler) renderEvent(w http.ResponseWriter, r *http.Request, status int, e events.Event, form formValues, errs validate.FormErrors, imageError string) {
	tts, err := h.events.TicketTypes(r.Context(), e.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	qs, err := h.events.Questions(r.Context(), e.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	stats, err := h.regs.EventStats(r.Context(), e.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, status, "admin_event", eventPage{
		Stats:         stats,
		eventFormPage: eventFormPage{page: h.newPage(r, e.Title), Event: e, Form: form, Errors: errs},
		Notice:        noticeFor(r, eventNotices),
		Problem:       eventProblems[r.URL.Query().Get("problem")],
		TicketTypes:   tts,
		Questions:     qs,
		Next:          e.Status.Next(),
		ImageError:    imageError,
		AcceptTypes:   strings.Join(blob.SupportedTypes(), ","),
		MaxUploadMB:   blob.MaxUploadBytes >> 20,
	})
}

// event loads the event named in the path, answering 404 itself when there is
// none, so a handler reads `e, ok := h.event(w, r); if !ok { return }`.
func (h *Handler) event(w http.ResponseWriter, r *http.Request) (events.Event, bool) {
	e, err := h.events.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		h.storeError(w, r, err)
		return events.Event{}, false
	}
	return e, true
}

func eventPath(id string) string { return "/admin/events/" + id }

// eventValues renders a stored event into the details form's fields.
func eventValues(e events.Event) formValues {
	loc := e.Location()
	f := formValues{
		"title": e.Title, "slug": e.Slug, "summary": e.Summary, "description": e.Description,
		"venue": e.Venue, "address": e.Address, "timezone": e.Timezone,
		"capacity": formatCount(e.Capacity), "listed": checkbox(e.Listed),
		"pay_later": checkbox(e.PayLater), "pay_later_instructions": e.PayLaterInstructions,
		"registration_opens_at":  formatLocal(e.RegistrationOpensAt, loc),
		"registration_closes_at": formatLocal(e.RegistrationClosesAt, loc),
	}
	if !e.StartsAt.IsZero() {
		f["starts_at"] = formatLocal(&e.StartsAt, loc)
		f["ends_at"] = formatLocal(&e.EndsAt, loc)
	}
	return f
}

// parseEvent reads the details form. Times are wall-clock times in the event's
// own zone — what an organiser means by "7pm" — so the zone is read first and
// every time parsed in it.
func parseEvent(r *http.Request) (events.Event, formValues, validate.FormErrors) {
	f := valuesOf(r)
	errs := validate.FormErrors{}

	tz := f["timezone"]
	if tz == "" {
		tz = events.DefaultTimezone
		f["timezone"] = tz
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		// Reported by validate.Event; parse in the default meanwhile so every
		// other field still gets checked in this one round trip.
		loc, _ = time.LoadLocation(events.DefaultTimezone)
	}

	e := events.Event{
		Title: f["title"], Slug: f["slug"], Summary: f["summary"],
		// The description keeps its inner whitespace: paragraphs are blank lines.
		Description: strings.TrimSpace(r.PostFormValue("description")),
		Venue:       f["venue"], Address: strings.TrimSpace(r.PostFormValue("address")),
		Timezone: tz, Listed: f.On("listed"),
		PayLater:             f.On("pay_later"),
		PayLaterInstructions: strings.TrimSpace(r.PostFormValue("pay_later_instructions")),
	}
	f["pay_later_instructions"] = e.PayLaterInstructions
	if e.Slug == "" {
		e.Slug = events.Slugify(e.Title)
		f["slug"] = e.Slug
	}

	for _, field := range []struct {
		name string
		dst  *time.Time
	}{{"starts_at", &e.StartsAt}, {"ends_at", &e.EndsAt}} {
		t, ok := parseLocal(f[field.name], loc)
		switch {
		case !ok:
			errs.Add(field.name, "Use the date and time picker.")
		case t == nil:
			errs.Add(field.name, "Required.")
		default:
			*field.dst = *t
		}
	}
	var ok bool
	if e.RegistrationOpensAt, ok = parseLocal(f["registration_opens_at"], loc); !ok {
		errs.Add("registration_opens_at", "Use the date and time picker.")
	}
	if e.RegistrationClosesAt, ok = parseLocal(f["registration_closes_at"], loc); !ok {
		errs.Add("registration_closes_at", "Use the date and time picker.")
	}
	if e.Capacity, ok = parseCount(f["capacity"]); !ok {
		errs.Add("capacity", "A whole number, or blank for no limit.")
	}

	for k, v := range validate.Event(e) {
		errs.Add(k, v)
	}
	return e, f, errs
}
