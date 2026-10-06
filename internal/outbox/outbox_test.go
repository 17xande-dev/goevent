package outbox_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/17xande-dev/goevent/internal/db/gen"
	"github.com/17xande-dev/goevent/internal/dbtest"
	"github.com/17xande-dev/goevent/internal/outbox"
	"github.com/jackc/pgx/v5/pgxpool"
)

// insertRegistration makes the smallest registration an email job can belong to.
func insertRegistration(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var id string
	err := pool.QueryRow(t.Context(), `
WITH e AS (
    INSERT INTO events (slug, title, starts_at, ends_at)
    VALUES ('e-' || substr(md5(random()::text), 1, 8), 'Event', now(), now() + interval '1 hour')
    RETURNING id
)
INSERT INTO registrations (event_id, reference, contact_first_name, contact_last_name, contact_email,
    status, total_cents, currency, hold_expires_at, checkout_key)
SELECT e.id, substr(md5(random()::text), 1, 8), 'Ada', 'Lovelace', 'ada@example.com',
    'confirmed', 100, 'ZAR', now(), 'k'
FROM e RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestWorkersDoNotSendTheSameJobConcurrently(t *testing.T) {
	pool := dbtest.Pool(t)
	s, err := outbox.New(pool, strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	id := insertRegistration(t, pool)
	if err := s.Enqueue(t.Context(), gen.New(pool), id, outbox.KindConfirmation, []byte("tickets")); err != nil {
		t.Fatal(err)
	}
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var calls atomic.Int32
	go func() {
		_, err := s.DeliverOne(t.Context(), func(ctx context.Context, job outbox.Job) error {
			if job.RegistrationID != id || job.Kind != outbox.KindConfirmation || string(job.Payload) != "tickets" {
				t.Errorf("job = %+v", job)
			}
			calls.Add(1)
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		done <- err
	}()
	<-started
	found, err := s.DeliverOne(t.Context(), func(context.Context, outbox.Job) error { calls.Add(1); return nil })
	close(release)
	if firstErr := <-done; firstErr != nil {
		t.Fatal(firstErr)
	}
	if found || err != nil || calls.Load() != 1 {
		t.Fatalf("second worker: found=%v err=%v sends=%d", found, err, calls.Load())
	}
}

func TestWrongKeyAndModifiedCiphertextNeverReachTransport(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		pool := dbtest.Pool(t)
		s, err := outbox.New(pool, strings.Repeat("ab", 32))
		if err != nil {
			t.Fatal(err)
		}
		id := insertRegistration(t, pool)
		if err := s.Enqueue(t.Context(), gen.New(pool), id, outbox.KindConfirmation, []byte("ticket code")); err != nil {
			t.Fatal(err)
		}
		if corrupt {
			// One bit of the stored ciphertext flipped: GCM must refuse it.
			if _, err := pool.Exec(t.Context(),
				`UPDATE email_jobs SET payload = set_byte(payload, length(payload) - 1, get_byte(payload, length(payload) - 1) # 1)`); err != nil {
				t.Fatal(err)
			}
		} else if s, err = outbox.New(pool, strings.Repeat("cd", 32)); err != nil {
			t.Fatal(err)
		}
		called := false
		found, err := s.DeliverOne(t.Context(), func(context.Context, outbox.Job) error {
			called = true
			return errors.New("must not reach transport")
		})
		if !found || err == nil || called {
			t.Fatalf("invalid ciphertext: found=%v err=%v sent=%v", found, err, called)
		}
		jobs, err := s.ForRegistration(t.Context(), id)
		if err != nil || len(jobs) != 1 || jobs[0].SentAt != nil || jobs[0].Attempts != 1 {
			t.Fatalf("job lost: %+v %v", jobs, err)
		}
	}
}
