package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/config"
	"github.com/17xande-dev/goevent/internal/dbtest"
	"github.com/17xande-dev/outbox"
	"github.com/17xande-dev/outbox/pgxstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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

type shutdownJobLog struct {
	slog.Handler
	entered chan struct{}
	release <-chan struct{}
	waiting chan struct{}
}

func (l shutdownJobLog) Enabled(context.Context, slog.Level) bool { return true }

func (l shutdownJobLog) Handle(_ context.Context, r slog.Record) error {
	var outcome string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "outcome" {
			outcome = a.Value.String()
		}
		return true
	})
	if r.Message == "job transition" && outcome == "running" {
		close(l.entered)
		<-l.release
	}
	if r.Message == "waiting for email worker to stop" {
		close(l.waiting)
	}
	return nil
}

func TestGracefulShutdownJoinsEmailWorkerBeforeClosingPool(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	t.Setenv("BASE_URL", "http://localhost:8080")
	t.Setenv("SECRET_KEY", strings.Repeat("01", 32))
	t.Setenv("SETUP_TOKEN", "unused-test-setup-token-fixture-000")
	t.Setenv("IMAGE_DIR", t.TempDir())
	t.Setenv("SMTP_HOST", "localhost")
	t.Setenv("EMAIL_FROM", "events@example.com")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = "0"
	sealer, _, err := deriveKeys(cfg.SecretKey.Reveal())
	if err != nil {
		t.Fatal(err)
	}
	// An independent pool can inspect the settled claim after the server
	// closes the pool it owns.
	observer, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	q, err := outbox.New(pgxstore.New(observer), map[string]outbox.Kind[pgx.Tx]{
		"resend": {Run: func(context.Context, outbox.Job) error { return nil }},
	}, outbox.Options{Sealer: sealer})
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := q.EnqueueNow(ctx, outbox.Message{Kind: "resend", Subject: "registration:00000000-0000-0000-0000-000000000001"})
	if err != nil {
		t.Fatal(err)
	}

	entered, blocked, waiting := make(chan struct{}), make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(blocked) })
	defer release()
	log := slog.New(shutdownJobLog{Handler: slog.DiscardHandler, entered: entered, release: blocked, waiting: waiting})
	done := make(chan error, 1)
	go func() { done <- runServer(ctx, cfg, pool, log) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("server exited before claiming the queued job: %v", err)
	case <-ctx.Done():
		t.Fatal("worker did not claim the queued job")
	}
	cancel()
	// Make one Stop call time out, then check the server still waits.
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("server exited before waiting for the worker: %v", err)
	case <-time.After(8 * time.Second):
		t.Fatal("Stop did not report a timeout")
	}
	select {
	case err := <-done:
		t.Fatalf("server exited before the worker joined: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := pool.Ping(t.Context()); err != nil {
		t.Fatalf("server closed the pool before the worker joined: %v", err)
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("server did not exit after the worker joined")
	}
	if err := pool.Ping(t.Context()); err == nil {
		t.Fatal("server left its pool open")
	}
	info, err := q.Get(t.Context(), id)
	if err != nil || info.State != outbox.StatePending || info.Attempts != 0 {
		t.Fatalf("server did not refund the interrupted claim: %+v %v", info, err)
	}
}
