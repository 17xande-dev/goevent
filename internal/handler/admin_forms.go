package handler

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// formValues is a submitted form as typed, for rendering it back. A rejected
// submission has to come back with what was entered — a date the parser
// refused included — so a form re-renders from these strings rather than from
// the half-parsed value the handler was building.
type formValues map[string]string

// Get is the field's value, for a template.
func (f formValues) Get(name string) string { return f[name] }

// On reports whether a checkbox was ticked. A checkbox sends "1" or nothing;
// anything else is not a tick.
func (f formValues) On(name string) bool { return f[name] == "1" }

// valuesOf takes the first value of every field in a parsed POST body, trimmed.
func valuesOf(r *http.Request) formValues {
	f := formValues{}
	for k, v := range r.PostForm {
		if len(v) > 0 {
			f[k] = strings.TrimSpace(v[0])
		}
	}
	return f
}

// localInput is the format of an <input type="datetime-local"> value: a wall
// time with no zone. The zone is the event's, which the handler supplies.
const localInput = "2006-01-02T15:04"

// parseLocal reads a datetime-local value in loc. Empty is nil and fine; a value
// that does not parse is ok=false, for the caller to put on the field.
func parseLocal(s string, loc *time.Location) (t *time.Time, ok bool) {
	if s == "" {
		return nil, true
	}
	v, err := time.ParseInLocation(localInput, s, loc)
	if err != nil {
		return nil, false
	}
	return &v, true
}

// formatLocal renders t for a datetime-local input in loc; nil is empty.
func formatLocal(t *time.Time, loc *time.Location) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.In(loc).Format(localInput)
}

// parseCount reads an optional whole number, nil when blank — "no limit", which
// is a different fact from 0.
func parseCount(s string) (n *int, ok bool) {
	if s == "" {
		return nil, true
	}
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return nil, false
	}
	return &v, true
}

// formatCount is parseCount's inverse.
func formatCount(n *int) string {
	if n == nil {
		return ""
	}
	return strconv.Itoa(*n)
}

// checkbox renders a bool as the value On reads.
func checkbox(b bool) string {
	if b {
		return "1"
	}
	return ""
}

// humanBytes is a size for a message about an upload limit.
func humanBytes(n int64) string {
	const mib = 1 << 20
	if n >= mib && n%mib == 0 {
		return strconv.FormatInt(n/mib, 10) + " MB"
	}
	return strconv.FormatInt(n, 10) + " bytes"
}
