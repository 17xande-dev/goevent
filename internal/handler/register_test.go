package handler

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/payment"
	"github.com/17xande-dev/goevent/internal/registrations"
)

// published makes a published event a week out with a paid ticket type capped
// at adultCap and a free one, through the store rather than the admin forms —
// these tests are about registering, not about setting events up.
func published(t *testing.T, s *app, adultCap int, edit ...func(*events.Event)) (events.Event, events.TicketType, events.TicketType) {
	t.Helper()
	start := time.Now().Add(7 * 24 * time.Hour).Truncate(time.Minute)
	e := events.Event{Slug: "family-camp", Title: "Family Camp", StartsAt: start, EndsAt: start.Add(48 * time.Hour),
		Timezone: events.DefaultTimezone, Listed: true, Venue: "Camp Oakwood"}
	for _, f := range edit {
		f(&e)
	}
	e, err := s.events.Create(t.Context(), e)
	if err != nil {
		t.Fatal(err)
	}
	adult, err := s.events.CreateTicketType(t.Context(), events.TicketType{EventID: e.ID, Name: "Adult",
		PriceCents: 45000, Capacity: &adultCap, MaxPerRegistration: 10})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.events.CreateTicketType(t.Context(), events.TicketType{EventID: e.ID, Name: "Child",
		MaxPerRegistration: 10, Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	if e, err = s.events.Transition(t.Context(), e.ID, events.StatusDraft, events.StatusPublished); err != nil {
		t.Fatal(err)
	}
	return e, adult, child
}

// registerForm opens the registration form for some quantities and returns its
// fields filled in as a careful person would, ready to post.
func registerForm(t *testing.T, s *app, e events.Event, qty map[string]int) url.Values {
	t.Helper()
	q := url.Values{}
	for id, n := range qty {
		q.Set("qty."+id, strconv.Itoa(n))
	}
	res, body := get(t, s.srv, "/events/"+e.Slug+"/register?"+q.Encode())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET register = %d %s", res.StatusCode, excerpt(body))
	}
	form := url.Values{
		"checkout_key":  {between(t, body, `name="checkout_key" value="`, `"`)},
		"contact_first": {"Ada"}, "contact_last": {"Lovelace"}, "contact_email": {"ada@example.com"},
	}
	// Every attendee block the page drew, with its hidden ticket type.
	for _, m := range regexp.MustCompile(`name="(a\d+)\.tt" value="([^"]+)"`).FindAllStringSubmatch(body, -1) {
		form.Set(m[1]+".tt", m[2])
		form.Set(m[1]+".first", "Person "+m[1])
		form.Set(m[1]+".last", "Lovelace")
	}
	if m := regexp.MustCompile(`name="method" value="([^"]+)"`).FindStringSubmatch(body); m != nil {
		form.Set("method", m[1])
	}
	return form
}

func submit(t *testing.T, s *app, e events.Event, form url.Values) (*http.Response, string) {
	t.Helper()
	return post(t, s.srv, "/events/"+e.Slug+"/register", form)
}

func onlyRegistration(t *testing.T, s *app) registrations.Registration {
	t.Helper()
	var id string
	if err := s.pool.QueryRow(t.Context(), `SELECT id FROM registrations`).Scan(&id); err != nil {
		t.Fatalf("expected exactly one registration: %v", err)
	}
	r, err := s.regs.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestPublic_ListShowsWhatIsPublic(t *testing.T) {
	s := newApp(t)
	e, _, _ := published(t, s, 10)
	draft, err := s.events.Create(t.Context(), events.Event{Slug: "secret", Title: "Secret planning",
		StartsAt: e.StartsAt, EndsAt: e.EndsAt, Timezone: events.DefaultTimezone, Listed: true})
	if err != nil {
		t.Fatal(err)
	}

	_, body := get(t, s.srv, "/events")
	if !strings.Contains(body, "Family Camp") || strings.Contains(body, "Secret planning") {
		t.Errorf("the list shows the wrong events:\n%s", excerpt(body))
	}
	// A draft is a 404 by its address too, not a page that says "draft".
	if res, _ := get(t, s.srv, "/events/"+draft.Slug); res.StatusCode != http.StatusNotFound {
		t.Errorf("GET a draft = %d, want 404", res.StatusCode)
	}
	if res, _ := get(t, s.srv, "/"); res.Header.Get("Location") != "/events" {
		t.Errorf("/ goes to %q", res.Header.Get("Location"))
	}
	res, page := get(t, s.srv, "/events/"+e.Slug)
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "ZAR 450.00") || !strings.Contains(page, "Camp Oakwood") {
		t.Errorf("event page = %d:\n%s", res.StatusCode, excerpt(page))
	}
	// Reading an event sets no cookie: only the form that changes things does.
	if len(res.Cookies()) != 0 {
		t.Errorf("the event page set cookies: %v", res.Cookies())
	}
}

func TestRegister_FreeIsConfirmedAndShownOnItsOwnPage(t *testing.T) {
	s := newApp(t)
	e, _, child := published(t, s, 10)

	res, body := submit(t, s, e, registerForm(t, s, e, map[string]int{child.ID: 2}))
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("register = %d %s", res.StatusCode, excerpt(body))
	}
	reg := onlyRegistration(t, s)
	if reg.Status != registrations.StatusConfirmed || reg.ContactEmail != "ada@example.com" {
		t.Errorf("registration = %+v", reg)
	}
	loc := res.Header.Get("Location")
	if !strings.HasPrefix(loc, "/r/"+reg.Reference+"?t=") {
		t.Fatalf("redirected to %q, want the registration's page", loc)
	}
	res, page := get(t, s.srv, loc)
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "Person a0 Lovelace") || !strings.Contains(page, "Confirmed") {
		t.Errorf("registration page = %d:\n%s", res.StatusCode, excerpt(page))
	}
	if res.Header.Get("Cache-Control") != "private, no-store" {
		t.Errorf("Cache-Control = %q on a private page", res.Header.Get("Cache-Control"))
	}
}

func TestManage_RefusesAWrongToken(t *testing.T) {
	s := newApp(t)
	e, _, child := published(t, s, 10)
	submit(t, s, e, registerForm(t, s, e, map[string]int{child.ID: 1}))
	reg := onlyRegistration(t, s)

	for _, path := range []string{
		"/r/" + reg.Reference,
		"/r/" + reg.Reference + "?t=wrong",
		"/r/" + reg.Reference + "?t=" + s.signer.ManageToken("another-id"),
		"/r/AAA-AAA?t=" + s.signer.ManageToken(reg.ID),
	} {
		if res, _ := get(t, s.srv, path); res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, res.StatusCode)
		}
	}
}

func TestRegister_PaidHandsOverAndTheCallbackConfirms(t *testing.T) {
	s := newApp(t)
	e, adult, child := published(t, s, 10)

	res, body := submit(t, s, e, registerForm(t, s, e, map[string]int{adult.ID: 2, child.ID: 1}))
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `action="https://gateway.example/pay"`) {
		t.Fatalf("paid register = %d, want the hand-over:\n%s", res.StatusCode, excerpt(body))
	}
	if res.Header.Get("Cache-Control") != "no-store" {
		t.Error("the hand-over page may be cached")
	}
	reg := onlyRegistration(t, s)
	if reg.Status != registrations.StatusPending || reg.TotalCents != 90000 || reg.Currency != "ZAR" {
		t.Fatalf("registration = %+v", reg)
	}
	reqs := s.gateway.Requests()
	if len(reqs) != 1 || reqs[0].AmountCents != 90000 || reqs[0].Currency != "ZAR" ||
		!strings.Contains(reqs[0].ItemName, reg.Reference) {
		t.Fatalf("gateway requests = %+v", reqs)
	}
	paymentID := reqs[0].PaymentID

	// The gateway's notification: outside CSRF, with no token, setting no cookie.
	cb := callback(t, s, paymentID, "COMPLETE", 90000)
	if cb.StatusCode != http.StatusOK || len(cb.Cookies()) != 0 {
		t.Fatalf("callback = %d, cookies %v", cb.StatusCode, cb.Cookies())
	}
	if got, _ := s.regs.Get(t.Context(), reg.ID); got.Status != registrations.StatusConfirmed {
		t.Fatalf("after payment: %q", got.Status)
	}

	// The browser that registered is offered its registration on the return page…
	_, ret := get(t, s.srv, "/checkout/success?payment="+paymentID)
	if !strings.Contains(ret, "Payment received") || !strings.Contains(ret, "/r/"+reg.Reference+"?t=") ||
		!strings.Contains(ret, reg.ContactEmail) {
		t.Errorf("return page for the registrant:\n%s", excerpt(ret))
	}
	// …and anybody else holding the payment id — the gateway knows it — is not.
	ret = stranger(t, s.srv.URL+"/checkout/success?payment="+paymentID)
	if !strings.Contains(ret, "Payment received") || strings.Contains(ret, "/r/") || strings.Contains(ret, reg.ContactEmail) {
		t.Errorf("return page for a stranger links the registration or shows its address:\n%s", excerpt(ret))
	}
}

// stranger fetches a page as a different browser: no cookies at all.
func stranger(t *testing.T, url string) string {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func callback(t *testing.T, s *app, paymentID, status string, cents int64) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.srv.URL+"/payments/fake/callback",
		bytes.NewReader(payment.FakeCallbackBody(paymentID, "gw-"+paymentID[:8], status, cents)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return res
}

func TestCallback_TheWrongAmountConfirmsNothing(t *testing.T) {
	s := newApp(t)
	e, adult, _ := published(t, s, 10)
	submit(t, s, e, registerForm(t, s, e, map[string]int{adult.ID: 1}))
	paymentID := s.gateway.Requests()[0].PaymentID

	if res := callback(t, s, paymentID, "COMPLETE", 100); res.StatusCode != http.StatusOK {
		t.Errorf("a mismatched callback = %d; it is acknowledged, not retried", res.StatusCode)
	}
	if got := onlyRegistration(t, s); got.Status != registrations.StatusPending {
		t.Errorf("a short payment confirmed the registration: %q", got.Status)
	}
	// An unknown payment is acknowledged too, and changes nothing.
	if res := callback(t, s, "3f2504e0-4f89-41d3-9a0c-0305e82c3301", "COMPLETE", 45000); res.StatusCode != http.StatusOK {
		t.Errorf("unknown payment callback = %d", res.StatusCode)
	}
}

func TestRegister_RejectedFormKeepsWhatWasTyped(t *testing.T) {
	s := newApp(t)
	e, _, child := published(t, s, 10)
	if _, err := s.events.CreateQuestion(t.Context(), events.Question{EventID: e.ID, Scope: events.ScopeAttendee,
		Kind: events.KindCheckbox, Label: "Photo consent", Required: true}); err != nil {
		t.Fatal(err)
	}

	form := registerForm(t, s, e, map[string]int{child.ID: 1})
	form.Set("a0.first", "")
	form.Set("contact_email", "not an address")
	form.Set("contact_phone", "082 123 4567")
	res, body := submit(t, s, e, form)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid form = %d", res.StatusCode)
	}
	for _, want := range []string{"Required.", "Does not look like an email address", "Please tick this", `value="082 123 4567"`, `value="Lovelace"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the rejected form is missing %q", want)
		}
	}
	var n int
	s.pool.QueryRow(t.Context(), `SELECT count(*) FROM registrations`).Scan(&n)
	if n != 0 {
		t.Errorf("a rejected form booked %d registrations", n)
	}
}

func TestRegister_SoldOutSaysSoAndBooksNothing(t *testing.T) {
	s := newApp(t)
	e, adult, _ := published(t, s, 1)
	form := registerForm(t, s, e, map[string]int{adult.ID: 1})

	// Somebody else takes the last seat while this form is open.
	other := registerForm(t, s, e, map[string]int{adult.ID: 1})
	if res, _ := submit(t, s, e, other); res.StatusCode != http.StatusOK {
		t.Fatalf("first registration = %d", res.StatusCode)
	}
	res, body := submit(t, s, e, form)
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "not enough Adult tickets left") {
		t.Errorf("the second = %d:\n%s", res.StatusCode, excerpt(body))
	}
}

func TestRegister_ResubmittingIsOneRegistration(t *testing.T) {
	s := newApp(t)
	e, _, child := published(t, s, 10)
	form := registerForm(t, s, e, map[string]int{child.ID: 1})
	first, _ := submit(t, s, e, form)
	again, _ := submit(t, s, e, form)
	if first.Header.Get("Location") != again.Header.Get("Location") {
		t.Errorf("a double submit went to %q then %q", first.Header.Get("Location"), again.Header.Get("Location"))
	}
	onlyRegistration(t, s)
}

func TestRegister_PayLaterHoldsAndExplains(t *testing.T) {
	s := newApp(t)
	e, adult, _ := published(t, s, 10, func(e *events.Event) {
		e.PayLater, e.PayLaterInstructions = true, "Bank: Example Bank\nAccount: 123456"
	})
	form := registerForm(t, s, e, map[string]int{adult.ID: 1})
	form.Set("method", string(registrations.MethodLater))
	res, body := submit(t, s, e, form)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("pay later = %d %s", res.StatusCode, excerpt(body))
	}
	_, page := get(t, s.srv, res.Header.Get("Location"))
	for _, want := range []string{"Awaiting payment", "Account: 123456"} {
		if !strings.Contains(page, want) {
			t.Errorf("the pay-later page is missing %q:\n%s", want, excerpt(page))
		}
	}
	if len(s.gateway.Requests()) != 0 {
		t.Error("a pay-later registration was handed to the gateway")
	}
}

func TestRegister_NothingChosenGoesBack(t *testing.T) {
	s := newApp(t)
	e, _, _ := published(t, s, 10)
	res, _ := get(t, s.srv, "/events/"+e.Slug+"/register")
	if res.Header.Get("Location") != "/events/"+e.Slug+"?problem=choose" {
		t.Errorf("no quantities went to %q", res.Header.Get("Location"))
	}
}

func TestCheckoutStatus_RedirectsHTMXOncePaid(t *testing.T) {
	s := newApp(t)
	e, adult, _ := published(t, s, 10)
	submit(t, s, e, registerForm(t, s, e, map[string]int{adult.ID: 1}))
	paymentID := s.gateway.Requests()[0].PaymentID

	req := newRequest(t, s.srv, http.MethodGet, "/checkout/status?payment="+paymentID, nil)
	req.Header.Set("HX-Request", "true")
	res, body := do(t, s.srv, req)
	if res.Header.Get("HX-Redirect") != "" || !strings.Contains(body, "Waiting") || strings.Contains(body, "<html") {
		t.Errorf("pending poll = %q %s", res.Header.Get("HX-Redirect"), excerpt(body))
	}
	callback(t, s, paymentID, "COMPLETE", 45000)
	req = newRequest(t, s.srv, http.MethodGet, "/checkout/status?payment="+paymentID, nil)
	req.Header.Set("HX-Request", "true")
	res, _ = do(t, s.srv, req)
	if res.Header.Get("HX-Redirect") != "/checkout/success?payment="+paymentID {
		t.Errorf("paid poll HX-Redirect = %q", res.Header.Get("HX-Redirect"))
	}
}
