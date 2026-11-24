package handler

import (
	"net/http"
	"strings"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/goevent/internal/blob"
	"github.com/17xande-dev/websec/csrf"
)

const eventImageUpload = "POST /admin/events/{id}/image"

// uploadTooLarge reports that the kit discarded an over-cap image upload from a
// permitted account, so the route renders its own "too large" page.
func uploadTooLarge(r *http.Request) bool { return csrf.Oversized(r) }

// mayUpload is whether the request carries a session allowed to upload event
// images. It reads no body, which is the point: it decides before anything is
// read or spooled.
func (h *Handler) mayUpload(r *http.Request) bool {
	user, ok := h.currentSession(r)
	return ok && user.Can(auth.PermEventsWrite) && !user.MustChangePassword
}

// bodyLimit is csrf.Config.Limit. The first-party mux's actual match selects the
// upload route (a nearby path spelling gets no larger cap). The kit calls it
// before routing, for every unsafe request.
func (h *Handler) bodyLimit(r *http.Request) csrf.BodyLimit {
	routes := h.firstParty.Load()
	if routes == nil {
		return csrf.BodyLimit{}
	}
	if _, pattern := routes.Handler(r); pattern == eventImageUpload {
		return csrf.BodyLimit{
			// Multipart only: any other body on this route keeps the small
			// default cap.
			MultipartMaxBytes: blob.MaxUploadBytes + 1024,
			MaxMemory:         blob.MaxUploadBytes,
			PassOversized:     h.mayUpload,
		}
	}
	return csrf.BodyLimit{}
}

// bodyRefused is csrf.Config.OnBodyError: the app's own wording for the kit's
// body refusals, which it has always answered in plain text.
func (h *Handler) bodyRefused(w http.ResponseWriter, r *http.Request, status int) {
	switch status {
	case http.StatusRequestEntityTooLarge:
		http.Error(w, "Request body too large.", status)
	case http.StatusUnsupportedMediaType:
		http.Error(w, "Multipart forms are not accepted on this route.", status)
	default:
		http.Error(w, "That form could not be read.", status)
	}
}

// gateUploads runs the one check that must happen before the kit reads any of
// an upload: a streamed multipart body has no declared length to refuse on, and
// an anonymous caller must not get it read and spooled. The mux's actual match
// selects the route; a nearby path gets no gate.
func (h *Handler) gateUploads(kit http.Handler, routes *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			kit.ServeHTTP(w, r)
			return
		}
		if _, pattern := routes.Handler(r); pattern == eventImageUpload &&
			isMultipart(r) && r.ContentLength < 0 && !h.mayUpload(r) {
			// Do not drain the rejected body to reuse an HTTP/1 connection.
			if r.ProtoMajor == 1 {
				w.Header().Set("Connection", "close")
			}
			http.Error(w, "Sign in required.", http.StatusUnauthorized)
			return
		}
		kit.ServeHTTP(w, r)
	})
}

// isMultipart reports whether the request claims to be multipart/form-data. A
// malformed parameter still counts, as it does for net/http's own form parsing.
func isMultipart(r *http.Request) bool {
	leading, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	return strings.EqualFold(strings.TrimSpace(leading), "multipart/form-data")
}
