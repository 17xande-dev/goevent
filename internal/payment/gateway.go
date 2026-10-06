// Package payment is the gateway-agnostic half of taking money: the interface a
// gateway implements, the values it exchanges with the rest of the store, and a
// fake for tests.
//
// Two real gateways ship, in internal/payment/payfast and
// internal/payment/snapscan, and a store may enable either or both. The split
// exists so that adding a third is a documented extension point rather than a
// fork, and so the handler tests never talk to a payment provider — see fake.go
// and CONTRIBUTING.md.
//
// # What the second gateway changed
//
// This interface was PayFast-shaped until SnapScan arrived, and the differences
// are worth stating because they are the differences between gateways generally,
// not a quirk of one:
//
//   - **A hand-over is not always a form post.** PayFast wants a cross-origin
//     POST of signed fields; SnapScan wants the shopper to open a single URL,
//     which on a phone opens its app and on a desktop is a QR code to scan. Hence
//     Handover, with a Kind, rather than a form and nothing else.
//   - **A notification is not always authenticated from its body.** PayFast signs
//     the body and notifies from published IP ranges; SnapScan puts an HMAC in an
//     Authorization header. Hence Notification, which carries the request's parts
//     rather than just its bytes.
//   - **Status vocabularies differ, and neither belongs in the handler.** PayFast
//     says COMPLETE and CANCELLED, SnapScan says completed and error. Each
//     gateway maps its own words onto Outcome, and the handler only ever reads
//     Outcome.
package payment

import (
	"context"
	"net/http"
)

// Request is an order presented to a gateway for payment. Amounts are integer
// cents, as everywhere else in this project: a float total rounded differently
// from a gateway's amount string is a real and hard-to-find class of bug.
type Request struct {
	// OrderID is this store's own order id. A gateway echoes it back on the
	// callback, and it is how the callback finds the order again.
	OrderID     string
	AmountCents int64
	Currency    string
	// ItemName is a one-line description of the purchase, shown on the
	// gateway's payment page and on the customer's statement. A gateway with
	// nowhere to put it ignores it.
	ItemName string

	NameFirst, NameLast, Email string
}

// Field is one form field. Insertion order is preserved because PayFast's
// signature is computed over the fields in the order they were submitted — not
// alphabetically — so anywhere a signature is computed or emitted this must be a
// slice and never a map.
type Field struct{ Name, Value string }

// HandoverKind is how the shopper reaches the gateway.
type HandoverKind int

const (
	// HandoverPostForm is a cross-origin POST of hidden fields, submitted on
	// load. PayFast.
	HandoverPostForm HandoverKind = iota
	// HandoverLink is a single URL the shopper follows, shown as both a link and
	// a QR code because one page has to serve a phone and a desktop. SnapScan.
	HandoverLink
)

// Handover is how a gateway takes the shopper, and the one place the two shapes
// meet. A template switches on Kind; nothing else needs to know which gateway is
// in play.
type Handover struct {
	Kind HandoverKind

	// Action is the POST target for HandoverPostForm, and the link target for
	// HandoverLink.
	Action string

	// Fields are the hidden fields to post. HandoverPostForm only.
	Fields []Field

	// QRImageURL is an image of Action, for scanning with a phone that is not
	// the device looking at the page. HandoverLink only.
	QRImageURL string
}

// IsLink reports whether this hand-over is a link and a QR code rather than a
// form post. Templates switch on it, because comparing a typed constant to a
// number in a template is both unreadable and quietly wrong when the constants
// are renumbered.
func (h Handover) IsLink() bool { return h.Kind == HandoverLink }

// CSPOrigins is what a gateway needs the Content-Security-Policy to permit.
//
// It is part of the interface, rather than configuration somebody has to
// remember to set, because a gateway whose origin is missing from the policy has
// its hand-over refused by the browser — silently, with a correct-looking page
// and a green test suite. Values are scheme+host and never carry a path: a CSP
// source with a path is an exact match, so a trailing path segment refuses
// everything beneath it. Empty means the gateway needs nothing from that
// directive.
type CSPOrigins struct {
	// FormAction is the origin a redirect form posts to.
	FormAction string
	// ImgSrc is the origin an image is loaded from — a hosted QR code.
	ImgSrc string
}

// Notification is one asynchronous callback as it arrived, before anything has
// been believed about it.
//
// It is a struct rather than three parameters because gateways authenticate from
// different parts of the request: PayFast from the body and the source IP,
// SnapScan from an Authorization header. A gateway reads the parts it needs and
// must treat all of them as hostile until its own checks have passed.
type Notification struct {
	// Body is the raw request body, unmodified. Signatures are computed over
	// exact bytes, so a re-serialised form is not a substitute.
	Body   []byte
	Header http.Header
	// SourceIP is the client address as the middleware resolved it, honouring
	// CLIENT_IP_SOURCE.
	SourceIP string
}

// Outcome is a gateway's verdict, normalised. Only OutcomePaid moves money in
// this store's records.
type Outcome int

const (
	// OutcomePending is the default on purpose: a status this code has not seen
	// before is not evidence that a payment will not arrive, and the safe
	// reading of an unknown word is "not yet", not "never".
	OutcomePending Outcome = iota
	OutcomePaid
	OutcomeFailed
	OutcomeCancelled
)

// Callback is an authenticated asynchronous notification from a gateway,
// normalised into this store's vocabulary.
type Callback struct {
	OrderID string // the gateway's echo of our order id
	Ref     string // the gateway's own payment id
	Status  string // the gateway's own status vocabulary, recorded verbatim
	// Outcome is Status normalised. The mapping lives in the gateway's package,
	// so no gateway's vocabulary reaches the handler.
	Outcome Outcome
	// Amount is the amount as received, kept as a string for the audit trail.
	// AmountCents is the same figure parsed, for comparing against the order.
	Amount      string
	AmountCents int64
	Raw         []byte // the callback body exactly as received, for disputes
}

// Paid reports whether the money is actually taken.
func (c Callback) Paid() bool { return c.Outcome == OutcomePaid }

// Gateway is everything the store needs from a payment provider.
//
// Implementing one is a small job on purpose. The parts that are genuinely
// difficult — proving a callback is real, keeping the order and stock consistent
// — are either inside the implementation or in the store, not spread across the
// handler.
type Gateway interface {
	// Name is the gateway's identifier, used in the callback route
	// (/payments/{gateway}/callback), stored on the order, and submitted by the
	// checkout form when a store offers more than one. It must be stable: it is
	// written into order rows that outlive any release.
	Name() string

	// Label is what the checkout calls this gateway to a shopper.
	Label() string

	// Currency is what this gateway settles in. The server refuses to start when
	// it disagrees with CURRENCY, because discovering it at the first checkout —
	// after an order row already exists — is worse.
	Currency() string

	// Handover builds the hand-over for one order.
	Handover(Request) (Handover, error)

	// CSP is what this gateway needs the Content-Security-Policy to permit.
	CSP() CSPOrigins

	// ParseCallback parses and fully authenticates an asynchronous
	// notification. It returns an error unless the callback is provably genuine;
	// a Callback that comes back without one has been proven to come from the
	// gateway, and nothing beyond that.
	ParseCallback(ctx context.Context, n Notification) (Callback, error)
}
