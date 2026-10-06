package payfast

import (
	"bytes"
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/17xande-dev/goevent/internal/payment"
)

// PayFast's ITN — Instant Transaction Notification — is a form-encoded POST to
// notify_url, and it is the only statement about a payment this store trusts. The
// shopper's browser returning to return_url proves nothing: anyone can navigate
// there without paying.
//
// So ParseCallback's job is to prove the notification is genuine, and it applies
// four independent checks in this order, failing on the first:
//
//  1. The signature recomputes, over the fields exactly as received.
//  2. The source IP is one of PayFast's.
//  3. PayFast itself confirms the notification, when the exact bytes received are
//     posted back to it.
//  4. The merchant id is ours.
//
// Any one of them passing alone would not be enough. The signature can be forged
// by anyone who has the passphrase; the source IP by anyone who can spoof it or
// sits behind the same proxy; the server-to-server check proves the data is
// PayFast's but not that it was meant for us. Together they are the standard
// PayFast validation, and this order puts the cheap local checks before the
// network call.

// MaxBodyBytes caps a notification. PayFast's are well under a kilobyte; the
// limit exists because the callback route is unauthenticated until ParseCallback
// has run, and an unauthenticated endpoint must not be persuadable into buffering
// megabytes. The handler applies the same limit while reading, so the cap is
// enforced before the bytes are held rather than after.
const MaxBodyBytes = 64 << 10

// itnFields are the PayFast field names this store reads.
const (
	fieldSignature   = "signature"
	fieldMerchantID  = "merchant_id"
	fieldPaymentID   = "m_payment_id"
	fieldGatewayRef  = "pf_payment_id"
	fieldStatus      = "payment_status"
	fieldAmountGross = "amount_gross"
)

// StatusComplete is the only payment_status that means the money is taken.
// PayFast also sends FAILED, PENDING and CANCELLED, which are recorded but never
// promote an order to paid.
const StatusComplete = "COMPLETE"

// ParseCallback authenticates one notification and normalises it.
//
// n.Body must be the raw request body, unmodified. Every check below depends on
// the exact bytes: re-serialising a parsed form reorders fields and re-encodes
// values, which invalidates the signature and the server-to-server check both.
// PayFast puts nothing in the headers, so n.Header is unread here — SnapScan is
// where that part of the Notification earns its place.
func (g *Gateway) ParseCallback(ctx context.Context, n payment.Notification) (payment.Callback, error) {
	body, sourceIP := n.Body, n.SourceIP
	if len(body) == 0 {
		return payment.Callback{}, fmt.Errorf("%w: empty body", ErrMalformed)
	}
	if len(body) > MaxBodyBytes {
		return payment.Callback{}, fmt.Errorf("%w: %d bytes", ErrMalformed, len(body))
	}

	signed, got, err := parseITN(body)
	if err != nil {
		return payment.Callback{}, err
	}

	// 1. The signature, over the fields as received and in the order received.
	//    Empty values are included here, unlike when building the redirect form:
	//    that is what the sender did, so it is what verifying has to do.
	want := sign(signed, g.cfg.Passphrase)
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(got)), []byte(want)) != 1 {
		return payment.Callback{}, ErrSignature
	}

	// 2. Source IP.
	if err := g.checkSourceIP(sourceIP); err != nil {
		return payment.Callback{}, err
	}

	// 3. PayFast's own confirmation, with the bytes exactly as they arrived.
	if err := g.confirm(ctx, body); err != nil {
		return payment.Callback{}, err
	}

	// 4. Ours, not somebody else's.
	if merchant := field(signed, fieldMerchantID); merchant != g.cfg.MerchantID {
		return payment.Callback{}, fmt.Errorf("%w: %q", ErrMerchant, merchant)
	}

	amount := field(signed, fieldAmountGross)
	cents, err := payment.ParseAmount(amount)
	if err != nil {
		return payment.Callback{}, fmt.Errorf("%w: %s = %q", ErrMalformed, fieldAmountGross, amount)
	}

	status := field(signed, fieldStatus)
	return payment.Callback{
		OrderID:     field(signed, fieldPaymentID),
		Ref:         field(signed, fieldGatewayRef),
		Status:      status,
		Outcome:     outcome(status),
		Amount:      amount,
		AmountCents: cents,
		Raw:         body,
	}, nil
}

// outcome maps PayFast's payment_status onto the store's vocabulary. Anything
// unrecognised stays pending rather than being called a failure: a status this
// code has not seen before is not evidence that a payment will not arrive.
func outcome(status string) payment.Outcome {
	switch status {
	case StatusComplete:
		return payment.OutcomePaid
	case "CANCELLED":
		return payment.OutcomeCancelled
	case "FAILED":
		return payment.OutcomeFailed
	default:
		return payment.OutcomePending
	}
}

// parseITN splits the body into the fields the signature covers, in the order
// they were sent, plus the signature itself.
//
// url.ParseQuery cannot be used: it returns a map, and the map loses the one
// property the signature depends on. Fields at and after `signature` are
// excluded, which is what PayFast's own reference implementation does — the
// signature is always last, so this only ever matters for a field somebody
// appended after it, and excluding those is the safe reading.
func parseITN(body []byte) (signed []payment.Field, signature string, err error) {
	for pair := range strings.SplitSeq(string(body), "&") {
		if pair == "" {
			continue
		}
		rawName, rawValue, _ := strings.Cut(pair, "=")
		name, nameErr := url.QueryUnescape(rawName)
		value, valueErr := url.QueryUnescape(rawValue)
		if nameErr != nil || valueErr != nil {
			return nil, "", fmt.Errorf("%w: %q", ErrMalformed, pair)
		}
		if name == fieldSignature {
			return signed, value, nil
		}
		signed = append(signed, payment.Field{Name: name, Value: value})
	}
	return nil, "", fmt.Errorf("%w: no %s field", ErrMalformed, fieldSignature)
}

// field returns the first value for a name, or "".
func field(fields []payment.Field, name string) string {
	for _, f := range fields {
		if f.Name == name {
			return f.Value
		}
	}
	return ""
}

// checkSourceIP compares the notification's source against PayFast's published
// ranges.
func (g *Gateway) checkSourceIP(sourceIP string) error {
	if g.cfg.AllowAnySourceIP {
		return nil
	}

	addr, err := netip.ParseAddr(sourceIP)
	if err != nil {
		return fmt.Errorf("%w: %q is not an IP address", ErrSourceIP, sourceIP)
	}
	// An IPv4 address arriving as ::ffff:a.b.c.d would match no IPv4 prefix.
	addr = addr.Unmap()

	for _, p := range g.allowed {
		if p.Contains(addr) {
			return nil
		}
	}
	return fmt.Errorf("%w: %s", ErrSourceIP, addr)
}

// confirm posts the notification back to PayFast and requires it to answer
// VALID.
//
// This is the check a forger cannot pass without PayFast's cooperation, and it
// only works with the *exact* bytes received — hence a body of raw bytes threaded
// through this whole path rather than a parsed form re-encoded.
func (g *Gateway) confirm(ctx context.Context, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.validateURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("payfast: build validation request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	res, err := g.client.Do(req)
	if err != nil {
		// A network failure is not a rejection: the notification may well be
		// genuine. It is still not confirmed, so the order stays pending and
		// PayFast's retry gets another chance.
		return fmt.Errorf("%w: %w: %w", payment.ErrRetryable, ErrNotValidated, err)
	}
	defer res.Body.Close()

	answer, err := io.ReadAll(io.LimitReader(res.Body, 1<<10))
	if err != nil {
		return fmt.Errorf("%w: %w: read response: %w", payment.ErrRetryable, ErrNotValidated, err)
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %w: validation returned %d", payment.ErrRetryable, ErrNotValidated, res.StatusCode)
	}

	// The response is the word VALID or INVALID, sometimes with trailing
	// whitespace and sometimes with more text after it.
	first, _, _ := strings.Cut(strings.TrimSpace(string(answer)), "\n")
	switch strings.TrimSpace(first) {
	case "VALID":
		return nil
	case "INVALID":
		return fmt.Errorf("%w: PayFast answered %q", ErrNotValidated, first)
	default:
		// A proxy error page can carry 200 too. Only an explicit INVALID is a
		// rejection; an unrecognised answer has not finished verification.
		return fmt.Errorf("%w: %w: unrecognised validation response", payment.ErrRetryable, ErrNotValidated)
	}
}
