package validate

import (
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/17xande-dev/goevent/internal/events"
)

// Email checks an address field: the weakest useful check, because the only
// real test of an address is whether mail to it arrives. required is false for
// an attendee's, which is optional.
func Email(errs FormErrors, field, email string, required bool) {
	switch {
	case email == "" && required:
		errs.Add(field, "Required — this is where the tickets go.")
	case email != "" && !isEmail(email):
		errs.Add(field, "Does not look like an email address.")
	default:
		maxLen(errs, field, email, 320)
	}
}

// Phone checks an optional phone number loosely: digits, spaces and the few
// characters people really type. South African numbers are written +27 82 …,
// 082 …, and (021) …, and refusing any of those would be refusing a person.
func Phone(errs FormErrors, field, phone string) {
	if phone == "" {
		return
	}
	// Fifteen digits allow only so much punctuation between them; without this a
	// number padded with spaces could be any length at all.
	if len(phone) > 40 {
		errs.Add(field, "Does not look like a phone number.")
		return
	}
	digits := 0
	for _, r := range phone {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case strings.ContainsRune(" +-()", r):
		default:
			errs.Add(field, "Use digits, spaces and + only.")
			return
		}
	}
	if digits < 7 || digits > 15 {
		errs.Add(field, "Does not look like a phone number.")
	}
}

// MaxLen bounds a field, for a caller outside this package.
func MaxLen(errs FormErrors, field, value string, max int) { maxLen(errs, field, value, max) }

// Answer checks one answer to one question and returns the message to show,
// or "" when it is fine. A tick box submits "1" or nothing.
func Answer(q events.Question, v string) string {
	if q.Kind == events.KindCheckbox {
		if q.Required && v != "1" {
			return "Please tick this to continue."
		}
		if v != "" && v != "1" {
			return "Tick the box or leave it."
		}
		return ""
	}
	if v == "" {
		if q.Required {
			return "Required."
		}
		return ""
	}
	switch q.Kind {
	case events.KindSelect:
		if !slices.Contains(q.Options, v) {
			return "Choose one of the options."
		}
	case events.KindDate:
		if _, err := time.Parse(time.DateOnly, v); err != nil {
			return "Use the date picker."
		}
	case events.KindPhone:
		e := FormErrors{}
		Phone(e, "x", v)
		return e["x"]
	case events.KindTextarea:
		if utf8.RuneCountInString(v) > 5_000 {
			return "Too long."
		}
	default:
		if utf8.RuneCountInString(v) > 1_000 {
			return "Too long."
		}
	}
	return ""
}
