package handler

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/blob"
)

type bodyProbe struct {
	io.Reader
	calls, bytes int
}

func (b *bodyProbe) Read(p []byte) (int, error) {
	b.calls++
	if b.Reader == nil {
		return 0, errors.New("request body must not be read")
	}
	n, err := b.Reader.Read(p)
	b.bytes += n
	return n, err
}

func TestLoginRejectsMultipartWithoutReading(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	s := newApp(t)
	body := &bodyProbe{}
	r := httptest.NewRequest(http.MethodPost, s.srv.URL+"/admin/login", body)
	r.Header.Set("Content-Type", "multipart/form-data; boundary=test-boundary")
	r.Header.Set("Origin", s.srv.URL)
	w := httptest.NewRecorder()
	s.srv.Config.Handler.ServeHTTP(w, r)
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusUnsupportedMediaType || body.calls != 0 || len(entries) != 0 {
		t.Errorf("multipart login: status %d, reads %d, temp files %d; want 415, 0, 0", w.Code, body.calls, len(entries))
	}
}

func TestLoginCapsDeclaredAndStreamedForms(t *testing.T) {
	const limit = 256 << 10
	s := newApp(t)
	token := csrfToken(t, s.srv)
	origin, err := url.Parse(s.srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, declared := range []bool{true, false} {
		body := &bodyProbe{Reader: strings.NewReader(url.Values{
			"csrf_token": {token}, "email": {testEmail}, "password": {testPassword},
		}.Encode() + "&padding=" + strings.Repeat("x", 8<<20))}
		r := httptest.NewRequest(http.MethodPost, s.srv.URL+"/admin/login", body)
		if declared {
			r.ContentLength = limit + 1
		}
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", s.srv.URL)
		for _, cookie := range s.srv.Client().Jar.Cookies(origin) {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		s.srv.Config.Handler.ServeHTTP(w, r)
		maxRead := limit + (64 << 10)
		if declared {
			maxRead = 0
		}
		if w.Code != http.StatusRequestEntityTooLarge || body.bytes > maxRead {
			t.Errorf("declared=%t: status %d, read %d bytes; want 413, at most %d bytes", declared, w.Code, body.bytes, maxRead)
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
	res, _ = uploadImage(t, s, e.ID, append(atLimit, make([]byte, 1<<20)...))
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("upload over limit: status %d; want 413", res.StatusCode)
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
