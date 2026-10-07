package handler

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/registrations"
)

// The door: one page per event where a volunteer scans tickets or looks people
// up by name, checks them in, and undoes a mistake.
//
// A USB or Bluetooth QR scanner is a keyboard that types the code and presses
// Enter, so the scan box is an ordinary autofocused form. Every action is a POST
// that redirects back here with what happened in the query string, so the box
// has focus again for the next person and a reload never repeats a check-in —
// though repeating one would only say "already".

type checkinPage struct {
	page
	Event   events.Event
	Counts  registrations.DoorCounts
	Query   string
	Results []registrations.Door
	// Last is the attendee the last action was about, and Result what it did:
	// "in", "undone", or a registrations.Refusal. "invalid" is a code that is not
	// a ticket at all, which has no attendee.
	Last   *registrations.Door
	Result string
}

func checkinPath(eventID string) string { return eventPath(eventID) + "/checkin" }

func (h *Handler) adminCheckin(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	data := checkinPage{
		page: h.newPage(r, "Check-in · "+e.Title), Event: e,
		Query: strings.TrimSpace(q.Get("q")), Result: q.Get("result"),
	}
	var err error
	if data.Counts, err = h.regs.DoorCounts(r.Context(), e.ID); err != nil {
		h.serverError(w, r, err)
		return
	}
	if data.Results, err = h.regs.SearchDoor(r.Context(), e.ID, data.Query); err != nil {
		h.serverError(w, r, err)
		return
	}
	if id := q.Get("a"); id != "" {
		d, err := h.regs.DoorAttendee(r.Context(), e.ID, id)
		switch {
		case err == nil:
			data.Last = &d
		case !errors.Is(err, registrations.ErrNotFound):
			h.serverError(w, r, err)
			return
		}
	}
	h.render(w, r, http.StatusOK, "admin_checkin", data)
}

// adminCheckinCounts is the counts on their own, which the page polls so that
// several volunteers at several doors each see the whole room filling.
func (h *Handler) adminCheckinCounts(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	counts, err := h.regs.DoorCounts(r.Context(), e.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	h.render(w, r, http.StatusOK, "checkin_counts_fragment", checkinPage{Event: e, Counts: counts})
}

// adminCheckinScan checks someone in, from either a scanned code or the button
// beside a search result. Scanned text that is not a ticket is taken as a
// search, so the one box serves a scanner and a volunteer typing a surname.
func (h *Handler) adminCheckinScan(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	back := url.Values{}
	if q := strings.TrimSpace(r.PostFormValue("q")); q != "" {
		back.Set("q", q)
	}
	attendee := r.PostFormValue("attendee")
	if attendee == "" {
		code := strings.TrimSpace(r.PostFormValue("code"))
		id, ok := h.signer.ParseTicket(code)
		switch {
		case ok:
			attendee = id
		case looksLikeTicket(code):
			// Shaped like a ticket but not signed by this server: a misread, or a
			// ticket from somewhere else. Searching for it would find nothing and
			// say so less clearly.
			back.Set("result", "invalid")
			h.backToDoor(w, r, e.ID, back)
			return
		default:
			back.Set("q", code)
			h.backToDoor(w, r, e.ID, back)
			return
		}
	}
	res, err := h.regs.CheckIn(r.Context(), e.ID, attendee, h.actorID(r))
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	result := string(res.Refused)
	if res.Refused == "" {
		result = "in"
		h.logger(r).Info("checked in", "event", e.ID, "attendee", attendee, "by", h.actorID(r))
	} else {
		h.logger(r).Info("check-in refused", "event", e.ID, "attendee", attendee, "why", res.Refused, "by", h.actorID(r))
	}
	if res.Refused != registrations.RefusedUnknown {
		back.Set("a", res.AttendeeID)
	}
	back.Set("result", result)
	h.backToDoor(w, r, e.ID, back)
}

func (h *Handler) adminCheckinUndo(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	attendee := r.PathValue("attendeeID")
	if err := h.regs.UndoCheckIn(r.Context(), e.ID, attendee); err != nil {
		h.storeError(w, r, err)
		return
	}
	h.logger(r).Info("check-in undone", "event", e.ID, "attendee", attendee, "by", h.actorID(r))
	back := url.Values{"a": {attendee}, "result": {"undone"}}
	if q := strings.TrimSpace(r.PostFormValue("q")); q != "" {
		back.Set("q", q)
	}
	h.backToDoor(w, r, e.ID, back)
}

func (h *Handler) backToDoor(w http.ResponseWriter, r *http.Request, eventID string, v url.Values) {
	target := checkinPath(eventID)
	if len(v) > 0 {
		target += "?" + v.Encode()
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// looksLikeTicket reports whether text has a ticket code's shape — a uuid, a
// dot, and a MAC — without checking the MAC.
func looksLikeTicket(s string) bool {
	id, sig, ok := strings.Cut(s, ".")
	return ok && len(id) == 36 && strings.Count(id, "-") == 4 && len(sig) >= 16 && !strings.ContainsAny(s, " \t")
}
