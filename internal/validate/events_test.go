package validate

import (
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/events"
)

func validEvent() events.Event {
	start := time.Date(2026, 11, 7, 7, 0, 0, 0, time.UTC)
	return events.Event{Title: "Camp", Slug: "camp", StartsAt: start, EndsAt: start.Add(time.Hour),
		Timezone: events.DefaultTimezone}
}

func TestEvent(t *testing.T) {
	if errs := Event(validEvent()); errs.Any() {
		t.Fatalf("a valid event was refused: %s", errs)
	}

	neg := -1
	early := validEvent().StartsAt
	for field, edit := range map[string]func(*events.Event){
		"title":                  func(e *events.Event) { e.Title = " " },
		"slug":                   func(e *events.Event) { e.Slug = "Not A Slug" },
		"timezone":               func(e *events.Event) { e.Timezone = "Mars/Base" },
		"ends_at":                func(e *events.Event) { e.EndsAt = e.StartsAt.Add(-time.Minute) },
		"capacity":               func(e *events.Event) { e.Capacity = &neg },
		"registration_closes_at": func(e *events.Event) { e.RegistrationOpensAt, e.RegistrationClosesAt = &early, &early },
	} {
		e := validEvent()
		edit(&e)
		if errs := Event(e); errs[field] == "" {
			t.Errorf("%s: no error, got %s", field, errs)
		}
	}
}

func TestTicketType(t *testing.T) {
	ok := events.TicketType{Name: "Adult", MaxPerRegistration: 10}
	if errs := TicketType(ok); errs.Any() {
		t.Fatalf("a valid ticket type was refused: %s", errs)
	}
	for field, edit := range map[string]func(*events.TicketType){
		"name":                 func(t *events.TicketType) { t.Name = "" },
		"max_per_registration": func(t *events.TicketType) { t.MaxPerRegistration = 0 },
		"price":                func(t *events.TicketType) { t.PriceCents = -1 },
	} {
		tt := ok
		edit(&tt)
		if errs := TicketType(tt); errs[field] == "" {
			t.Errorf("%s: no error, got %s", field, errs)
		}
	}
}

func TestQuestion(t *testing.T) {
	q := events.Question{Label: "Size", Scope: events.ScopeAttendee, Kind: events.KindSelect, Options: []string{"S", "M"}}
	if errs := Question(q); errs.Any() {
		t.Fatalf("a valid choice was refused: %s", errs)
	}
	q.Options = []string{"S"}
	if Question(q)["options"] == "" {
		t.Error("a one-option choice was accepted")
	}
	// Options left over from changing the kind would otherwise be kept invisibly.
	q.Kind, q.Options = events.KindText, []string{"S", "M"}
	if Question(q)["options"] == "" {
		t.Error("a text question with options was accepted")
	}
	q.Options, q.Scope = nil, "everyone"
	if Question(q)["scope"] == "" {
		t.Error("an unknown scope was accepted")
	}
}
