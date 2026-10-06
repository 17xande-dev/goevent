package validate

import (
	"testing"
)

func TestFormErrors(t *testing.T) {
	e := FormErrors{}
	if e.Any() {
		t.Error("an empty FormErrors reports errors")
	}

	e.Add("title", "Required.")
	e.Add("title", "Something else.")
	if got := e["title"]; got != "Required." {
		t.Errorf("title = %q, want the first message to win", got)
	}

	e.Add("slug", "Bad.")
	if got, want := e.String(), "slug: Bad.; title: Required."; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestPassword(t *testing.T) {
	cases := []struct {
		name, password, confirm string
		wantFields              []string
	}{
		{"good", "correct horse battery", "correct horse battery", nil},
		// Twelve runes exactly, so the boundary is inclusive.
		{"exactly the minimum", "abcdefghijkl", "abcdefghijkl", nil},
		{"one short", "abcdefghijk", "abcdefghijk", []string{"password"}},
		{"empty", "", "", []string{"password"}},
		{"mismatch", "correct horse battery", "correct horse batery", []string{"password_confirm"}},
		// A blank pair must not produce two errors saying the same thing: the
		// mismatch is only worth reporting once the password itself is acceptable.
		{"empty and mismatched", "", "something", []string{"password"}},
		// Counted in runes, not bytes: this is 12 characters and 36 bytes, so a
		// byte count would wrongly accept a shorter passphrase in this script.
		{"twelve non-Latin runes", "日本語日本語日本語日本語", "日本語日本語日本語日本語", nil},
		{"eleven non-Latin runes", "日本語日本語日本語日本", "日本語日本語日本語日本", []string{"password"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := FormErrors{}
			Password(e, c.password, c.confirm)
			if len(e) != len(c.wantFields) {
				t.Fatalf("Password(%q, %q) = %v, want errors on %v", c.password, c.confirm, e, c.wantFields)
			}
			for _, field := range c.wantFields {
				if _, ok := e[field]; !ok {
					t.Errorf("no error on %q; got %v", field, e)
				}
			}
		})
	}
}

func TestNormalizeEmail(t *testing.T) {
	cases := []struct{ in, want string }{
		{"alex@example.com", "alex@example.com"},
		{"  alex@example.com  ", "alex@example.com"},
		// The case this exists for. Stored whole, the display-name form would be a
		// second account for one mailbox: it does not collide with the plain
		// address under admin_users' lower(email) index, and it could never sign
		// in, because the login form's type=email input will not accept it back.
		{"Alex <alex@example.com>", "alex@example.com"},
		{`"Alex F" <alex@example.com>`, "alex@example.com"},
		// Unparseable input comes back trimmed but otherwise untouched, so the
		// validator gets to be the one that reports it.
		{"not an address", "not an address"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeEmail(c.in); got != c.want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAdminUser(t *testing.T) {
	if e := AdminUser("alex@example.com", "Alex"); e.Any() {
		t.Errorf("a valid account reported %v", e)
	}
	if e := AdminUser("", "Alex"); !e.Any() {
		t.Error("a missing email was accepted")
	}
	if e := AdminUser("not an address", "Alex"); !e.Any() {
		t.Error("a malformed email was accepted")
	}
	// A name is optional: the display falls back to the address, and demanding
	// one is a field to argue with rather than a property anything depends on.
	if e := AdminUser("alex@example.com", ""); e.Any() {
		t.Errorf("a missing name reported %v", e)
	}
	// The display-name form only reaches validation after NormalizeEmail, which
	// is the order the handler uses; on its own it has a space and is refused.
	if e := AdminUser(NormalizeEmail("Alex <alex@example.com>"), "Alex"); e.Any() {
		t.Errorf("a normalised display-name address reported %v", e)
	}
}
