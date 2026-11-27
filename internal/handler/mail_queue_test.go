package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/17xande-dev/mailer"
	"github.com/17xande-dev/outbox"
	"github.com/17xande-dev/outbox/outboxtest"
	"github.com/17xande-dev/outbox/pgxstore"
)

func TestMail_CheckoutRollsBackEnqueuedJobs(t *testing.T) {
	s := newApp(t, withNotify)
	e, _, free := published(t, s, 10)
	ctx := t.Context()
	// Defer this failure until commit, after the confirmation and organiser
	// jobs have been inserted. Neither job is meaningful without the registration.
	if _, err := s.pool.Exec(ctx, `
		CREATE FUNCTION reject_checkout() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'reject checkout'; END $$;
		CREATE CONSTRAINT TRIGGER reject_checkout AFTER INSERT ON registrations
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_checkout()
	`); err != nil {
		t.Fatal(err)
	}
	res, _ := submit(t, s, e, registerForm(t, s, e, map[string]int{free.ID: 1}))
	if res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("checkout status = %d, want 500", res.StatusCode)
	}
	rows, err := s.regs.List(ctx, registrations.Filter{})
	if err != nil || len(rows) != 0 {
		t.Fatalf("registrations after rollback = %d, error = %v", len(rows), err)
	}
	if jobs := outboxtest.Enqueued(t, s.outbox, outbox.Filter{}); len(jobs) != 0 {
		t.Fatalf("%d jobs escaped the checkout rollback", len(jobs))
	}
}

func TestMail_ConfirmationCompletionAndEmailedFlagCommitTogether(t *testing.T) {
	s := newApp(t)
	e, _, free := published(t, s, 10)
	submit(t, s, e, registerForm(t, s, e, map[string]int{free.ID: 1}))
	reg := onlyRegistration(t, s)
	ctx := t.Context()
	// Fail the commit after OnDone would have run, so an update made in
	// another transaction would leak the emailed flag.
	if _, err := s.pool.Exec(ctx, `
		CREATE FUNCTION reject_done() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'reject completion'; END $$;
		CREATE CONSTRAINT TRIGGER reject_done AFTER UPDATE ON outbox_jobs
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
		WHEN (NEW.state = 'done') EXECUTE FUNCTION reject_done()
	`); err != nil {
		t.Fatal(err)
	}
	if n := outboxtest.Drain(t, s.outbox); n != 1 || len(s.mail.To("ada@example.com")) != 1 {
		t.Fatalf("worker ran %d jobs; want one delivered confirmation", n)
	}
	got, err := s.regs.Get(ctx, reg.ID)
	if err != nil || got.Emailed {
		t.Fatalf("emailed after rolled-back completion = %v, error = %v", got.Emailed, err)
	}
	jobs := outboxtest.Enqueued(t, s.outbox, outbox.Filter{Subject: "registration:" + reg.ID})
	if len(jobs) != 1 || jobs[0].State != outbox.StateRunning {
		t.Fatalf("completion should leave the claim recoverable: %+v", jobs)
	}
}

func TestMail_OnceKindsSendOnceWhenEnqueuedTwice(t *testing.T) {
	s := newApp(t, withNotify)
	e, adult, free := published(t, s, 10, func(e *events.Event) {
		e.PayLater, e.PayLaterInstructions = true, "Example Bank"
	})
	submit(t, s, e, registerForm(t, s, e, map[string]int{free.ID: 1}))
	confirmed := onlyRegistration(t, s)
	form := registerForm(t, s, e, map[string]int{adult.ID: 1})
	form.Set("method", string(registrations.MethodLater))
	if res, _ := submit(t, s, e, form); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("pay-later status = %d, want 303", res.StatusCode)
	}

	ctx := t.Context()
	var pendingID string
	if err := s.pool.QueryRow(ctx, "SELECT id FROM registrations WHERE status = 'pending'").Scan(&pendingID); err != nil {
		t.Fatal(err)
	}
	pending, err := s.regs.Get(ctx, pendingID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if err := s.handler.onConfirm()(ctx, tx, registrations.Confirmation{Registration: confirmed}); err != nil {
		t.Fatal(err)
	}
	if err := s.handler.onAwaitingPayment(ctx, tx, pending); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n := outboxtest.Drain(t, s.outbox); n != 3 || len(s.mail.Sent()) != 3 {
		t.Fatalf("ran %d jobs and sent %d emails, want one confirmation, received, and notify", n, len(s.mail.Sent()))
	}
	for kind, reg := range map[string]registrations.Registration{
		"confirmation": confirmed, "received": pending, "notify": confirmed,
	} {
		subject := "registration:" + reg.ID
		jobs := outboxtest.Enqueued(t, s.outbox, outbox.Filter{Subject: subject, Kind: kind})
		if len(jobs) != 1 || jobs[0].UniqueKey != subject+":"+kind || jobs[0].State != outbox.StateDone {
			t.Errorf("%s: want exactly one completed job with the registration's key; got %+v", kind, jobs)
		}
	}
}

type senderFunc func(context.Context, mailer.Message) error

func (f senderFunc) Send(ctx context.Context, m mailer.Message) error { return f(ctx, m) }

func TestMail_WorkersDoNotSendTheSameJobConcurrently(t *testing.T) {
	s := newApp(t)
	e, _, free := published(t, s, 10)
	submit(t, s, e, registerForm(t, s, e, map[string]int{free.ID: 1}))
	var sends atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	s.handler.mail = senderFunc(func(ctx context.Context, m mailer.Message) error {
		if len(m.To) != 1 || m.To[0] != "ada@example.com" || !strings.HasPrefix(m.Subject, "Your tickets for ") {
			t.Error("delivery did not render the confirmation from the registration")
		}
		if sends.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	second, err := s.handler.newMailQueue(pgxstore.New(s.pool), outbox.Options{Sealer: outboxtest.Sealer(t, 1)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- s.outbox.RunDue(ctx) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("first worker never reached the transport")
	}
	// A row lock held across delivery would prevent this update even though
	// the second worker's claim could skip it.
	tag, err := s.pool.Exec(ctx, "UPDATE outbox_jobs SET result = '' WHERE state = 'running'")
	if err != nil || tag.RowsAffected() != 1 {
		close(release)
		<-done
		t.Fatalf("could not update the live claim during delivery: %s %v", tag, err)
	}
	n := second.RunDue(ctx)
	close(release)
	if first := <-done; first != 1 || n != 0 || sends.Load() != 1 {
		t.Fatalf("jobs run = %d and %d, sends = %d; want one delivery", first, n, sends.Load())
	}
}

func TestMail_WrongKeyAndFlippedBitNeverReachTransport(t *testing.T) {
	for _, corruption := range []string{"wrong_key", "flipped_bit"} {
		t.Run(corruption, func(t *testing.T) {
			s := newApp(t)
			e, _, free := published(t, s, 10)
			submit(t, s, e, registerForm(t, s, e, map[string]int{free.ID: 1}))
			reg := onlyRegistration(t, s)
			ctx := t.Context()
			sealer := outboxtest.Sealer(t, 1)
			if corruption == "wrong_key" {
				sealer = outboxtest.Sealer(t, 2)
			} else if _, err := s.pool.Exec(ctx, `
				UPDATE outbox_jobs SET payload = set_byte(payload, length(payload) - 1,
					get_byte(payload, length(payload) - 1) # 1)`); err != nil {
				t.Fatal(err)
			}
			q, err := s.handler.newMailQueue(pgxstore.New(s.pool), outbox.Options{Sealer: sealer})
			if err != nil {
				t.Fatal(err)
			}
			if n := outboxtest.Drain(t, q); n != 1 || len(s.mail.Sent()) != 0 {
				t.Fatalf("ran %d jobs and sent %d emails; want a rejected claim and no delivery", n, len(s.mail.Sent()))
			}
			jobs := outboxtest.Enqueued(t, q, outbox.Filter{Subject: "registration:" + reg.ID})
			if len(jobs) != 1 || jobs[0].State != outbox.StateDead || jobs[0].Attempts != 1 ||
				jobs[0].LastError != "cannot open payload: check the key" {
				t.Fatalf("invalid ciphertext should leave a retryable dead job: %+v", jobs)
			}
		})
	}
}

func TestMail_SealedPayloadAndTransportErrorAreNeverLogged(t *testing.T) {
	s := newApp(t)
	e, _, free := published(t, s, 10)
	submit(t, s, e, registerForm(t, s, e, map[string]int{free.ID: 1}))
	reg := onlyRegistration(t, s)
	ctx := t.Context()
	const payload = "test-payload-marker-0b5497"
	const transport = "test-transport-marker-ad25b1"
	log := outboxtest.NewLog()
	s.handler.log = log.Logger()
	q, err := s.handler.newMailQueue(pgxstore.New(s.pool), outbox.Options{
		Sealer: outboxtest.Sealer(t, 1), Logger: log.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := q.EnqueueNow(ctx, outbox.Message{Kind: "resend", Subject: "registration:" + reg.ID, Payload: []byte(payload)})
	if err != nil {
		t.Fatal(err)
	}
	var sealed []byte
	if err := s.pool.QueryRow(ctx, "SELECT payload FROM outbox_jobs WHERE id=$1", id).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(sealed, []byte{1, 1}) || bytes.Contains(sealed, []byte(payload)) {
		t.Fatal("payload should be stored as AES-GCM ciphertext with key ID 1")
	}
	s.mail.Err = errors.New(transport)
	outboxtest.Drain(t, q)
	job, err := q.Get(ctx, id)
	if err != nil || job.State != outbox.StatePending || job.Attempts != 1 ||
		job.LastError != "Email delivery failed; check mail configuration and retry." {
		t.Fatalf("expected a failed send with a fixed description: %+v %v", job, err)
	}
	if !strings.Contains(log.String(), "job transition") || !strings.Contains(log.String(), "error_class=") {
		t.Fatal("expected the queue to log the transition and error class")
	}
	log.RequireAbsent(t, payload, transport, "registration:"+reg.ID, fmt.Sprintf("%x", sealed),
		base64.StdEncoding.EncodeToString(sealed))

	// A write error may itself contain the sealed row. It must stay out of
	// the request's server-error log as well as out of worker transitions.
	const diagnostic = "test-database-row-marker-cc98"
	if _, err := s.pool.Exec(ctx, `
		CREATE FUNCTION reject_resend() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'test-database-row-marker-cc98 %', encode(NEW.payload, 'hex'); END $$;
		CREATE TRIGGER reject_resend BEFORE INSERT ON outbox_jobs
		FOR EACH ROW EXECUTE FUNCTION reject_resend()
	`); err != nil {
		t.Fatal(err)
	}
	signIn(t, s.srv)
	res, _ := post(t, s.srv, registrationPath(reg.ID)+"/resend", url.Values{})
	if res.StatusCode != http.StatusInternalServerError || !strings.Contains(log.String(), `msg="server error"`) {
		t.Fatalf("resend failure status = %d; want a logged 500", res.StatusCode)
	}
	log.RequireAbsent(t, diagnostic)
}

func TestMail_TransportPanicIsContainedWithoutLoggingItsValue(t *testing.T) {
	s := newApp(t)
	e, _, free := published(t, s, 10)
	submit(t, s, e, registerForm(t, s, e, map[string]int{free.ID: 1}))
	const marker = "test-panic-marker-8b925d"
	s.handler.mail = senderFunc(func(context.Context, mailer.Message) error { panic(marker) })
	log := outboxtest.NewLog()
	q, err := s.handler.newMailQueue(pgxstore.New(s.pool), outbox.Options{
		Sealer: outboxtest.Sealer(t, 1), Logger: log.Logger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := outboxtest.Drain(t, q); n != 1 {
		t.Fatalf("ran %d jobs; want one failed attempt", n)
	}
	jobs := outboxtest.Enqueued(t, q, outbox.Filter{})
	if len(jobs) != 1 || jobs[0].State != outbox.StatePending || jobs[0].Attempts != 1 || onlyRegistration(t, s).Emailed {
		t.Fatalf("panicked delivery should stay retryable: %+v", jobs)
	}
	if !strings.Contains(log.String(), "job callback panicked") {
		t.Fatal("expected a panic log with its location")
	}
	log.RequireAbsent(t, marker)
}
