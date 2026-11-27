package handler

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/17xande-dev/outbox"
	"github.com/17xande-dev/outbox/outboxtest"
)

// failUntilDead drains the queue while the mail relay refuses everything, past
// every backoff, until each job has used all of its attempts.
func failUntilDead(t *testing.T, s *app) {
	t.Helper()
	s.mail.Err = errors.New("relay unavailable")
	for range 6 {
		outboxtest.Drain(t, s.outbox)
		s.clock.Advance(7 * time.Hour)
	}
}

// deadConfirmation registers one free adult, signs in as the owner, and returns
// the registration with its confirmation email dead.
func deadConfirmation(t *testing.T) (*app, registrations.Registration, outbox.Info) {
	t.Helper()
	s := newApp(t)
	e, _, free := published(t, s, 10)
	submit(t, s, e, registerForm(t, s, e, map[string]int{free.ID: 1}))
	reg := onlyRegistration(t, s)
	signIn(t, s.srv)
	failUntilDead(t, s)
	jobs := outboxtest.Enqueued(t, s.outbox, outbox.Filter{Subject: registrationSubject(reg.ID), State: outbox.StateDead})
	if len(jobs) != 1 {
		t.Fatalf("want one dead confirmation, got %+v", jobs)
	}
	return s, reg, jobs[0]
}

func mailRetryPath(job outbox.Info) string {
	return mailPath + "/" + strconv.FormatInt(job.ID, 10) + "/retry"
}

func TestAdminMail_ListsDeadJobsWithFixedTextCountsAndALinkToTheRegistration(t *testing.T) {
	s, reg, job := deadConfirmation(t)

	res, page := get(t, s.srv, mailPath)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", mailPath, res.StatusCode)
	}
	for _, want := range []string{
		"Dead: 1", "Pending: 0",
		">" + kindConfirmation + "</td>", ">6/6</td>",
		`href="/admin/registrations/` + reg.ID + `"`,
		"Email delivery failed; check mail configuration and retry.",
		mailRetryPath(job),
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the mail page lacks %q", want)
		}
	}
	// What the transport said can carry credentials; only the fixed class text
	// may reach an admin's screen.
	if strings.Contains(page, "relay unavailable") {
		t.Error("the mail page shows the transport's own error text")
	}

	_, events := get(t, s.srv, "/admin/events")
	if !strings.Contains(events, `href="/admin/mail"`) || !strings.Contains(events, "1 email has failed") {
		t.Errorf("the events page does not tell an admin about the dead email: %s", excerpt(events))
	}
}

func TestAdminMail_NothingDeadSaysSoAndTheEventsPageStaysQuiet(t *testing.T) {
	s := newApp(t)
	signIn(t, s.srv)
	_, page := get(t, s.srv, mailPath)
	if !strings.Contains(page, "No dead emails.") || !strings.Contains(page, "Dead: 0") {
		t.Errorf("an empty queue page: %s", excerpt(page))
	}
	if _, events := get(t, s.srv, "/admin/events"); strings.Contains(events, "failed for good") {
		t.Error("the events page warns about dead emails when there are none")
	}
}

// The queue page is reachable from the events page only when something is
// dead; the menu link is what an administrator finds it by the rest of the time.
func TestAdminMail_AdminMenuLinksToTheQueuePage(t *testing.T) {
	s := newApp(t)
	signIn(t, s.srv)
	_, page := get(t, s.srv, "/admin/registrations")
	if !strings.Contains(page, `<a href="/admin/mail">Email</a>`) {
		t.Errorf("the admin menu has no Email link: %s", excerpt(page))
	}
}

func TestAdminMail_RetryNeedsACSRFToken(t *testing.T) {
	s, _, job := deadConfirmation(t)

	form := url.Values{"csrf_token": {""}} // Explicitly empty, so the helper adds none.
	res, _ := post(t, s.srv, mailRetryPath(job), form)
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("retry without a CSRF token = %d, want 403", res.StatusCode)
	}
	if got, err := s.outbox.Get(t.Context(), job.ID); err != nil || got.State != outbox.StateDead {
		t.Errorf("a forged retry changed the job: %+v %v", got, err)
	}
}

func TestAdminMail_ViewerSeesTheListButCannotRetry(t *testing.T) {
	s, _, job := deadConfirmation(t)
	mustAccount(t, s, "viewer@example.com", testPassword, auth.RoleViewer)
	signInAs(t, s.srv, "viewer@example.com", testPassword)

	res, page := get(t, s.srv, mailPath)
	if res.StatusCode != http.StatusOK || strings.Contains(page, mailRetryPath(job)) {
		t.Errorf("viewer GET = %d; offered the retry form = %v", res.StatusCode, strings.Contains(page, mailRetryPath(job)))
	}
	res, _ = post(t, s.srv, mailRetryPath(job), url.Values{})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("a viewer's retry = %d, want 403", res.StatusCode)
	}
	if got, err := s.outbox.Get(t.Context(), job.ID); err != nil || got.State != outbox.StateDead {
		t.Errorf("a viewer's retry changed the job: %+v %v", got, err)
	}

	// Someone who may not even read the registrations must not see the page.
	mustAccount(t, s, "door@example.com", testPassword, auth.RoleCheckin)
	signInAs(t, s.srv, "door@example.com", testPassword)
	if res, _ := get(t, s.srv, mailPath); res.StatusCode != http.StatusForbidden {
		t.Errorf("a check-in volunteer's GET = %d, want 403", res.StatusCode)
	}
}

func TestAdminMail_RetryRequeuesWithAnActorAndAStaleClickSaysSo(t *testing.T) {
	s, _, job := deadConfirmation(t)

	// Through the page's own form, so a form that forgot its CSRF field fails here.
	s.srv.browser.With(t).Page(mailPath).Form(mailRetryPath(job)).Submit().
		WantRedirect(http.StatusSeeOther, mailPath+"?notice=retried")
	got, err := s.outbox.Get(t.Context(), job.ID)
	if err != nil || got.State != outbox.StatePending || got.Attempts != 0 || got.RetryCount != 1 || got.RetriedBy != s.owner.ID {
		t.Fatalf("after retry: %+v %v", got, err)
	}
	_, page := get(t, s.srv, mailPath+"?notice=retried")
	if !strings.Contains(page, "queued to be sent again") || !strings.Contains(page, "No dead emails.") {
		t.Errorf("page after retry: %s", excerpt(page))
	}

	// The same button clicked again, from a page that is out of date.
	res, _ := post(t, s.srv, mailRetryPath(job), url.Values{})
	if loc := res.Header.Get("Location"); loc != mailPath+"?notice=not_dead" {
		t.Errorf("stale retry went to %q", loc)
	}
	if again, _ := s.outbox.Get(t.Context(), job.ID); again.RetryCount != 1 {
		t.Errorf("a stale click retried the job again: %+v", again)
	}
	_, page = get(t, s.srv, mailPath+"?notice=not_dead")
	if !strings.Contains(page, "not dead any more") {
		t.Errorf("a stale click is not explained: %s", excerpt(page))
	}

	s.mail.Err = nil
	outboxtest.Drain(t, s.outbox)
	if len(s.mail.To("ada@example.com")) != 1 {
		t.Errorf("the retried confirmation was not sent: %d emails", len(s.mail.To("ada@example.com")))
	}
}

func TestAdminMail_MoreThanOnePageOfDeadJobsIsCappedAndSaysSo(t *testing.T) {
	s := newApp(t)
	signIn(t, s.srv)
	const total = deadMailLimit + 1
	for i := range total {
		// A subject with no registration behind it: the job dies on its own,
		// and the row has no link rather than a broken one.
		if _, _, err := s.outbox.EnqueueNow(t.Context(), outbox.Message{Kind: kindResend, Subject: "registration:gone-" + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	failUntilDead(t, s)

	_, page := get(t, s.srv, mailPath)
	if rows := strings.Count(page, "/retry\""); rows != deadMailLimit {
		t.Errorf("the page lists %d retry forms, want the cap of %d", rows, deadMailLimit)
	}
	if !strings.Contains(page, "Showing the newest 100 of 101") || !strings.Contains(page, "Dead: 101") {
		t.Errorf("the cap is not shown honestly: %s", excerpt(page))
	}
}
