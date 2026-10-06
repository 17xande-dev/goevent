package handler

import "net/http"

// RegisterPublic wires the routes anybody may reach without a session and that
// change nothing: the bundled assets and the not-found catch-all.
func (h *Handler) RegisterPublic(mux *http.ServeMux) {
	// "/" — the bare subtree, which matches every path no other pattern claimed —
	// is how a custom 404 page is installed, since ServeMux has no
	// NotFoundHandler to set.
	//
	// One consequence worth knowing: a request to a *known* path under an
	// unregistered method lands here as a 404 rather than getting a 405, because
	// a pattern that matches beats one that would only have matched with a
	// different method.
	mux.HandleFunc("/", h.notFoundFor(mux))

	// Vendored htmx and the theme, served from the binary so no page needs a CDN.
	mux.Handle("GET /static/", http.HandlerFunc(h.static))
}

// isHTMX reports whether htmx made this request.
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }
