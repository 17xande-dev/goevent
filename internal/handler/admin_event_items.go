package handler

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/validate"
)

// Ticket types and questions: the two lists an event page carries, each edited
// on a page of its own so a rejected form comes back with its own messages and
// nothing else's.
//
// Removing either deletes it when nothing refers to it and archives it when
// something does — a ticket somebody holds, a question somebody answered. The
// store decides which by trying; the page says which happened.

type ticketFormPage struct {
	page
	Event    events.Event
	IsNew    bool
	TicketID string
	Form     formValues
	Errors   validate.FormErrors
}

type questionFormPage struct {
	page
	Event       events.Event
	IsNew       bool
	QuestionID  string
	Form        formValues
	Errors      validate.FormErrors
	TicketTypes []events.TicketType
	// Limited is the ticket types ticked on the form.
	Limited []string
	Scopes  []events.Scope
	Kinds   []events.Kind
}

// Ticked reports whether a ticket type's box is ticked.
func (p questionFormPage) Ticked(id string) bool { return slices.Contains(p.Limited, id) }

func (h *Handler) adminTicketNew(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	form := ticketValues(e, events.TicketType{MaxPerRegistration: 10})
	// Blank, not "0.00": a free ticket has to be typed, or every ticket an
	// organiser forgot to price would quietly be free.
	form["price"] = ""
	h.render(w, r, http.StatusOK, "admin_ticket_form", ticketFormPage{
		page: h.newPage(r, "New ticket type"), Event: e, IsNew: true, Form: form,
	})
}

func (h *Handler) adminTicketCreate(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return
	}
	t, form, errs := parseTicket(r, e)
	if errs.Any() {
		h.render(w, r, http.StatusUnprocessableEntity, "admin_ticket_form", ticketFormPage{
			page: h.newPage(r, "New ticket type"), Event: e, IsNew: true, Form: form, Errors: errs,
		})
		return
	}
	if _, err := h.events.CreateTicketType(r.Context(), t); err != nil {
		h.storeError(w, r, err)
		return
	}
	http.Redirect(w, r, eventPath(e.ID)+"?notice=ticket_saved#tickets", http.StatusSeeOther)
}

func (h *Handler) adminTicketEdit(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	t, err := h.events.TicketType(r.Context(), e.ID, r.PathValue("ticketID"))
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "admin_ticket_form", ticketFormPage{
		page: h.newPage(r, t.Name), Event: e, TicketID: t.ID, Form: ticketValues(e, t),
	})
}

func (h *Handler) adminTicketUpdate(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	stored, err := h.events.TicketType(r.Context(), e.ID, r.PathValue("ticketID"))
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return
	}
	t, form, errs := parseTicket(r, e)
	t.ID = stored.ID
	if errs.Any() {
		h.render(w, r, http.StatusUnprocessableEntity, "admin_ticket_form", ticketFormPage{
			page: h.newPage(r, stored.Name), Event: e, TicketID: stored.ID, Form: form, Errors: errs,
		})
		return
	}
	if _, err := h.events.UpdateTicketType(r.Context(), t); err != nil {
		h.storeError(w, r, err)
		return
	}
	http.Redirect(w, r, eventPath(e.ID)+"?notice=ticket_saved#tickets", http.StatusSeeOther)
}

func (h *Handler) adminTicketDelete(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	archived, err := h.events.RemoveTicketType(r.Context(), e.ID, r.PathValue("ticketID"))
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	notice := "ticket_deleted"
	if archived {
		notice = "ticket_archived"
	}
	http.Redirect(w, r, eventPath(e.ID)+"?notice="+notice+"#tickets", http.StatusSeeOther)
}

func (h *Handler) adminQuestionNew(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	q := events.Question{Scope: events.ScopeAttendee, Kind: events.KindText}
	h.renderQuestion(w, r, http.StatusOK, e, "", questionValues(q), q.TicketTypeIDs, nil)
}

func (h *Handler) adminQuestionCreate(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return
	}
	tts, err := h.events.TicketTypes(r.Context(), e.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	q, form, errs := parseQuestion(r, e, tts)
	if errs.Any() {
		h.renderQuestion(w, r, http.StatusUnprocessableEntity, e, "", form, q.TicketTypeIDs, errs)
		return
	}
	if _, err := h.events.CreateQuestion(r.Context(), q); err != nil {
		h.storeError(w, r, err)
		return
	}
	http.Redirect(w, r, eventPath(e.ID)+"?notice=question_saved#questions", http.StatusSeeOther)
}

func (h *Handler) adminQuestionEdit(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	q, err := h.events.Question(r.Context(), e.ID, r.PathValue("questionID"))
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	h.renderQuestion(w, r, http.StatusOK, e, q.ID, questionValues(q), q.TicketTypeIDs, nil)
}

func (h *Handler) adminQuestionUpdate(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	stored, err := h.events.Question(r.Context(), e.ID, r.PathValue("questionID"))
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return
	}
	tts, err := h.events.TicketTypes(r.Context(), e.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	q, form, errs := parseQuestion(r, e, tts)
	q.ID = stored.ID
	if errs.Any() {
		h.renderQuestion(w, r, http.StatusUnprocessableEntity, e, stored.ID, form, q.TicketTypeIDs, errs)
		return
	}
	if _, err := h.events.UpdateQuestion(r.Context(), q); err != nil {
		h.storeError(w, r, err)
		return
	}
	http.Redirect(w, r, eventPath(e.ID)+"?notice=question_saved#questions", http.StatusSeeOther)
}

func (h *Handler) adminQuestionDelete(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	archived, err := h.events.RemoveQuestion(r.Context(), e.ID, r.PathValue("questionID"))
	if err != nil {
		h.storeError(w, r, err)
		return
	}
	notice := "question_deleted"
	if archived {
		notice = "question_archived"
	}
	http.Redirect(w, r, eventPath(e.ID)+"?notice="+notice+"#questions", http.StatusSeeOther)
}

func (h *Handler) renderQuestion(w http.ResponseWriter, r *http.Request, status int, e events.Event, id string, form formValues, limited []string, errs validate.FormErrors) {
	tts, err := h.events.TicketTypes(r.Context(), e.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	title := "New question"
	if id != "" {
		title = form.Get("label")
	}
	h.render(w, r, status, "admin_question_form", questionFormPage{
		page: h.newPage(r, title), Event: e, IsNew: id == "", QuestionID: id,
		Form: form, Errors: errs, TicketTypes: tts, Limited: limited,
		Scopes: events.Scopes, Kinds: events.Kinds,
	})
}

func ticketValues(e events.Event, t events.TicketType) formValues {
	loc := e.Location()
	return formValues{
		"name": t.Name, "description": t.Description,
		"price":                events.FormatPrice(t.PriceCents),
		"capacity":             formatCount(t.Capacity),
		"max_per_registration": strconv.Itoa(t.MaxPerRegistration),
		"sales_start_at":       formatLocal(t.SalesStartAt, loc),
		"sales_end_at":         formatLocal(t.SalesEndAt, loc),
		"hidden":               checkbox(t.Hidden),
		"archived":             checkbox(t.Archived),
		"position":             strconv.Itoa(t.Position),
	}
}

// parseTicket reads the ticket-type form. The price is required rather than
// blank-means-free: "0" is a decision, an empty box is not.
func parseTicket(r *http.Request, e events.Event) (events.TicketType, formValues, validate.FormErrors) {
	f := valuesOf(r)
	errs := validate.FormErrors{}
	loc := e.Location()

	t := events.TicketType{
		EventID: e.ID, Name: f["name"], Description: strings.TrimSpace(r.PostFormValue("description")),
		Hidden: f.On("hidden"), Archived: f.On("archived"),
	}
	if f["price"] == "" {
		errs.Add("price", "Required. Use 0 for a free ticket.")
	} else if cents, err := events.ParsePrice(f["price"]); err != nil {
		errs.Add("price", "An amount like 150 or 150.00.")
	} else {
		t.PriceCents = cents
	}
	var ok bool
	if t.Capacity, ok = parseCount(f["capacity"]); !ok {
		errs.Add("capacity", "A whole number, or blank for no limit.")
	}
	if n, err := strconv.Atoi(f["max_per_registration"]); err != nil {
		errs.Add("max_per_registration", "A whole number.")
	} else {
		t.MaxPerRegistration = n
	}
	if t.SalesStartAt, ok = parseLocal(f["sales_start_at"], loc); !ok {
		errs.Add("sales_start_at", "Use the date and time picker.")
	}
	if t.SalesEndAt, ok = parseLocal(f["sales_end_at"], loc); !ok {
		errs.Add("sales_end_at", "Use the date and time picker.")
	}
	t.Position = position(f, errs)

	for k, v := range validate.TicketType(t) {
		errs.Add(k, v)
	}
	return t, f, errs
}

func questionValues(q events.Question) formValues {
	return formValues{
		"scope": string(q.Scope), "kind": string(q.Kind), "label": q.Label, "help": q.Help,
		"options":  strings.Join(q.Options, "\n"),
		"required": checkbox(q.Required), "archived": checkbox(q.Archived),
		"position": strconv.Itoa(q.Position),
	}
}

// parseQuestion reads the question form. Options are one per line. The ticket
// types ticked must be this event's: an id from anywhere else is a hand-made
// request, refused rather than stored as a limit that matches nothing.
func parseQuestion(r *http.Request, e events.Event, tts []events.TicketType) (events.Question, formValues, validate.FormErrors) {
	f := valuesOf(r)
	f["options"] = strings.TrimSpace(r.PostFormValue("options"))
	errs := validate.FormErrors{}

	q := events.Question{
		EventID: e.ID, Scope: events.Scope(f["scope"]), Kind: events.Kind(f["kind"]),
		Label: f["label"], Help: f["help"], Required: f.On("required"), Archived: f.On("archived"),
	}
	for line := range strings.SplitSeq(f["options"], "\n") {
		if line = strings.TrimSpace(line); line != "" && !slices.Contains(q.Options, line) {
			q.Options = append(q.Options, line)
		}
	}
	for _, id := range r.PostForm["ticket_type"] {
		if !slices.ContainsFunc(tts, func(t events.TicketType) bool { return t.ID == id }) {
			errs.Add("ticket_type", "That ticket type is not one of this event's.")
			continue
		}
		q.TicketTypeIDs = append(q.TicketTypeIDs, id)
	}
	q.Position = position(f, errs)

	for k, v := range validate.Question(q) {
		errs.Add(k, v)
	}
	return q, f, errs
}

// position reads the optional ordering field; blank is 0.
func position(f formValues, errs validate.FormErrors) int {
	if f["position"] == "" {
		return 0
	}
	n, err := strconv.Atoi(f["position"])
	if err != nil {
		errs.Add("position", "A whole number.")
	}
	return n
}
