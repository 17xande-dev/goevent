package handler

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/blob"
	"github.com/17xande-dev/websec/webtest"
)

type bodyProbe struct {
	io.Reader
	calls int
}

func (b *bodyProbe) Read(p []byte) (int, error) {
	b.calls++
	if b.Reader == nil {
		return 0, errors.New("request body must not be read")
	}
	return b.Reader.Read(p)
}

func TestLoginRejectsMultipartWithoutReading(t *testing.T) {
	checkTemp := webtest.NoTempFiles(t)
	s := newApp(t)
	body := &bodyProbe{}
	r := httptest.NewRequest(http.MethodPost, s.srv.URL+"/admin/login", body)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=test-boundary")
	r.Header.Set("Origin", s.srv.URL)
	w := httptest.NewRecorder()
	s.srv.Config.Handler.ServeHTTP(w, r)
	checkTemp()
	if w.Code != http.StatusUnsupportedMediaType || body.calls != 0 {
		t.Errorf("multipart login: status %d, reads %d; want 415, 0, 0", w.Code, body.calls)
	}
}

func TestLoginCapsDeclaredAndStreamedForms(t *testing.T) {
	const limit = 256 << 10
	s := newApp(t)
	token := csrfToken(t, s.srv)
	tokenCookie, ok := s.srv.browser.With(t).Cookie(webtest.TokenField)
	if !ok {
		t.Fatal("login form did not set a CSRF cookie")
	}
	for _, declared := range []bool{true, false} {
		form := url.Values{
			"csrf_token": {token}, "email": {testEmail}, "password": {testPassword},
		}
		body := webtest.NewCountingReader(io.MultiReader(
			strings.NewReader(form.Encode()+"&padding="), webtest.Filler(8<<20),
		))
		r := httptest.NewRequest(http.MethodPost, s.srv.URL+"/admin/login", body)
		if declared {
			r.ContentLength = limit + 1
		}
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", s.srv.URL)
		r.AddCookie(&http.Cookie{Name: webtest.TokenField, Value: tokenCookie})
		w := httptest.NewRecorder()
		s.srv.Config.Handler.ServeHTTP(w, r)
		maxRead := int64(limit + (64 << 10))
		if declared {
			maxRead = 0
		}
		if w.Code != http.StatusRequestEntityTooLarge || body.N() > maxRead {
			t.Errorf("declared=%t: status %d, read %d bytes; want 413, at most %d bytes", declared, w.Code, body.N(), maxRead)
		}
	}
	if n, err := s.users.CountSessionsForUser(t.Context(), s.owner.ID); err != nil || n != 0 {
		t.Fatalf("oversized forms created %d sessions (error %v)", n, err)
	}
	signIn(t, s.srv)
}

func TestEventImageBodyLimit(t *testing.T) {
	s := setupApp(t)
	e := createEvent(t, s, "Camp")
	atLimit := make([]byte, blob.MaxUploadBytes)
	copy(atLimit, testJPEG)
	res, _ := uploadImage(t, s, e.ID, atLimit)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("upload at limit: status %d; want 303", res.StatusCode)
	}
	path := s.srv.URL + "/admin/events/" + e.ID + "/image"
	form := csrfPage(t, s.srv)
	token, signedInCookies := form.Page().Token(), form.Request.Cookies()
	for _, tc := range []struct {
		name   string
		signed bool
		stream bool
		origin string
		status int
		msg    string
	}{
		{"staff", true, false, s.srv.URL, http.StatusUnprocessableEntity, "That image is larger than 5 MiB."},
		{"anonymous", false, false, s.srv.URL, http.StatusRequestEntityTooLarge, "Request body too large."},
		{"anonymous streamed", false, true, s.srv.URL, http.StatusUnauthorized, "Sign in required."},
		{"cross origin", true, false, "https://elsewhere.example", http.StatusForbidden, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, contentType := oversizedMultipart(t, "image", token)
			body := &bodyProbe{Reader: data}
			r := httptest.NewRequest(http.MethodPost, path, body)
			r.ContentLength = -1
			if !tc.stream {
				r.ContentLength = int64(data.Len())
			}
			r.Header.Set("Content-Type", contentType)
			r.Header.Set("Origin", tc.origin)
			if tc.signed {
				for _, c := range signedInCookies {
					r.AddCookie(c)
				}
			}
			w := httptest.NewRecorder()
			s.srv.Config.Handler.ServeHTTP(w, r)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.msg) || body.calls != 0 {
				t.Fatalf("oversized upload: status %d, reads %d, body %q; want %d and %q", w.Code, body.calls, excerpt(w.Body.String()), tc.status, tc.msg)
			}
			if tc.status == http.StatusUnprocessableEntity && (!strings.Contains(w.Body.String(), "Camp") || !csrfFieldRE.MatchString(w.Body.String())) {
				t.Fatal("oversize response did not render the editable event page")
			}
		})
	}
	got, err := s.events.Get(t.Context(), e.ID)
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := s.images.Get(got.ImageKey)
	if !ok || !bytes.Equal(obj.Body, atLimit) || len(s.images.Keys()) != 1 {
		t.Fatal("refused request changed the stored image")
	}
}

func oversizedMultipart(t *testing.T, field, token string) (*bytes.Buffer, string) {
	t.Helper()
	var data bytes.Buffer
	m := multipart.NewWriter(&data)
	if err := m.WriteField("csrf_token", token); err != nil {
		t.Fatal(err)
	}
	part, err := m.CreateFormFile(field, "large.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(make([]byte, 6<<20)); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	return &data, m.FormDataContentType()
}

// A script that sends a valid X-CSRF-Token with an oversized upload gets the bare
// 403, not the friendly page: the page is for browser forms, whose token travels
// in the discarded body, and a valid token must never deliver a discarded body to
// the route. (Before the kit, this request got the 422 page.)
func TestEventImageOversizedWithHeaderTokenIsRefused(t *testing.T) {
	s := setupApp(t)
	e := createEvent(t, s, "Camp")
	data, contentType := oversizedMultipart(t, "image", "")
	body := &bodyProbe{Reader: data}
	r := httptest.NewRequest(http.MethodPost, s.srv.URL+"/admin/events/"+e.ID+"/image", body)
	r.ContentLength = int64(data.Len())
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Origin", s.srv.URL)
	form := csrfPage(t, s.srv)
	r.Header.Set("X-CSRF-Token", form.Page().Token())
	for _, c := range form.Request.Cookies() {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.srv.Config.Handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || body.calls != 0 || strings.Contains(w.Body.String(), "larger than") {
		t.Fatalf("oversized upload with a header token: status %d, reads %d, body %q; want a bare 403 and no reads", w.Code, body.calls, excerpt(w.Body.String()))
	}
}

// Only a multipart body gets the image allowance. Any other body on the image
// route keeps the small default cap, so a 5 MiB urlencoded, JSON or text body is
// refused from its declared length without a single read.
func TestEventImageRouteCapsNonMultipartBodies(t *testing.T) {
	s := setupApp(t)
	e := createEvent(t, s, "Camp")
	path := s.srv.URL + "/admin/events/" + e.ID + "/image"
	signedInCookies := csrfPage(t, s.srv).Request.Cookies()
	for _, ct := range []string{"application/x-www-form-urlencoded", "application/json", "text/plain"} {
		t.Run(ct, func(t *testing.T) {
			body := &bodyProbe{}
			r := httptest.NewRequest(http.MethodPost, path, body)
			r.ContentLength = blob.MaxUploadBytes
			r.Header.Set("Content-Type", ct)
			r.Header.Set("Origin", s.srv.URL)
			for _, c := range signedInCookies {
				r.AddCookie(c)
			}
			w := httptest.NewRecorder()
			s.srv.Config.Handler.ServeHTTP(w, r)
			if w.Code != http.StatusRequestEntityTooLarge || body.calls != 0 {
				t.Errorf("status %d, reads %d; want 413, 0", w.Code, body.calls)
			}
		})
	}
}
