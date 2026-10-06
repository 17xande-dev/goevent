package registrations

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
)

// Signer makes and checks the two credentials a registrant holds: the manage
// link to their registration, and a code per ticket for the door.
//
// Both are an HMAC of the row's id under a server key rather than a random
// secret stored as a hash. A stored hash can only ever be checked, never shown
// again — so the confirmation sent when a pay-later registration is finally
// paid, days after checkout, could not carry the link the registrant already
// has, and re-sending tickets would mean replacing them. Derived from the key,
// a credential can be produced again whenever it is needed and checked without
// a lookup, which is what an offline door scanner wants.
//
// The cost is that nothing is revocable short of changing the key, which
// replaces every link and ticket at once. A cancelled ticket is refused by its
// attendee's status at the door, not by its code.
type Signer struct {
	manage, ticket []byte
}

// macLen is how much of the HMAC a credential carries: 128 bits, which is
// unguessable and keeps a ticket's QR code small.
const macLen = 16

// NewSigner derives the two keys from one secret, with a label each, so a
// manage token can never be presented as a ticket or the reverse.
func NewSigner(secret []byte) Signer {
	derive := func(label string) []byte {
		m := hmac.New(sha256.New, secret)
		m.Write([]byte(label))
		return m.Sum(nil)
	}
	return Signer{manage: derive("goevent/manage-link/v1"), ticket: derive("goevent/ticket/v1")}
}

func mac(key []byte, id string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(id))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:macLen])
}

func equal(a, b string) bool { return hmac.Equal([]byte(a), []byte(b)) }

// ManageToken is the token in a registration's manage link.
func (s Signer) ManageToken(registrationID string) string { return mac(s.manage, registrationID) }

// CheckManage reports whether token belongs to the registration.
func (s Signer) CheckManage(registrationID, token string) bool {
	return token != "" && equal(token, s.ManageToken(registrationID))
}

// TicketCode is what a ticket's QR code holds: the attendee id and its MAC.
func (s Signer) TicketCode(attendeeID string) string {
	return attendeeID + "." + mac(s.ticket, attendeeID)
}

// ParseTicket checks a scanned code and returns the attendee it names. A code
// that was not made by this key — mistyped, forged, or from another event
// system's ticket — is ok=false.
func (s Signer) ParseTicket(code string) (attendeeID string, ok bool) {
	id, sig, found := strings.Cut(strings.TrimSpace(code), ".")
	if !found || id == "" || !equal(sig, mac(s.ticket, id)) {
		return "", false
	}
	return id, true
}
