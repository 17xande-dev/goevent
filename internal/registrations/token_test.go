package registrations

import "testing"

func TestSigner(t *testing.T) {
	s := NewSigner([]byte("a key"))
	const id = "3f2504e0-4f89-41d3-9a0c-0305e82c3301"

	tok := s.ManageToken(id)
	if !s.CheckManage(id, tok) {
		t.Error("a manage token does not check against its own registration")
	}
	if s.CheckManage("another-id", tok) || s.CheckManage(id, "") || s.CheckManage(id, tok+"x") {
		t.Error("a manage token checked where it should not")
	}

	code := s.TicketCode(id)
	if got, ok := s.ParseTicket(code); !ok || got != id {
		t.Errorf("ParseTicket(own code) = %q, %v", got, ok)
	}
	if _, ok := s.ParseTicket(id + "." + tok); ok {
		t.Error("a manage token was accepted as a ticket: the two must use different keys")
	}
	if _, ok := NewSigner([]byte("another key")).ParseTicket(code); ok {
		t.Error("a ticket from another key was accepted")
	}
	for _, bad := range []string{"", id, "." + code, id + ".", "x.y"} {
		if _, ok := s.ParseTicket(bad); ok {
			t.Errorf("ParseTicket(%q) accepted", bad)
		}
	}
}
