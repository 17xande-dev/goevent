package handler

import (
	"errors"
	"mime"
	"net/http"
	"strings"

	"github.com/17xande-dev/goevent/internal/blob"
)

const eventImageUpload = "POST /admin/events/{id}/image"

// limitBodies runs before nosurf, which otherwise parses form bodies to find
// the token before any route's own limits apply.
func limitBodies(next http.Handler, routes *http.ServeMux) http.Handler {
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
		if r.ContentLength > limit {
			http.Error(w, "Request body too large.", http.StatusRequestEntityTooLarge)
			return
		}
		if mediaType == "multipart/form-data" && !upload {
			http.Error(w, "Multipart forms are not accepted on this route.", http.StatusUnsupportedMediaType)
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
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "Request body too large.", http.StatusRequestEntityTooLarge)
			} else {
				http.Error(w, "That form could not be read.", http.StatusBadRequest)
			}
			return
		}
		next.ServeHTTP(w, r)
	})
}
