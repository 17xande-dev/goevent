package handler

import (
	"context"
	"errors"
	"mime"
	"net/http"
	"strings"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/goevent/internal/blob"
)

const eventImageUpload = "POST /admin/events/{id}/image"

type uploadTooLargeKey struct{}

func uploadTooLarge(r *http.Request) bool {
	tooLarge, _ := r.Context().Value(uploadTooLargeKey{}).(bool)
	return tooLarge
}

// limitBodies runs before nosurf, which otherwise parses form bodies to find
// the token before any route's own limits apply.
func (h *Handler) limitBodies(next http.Handler, routes *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		limit := int64(256 << 10)
		_, pattern := routes.Handler(r)
		upload := pattern == eventImageUpload
		if upload {
			limit = blob.MaxUploadBytes + 1024
		}
		rawType := r.Header.Get("Content-Type")
		mediaType, _, err := mime.ParseMediaType(rawType)
		if err != nil {
			// ParseMediaType can reject parameters while net/http still
			// recognises the leading media type when reading form values.
			mediaType = strings.ToLower(strings.TrimSpace(strings.SplitN(rawType, ";", 2)[0]))
		}
		mayUpload := func() bool {
			user, ok := h.currentSession(r)
			return ok && user.Can(auth.PermEventsWrite) && !user.MustChangePassword
		}
		closeUnread := func() {
			// Do not drain the rejected body to reuse an HTTP/1 connection.
			if r.ProtoMajor == 1 {
				w.Header().Set("Connection", "close")
			}
		}
		tooLarge := func() {
			closeUnread()
			if upload && mediaType == "multipart/form-data" {
				if mayUpload() {
					rejected := r.WithContext(context.WithValue(r.Context(), uploadTooLargeKey{}, true))
					rejected.Body, rejected.ContentLength = http.NoBody, 0
					rejected.Form, rejected.PostForm, rejected.MultipartForm = nil, nil, nil
					next.ServeHTTP(w, rejected)
					return
				}
			}
			http.Error(w, "Request body too large.", http.StatusRequestEntityTooLarge)
		}
		if r.ContentLength > limit {
			tooLarge()
			return
		}
		if mediaType == "multipart/form-data" && !upload {
			http.Error(w, "Multipart forms are not accepted on this route.", http.StatusUnsupportedMediaType)
			return
		}
		// With no declared length, determine whether an upload may be read
		// before trying to find its size.
		if upload && mediaType == "multipart/form-data" && r.ContentLength < 0 && !mayUpload() {
			closeUnread()
			http.Error(w, "Sign in required.", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		// Parse here so nosurf cannot swallow a size error as a missing token.
		switch mediaType {
		case "application/x-www-form-urlencoded":
			err = r.ParseForm()
		case "multipart/form-data":
			err = r.ParseMultipartForm(blob.MaxUploadBytes)
			if r.MultipartForm != nil {
				defer r.MultipartForm.RemoveAll()
			}
		default:
			err = nil
		}
		if err != nil {
			var sizeErr *http.MaxBytesError
			if errors.As(err, &sizeErr) {
				tooLarge()
			} else {
				http.Error(w, "That form could not be read.", http.StatusBadRequest)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}
