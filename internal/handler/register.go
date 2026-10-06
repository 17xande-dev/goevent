package handler

import (
	"crypto/rand"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/payment"
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/17xande-dev/goevent/internal/validate"
)

// The registration form: who is coming, the questions, the person registering
// and how they will pay. It is reached from the event page's quantities, which
// arrive in the query string as qty.<ticket type id>.
//
// The form carries its attendees as numbered blocks (a0., a1., …), each with a
// hidden ticket type, so the POST rebuilds exactly the slots the GET drew
// rather than trusting a quantity a second time. Prices never travel in the
// form: the checkout reads them from the database under its lock.

// registerProblems are the refusals the event page explains when the form
// sends somebody back.
var registerProblems = map[string]string{
	"choose": "Choose how many tickets you would like first.",
	"closed": "Registration is not open for this event.",
}

// regCookie carries a registration's manage token from the checkout to the
// gateway's return page, so that page — reached by a payment id the gateway
// also knows — links to the registration only for the browser that made it.
const regCookie = "goevent_reg"

func (h *Handler) registerRegistration(mux *http.ServeMux) {
	mux.HandleFunc("GET "+RegisterPath, h.registerForm)
	// Rate limited: each submission takes the event's lock and may create a
	// payment, and a script hammering it could hold seats nobody means to buy.
	mux.Handle("POST "+RegisterPath, h.limits.checkout(http.HandlerFunc(h.registerSubmit)))
}

// slot is one attendee block on the form.
type slot struct {
	Index     int
	Ticket    events.TicketType
	Number    int // 1-based within its ticket type, for "Adult 2"
	Questions []events.Question
}

// Prefix is the slot's field-name prefix.
func (s slot) Prefix() string { return "a" + strconv.Itoa(s.Index) + "." }

// method is one way to pay offered on the form.
type method struct {
	Name, Label string
}

type registerPage struct {
	page
	Event     events.Event
	Slots     []slot
	Questions []events.Question // asked once, of the person registering
	Methods   []method
	Total     int64
	// Lines summarises the order: "2 × Adult".
	Lines []string
	Form  formValues
	// Errors are per field; Problem is about the whole order — sold out, closed.
	Errors  validate.FormErrors
	Problem string
	// Blocked is an order this form cannot take at all: it costs money and there
	// is no way to pay. The form is shown, without its button, so the person
	// sees why.
	Blocked bool
}

// Field is a slot's field name, for the template.
func (p registerPage) Field(s slot, name string) string { return s.Prefix() + name }

func (h *Handler) registerForm(w http.ResponseWriter, r *http.Request) {
	e, ok := h.publicEventBySlug(w, r)
	if !ok {
		return
	}
	data, ok := h.registerData(w, r, e, func(t events.TicketType) int {
		n, _ := strconv.Atoi(r.URL.Query().Get("qty." + t.ID))
		return n
	})
	if !ok {
		return
	}
	data.Form = formValues{"checkout_key": rand.Text()}
	if len(data.Methods) > 0 {
		data.Form["method"] = data.Methods[0].Name
	}
	h.render(w, r, http.StatusOK, "register", data)
}

// registerData builds the form for the quantities count reports, sending the
// browser back to the event page when there is nothing to register for.
func (h *Handler) registerData(w http.ResponseWriter, r *http.Request, e events.Event, count func(events.TicketType) int) (registerPage, bool) {
	now := time.Now()
	if !e.RegistrationOpen(now) {
		http.Redirect(w, r, "/events/"+e.Slug+"?problem=closed", http.StatusSeeOther)
		return registerPage{}, false
	}
	tts, err := h.events.TicketTypes(r.Context(), e.ID)
	if err != nil {
		h.serverError(w, r, err)
		return registerPage{}, false
	}
	qs, err := h.events.Questions(r.Context(), e.ID)
	if err != nil {
		h.serverError(w, r, err)
		return registerPage{}, false
	}

	data := registerPage{page: h.newPage(r, "Register for "+e.Title), Event: e, Errors: validate.FormErrors{}}
	for _, t := range tts {
		if !t.OnSale(now) {
			continue
		}
		// Clamped to what one registration may take; whether there is room is
		// the checkout's question, asked under the lock.
		n := min(max(count(t), 0), t.MaxPerRegistration)
		if n == 0 {
			continue
		}
		data.Lines = append(data.Lines, strconv.Itoa(n)+" × "+t.Name)
		data.Total += int64(n) * t.PriceCents
		for i := range n {
			s := slot{Index: len(data.Slots), Ticket: t, Number: i + 1}
			for _, q := range qs {
				if !q.Archived && q.AppliesTo(t.ID) {
					s.Questions = append(s.Questions, q)
				}
			}
			data.Slots = append(data.Slots, s)
		}
	}
	if len(data.Slots) == 0 {
		http.Redirect(w, r, "/events/"+e.Slug+"?problem=choose", http.StatusSeeOther)
		return registerPage{}, false
	}
	for _, q := range qs {
		if !q.Archived && q.Scope == events.ScopeRegistration {
			data.Questions = append(data.Questions, q)
		}
	}
	if data.Total > 0 {
		for _, g := range h.gateways.All() {
			data.Methods = append(data.Methods, method{Name: g.Name(), Label: "Pay now with " + g.Label()})
		}
		if e.PayLater {
			data.Methods = append(data.Methods, method{Name: string(registrations.MethodLater), Label: "Pay later by EFT or cash"})
		}
		if len(data.Methods) == 0 {
			data.Blocked = true
			data.Problem = "Online payment is not available for this event at the moment. Please contact the organisers."
		}
	}
	return data, true
}

func (h *Handler) registerSubmit(w http.ResponseWriter, r *http.Request) {
	e, ok := h.publicEventBySlug(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		h.badForm(w, r)
		return
	}
	// The slots the GET drew, rebuilt from their hidden ticket types.
	counts := map[string]int{}
	for i := 0; ; i++ {
		tt := r.PostFormValue("a" + strconv.Itoa(i) + ".tt")
		if tt == "" {
			break
		}
		counts[tt]++
	}
	data, ok := h.registerData(w, r, e, func(t events.TicketType) int { return counts[t.ID] })
	if !ok {
		return
	}
	data.Form = valuesOf(r)
	order, errs := parseRegistration(r, data)
	order.Currency = h.cfg.Currency
	data.Errors = errs
	if data.Blocked || errs.Any() {
		h.render(w, r, http.StatusUnprocessableEntity, "register", data)
		return
	}

	res, err := h.regs.CheckoutWith(r.Context(), order, time.Now(), h.hooks())
	if err != nil {
		data.Problem = checkoutProblem(err)
		if data.Problem == "" {
			h.serverError(w, r, err)
			return
		}
		status := http.StatusConflict
		if errors.Is(err, registrations.ErrNoMethod) || errors.Is(err, registrations.ErrPayLater) {
			status = http.StatusUnprocessableEntity
		}
		h.render(w, r, status, "register", data)
		return
	}
	reg := res.Registration
	if !res.Reused {
		h.logger(r).Info("registration made", "registration", reg.ID, "reference", reg.Reference,
			"event", e.ID, "attendees", len(res.Attendees), "total_cents", reg.TotalCents, "status", reg.Status)
	}

	token := h.signer.ManageToken(reg.ID)
	http.SetCookie(w, &http.Cookie{
		Name: regCookie, Value: reg.ID + "." + token, Path: "/",
		MaxAge: 24 * 60 * 60, HttpOnly: true, Secure: h.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
	})

	if res.Payment == nil {
		// Free and confirmed, paying later, or a resubmission with nothing left to
		// pay: the registration's own page says which.
		http.Redirect(w, r, h.managePath(reg), http.StatusSeeOther)
		return
	}
	h.handover(w, r, e, reg, *res.Payment)
}

// parseRegistration reads the form into an order, checking every field.
func parseRegistration(r *http.Request, data registerPage) (registrations.Order, validate.FormErrors) {
	f := data.Form
	errs := validate.FormErrors{}
	o := registrations.Order{
		EventID: data.Event.ID, CheckoutKey: f["checkout_key"],
		Contact: registrations.Contact{
			FirstName: f["contact_first"], LastName: f["contact_last"],
			Email: validate.NormalizeEmail(f["contact_email"]), Phone: f["contact_phone"],
		},
	}
	if o.CheckoutKey == "" {
		errs.Add("form", "This form is incomplete; reload the page and try again.")
	}
	person(errs, "contact_", o.Contact.FirstName, o.Contact.LastName)
	validate.Email(errs, "contact_email", o.Contact.Email, true)
	validate.Phone(errs, "contact_phone", o.Contact.Phone)

	for _, s := range data.Slots {
		a := registrations.AttendeeOrder{
			TicketTypeID: s.Ticket.ID,
			FirstName:    f[s.Prefix()+"first"], LastName: f[s.Prefix()+"last"],
			Email: validate.NormalizeEmail(f[s.Prefix()+"email"]),
		}
		person(errs, s.Prefix(), a.FirstName, a.LastName)
		validate.Email(errs, s.Prefix()+"email", a.Email, false)
		a.Answers = answers(r, errs, s.Prefix(), s.Questions)
		o.Attendees = append(o.Attendees, a)
	}
	o.Answers = answers(r, errs, "r.", data.Questions)

	if data.Total > 0 {
		m := f["method"]
		if !slices.ContainsFunc(data.Methods, func(x method) bool { return x.Name == m }) {
			errs.Add("method", "Choose how to pay.")
		}
		o.Method = registrations.Method(m)
	}
	return o, errs
}

func person(errs validate.FormErrors, prefix, first, last string) {
	if first == "" {
		errs.Add(prefix+"first", "Required.")
	}
	if last == "" {
		errs.Add(prefix+"last", "Required.")
	}
	validate.MaxLen(errs, prefix+"first", first, 100)
	validate.MaxLen(errs, prefix+"last", last, 100)
}

// answers reads and checks one set of questions' answers, keyed prefix+"q."+id.
func answers(r *http.Request, errs validate.FormErrors, prefix string, qs []events.Question) []registrations.Answer {
	var out []registrations.Answer
	for _, q := range qs {
		field := prefix + "q." + q.ID
		v := strings.TrimSpace(r.PostFormValue(field))
		if msg := validate.Answer(q, v); msg != "" {
			errs.Add(field, msg)
			continue
		}
		if q.Kind == events.KindCheckbox {
			v = map[bool]string{true: "Yes", false: "No"}[v == "1"]
		}
		if v == "" {
			continue
		}
		out = append(out, registrations.Answer{QuestionID: q.ID, Label: q.Label, Value: v})
	}
	return out
}

// checkoutProblem is what to tell somebody whose order the checkout refused,
// or "" for a fault that is not theirs.
func checkoutProblem(err error) string {
	var sold *registrations.SoldOutError
	var unavail *registrations.UnavailableError
	switch {
	case errors.As(err, &sold) && sold.Event:
		return "Sorry — the event does not have room for that many any more. Try fewer tickets."
	case errors.As(err, &sold):
		return "Sorry — there are not enough " + strings.Join(sold.Tickets, ", ") + " tickets left for that. Try fewer."
	case errors.As(err, &unavail):
		return "Sorry — " + unavail.Ticket + " " + unavail.Reason + "."
	case errors.Is(err, registrations.ErrClosed):
		return "Registration for this event has closed."
	case errors.Is(err, registrations.ErrNoMethod), errors.Is(err, registrations.ErrPayLater):
		return "Choose how to pay."
	default:
		return ""
	}
}

type handoverPage struct {
	page
	Event        events.Event
	Registration registrations.Registration
	Payment      registrations.Payment
	Handover     payment.Handover
	GatewayLabel string
}

// handover sends the registrant to the gateway: a form posted for them, or a
// link and QR code they act on themselves.
func (h *Handler) handover(w http.ResponseWriter, r *http.Request, e events.Event, reg registrations.Registration, p registrations.Payment) {
	g, err := h.gateways.Lookup(p.Method)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	ho, err := g.Handover(payment.Request{
		PaymentID: p.ID, AmountCents: p.AmountCents, Currency: p.Currency,
		ItemName:  e.Title + " " + reg.Reference,
		NameFirst: reg.ContactFirst, NameLast: reg.ContactLast, Email: reg.ContactEmail,
	})
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	// The page holds a live payment link; nothing between here and the browser
	// should keep it.
	w.Header().Set("Cache-Control", "no-store")
	h.render(w, r, http.StatusOK, "checkout_redirect", handoverPage{
		page: h.newPage(r, "Pay for "+e.Title), Event: e, Registration: reg, Payment: p,
		Handover: ho, GatewayLabel: g.Label(),
	})
}

// managePath is a registration's own page, with its token.
func (h *Handler) managePath(reg registrations.Registration) string {
	return "/r/" + reg.Reference + "?t=" + url.QueryEscape(h.signer.ManageToken(reg.ID))
}
