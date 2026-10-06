package handler

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/goevent/internal/events"
)

// eventForm is a valid details submission.
func eventForm(title string) url.Values {
	return url.Values{
		"title":     {title},
		"starts_at": {"2026-11-07T09:00"},
		"ends_at":   {"2026-11-07T12:00"},
		"timezone":  {"Africa/Johannesburg"},
		"listed":    {"1"},
	}
}

// createEvent makes an event through the form and returns it as stored.
func createEvent(t *testing.T, s *app, title string) events.Event {
	t.Helper()
	res, body := post(t, s.srv, "/admin/events", eventForm(title))
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("create event = %d %s", res.StatusCode, excerpt(body))
	}
	id := strings.TrimPrefix(strings.Split(res.Header.Get("Location"), "?")[0], "/admin/events/")
	e, err := s.events.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("the created event %q is not stored: %v", id, err)
	}
	return e
}

func addTicket(t *testing.T, s *app, e events.Event, form url.Values) {
	t.Helper()
	res, body := post(t, s.srv, "/admin/events/"+e.ID+"/tickets", form)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("add ticket = %d %s", res.StatusCode, excerpt(body))
	}
}

func ticketForm(name, price string) url.Values {
	return url.Values{"name": {name}, "price": {price}, "max_per_registration": {"10"}}
}

func TestAdminEvents_CreateIsADraftWithTimesInItsOwnZone(t *testing.T) {
	s := setupApp(t)
	e := createEvent(t, s, "Men's Breakfast")

	if e.Status != events.StatusDraft {
		t.Errorf("Status = %q, want draft", e.Status)
	}
	if e.Slug != "men-s-breakfast" {
		t.Errorf("Slug = %q, want it derived from the title", e.Slug)
	}
	// 09:00 in Johannesburg is 07:00 UTC. Parsed in the server's zone instead,
	// every event would be off by however far the server is from the venue.
	if want := time.Date(2026, 11, 7, 7, 0, 0, 0, time.UTC); !e.StartsAt.Equal(want) {
		t.Errorf("StartsAt = %s, want %s", e.StartsAt.UTC(), want)
	}

	// And it reads back as the organiser typed it.
	_, body := get(t, s.srv, "/admin/events/"+e.ID)
	if !strings.Contains(body, `value="2026-11-07T09:00"`) {
		t.Errorf("the form does not show the start as typed:\n%s", excerpt(body))
	}
}

func TestAdminEvents_RejectedFormKeepsWhatWasTyped(t *testing.T) {
	s := setupApp(t)

	form := eventForm("")
	form.Set("ends_at", "2026-11-07T08:00") // before it starts
	form.Set("timezone", "Mars/Olympus_Mons")
	form.Set("capacity", "lots")
	form.Set("venue", "The hall")
	res, body := post(t, s.srv, "/admin/events", form)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("invalid event = %d, want 422", res.StatusCode)
	}
	for _, want := range []string{"Required.", "Ends before it starts.", "Not a time zone", "A whole number", `value="The hall"`, `value="lots"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the rejected form is missing %q", want)
		}
	}
	if list, _ := s.events.List(t.Context()); len(list) != 0 {
		t.Errorf("a rejected form stored %d events", len(list))
	}
}

func TestAdminEvents_DuplicateSlugIsAFieldError(t *testing.T) {
	s := setupApp(t)
	createEvent(t, s, "Camp")

	res, body := post(t, s.srv, "/admin/events", eventForm("Camp"))
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("duplicate slug = %d, want 422", res.StatusCode)
	}
	if !strings.Contains(body, "Another event already uses this address.") {
		t.Errorf("no message on the slug field:\n%s", excerpt(body))
	}
}

func TestAdminEvents_UpdateSavesDetailsOnly(t *testing.T) {
	s := setupApp(t)
	e := createEvent(t, s, "Camp")
	addTicket(t, s, e, ticketForm("Adult", "0"))
	post(t, s.srv, "/admin/events/"+e.ID+"/status", url.Values{"status": {"published"}})

	form := eventForm("Winter camp")
	form.Set("slug", "winter-camp")
	form.Set("capacity", "120")
	form.Set("status", "draft") // not a field this form has; must change nothing
	res, body := post(t, s.srv, "/admin/events/"+e.ID, form)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("update = %d %s", res.StatusCode, excerpt(body))
	}
	got, _ := s.events.Get(t.Context(), e.ID)
	if got.Title != "Winter camp" || got.Slug != "winter-camp" || got.Capacity == nil || *got.Capacity != 120 {
		t.Errorf("not saved: %+v", got)
	}
	if got.Status != events.StatusPublished {
		t.Errorf("saving details changed the status to %q", got.Status)
	}
}

func TestAdminEvents_PublishNeedsAnOfferedTicketType(t *testing.T) {
	s := setupApp(t)
	e := createEvent(t, s, "Camp")

	res, _ := post(t, s.srv, "/admin/events/"+e.ID+"/status", url.Values{"status": {"published"}})
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "problem=needs_ticket") {
		t.Errorf("publish with no tickets went to %q", loc)
	}
	// A hidden ticket type does not count: nobody could register.
	hidden := ticketForm("Staff", "0")
	hidden.Set("hidden", "1")
	addTicket(t, s, e, hidden)
	post(t, s.srv, "/admin/events/"+e.ID+"/status", url.Values{"status": {"published"}})
	if got, _ := s.events.Get(t.Context(), e.ID); got.Status != events.StatusDraft {
		t.Fatalf("published with only a hidden ticket type: %q", got.Status)
	}

	addTicket(t, s, e, ticketForm("Adult", "150"))
	res, _ = post(t, s.srv, "/admin/events/"+e.ID+"/status", url.Values{"status": {"published"}})
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "notice=published") {
		t.Errorf("publish went to %q", loc)
	}
	if got, _ := s.events.Get(t.Context(), e.ID); got.Status != events.StatusPublished {
		t.Errorf("Status = %q, want published", got.Status)
	}
}

func TestAdminEvents_StatusIsReadStrictly(t *testing.T) {
	s := setupApp(t)
	e := createEvent(t, s, "Camp")

	// No status at all must not mean any particular status.
	if res, _ := post(t, s.srv, "/admin/events/"+e.ID+"/status", url.Values{}); res.StatusCode != http.StatusBadRequest {
		t.Errorf("empty status = %d, want 400", res.StatusCode)
	}
	if res, _ := post(t, s.srv, "/admin/events/"+e.ID+"/status", url.Values{"status": {"live"}}); res.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown status = %d, want 400", res.StatusCode)
	}
	// A real status that is not a move from here: a draft cannot be closed.
	res, _ := post(t, s.srv, "/admin/events/"+e.ID+"/status", url.Values{"status": {"closed"}})
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "problem=status") {
		t.Errorf("draft → closed went to %q", loc)
	}
	if got, _ := s.events.Get(t.Context(), e.ID); got.Status != events.StatusDraft {
		t.Errorf("Status = %q after refused moves", got.Status)
	}
}

func TestAdminTickets_PriceIsCentsAndBlankIsNotFree(t *testing.T) {
	s := setupApp(t)
	e := createEvent(t, s, "Camp")

	// The form must not offer a price to submit unread.
	_, page := get(t, s.srv, "/admin/events/"+e.ID+"/tickets/new")
	if !strings.Contains(page, `name="price" inputmode="decimal" value=""`) {
		t.Errorf("the new-ticket form pre-fills a price:\n%s", excerpt(between(t, page, `id="price"`, ">")))
	}

	res, body := post(t, s.srv, "/admin/events/"+e.ID+"/tickets", ticketForm("Adult", ""))
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Use 0 for a free ticket") {
		t.Errorf("blank price = %d; a free ticket must be a decision", res.StatusCode)
	}

	addTicket(t, s, e, ticketForm("Adult", "1,250.50"))
	tts, _ := s.events.TicketTypes(t.Context(), e.ID)
	if len(tts) != 1 || tts[0].PriceCents != 125050 {
		t.Fatalf("ticket types = %+v, want one at 125050 cents", tts)
	}
	_, body = get(t, s.srv, "/admin/events/"+e.ID)
	if !strings.Contains(body, "ZAR 1250.50") {
		t.Errorf("the event page does not show the price:\n%s", excerpt(body))
	}
}

func TestAdminTickets_RemovingAHeldTypeArchivesIt(t *testing.T) {
	s := setupApp(t)
	e := createEvent(t, s, "Camp")
	addTicket(t, s, e, ticketForm("Adult", "100"))
	tts, _ := s.events.TicketTypes(t.Context(), e.ID)
	tt := tts[0]

	var regID string
	if err := s.pool.QueryRow(t.Context(), `
INSERT INTO registrations (event_id, reference, contact_first_name, contact_last_name, contact_email,
    total_cents, currency, hold_expires_at, checkout_key)
VALUES ($1, 'AAA-111', 'A', 'B', 'a@example.com', 100, 'ZAR', now(), 'k') RETURNING id`, e.ID).Scan(&regID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `
INSERT INTO attendees (registration_id, ticket_type_id, first_name, last_name, ticket_name, unit_price_cents)
VALUES ($1, $2, 'A', 'B', 'Adult', 100)`, regID, tt.ID); err != nil {
		t.Fatal(err)
	}

	res, _ := post(t, s.srv, "/admin/events/"+e.ID+"/tickets/"+tt.ID+"/delete", url.Values{})
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "notice=ticket_archived") {
		t.Errorf("remove went to %q", loc)
	}
	// And deleting the event is refused with an explanation, not a 500.
	res, _ = post(t, s.srv, "/admin/events/"+e.ID+"/delete", url.Values{})
	if loc := res.Header.Get("Location"); !strings.Contains(loc, "problem=in_use") {
		t.Errorf("delete with a registration went to %q", loc)
	}
	_, body := get(t, s.srv, "/admin/events/"+e.ID+"?problem=in_use")
	if !strings.Contains(body, "Cancel it instead") {
		t.Error("the in-use problem is not explained")
	}
}

func TestAdminTickets_AnotherEventsTicketIs404(t *testing.T) {
	s := setupApp(t)
	a := createEvent(t, s, "A")
	b := createEvent(t, s, "B")
	addTicket(t, s, b, ticketForm("Adult", "0"))
	tts, _ := s.events.TicketTypes(t.Context(), b.ID)

	if res, _ := get(t, s.srv, "/admin/events/"+a.ID+"/tickets/"+tts[0].ID); res.StatusCode != http.StatusNotFound {
		t.Errorf("B's ticket through A's URL = %d, want 404", res.StatusCode)
	}
}

func TestAdminQuestions_ValidatesOptionsAndOwnTicketTypes(t *testing.T) {
	s := setupApp(t)
	e := createEvent(t, s, "Camp")
	other := createEvent(t, s, "Other")
	addTicket(t, s, e, ticketForm("Adult", "0"))
	addTicket(t, s, other, ticketForm("Theirs", "0"))
	ours, _ := s.events.TicketTypes(t.Context(), e.ID)
	theirs, _ := s.events.TicketTypes(t.Context(), other.ID)

	form := url.Values{"label": {"T-shirt size"}, "scope": {"attendee"}, "kind": {"select"}, "options": {"M"}}
	res, body := post(t, s.srv, "/admin/events/"+e.ID+"/questions", form)
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "at least two options") {
		t.Errorf("one-option choice = %d", res.StatusCode)
	}

	form.Set("options", "S\nM\r\n\nL\nM")
	form["ticket_type"] = []string{theirs[0].ID}
	res, _ = post(t, s.srv, "/admin/events/"+e.ID+"/questions", form)
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("another event's ticket type = %d, want 422", res.StatusCode)
	}

	form["ticket_type"] = []string{ours[0].ID}
	if res, body := post(t, s.srv, "/admin/events/"+e.ID+"/questions", form); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("valid question = %d %s", res.StatusCode, excerpt(body))
	}
	qs, _ := s.events.Questions(t.Context(), e.ID)
	if len(qs) != 1 || !slices.Equal(qs[0].Options, []string{"S", "M", "L"}) || !slices.Equal(qs[0].TicketTypeIDs, []string{ours[0].ID}) {
		t.Errorf("stored question = %+v", qs)
	}
}

// uploadImage posts a file to the event's image form.
func uploadImage(t *testing.T, s *app, eventID string, content []byte) (*http.Response, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("csrf_token", csrfToken(t, s.srv))
	fw, err := mw.CreateFormFile("image", "picture.jpg")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write(content)
	mw.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.srv.URL+"/admin/events/"+eventID+"/image", &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", s.srv.URL)
	return do(t, s.srv, req)
}

func TestAdminEventImage_UploadReplaceAndRemove(t *testing.T) {
	s := setupApp(t)
	e := createEvent(t, s, "Camp")

	res, body := uploadImage(t, s, e.ID, testJPEG)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("upload = %d %s", res.StatusCode, excerpt(body))
	}
	first, _ := s.events.Get(t.Context(), e.ID)
	obj, ok := s.images.Get(first.ImageKey)
	if !ok || !bytes.Equal(obj.Body, testJPEG) || obj.ContentType != "image/jpeg" {
		t.Fatalf("stored object for %q: %v %q", first.ImageKey, ok, obj.ContentType)
	}
	if !strings.HasPrefix(first.ImageKey, "events/"+e.ID+"/") {
		t.Errorf("key %q is not under the event", first.ImageKey)
	}

	// A replacement is a new key, and the old object goes.
	uploadImage(t, s, e.ID, testJPEG)
	second, _ := s.events.Get(t.Context(), e.ID)
	if second.ImageKey == first.ImageKey {
		t.Error("a replaced image kept its key, so a CDN would keep serving the old one")
	}
	if !slices.Contains(s.images.Deleted(), first.ImageKey) {
		t.Error("the replaced object was not deleted")
	}

	post(t, s.srv, "/admin/events/"+e.ID+"/image/delete", url.Values{})
	if got, _ := s.events.Get(t.Context(), e.ID); got.ImageKey != "" {
		t.Errorf("ImageKey = %q after removal", got.ImageKey)
	}
}

func TestAdminEventImage_RefusesWhatIsNotAnImage(t *testing.T) {
	s := setupApp(t)
	e := createEvent(t, s, "Camp")

	res, body := uploadImage(t, s, e.ID, []byte("<html><script>alert(1)</script></html>"))
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("HTML upload = %d, want 422", res.StatusCode)
	}
	if !strings.Contains(body, "not an image this site can serve") {
		t.Errorf("no explanation:\n%s", excerpt(body))
	}
	if len(s.images.Keys()) != 0 {
		t.Error("a refused upload reached the bucket")
	}
}

func TestAdminEvents_ViewerSeesNoWriteControls(t *testing.T) {
	s := setupApp(t)
	e := createEvent(t, s, "Camp")
	addTicket(t, s, e, ticketForm("Adult", "0"))

	mustAccount(t, s, "viewer@example.com", testPassword, auth.RoleViewer)
	signInAs(t, s.srv, "viewer@example.com", testPassword)

	_, list := get(t, s.srv, "/admin/events")
	if strings.Contains(list, "New event") {
		t.Error("a viewer is offered New event")
	}
	if !strings.Contains(list, "Camp") {
		t.Error("a viewer cannot see the events")
	}
	_, page := get(t, s.srv, "/admin/events/"+e.ID)
	for _, absent := range []string{"Save details", "Publish", "Add a ticket type", "Upload"} {
		if strings.Contains(page, absent) {
			t.Errorf("a viewer's event page offers %q", absent)
		}
	}
	if !strings.Contains(page, "disabled") {
		t.Error("the details form is not disabled for a viewer")
	}
}

func TestAdminEvents_DeleteADraftNobodyRegisteredFor(t *testing.T) {
	s := setupApp(t)
	e := createEvent(t, s, "Camp")
	uploadImage(t, s, e.ID, testJPEG)
	stored, _ := s.events.Get(t.Context(), e.ID)

	res, _ := post(t, s.srv, "/admin/events/"+e.ID+"/delete", url.Values{})
	if loc := res.Header.Get("Location"); loc != "/admin/events?notice=deleted" {
		t.Errorf("delete went to %q", loc)
	}
	if _, err := s.events.Get(t.Context(), e.ID); err == nil {
		t.Error("the event still exists")
	}
	if !slices.Contains(s.images.Deleted(), stored.ImageKey) {
		t.Error("the deleted event's image was left in the bucket")
	}
}
