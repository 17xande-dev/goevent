// Package outbox keeps email jobs in the database, written inside the
// transaction that makes them true — a confirmation commits with the payment
// that confirmed it, or neither does — and delivered by a worker afterwards.
// Delivery is at least once: SMTP cannot atomically commit with Postgres.
//
// A job names a registration and a kind of email; the worker renders it from the
// database when it sends. Its payload is still encrypted, under a key derived
// from SECRET_KEY, so a caller that does put something sensitive in one — a
// note to the registrant, say — does not have to remember to.
package outbox

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/17xande-dev/goevent/internal/db/gen"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The kinds of email, which the schema's CHECK constraint also lists.
const (
	// KindConfirmation is the registrant's confirmation, with their tickets.
	KindConfirmation = "confirmation"
	// KindReceived acknowledges a pay-later registration: how to pay, no tickets.
	KindReceived = "received"
	// KindNotify is the organiser's copy of a confirmed registration.
	KindNotify = "notify"
	// KindResend sends the tickets again, at an administrator's request.
	KindResend = "resend"
)

type Store struct {
	pool *pgxpool.Pool
	aead cipher.AEAD
}

func New(pool *pgxpool.Pool, key string) (*Store, error) {
	b, err := hex.DecodeString(key)
	if err != nil || len(b) != 32 {
		return nil, errors.New("outbox: the key must be 64 hexadecimal characters (32 bytes)")
	}
	block, err := aes.NewCipher(b)
	if err != nil {
		return nil, err
	}
	// The stdlib generates a fresh random nonce and prepends it to the ciphertext.
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool, aead: aead}, nil
}

// Enqueue writes a job inside the caller's transaction, q. The ciphertext is
// bound to its registration and kind, so a payload moved to another row fails
// to decrypt rather than becoming somebody else's email.
func (s *Store) Enqueue(ctx context.Context, q *gen.Queries, registrationID, kind string, payload []byte) error {
	sealed := s.aead.Seal(nil, nil, payload, []byte(registrationID+":"+kind))
	if err := q.EnqueueEmail(ctx, gen.EnqueueEmailParams{
		RegistrationID: registrationID, Kind: kind, Payload: sealed,
	}); err != nil {
		return fmt.Errorf("outbox: enqueue %s: %w", kind, err)
	}
	return nil
}

// Queue writes a job on its own, for an email nothing else has to commit with:
// an administrator's "send the tickets again".
func (s *Store) Queue(ctx context.Context, registrationID, kind string, payload []byte) error {
	return s.Enqueue(ctx, gen.New(s.pool), registrationID, kind, payload)
}

// Job is one email to send, decrypted.
type Job struct {
	RegistrationID string
	Kind           string
	Payload        []byte
}

// DeliverOne sends the next due job, holding its row lock during a bounded
// send so two workers never send the same one. It never logs payloads or
// transport error text: either can carry a ticket code or a credential.
func (s *Store) DeliverOne(ctx context.Context, send func(context.Context, Job) error) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := gen.New(tx)
	job, err := q.NextEmail(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	plain, deliveryErr := s.aead.Open(nil, nil, job.Payload, []byte(job.RegistrationID+":"+job.Kind))
	reason := "Unable to decrypt queued email. Check SECRET_KEY."
	if deliveryErr == nil {
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		deliveryErr = send(sendCtx, Job{RegistrationID: job.RegistrationID, Kind: job.Kind, Payload: plain})
		cancel()
		reason = "Email delivery failed; check mail configuration and retry."
	}
	if deliveryErr != nil {
		if err := q.FailEmail(ctx, gen.FailEmailParams{ID: job.ID, LastError: reason}); err != nil {
			return true, err
		}
	} else {
		if err := q.CompleteEmail(ctx, job.ID); err != nil {
			return true, err
		}
		if job.Kind == KindConfirmation {
			if err := q.MarkRegistrationEmailed(ctx, job.RegistrationID); err != nil {
				return true, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return true, err
	}
	if deliveryErr != nil {
		return true, fmt.Errorf("email job %d: %s", job.ID, reason)
	}
	return true, nil
}

type Status = gen.ListRegistrationEmailsRow

func (s *Store) ForRegistration(ctx context.Context, registrationID string) ([]Status, error) {
	return gen.New(s.pool).ListRegistrationEmails(ctx, registrationID)
}

func (s *Store) Retry(ctx context.Context, registrationID string) error {
	_, err := gen.New(s.pool).RetryRegistrationEmails(ctx, registrationID)
	return err
}
