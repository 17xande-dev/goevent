package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/config"
	"github.com/17xande-dev/goevent/internal/dbtest"
	"github.com/17xande-dev/outbox"
	"github.com/17xande-dev/outbox/pgxstore"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The outbox sealer's key is part of what an operator was promised
// (docs/configuration.md: the HMAC label and key ID 1), so a change to either
// must be a decision, not an accident. A payload sealed under the old
// derivation has to stay readable after an upgrade.

const testSecretHex = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

// independentSealer is the documented derivation, written out separately from
// deriveKeys: HMAC-SHA256(secret, "goevent/outbox/v1") as the AES-256-GCM key,
// key ID 1.
func independentSealer(t *testing.T, secretHex string) outbox.Sealer {
	t.Helper()
	secret, err := hex.DecodeString(secretHex)
	if err != nil {
		t.Fatal(err)
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("goevent/outbox/v1"))
	s, err := outbox.AESGCM(1, map[byte][]byte{1: m.Sum(nil)})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDeriveKeys_OutboxSealerIsTheDocumentedLabelAndKeyID1(t *testing.T) {
	got, _, err := deriveKeys(testSecretHex)
	if err != nil {
		t.Fatal(err)
	}
	want := independentSealer(t, testSecretHex)
	ad := []byte("outbox/v1\x00resend\x00registration:1\x00")

	// Sealed by one, opened by the other, in both directions.
	for name, pair := range map[string][2]outbox.Sealer{"deriveKeys seals": {got, want}, "documented seals": {want, got}} {
		sealed, err := pair[0].Seal([]byte("payload"), ad)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(sealed) < 2 || sealed[0] != 0x01 || sealed[1] != 0x01 {
			t.Errorf("%s: ciphertext starts % x, want version 0x01 and key ID 0x01", name, sealed[:min(2, len(sealed))])
		}
		plain, err := pair[1].Open(sealed, ad)
		if err != nil || !bytes.Equal(plain, []byte("payload")) {
			t.Errorf("%s: the other side opened it as %q, %v", name, plain, err)
		}
	}

	// A different secret must not open it, so the test cannot pass by both
	// sides sharing a constant key.
	other, _, err := deriveKeys(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := got.Seal([]byte("payload"), ad)
	if _, err := other.Open(sealed, ad); err == nil {
		t.Error("a sealer from another secret opened the payload")
	}
}

// The server's queue must be built with that sealer. A queue without one stores
// payloads in the clear and cannot read the sealed ones already in the table.
func TestRunServer_QueueOpensPayloadsSealedWithTheDocumentedKeyAndRefusesOthers(t *testing.T) {
	pool := dbtest.Pool(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	t.Setenv("DATABASE_URL", os.Getenv("TEST_DATABASE_URL"))
	t.Setenv("BASE_URL", "http://localhost:8080")
	t.Setenv("SECRET_KEY", testSecretHex)
	t.Setenv("SETUP_TOKEN", "unused-test-setup-token-fixture-000")
	t.Setenv("IMAGE_DIR", t.TempDir())
	t.Setenv("SMTP_HOST", "localhost")
	t.Setenv("EMAIL_FROM", "events@example.com")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Port = "0"

	// Enqueue before the server starts, through an independent queue, so the
	// only thing under test is what the server's own queue does with the rows.
	observer, err := pgxpool.NewWithConfig(ctx, pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	subject := "registration:00000000-0000-0000-0000-000000000001"
	enqueue := func(sealer outbox.Sealer) int64 {
		t.Helper()
		q, err := outbox.New(pgxstore.New(observer), map[string]outbox.Kind[pgx.Tx]{
			"resend": {Run: func(context.Context, outbox.Job) error { return nil }},
		}, outbox.Options{Sealer: sealer})
		if err != nil {
			t.Fatal(err)
		}
		id, _, err := q.EnqueueNow(ctx, outbox.Message{Kind: "resend", Subject: subject, Payload: []byte("secret")})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	good := enqueue(independentSealer(t, testSecretHex))
	bad := enqueue(independentSealer(t, strings.Repeat("ab", 32)))
	reader, err := outbox.New(pgxstore.New(observer), map[string]outbox.Kind[pgx.Tx]{
		"resend": {Run: func(context.Context, outbox.Job) error { return errors.New("not run here") }},
	}, outbox.Options{})
	if err != nil {
		t.Fatal(err)
	}

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runServer(runCtx, cfg, pool, slog.New(slog.DiscardHandler)) }()
	defer func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("server did not stop")
		}
	}()

	// The wrongly keyed job must die for its key. The right one is opened and
	// handed to the app's handler, which cannot load a registration that does
	// not exist and so fails for a different reason, still pending.
	deadline := time.After(15 * time.Second)
	for {
		g, err := reader.Get(ctx, good)
		if err != nil {
			t.Fatal(err)
		}
		b, err := reader.Get(ctx, bad)
		if err != nil {
			t.Fatal(err)
		}
		if b.State == outbox.StateDead && g.Attempts >= 1 {
			if !strings.Contains(b.LastError, "cannot open payload") {
				t.Errorf("wrongly keyed job died of %q, want a payload that cannot be opened", b.LastError)
			}
			if g.State == outbox.StateDead || strings.Contains(g.LastError, "cannot open payload") {
				t.Errorf("a payload sealed with the documented key was refused: %+v", g)
			}
			return
		}
		select {
		case <-deadline:
			t.Fatalf("the server's queue did not settle both jobs: good %+v, bad %+v", g, b)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
