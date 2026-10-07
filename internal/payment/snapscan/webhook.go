package snapscan

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/17xande-dev/goevent/internal/payment"
)

// MaxBodyBytes caps what ParseCallback will look at. A notification is a few
// hundred bytes of JSON inside a form field; anything larger is not one.
const MaxBodyBytes = 64 << 10

// SnapScan's payment statuses. Only completed means the money is taken; the
// webhook fires for completed and error only, but a payment read back from the
// API can also be pending.
const (
	StatusCompleted = "completed"
	StatusError     = "error"
	StatusPending   = "pending"
)

// authScheme is the prefix of the Authorization header SnapScan sends.
const authScheme = "SnapScan signature="

// ParseCallback authenticates one notification and normalises it.
//
// Two independent things have to be true before this returns without an error,
// and neither is sufficient alone:
//
//  1. **The body is signed with the webhook key.** That proves the request came
//     from someone holding the shared secret — but SnapScan's own documentation
//     calls the webhook an unauthenticated event stream, and a shared secret is
//     only as good as everywhere it has ever been copied.
//  2. **SnapScan's API says the same thing.** The payment is read back over an
//     authenticated connection, and the status and amount this server acts on come
//     from *that* response, never from the notification body. A forged body that
//     somehow cleared the first check still cannot invent a completed payment.
//
// This is the same shape as the PayFast implementation's server-to-server
// confirmation, for the same reason, and it is the whole argument for the
// network call: without it, one leaked key is a free registration.
func (g *Gateway) ParseCallback(ctx context.Context, n payment.Notification) (payment.Callback, error) {
	if len(n.Body) == 0 {
		return payment.Callback{}, fmt.Errorf("%w: empty body", ErrMalformed)
	}
	if len(n.Body) > MaxBodyBytes {
		return payment.Callback{}, fmt.Errorf("%w: %d bytes", ErrMalformed, len(n.Body))
	}

	// 1. The signature, over the raw body exactly as it arrived — the whole
	//    form-encoded body including the payload key, not the JSON inside it.
	if err := g.verifySignature(n.Header.Get("Authorization"), n.Body); err != nil {
		return payment.Callback{}, err
	}

	notified, err := parsePayload(n.Body)
	if err != nil {
		return payment.Callback{}, err
	}

	// 2. SnapScan's own answer, fetched by the id the notification named. Note
	//    what is taken from where: the id comes from the notification, and every
	//    fact acted on comes from the response.
	confirmed, err := g.getPayment(ctx, notified.ID)
	if err != nil {
		return payment.Callback{}, err
	}

	// 3. Ours, not somebody else's. Two sites sharing one deployment's
	//    configuration is the realistic version of this, and it would otherwise
	//    credit registrations here against payments made there.
	if !strings.EqualFold(confirmed.SnapCode, g.cfg.SnapCode) {
		return payment.Callback{}, fmt.Errorf("%w: %q", ErrSnapCode, confirmed.SnapCode)
	}

	// The amount matched against the payment is requiredAmount, the figure this
	// server asked for — not totalAmount, which includes a tip on an account with
	// tipping enabled and would then fail the handler's equality check on a
	// perfectly good payment. Its absence means the payment was not made against
	// a QR this server generated, since every one of ours carries an amount.
	if confirmed.RequiredAmount == nil {
		return payment.Callback{}, fmt.Errorf("%w: payment %d has no requiredAmount", ErrMalformed, confirmed.ID)
	}
	cents := *confirmed.RequiredAmount

	return payment.Callback{
		PaymentID: confirmed.MerchantReference,
		// SnapScan's payment id is an integer; everything downstream keys on
		// strings, and the unique index on (gateway, gateway_ref) does not care
		// which.
		Ref:         fmt.Sprintf("%d", confirmed.ID),
		Status:      confirmed.Status,
		Outcome:     outcome(confirmed.Status),
		Amount:      payment.FormatAmount(cents),
		AmountCents: cents,
		// The body as received, not the confirmation: this column exists for
		// disputes, and what arrived is the thing in dispute.
		Raw: n.Body,
	}, nil
}

// verifySignature checks the Authorization header against an HMAC of the raw
// body.
//
// The comparison is over the whole header value rather than the hex digest
// alone, which is what SnapScan's reference implementation does, and it is
// constant time: a byte-at-a-time comparison of a secret-derived value is a
// timing oracle for forging one.
func (g *Gateway) verifySignature(header string, body []byte) error {
	if header == "" {
		return fmt.Errorf("%w: no Authorization header", ErrSignature)
	}
	mac := hmac.New(sha256.New, []byte(g.cfg.WebhookAuthKey))
	mac.Write(body)
	want := authScheme + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(header), []byte(want)) {
		return ErrSignature
	}
	return nil
}

// notification is the part of the webhook body this server reads. Everything
// acted on is re-read from the API, so this exists only to find the payment
// again — which is why it names one field.
type notification struct {
	ID int64 `json:"id"`
}

// parsePayload unwraps the form-encoded body and the JSON inside its payload
// key. SnapScan posts application/x-www-form-urlencoded with the whole payment
// object as a JSON string under `payload`, which is unusual enough to be worth
// naming here rather than leaving a reader to infer it.
func parsePayload(body []byte) (notification, error) {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return notification{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	raw := values.Get("payload")
	if raw == "" {
		return notification{}, fmt.Errorf("%w: no payload field", ErrMalformed)
	}

	var n notification
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		return notification{}, fmt.Errorf("%w: payload is not JSON: %v", ErrMalformed, err)
	}
	if n.ID <= 0 {
		return notification{}, fmt.Errorf("%w: payload names no payment id", ErrMalformed)
	}
	return n, nil
}

// outcome maps SnapScan's status onto the server's vocabulary.
//
// There is no cancelled: SnapScan reports an abandoned payment as an error, or
// not at all. Anything unrecognised stays pending rather than being called a
// failure — a status this code has not seen before is not evidence that a
// payment will not arrive.
func outcome(status string) payment.Outcome {
	switch status {
	case StatusCompleted:
		return payment.OutcomePaid
	case StatusError:
		return payment.OutcomeFailed
	default:
		return payment.OutcomePending
	}
}
