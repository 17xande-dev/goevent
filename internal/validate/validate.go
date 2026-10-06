// Package validate holds form validation: a field-keyed error map plus one
// function per form, so handlers stay about HTTP and templates can render an
// error next to the input that caused it.
package validate

import (
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"unicode/utf8"
)

// FormErrors maps a form field name to a single message. One message per field
// is deliberate: a form that lists three complaints about the same input is
// harder to act on than the first one.
type FormErrors map[string]string

// Add records a message unless the field already has one.
func (e FormErrors) Add(field, msg string) {
	if _, seen := e[field]; !seen {
		e[field] = msg
	}
}

// Any reports whether the form failed validation.
func (e FormErrors) Any() bool { return len(e) > 0 }

// String renders every message in field order, for logs and command-line tools
// that have no form to render them into.
func (e FormErrors) String() string {
	fields := make([]string, 0, len(e))
	for field := range e {
		fields = append(fields, field)
	}
	slices.Sort(fields)

	var b strings.Builder
	for i, field := range fields {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(field)
		b.WriteString(": ")
		b.WriteString(e[field])
	}
	return b.String()
}

// isEmail is the weakest useful check: one @ with something either side, and no
// spaces. Anything stricter rejects addresses that are perfectly valid — the
// grammar in RFC 5322 permits far more than most validators believe — and the only
// real test of an address is whether mail to it arrives.
func isEmail(s string) bool {
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	local, domain, found := strings.Cut(s, "@")
	return found && local != "" && domain != "" &&
		!strings.Contains(domain, "@") && strings.Contains(domain, ".")
}

func required(e FormErrors, field, value string) {
	if strings.TrimSpace(value) == "" {
		e.Add(field, "Required.")
	}
}

func maxLen(e FormErrors, field, value string, max int) {
	if utf8.RuneCountInString(value) > max {
		e.Add(field, "Too long.")
	}
}

// MinPasswordLength is the shortest admin password accepted.
//
// Length is the only rule. Composition requirements ("one digit, one symbol")
// push people towards short mangled words, while argon2id already makes an
// offline guess expensive. Twelve characters admits a short passphrase, which is
// what we would rather people chose.
//
// It is counted in runes, so a passphrase in a non-Latin script is measured the
// way its writer would measure it rather than in UTF-8 bytes.
const MinPasswordLength = 12

// MaxPasswordLength bounds the other end. Nothing about a password needs to be
// longer than this, and the input is fed straight to argon2id, which allocates
// its 64 MiB and hashes whatever it is given: an unbounded field is a way to
// spend a server's memory and CPU from a form, and the login rate limit counts
// requests rather than bytes. Every other field in this package is bounded for
// less reason than this one.
const MaxPasswordLength = 1024

// Password checks a new password and its confirmation, writing any problems into
// e under "password" and "password_confirm".
//
// It takes the error map rather than returning its own so a caller validating a
// whole form — email, name and password together — reports every problem in one
// pass instead of making somebody fix them one page load at a time.
func Password(e FormErrors, password, confirm string) {
	switch {
	case password == "":
		e.Add("password", "Required.")
	case utf8.RuneCountInString(password) < MinPasswordLength:
		e.Add("password", fmt.Sprintf("Use at least %d characters.", MinPasswordLength))
	case utf8.RuneCountInString(password) > MaxPasswordLength:
		e.Add("password", fmt.Sprintf("Use at most %d characters.", MaxPasswordLength))
	}
	// Only worth reporting once the password itself is acceptable — otherwise a
	// blank pair produces two errors saying the same thing.
	if _, bad := e["password"]; !bad && password != confirm {
		e.Add("password_confirm", "The two passwords do not match.")
	}
}

// AdminUser validates the account half of the new-administrator form. The
// password is validated separately by Password, because resetting a password
// reuses that half on its own.
func AdminUser(email, name string) FormErrors {
	e := FormErrors{}
	required(e, "email", email)
	if email != "" && !isEmail(email) {
		e.Add("email", "Enter a valid email address.")
	}
	maxLen(e, "email", email, 320)
	maxLen(e, "name", name, 200)
	return e
}

// NormalizeEmail reduces an address to its addr-spec, so `Alex <a@example.com>`
// is stored as `a@example.com`. It returns s trimmed but otherwise unchanged if
// it does not parse.
//
// This is not cosmetic. mail.ParseAddress accepts RFC 5322's display-name form,
// and an account stored under the whole string would be a second account for one
// mailbox: it would not collide with the plain address under admin_users'
// lower(email) unique index, so neither that nor auth.ErrEmailTaken would catch
// it — and it could never sign in, because the login form's type=email input will
// not accept the string back.
func NormalizeEmail(s string) string {
	s = strings.TrimSpace(s)
	addr, err := mail.ParseAddress(s)
	if err != nil {
		return s
	}
	return addr.Address
}
