package handler

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/17xande-dev/goevent/internal/middleware"
)

// How the store answers when something has gone wrong.
//
// Three pages, because they say three different things. `not_found` is the common
// one and has its own copy and a search box. `error_client` covers the rest of the
// 4xx range — you asked for something that is not yours, or came too fast — and
// `error_server` covers 5xx, where the honest message is that this is ours and not
// yours.
//
// Two rules hold everywhere in this file:
//
//   - **Every one falls back to plain text.** These are the responses an adopter's
//     broken theme override is most likely to break, and a 500 that fails to render
//     must still be a 500. h.render would log and leave the status at 200, which is
//     the one outcome a crawler must never see.
//   - **A full page never lands in a fragment.** htmx is configured to swap error
//     responses (see the htmx-config meta tag in layout.gohtml), so an error page sent
//     to an htmx request must say "replace the document" or it would be pasted into
//     whatever small target the request was aimed at.
//
// Byte endpoints use none of it: a missing image or asset answers with the plain
// text, because nothing is going to read an HTML page in an <img> tag and sending
// one would only make the failure larger.

// errorPageData is what both error templates render. Detail is populated only in
// development; see config.ShowErrorDetail.
type errorPageData struct {
	page
	Status  int
	Heading string
	Message string

	// Reference is the request id, so that "I got an error, it said 7f3a9c2e" is
	// enough to find the log line. Empty when the middleware did not run.
	Reference string

	// Detail is the underlying error, shown in development and never in
	// production: it names tables, columns and constraints.
	Detail string
}

// clientError answers a 4xx with the client-error page.
func (h *Handler) clientError(w http.ResponseWriter, r *http.Request, status int, heading, message string) {
	h.renderError(w, r, "error_client", errorPageData{
		page:      h.newPage(r, heading),
		Status:    status,
		Heading:   heading,
		Message:   message,
		Reference: middleware.RequestIDFrom(r.Context()),
	})
}

// serverError logs a fault and answers 500.
//
// The log line and the page carry the same reference, which is the whole point:
// the visitor can quote something, and it leads to the one line that says what
// actually happened.
func (h *Handler) serverError(w http.ResponseWriter, r *http.Request, err error) {
	h.logger(r).Error("server error", "method", r.Method, "path", r.URL.Path, "error", err)

	data := errorPageData{
		page:      h.newPage(r, "Something went wrong"),
		Status:    http.StatusInternalServerError,
		Heading:   "Something went wrong",
		Message:   "This is a fault on our side, not anything you did. It has been logged.",
		Reference: middleware.RequestIDFrom(r.Context()),
	}
	if h.cfg.ShowErrorDetail {
		data.Detail = err.Error()
	}
	h.renderError(w, r, "error_server", data)
}

// badForm answers a request whose body could not be parsed as a form at all —
// not a validation failure, which re-renders the form with its messages, but a
// body that was never a form. A browser does not produce one, so this is a
// hand-made request or a truncated upload.
func (h *Handler) badForm(w http.ResponseWriter, r *http.Request) {
	h.clientError(w, r, http.StatusBadRequest, "That request could not be read",
		"The form data did not arrive in a form we could read. If you were uploading something, it may have been cut short.")
}

// notFound renders the 404 page.
func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.renderError(w, r, "not_found", errorPageData{
		page:      h.newPage(r, "Page not found"),
		Status:    http.StatusNotFound,
		Reference: middleware.RequestIDFrom(r.Context()),
	})
}

// notFoundFor is the catch-all handler, closed over the mux it is registered on so
// that it can tell "no such path" from "not that method".
//
// The mux itself can no longer tell us: a bare "/" pattern matches everything, and
// a pattern that matches beats one that would only have matched under a different
// method, so ServeMux's own 405 branch is unreachable. Rather than registering a
// second pattern beside all thirty-odd routes — a line per route that the next
// route can forget — this re-asks the mux for the same path under every other
// method and sees which ones land on a real pattern.
//
// Self-maintaining, which is the property that matters: a route added later
// answers 405 correctly without anyone remembering to do anything.
func (h *Handler) notFoundFor(mux *http.ServeMux) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if allowed := allowedMethods(mux, r); len(allowed) > 0 {
			w.Header().Set("Allow", strings.Join(allowed, ", "))
			h.clientError(w, r, http.StatusMethodNotAllowed, "Method not allowed",
				"That address exists, but not for a "+r.Method+" request.")
			return
		}
		h.notFound(w, r)
	}
}

// probeMethods are the methods worth asking about. HEAD is omitted because
// ServeMux answers it from the GET pattern, so it would report a 405 as allowed
// under a method the caller did not ask about.
var probeMethods = []string{
	http.MethodGet, http.MethodPost, http.MethodPut,
	http.MethodPatch, http.MethodDelete, http.MethodOptions,
}

// allowedMethods returns the methods this exact path would be served under. An
// empty result means the path matches nothing but the catch-all, which is a
// genuine 404.
func allowedMethods(mux *http.ServeMux, r *http.Request) []string {
	var allowed []string
	for _, method := range probeMethods {
		if method == r.Method {
			continue
		}
		probe := r.Clone(r.Context())
		probe.Method = method

		_, pattern := mux.Handler(probe)
		// "" is no match at all; "/" is this handler, and a path matching only the
		// catch-all has not been claimed by anything.
		if pattern == "" || pattern == "/" {
			continue
		}
		allowed = append(allowed, method)
	}
	// GET implies HEAD, and a client that asked about one is owed the other.
	if slices.Contains(allowed, http.MethodGet) {
		allowed = append(allowed, http.MethodHead)
	}
	return allowed
}

// renderError writes an error page, falling back to plain text when the template
// cannot be rendered.
//
// Executing to bytes first is what makes the fallback possible: nothing has been
// written when the error comes back, so the status is still ours to set.
func (h *Handler) renderError(w http.ResponseWriter, r *http.Request, name string, data errorPageData) {
	body, err := h.tmpl.Execute(name, data)
	if err != nil {
		h.logger(r).Error("render failed", "template", name, "path", r.URL.Path, "error", err)
		http.Error(w, http.StatusText(data.Status), data.Status)
		return
	}

	// htmx swaps error responses, so without this the whole document would be
	// pasted into whatever the request was aimed at — the cart-count span, say.
	// Errors that are *meant* for their target, like the cart's refusals, render a
	// fragment and never come through here.
	if isHTMX(r) {
		w.Header().Set("HX-Retarget", "body")
		w.Header().Set("HX-Reswap", "innerHTML")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(data.Status)
	if _, err := w.Write(body); err != nil {
		h.logger(r).Error("writing an error page failed", "path", r.URL.Path, "error", err)
	}
}

// logger returns the request's logger: the server's, with the request id
// attached, so every line about one request can be found together.
func (h *Handler) logger(r *http.Request) *slog.Logger {
	id := middleware.RequestIDFrom(r.Context())
	if id == "" {
		return h.log
	}
	return h.log.With("request_id", id)
}
