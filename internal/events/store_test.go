package events_test

import (
	"errors"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/dbtest"
	"github.com/17xande-dev/goevent/internal/events"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newEvent(slug string) events.Event {
	start := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	return events.Event{
		Slug: slug, Title: "Men's breakfast", StartsAt: start, EndsAt: start.Add(3 * time.Hour),
		Timezone: events.DefaultTimezone, Listed: true,
	}
}

func setup(t *testing.T) (*events.Store, *pgxpool.Pool, events.Event) {
	t.Helper()
	pool := dbtest.Pool(t)
	s := events.NewStore(pool)
	e, err := s.Create(t.Context(), newEvent("breakfast"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return s, pool, e
}

// hold makes a registration holding one ticket of tt, so a ticket type and a
// question have history pointing at them.
func hold(t *testing.T, pool *pgxpool.Pool, e events.Event, tt events.TicketType, q *events.Question) {
	t.Helper()
	var regID, attID string
	err := pool.QueryRow(t.Context(), `
INSERT INTO registrations (event_id, reference, contact_first_name, contact_last_name, contact_email,
    total_cents, currency, hold_expires_at, checkout_key, manage_token_hash)
VALUES ($1, 'ABC-123', 'Ada', 'Lovelace', 'ada@example.com', 0, 'ZAR', now(), 'k', '\x01')
RETURNING id`, e.ID).Scan(&regID)
	if err != nil {
		t.Fatalf("insert registration: %v", err)
	}
	err = pool.QueryRow(t.Context(), `
INSERT INTO attendees (registration_id, ticket_type_id, first_name, last_name, ticket_name, unit_price_cents)
VALUES ($1, $2, 'Ada', 'Lovelace', $3, $4) RETURNING id`, regID, tt.ID, tt.Name, tt.PriceCents).Scan(&attID)
	if err != nil {
		t.Fatalf("insert attendee: %v", err)
	}
	if q != nil {
		if _, err := pool.Exec(t.Context(), `
INSERT INTO answers (registration_id, attendee_id, question_id, label, value)
VALUES ($1, $2, $3, $4, 'yes')`, regID, attID, q.ID, q.Label); err != nil {
			t.Fatalf("insert answer: %v", err)
		}
	}
}

func TestCreate_IsADraftWhateverItSays(t *testing.T) {
	pool := dbtest.Pool(t)
	s := events.NewStore(pool)

	// The details form must never be a way to publish.
	e := newEvent("sneaky")
	e.Status = events.StatusPublished
	got, err := s.Create(t.Context(), e)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.Status != events.StatusDraft {
		t.Errorf("Status = %q, want draft", got.Status)
	}
}

func TestCreate_SlugTakenIsItsOwnError(t *testing.T) {
	s, _, _ := setup(t)
	if _, err := s.Create(t.Context(), newEvent("breakfast")); !errors.Is(err, events.ErrSlugTaken) {
		t.Errorf("duplicate slug: err = %v, want ErrSlugTaken", err)
	}
}

func TestGet_MalformedIDIsNotFound(t *testing.T) {
	s, _, _ := setup(t)
	if _, err := s.Get(t.Context(), "not-a-uuid"); !errors.Is(err, events.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestUpdate_LeavesStatusAndImageAlone(t *testing.T) {
	s, _, e := setup(t)
	if _, err := s.CreateTicketType(t.Context(), events.TicketType{EventID: e.ID, Name: "Adult", MaxPerRegistration: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(t.Context(), e.ID, events.StatusDraft, events.StatusPublished); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if _, err := s.SetImage(t.Context(), e.ID, "events/x/y.jpg"); err != nil {
		t.Fatalf("SetImage: %v", err)
	}

	e.Title = "Renamed"
	e.Status = events.StatusDraft
	e.ImageKey = ""
	got, err := s.Update(t.Context(), e)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Title != "Renamed" {
		t.Errorf("Title = %q", got.Title)
	}
	if got.Status != events.StatusPublished || got.ImageKey != "events/x/y.jpg" {
		t.Errorf("saving details changed status %q or image %q", got.Status, got.ImageKey)
	}
}

// Two administrators looking at the same draft: one cancels it, the other then
// presses Publish on the page they still have open. The second move was made
// from a status the event no longer has, and must not happen.
func TestTransition_RefusesAMoveFromAStaleStatus(t *testing.T) {
	s, _, e := setup(t)
	if _, err := s.CreateTicketType(t.Context(), events.TicketType{EventID: e.ID, Name: "Adult", MaxPerRegistration: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(t.Context(), e.ID, events.StatusDraft, events.StatusCancelled); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	_, err := s.Transition(t.Context(), e.ID, events.StatusDraft, events.StatusPublished)
	if !errors.Is(err, events.ErrStatusMove) {
		t.Fatalf("publish from a stale draft: err = %v, want ErrStatusMove", err)
	}
	if got, _ := s.Get(t.Context(), e.ID); got.Status != events.StatusCancelled {
		t.Errorf("Status = %q, want cancelled", got.Status)
	}
}

func TestTransition_PublishNeedsSomethingOnOffer(t *testing.T) {
	s, _, e := setup(t)
	if _, err := s.CreateTicketType(t.Context(), events.TicketType{EventID: e.ID, Name: "Staff", Hidden: true, MaxPerRegistration: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(t.Context(), e.ID, events.StatusDraft, events.StatusPublished); !errors.Is(err, events.ErrNothingOffered) {
		t.Errorf("publish with only a hidden ticket: err = %v, want ErrNothingOffered", err)
	}
	// A move the status machine does not allow is refused before the database.
	if _, err := s.Transition(t.Context(), e.ID, events.StatusDraft, events.StatusClosed); !errors.Is(err, events.ErrStatusMove) {
		t.Errorf("draft → closed: err = %v, want ErrStatusMove", err)
	}
}

func TestDelete_RefusesAnEventWithRegistrations(t *testing.T) {
	s, pool, e := setup(t)
	tt, err := s.CreateTicketType(t.Context(), events.TicketType{EventID: e.ID, Name: "Adult", MaxPerRegistration: 4})
	if err != nil {
		t.Fatalf("CreateTicketType: %v", err)
	}
	hold(t, pool, e, tt, nil)

	if err := s.Delete(t.Context(), e.ID); !errors.Is(err, events.ErrInUse) {
		t.Fatalf("Delete with a registration: err = %v, want ErrInUse", err)
	}
	if _, err := s.Get(t.Context(), e.ID); err != nil {
		t.Errorf("the event is gone: %v", err)
	}
}

func TestRemoveTicketType_DeletesUnusedAndArchivesHeld(t *testing.T) {
	s, pool, e := setup(t)
	unused, err := s.CreateTicketType(t.Context(), events.TicketType{EventID: e.ID, Name: "Child", MaxPerRegistration: 4})
	if err != nil {
		t.Fatal(err)
	}
	held, err := s.CreateTicketType(t.Context(), events.TicketType{EventID: e.ID, Name: "Adult", PriceCents: 15000, MaxPerRegistration: 4})
	if err != nil {
		t.Fatal(err)
	}
	hold(t, pool, e, held, nil)

	if archived, err := s.RemoveTicketType(t.Context(), e.ID, unused.ID); err != nil || archived {
		t.Errorf("unused: archived=%v err=%v, want a delete", archived, err)
	}
	if _, err := s.TicketType(t.Context(), e.ID, unused.ID); !errors.Is(err, events.ErrNotFound) {
		t.Errorf("the unused ticket type still exists: %v", err)
	}

	// Somebody holds this one, so it must survive for their attendee row to keep
	// pointing at what they bought.
	if archived, err := s.RemoveTicketType(t.Context(), e.ID, held.ID); err != nil || !archived {
		t.Fatalf("held: archived=%v err=%v, want an archive", archived, err)
	}
	got, err := s.TicketType(t.Context(), e.ID, held.ID)
	if err != nil || !got.Archived {
		t.Errorf("held ticket type after removal: %+v, %v", got, err)
	}
}

func TestRemoveTicketType_IsScopedToItsEvent(t *testing.T) {
	s, _, e := setup(t)
	other, err := s.Create(t.Context(), newEvent("other"))
	if err != nil {
		t.Fatal(err)
	}
	tt, err := s.CreateTicketType(t.Context(), events.TicketType{EventID: other.ID, Name: "Adult", MaxPerRegistration: 4})
	if err != nil {
		t.Fatal(err)
	}
	// An id from another event, through this event's URL, must touch nothing.
	if _, err := s.RemoveTicketType(t.Context(), e.ID, tt.ID); !errors.Is(err, events.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if _, err := s.TicketType(t.Context(), other.ID, tt.ID); err != nil {
		t.Errorf("the other event's ticket type was removed: %v", err)
	}
}

func TestQuestion_EmptyLimitMeansEveryTicketType(t *testing.T) {
	s, _, e := setup(t)
	tt, err := s.CreateTicketType(t.Context(), events.TicketType{EventID: e.ID, Name: "Adult", MaxPerRegistration: 4})
	if err != nil {
		t.Fatal(err)
	}

	q, err := s.CreateQuestion(t.Context(), events.Question{
		EventID: e.ID, Scope: events.ScopeAttendee, Kind: events.KindText, Label: "Dietary needs",
		TicketTypeIDs: []string{},
	})
	if err != nil {
		t.Fatalf("CreateQuestion: %v", err)
	}
	// Stored as an empty array this would mean "asked of nobody".
	if q.TicketTypeIDs != nil {
		t.Errorf("TicketTypeIDs = %#v, want nil", q.TicketTypeIDs)
	}
	if !q.AppliesTo(tt.ID) {
		t.Error("an unlimited attendee question does not apply to the event's ticket type")
	}

	q.TicketTypeIDs = []string{tt.ID}
	q.Options = nil
	q, err = s.UpdateQuestion(t.Context(), q)
	if err != nil {
		t.Fatalf("UpdateQuestion: %v", err)
	}
	if !q.AppliesTo(tt.ID) || q.AppliesTo("someone-else") {
		t.Errorf("limited question applies wrongly: %+v", q.TicketTypeIDs)
	}

	// A registration question has no ticket-type limit, whatever was submitted.
	q.Scope = events.ScopeRegistration
	q, err = s.UpdateQuestion(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if q.TicketTypeIDs != nil {
		t.Errorf("registration question kept a limit %#v", q.TicketTypeIDs)
	}
}

func TestRemoveQuestion_ArchivesAnAnsweredQuestion(t *testing.T) {
	s, pool, e := setup(t)
	tt, err := s.CreateTicketType(t.Context(), events.TicketType{EventID: e.ID, Name: "Adult", MaxPerRegistration: 4})
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.CreateQuestion(t.Context(), events.Question{
		EventID: e.ID, Scope: events.ScopeAttendee, Kind: events.KindCheckbox, Label: "Photo consent",
	})
	if err != nil {
		t.Fatal(err)
	}
	hold(t, pool, e, tt, &q)

	if archived, err := s.RemoveQuestion(t.Context(), e.ID, q.ID); err != nil || !archived {
		t.Fatalf("archived=%v err=%v, want an archive", archived, err)
	}
	qs, err := s.Questions(t.Context(), e.ID)
	if err != nil || len(qs) != 1 || !qs[0].Archived {
		t.Errorf("questions after removal: %+v, %v", qs, err)
	}
}

func TestRegistrationOpen(t *testing.T) {
	e := newEvent("x")
	e.Status = events.StatusPublished
	before := e.StartsAt.Add(-time.Hour)

	if !e.RegistrationOpen(before) {
		t.Error("a published event with no window is not open before it starts")
	}
	if e.RegistrationOpen(e.EndsAt) {
		t.Error("an event that has ended is still open")
	}

	opens := before.Add(time.Minute)
	e.RegistrationOpensAt = &opens
	if e.RegistrationOpen(before) {
		t.Error("open before the registration window opens")
	}
	closes := before
	e.RegistrationOpensAt = nil
	e.RegistrationClosesAt = &closes
	if e.RegistrationOpen(before) {
		t.Error("open at the instant the window closes")
	}

	e.RegistrationClosesAt = nil
	for _, s := range []events.Status{events.StatusDraft, events.StatusClosed, events.StatusCancelled} {
		e.Status = s
		if e.RegistrationOpen(before) {
			t.Errorf("a %s event is open", s)
		}
	}
}

func TestWhen_NamesBothDaysOfAMultiDayEvent(t *testing.T) {
	e := newEvent("x") // 09:00–12:00 UTC on 7 Nov: 11:00–14:00 in Johannesburg
	if got, want := e.When(), "Sat 7 Nov 2026, 11:00–14:00"; got != want {
		t.Errorf("same day: %q, want %q", got, want)
	}
	e.EndsAt = e.StartsAt.Add(48 * time.Hour)
	if got, want := e.When(), "Sat 7 Nov 2026, 11:00 – Mon 9 Nov 2026, 11:00"; got != want {
		t.Errorf("two days: %q, want %q", got, want)
	}
}

func TestLocation_FallsBackRatherThanFailing(t *testing.T) {
	e := events.Event{Timezone: "Nowhere/Special"}
	if got := e.Location().String(); got != events.DefaultTimezone {
		t.Errorf("Location = %s, want the default", got)
	}
	e.Timezone = "Europe/London"
	if got := e.Location().String(); got != "Europe/London" {
		t.Errorf("Location = %s", got)
	}
}
