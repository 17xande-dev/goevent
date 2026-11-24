package handler

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/ratelimit"
)

func TestFailedLoginDoesNotLogSubmittedAddress(t *testing.T) {
	s := newApp(t)
	var logs bytes.Buffer
	s.handler.log = slog.New(slog.NewJSONHandler(&logs, nil))

	check := func(email string) (string, string) {
		t.Helper()
		logs.Reset()
		w := loginAttempt(t, s, email, "wrong password", 1)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("failed login: status %d, want 401", w.Code)
		}
		logged := logs.String()
		for _, private := range []string{
			email,
			fmt.Sprintf("%x", sha256.Sum256([]byte(email))),
			fmt.Sprintf("%x", sha256.Sum256([]byte(ratelimit.AccountKey(email)))),
		} {
			if strings.Contains(logged, private) {
				t.Fatal("failed-login log contains submitted data")
			}
		}
		if !strings.Contains(logged, `"client_ip":"192.0.2.1"`) {
			t.Error("failed-login log lacks resolved client IP")
		}
		// The form masks the same CSRF cookie with fresh randomness per response.
		return csrfFieldRE.ReplaceAllString(w.Body.String(), `name="csrf_token" value="masked"`), logged
	}

	if _, logged := check("secret-pw-123"); !strings.Contains(logged, `"reason":"unknown account"`) || strings.Contains(logged, `"user_id"`) {
		t.Error("unknown login should log only the outcome and client IP")
	}

	const address = "compare@example.com"
	unknownBody, _ := check(address)
	u := mustAccount(t, s, address, testPassword, auth.RoleOwner)
	knownBody, logged := check(address)
	if !strings.Contains(logged, `"user_id":"`+u.ID+`"`) {
		t.Error("known failed login did not log the user id")
	}
	if knownBody != unknownBody {
		t.Error("known and unknown accounts returned different failure pages")
	}
}
