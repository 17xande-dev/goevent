package handler

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/17xande-dev/goevent/internal/blob"
)

// Event image uploads.
//
// The order is chosen so that no failure leaves an event pointing at nothing:
// check the bytes are an image, put them under a fresh key, point the event at
// it, and only then delete the object it used to own. A failure before the last
// step leaves the old image in place and working; a failure at the last step
// leaves an orphaned object, which costs a few kilobytes and is logged.
//
// A fresh key per upload is also what makes this work behind a CDN: a new image
// is a new URL, visible immediately, with no cache purge this server has no
// credentials to perform.

func (h *Handler) adminEventImageUpload(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	problem := func(msg string) {
		h.renderEvent(w, r, http.StatusUnprocessableEntity, e, eventValues(e), nil, msg)
	}

	// A body bigger than the cap is cut off here, while it is still arriving,
	// rather than after it has all been buffered.
	r.Body = http.MaxBytesReader(w, r.Body, blob.MaxUploadBytes+1024)
	if err := r.ParseMultipartForm(blob.MaxUploadBytes); err != nil {
		problem("That file is too large, or the upload was interrupted. The limit is " +
			humanBytes(blob.MaxUploadBytes) + ".")
		return
	}
	defer r.MultipartForm.RemoveAll()

	file, header, err := r.FormFile("image")
	if err != nil {
		problem("Choose an image file to upload.")
		return
	}
	defer file.Close()

	// Buffered whole rather than streamed: the type has to be sniffed from the
	// leading bytes before anything is written to a public bucket, and the size
	// has to be known to store it. Safe in memory only because of the cap above.
	body, err := io.ReadAll(io.LimitReader(file, blob.MaxUploadBytes+1))
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	switch {
	case len(body) == 0:
		problem("That file is empty.")
		return
	case int64(len(body)) > blob.MaxUploadBytes:
		problem("That image is larger than " + humanBytes(blob.MaxUploadBytes) + ".")
		return
	}
	contentType, ext, err := blob.Validate(body)
	if err != nil {
		// Never the filename or the browser's Content-Type: both are the sender's
		// claim. The filename is logged because, with several files to hand, it is
		// the only way to tell which one was refused.
		h.logger(r).Warn("refused an image upload", "event", e.ID, "filename", header.Filename, "error", err)
		problem("That file is not an image this site can serve. Accepted: " +
			strings.Join(blob.SupportedTypes(), ", ") + ".")
		return
	}

	key, err := blob.ImageKey(e.ID, ext)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	if _, err := h.blob.Put(r.Context(), key, bytes.NewReader(body), int64(len(body)), contentType); err != nil {
		if errors.Is(err, blob.ErrNotConfigured) {
			problem("Image uploads are not configured on this deployment.")
			return
		}
		h.serverError(w, r, err)
		return
	}
	if _, err := h.events.SetImage(r.Context(), e.ID, key); err != nil {
		// Stored but unreferenced. Logged as an orphan rather than deleted: a
		// delete here could fail just the same, and the next attempt should not be
		// racing this one.
		h.logger(r).Error("uploaded an image but failed to record it", "event", e.ID, "key", key, "error", err)
		h.storeError(w, r, err)
		return
	}
	h.deleteImageObject(r, e.ImageKey, e.ID)

	h.logger(r).Info("event image uploaded", "event", e.ID, "key", key, "bytes", len(body), "content_type", contentType)
	http.Redirect(w, r, eventPath(e.ID)+"?notice=image_saved", http.StatusSeeOther)
}

func (h *Handler) adminEventImageDelete(w http.ResponseWriter, r *http.Request) {
	e, ok := h.event(w, r)
	if !ok {
		return
	}
	// The row is cleared first here, unlike an upload: the event ends with no
	// image either way, so the failure worth avoiding is a deleted object with a
	// row still pointing at it.
	if _, err := h.events.SetImage(r.Context(), e.ID, ""); err != nil {
		h.storeError(w, r, err)
		return
	}
	h.deleteImageObject(r, e.ImageKey, e.ID)
	http.Redirect(w, r, eventPath(e.ID)+"?notice=image_removed", http.StatusSeeOther)
}

// deleteImageObject removes an object nothing references any more. It never
// fails its caller: the database already says the object is unreferenced, so the
// worst case is an orphan, which is a logged housekeeping problem rather than
// something to show anyone mid-task.
func (h *Handler) deleteImageObject(r *http.Request, key, eventID string) {
	if key == "" {
		return
	}
	if err := h.blob.Delete(r.Context(), key); err != nil {
		h.logger(r).Error("failed to delete a replaced event image; it is now an orphaned object",
			"event", eventID, "key", key, "error", err)
	}
}
