// Package outbox keeps encrypted email jobs in the payment's database transaction.
// Delivery is at least once: SMTP cannot atomically commit with Postgres.
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

type Store struct {
	pool *pgxpool.Pool
	aead cipher.AEAD
}

func New(pool *pgxpool.Pool, key string) (*Store, error) {
	b, err := hex.DecodeString(key)
	if err != nil || len(b) != 32 {
		return nil, errors.New("EMAIL_QUEUE_KEY must be 64 hexadecimal characters (32 random bytes)")
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

// Message is already encrypted when handed to the confirming transaction. Binding
// ciphertext to registration and kind prevents a moved payload becoming another
// registration's tickets.
type Message struct {
	Kind    string
	Payload []byte
}

func (s *Store) Encrypt(registrationID, kind string, plain []byte) Message {
	return Message{Kind: kind, Payload: s.aead.Seal(nil, nil, plain, []byte(registrationID+":"+kind))}
}

// DeliverOne holds a row lock during a bounded send. It never logs payloads or
// transport error text: either can include a ticket code or credential.
func (s *Store) DeliverOne(ctx context.Context, send func(context.Context, string, []byte) error) (bool, error) {
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
	reason := "Unable to decrypt queued email. Check EMAIL_QUEUE_KEY."
	if deliveryErr == nil {
		sendCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		deliveryErr = send(sendCtx, job.Kind, plain)
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
		if job.Kind == "confirmation" {
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
