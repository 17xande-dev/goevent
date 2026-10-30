package main

import (
	"net"
	"net/http"
	"strings"
	"testing"
)

// The server must refuse a request whose headers are far larger than anything the
// application sets or reads: a rate-limit key can be taken from a header.
func TestNewServerCapsRequestHeaders(t *testing.T) {
	var served bool
	srv := newServer(":0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
	}))
	// Serve the very *http.Server newServer built, not a copy of its settings.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	url := "http://" + ln.Addr().String()

	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Forwarded-For", strings.Repeat("a", 200<<10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("oversized request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestHeaderFieldsTooLarge || served {
		t.Errorf("200 KiB header: status %d (served=%v), want 431 and not served", resp.StatusCode, served)
	}

	req, _ = http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ordinary request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !served {
		t.Errorf("ordinary request: status %d (served=%v), want 200", resp.StatusCode, served)
	}
}
