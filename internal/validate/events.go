package validate

import (
	"strings"
	"time"

	"github.com/17xande-dev/goevent/internal/events"
)

// Event validates an event as submitted by the admin's details form. The slug
// is held strictly because it is a public URL that people share, and changing
// it later breaks the links already out there.
func Event(e events.Event) FormErrors {
	errs := FormErrors{}

	required(errs, "title", e.Title)
	maxLen(errs, "title", e.Title, 200)
	switch {
	case e.Slug == "":
		errs.Add("slug", "Required.")
	case e.Slug != events.Slugify(e.Slug):
		errs.Add("slug", "Use lower-case letters, digits and single hyphens, like "+events.Slugify(e.Slug)+".")
	default:
		maxLen(errs, "slug", e.Slug, 100)
	}
	maxLen(errs, "summary", e.Summary, 300)
	maxLen(errs, "description", e.Description, 20_000)
	maxLen(errs, "venue", e.Venue, 200)
	maxLen(errs, "address", e.Address, 1_000)

	if _, err := time.LoadLocation(e.Timezone); e.Timezone == "" || err != nil {
		errs.Add("timezone", "Not a time zone this server knows.")
	}
	if !e.StartsAt.IsZero() && !e.EndsAt.IsZero() && e.EndsAt.Before(e.StartsAt) {
		errs.Add("ends_at", "Ends before it starts.")
	}
	if e.Capacity != nil && *e.Capacity < 0 {
		errs.Add("capacity", "Cannot be negative.")
	}
	if o, c := e.RegistrationOpensAt, e.RegistrationClosesAt; o != nil && c != nil && !c.After(*o) {
		errs.Add("registration_closes_at", "Closes before it opens.")
	}
	// Somebody who chooses to pay later has to be told how.
	if e.PayLater && strings.TrimSpace(e.PayLaterInstructions) == "" {
		errs.Add("pay_later_instructions", "Say how to pay — bank details and the reference to quote.")
	}
	maxLen(errs, "pay_later_instructions", e.PayLaterInstructions, 2_000)
	return errs
}

// TicketType validates the ticket-type form.
func TicketType(t events.TicketType) FormErrors {
	errs := FormErrors{}
	required(errs, "name", t.Name)
	maxLen(errs, "name", t.Name, 120)
	maxLen(errs, "description", t.Description, 2_000)
	if t.PriceCents < 0 {
		errs.Add("price", "Cannot be negative.")
	}
	if t.Capacity != nil && *t.Capacity < 0 {
		errs.Add("capacity", "Cannot be negative.")
	}
	if t.MaxPerRegistration < 1 || t.MaxPerRegistration > 100 {
		errs.Add("max_per_registration", "Between 1 and 100.")
	}
	if s, e := t.SalesStartAt, t.SalesEndAt; s != nil && e != nil && !e.After(*s) {
		errs.Add("sales_end_at", "Ends before it starts.")
	}
	return errs
}

// Question validates the question form. A choose-one question needs at least
// two options to be a choice; any other kind takes none, so a list left over
// from changing the kind is refused rather than silently kept.
func Question(q events.Question) FormErrors {
	errs := FormErrors{}
	required(errs, "label", q.Label)
	maxLen(errs, "label", q.Label, 300)
	maxLen(errs, "help", q.Help, 1_000)
	if !q.Scope.Valid() {
		errs.Add("scope", "Choose who is asked.")
	}
	if !q.Kind.Valid() {
		errs.Add("kind", "Choose a kind of answer.")
	}
	switch {
	case q.Kind == events.KindSelect && len(q.Options) < 2:
		errs.Add("options", "A choice needs at least two options, one per line.")
	case q.Kind != events.KindSelect && len(q.Options) > 0:
		errs.Add("options", "Only a choose-one question has options; clear them or change the kind.")
	}
	for _, o := range q.Options {
		if len([]rune(o)) > 200 {
			errs.Add("options", "Each option must be under 200 characters.")
		}
	}
	return errs
}
