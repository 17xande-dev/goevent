package validate

import (
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/events"
)

func TestPhone_AcceptsHowSouthAfricansWriteThem(t *testing.T) {
	for _, ok := range []string{"", "+27 82 123 4567", "082 123 4567", "(021) 555-1234", "0821234567"} {
		e := FormErrors{}
		if Phone(e, "p", ok); e.Any() {
			t.Errorf("Phone(%q) refused: %s", ok, e)
		}
	}
	for _, bad := range []string{"call me", "123", "+27 82 123 4567 89 01 23", "082" + strings.Repeat(" ", 100) + "1234567"} {
		e := FormErrors{}
		if Phone(e, "p", bad); !e.Any() {
			t.Errorf("Phone(%q) accepted", bad)
		}
	}
}

func TestAnswer(t *testing.T) {
	sel := events.Question{Kind: events.KindSelect, Options: []string{"S", "M"}, Required: true}
	tick := events.Question{Kind: events.KindCheckbox, Required: true}
	date := events.Question{Kind: events.KindDate}
	for _, tc := range []struct {
		q    events.Question
		v    string
		fine bool
	}{
		{sel, "M", true},
		{sel, "XL", false}, // not an option: a hand-made request
		{sel, "", false},   // required
		{tick, "1", true},
		{tick, "", false}, // a required tick box — consent — must be ticked
		{date, "", true},
		{date, "2026-11-07", true},
		{date, "07/11/2026", false},
		{events.Question{Kind: events.KindText}, "anything", true},
	} {
		if got := Answer(tc.q, tc.v) == ""; got != tc.fine {
			t.Errorf("Answer(%s %q) fine = %v, want %v", tc.q.Kind, tc.v, got, tc.fine)
		}
	}
}
