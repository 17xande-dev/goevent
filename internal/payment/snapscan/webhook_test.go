package snapscan

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/payment"
)

// paymentJSON is SnapScan's payment object, as their documentation prints it.
// Tests edit one field at a time from here so that what a case is about is the
// line that differs.
func paymentJSON(edit func(map[string]any)) map[string]any {
	p := map[string]any{
		"id":                7421,
		"status":            StatusCompleted,
		"date":              "2026-09-22T09:00:00Z",
		"totalAmount":       12550,
		"tipAmount":         0,
		"requiredAmount":    12550,
		"snapCode":          "shopalot",
		"snapCodeReference": "2b1f0a3c-0000-4000-8000-000000000001",
		"userReference":     "A Buyer",
		"merchantReference": "5f1e4a2c-0000-4000-8000-00000000abcd",
		"authCode":          "123456",
		"transactionType":   "payment",
	}
	if edit != nil {
		edit(p)
	}
	return p
}

// encodeWebhook builds the body SnapScan posts: form-encoded, with the payment
// object as a JSON string under `payload`.
func encodeWebhook(t *testing.T, p map[string]any) []byte {
	t.Helper()
	return []byte(url.Values{"payload": {jsonString(t, p)}}.Encode())
}

func jsonString(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return string(b)
}

// signed wraps a body with the Authorization header SnapScan sends for it.
func signed(body []byte, key string) payment.Notification {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	h := http.Header{}
	h.Set("Authorization", authScheme+hex.EncodeToString(mac.Sum(nil)))
	return payment.Notification{Body: body, Header: h}
}

// apiSaying stands in for SnapScan's merchant API. It records what was asked for,
// so a test can assert the confirmation actually happened and carried credentials.
func apiSaying(t *testing.T, status int, body any) (base string, calls *[]*http.Request) {
	t.Helper()
	var received []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = append(received, r.Clone(r.Context()))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if s, ok := body.(string); ok {
			fmt.Fprint(w, s)
			return
		}
		fmt.Fprint(w, jsonString(t, body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &received
}

func gatewayTalkingTo(t *testing.T, base string, edit func(*Config)) *Gateway {
	t.Helper()
	return testGateway(t, func(c *Config) {
		c.BaseURL = base
		if edit != nil {
			edit(c)
		}
	})
}

func TestParseCallback_AcceptsAConfirmedPayment(t *testing.T) {
	base, calls := apiSaying(t, http.StatusOK, paymentJSON(nil))
	g := gatewayTalkingTo(t, base, nil)
	body := encodeWebhook(t, paymentJSON(nil))

	cb, err := g.ParseCallback(t.Context(), signed(body, "test-webhook-key"))
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}

	if cb.OrderID != "5f1e4a2c-0000-4000-8000-00000000abcd" {
		t.Errorf("OrderID = %q, want the merchantReference", cb.OrderID)
	}
	if cb.Ref != "7421" {
		t.Errorf("Ref = %q, want SnapScan's payment id", cb.Ref)
	}
	if !cb.Paid() {
		t.Errorf("Outcome = %v for a completed payment", cb.Outcome)
	}
	if cb.AmountCents != 12550 {
		t.Errorf("AmountCents = %d, want the requiredAmount", cb.AmountCents)
	}
	if cb.Amount != "125.50" {
		t.Errorf("Amount = %q, want the decimal string kept for the audit trail", cb.Amount)
	}
	if string(cb.Raw) != string(body) {
		t.Error("Raw is not the body as received; that column exists for disputes")
	}

	// The confirmation is the point of this design, so assert it happened and
	// carried the API key rather than trusting the parse.
	if len(*calls) != 1 {
		t.Fatalf("made %d API calls, want exactly 1", len(*calls))
	}
	got := (*calls)[0]
	if got.URL.Path != "/merchant/api/v1/payments/7421" {
		t.Errorf("confirmation path = %q", got.URL.Path)
	}
	user, pass, ok := got.BasicAuth()
	if !ok || user != "test-api-key" || pass != "" {
		t.Errorf("confirmation auth = (%q, %q, %v), want the API key as the username", user, pass, ok)
	}
}

// The first check. A body anyone can post is not a payment.
func TestParseCallback_RejectsBadSignatures(t *testing.T) {
	base, calls := apiSaying(t, http.StatusOK, paymentJSON(nil))
	g := gatewayTalkingTo(t, base, nil)
	body := encodeWebhook(t, paymentJSON(nil))

	for name, n := range map[string]payment.Notification{
		"signed with another key": signed(body, "not-the-webhook-key"),
		"no Authorization header": {Body: body, Header: http.Header{}},
		"the scheme alone":        withAuth(body, authScheme),
		"a bare digest":           withAuth(body, strings.TrimPrefix(signed(body, "test-webhook-key").Header.Get("Authorization"), authScheme)),
		"another scheme": withAuth(body, "Bearer "+strings.TrimPrefix(
			signed(body, "test-webhook-key").Header.Get("Authorization"), authScheme)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := g.ParseCallback(t.Context(), n); !errors.Is(err, ErrSignature) {
				t.Errorf("ParseCallback: %v, want %v", err, ErrSignature)
			}
		})
	}

	// Nothing unauthenticated should have reached the network: this endpoint is
	// unauthenticated and rate-limited, and a forged body that still costs an
	// outbound call is an amplifier.
	if len(*calls) != 0 {
		t.Errorf("made %d API calls for notifications that failed the signature check", len(*calls))
	}
}

// A body signed with the right key but altered afterwards must not verify — which
// is only true if the HMAC covers the raw bytes rather than the parsed form.
func TestParseCallback_SignatureCoversTheRawBody(t *testing.T) {
	base, _ := apiSaying(t, http.StatusOK, paymentJSON(nil))
	g := gatewayTalkingTo(t, base, nil)

	body := encodeWebhook(t, paymentJSON(nil))
	n := signed(body, "test-webhook-key")
	n.Body = encodeWebhook(t, paymentJSON(func(p map[string]any) { p["id"] = 9999 }))

	if _, err := g.ParseCallback(t.Context(), n); !errors.Is(err, ErrSignature) {
		t.Errorf("ParseCallback: %v, want %v", err, ErrSignature)
	}
}

// The whole argument for the API call: what the notification says is not what the
// store acts on. A body claiming a completed payment against an API that says
// pending must not pay an order.
func TestParseCallback_BelievesTheAPIAndNotTheNotification(t *testing.T) {
	base, _ := apiSaying(t, http.StatusOK, paymentJSON(func(p map[string]any) {
		p["status"] = StatusPending
	}))
	g := gatewayTalkingTo(t, base, nil)
	body := encodeWebhook(t, paymentJSON(nil)) // claims completed

	cb, err := g.ParseCallback(t.Context(), signed(body, "test-webhook-key"))
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if cb.Paid() {
		t.Error("a notification claiming completed paid an order SnapScan calls pending")
	}
	if cb.Status != StatusPending {
		t.Errorf("Status = %q, want the API's word", cb.Status)
	}
}

// Likewise the amount: a forged body cannot talk the store into crediting more
// than SnapScan says was taken.
func TestParseCallback_TakesTheAmountFromTheAPI(t *testing.T) {
	base, _ := apiSaying(t, http.StatusOK, paymentJSON(nil)) // requiredAmount 12550
	g := gatewayTalkingTo(t, base, nil)
	body := encodeWebhook(t, paymentJSON(func(p map[string]any) {
		p["requiredAmount"] = 1
		p["totalAmount"] = 1
	}))

	cb, err := g.ParseCallback(t.Context(), signed(body, "test-webhook-key"))
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if cb.AmountCents != 12550 {
		t.Errorf("AmountCents = %d, want the confirmed figure", cb.AmountCents)
	}
}

// A tip makes totalAmount larger than what the store asked for. Matching on
// totalAmount would fail the handler's equality check on a perfectly good payment
// — a customer's generosity reading as a mismatched amount.
func TestParseCallback_IgnoresATip(t *testing.T) {
	base, _ := apiSaying(t, http.StatusOK, paymentJSON(func(p map[string]any) {
		p["tipAmount"] = 1000
		p["totalAmount"] = 13550
	}))
	g := gatewayTalkingTo(t, base, nil)

	cb, err := g.ParseCallback(t.Context(), signed(encodeWebhook(t, paymentJSON(nil)), "test-webhook-key"))
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if cb.AmountCents != 12550 {
		t.Errorf("AmountCents = %d, want the requiredAmount and not the tipped total", cb.AmountCents)
	}
}

// Every QR this store builds carries an amount, so a payment without one was made
// against somebody else's code.
func TestParseCallback_RejectsAPaymentWithNoRequiredAmount(t *testing.T) {
	base, _ := apiSaying(t, http.StatusOK, paymentJSON(func(p map[string]any) {
		delete(p, "requiredAmount")
	}))
	g := gatewayTalkingTo(t, base, nil)

	_, err := g.ParseCallback(t.Context(), signed(encodeWebhook(t, paymentJSON(nil)), "test-webhook-key"))
	if !errors.Is(err, ErrMalformed) {
		t.Errorf("ParseCallback: %v, want %v", err, ErrMalformed)
	}
}

// Two stores sharing one deployment's configuration is the realistic version of
// this, and it would otherwise credit orders here against payments made there.
func TestParseCallback_RejectsAnotherMerchantsSnapCode(t *testing.T) {
	base, _ := apiSaying(t, http.StatusOK, paymentJSON(func(p map[string]any) {
		p["snapCode"] = "someoneelse"
	}))
	g := gatewayTalkingTo(t, base, nil)

	_, err := g.ParseCallback(t.Context(), signed(encodeWebhook(t, paymentJSON(nil)), "test-webhook-key"))
	if !errors.Is(err, ErrSnapCode) {
		t.Errorf("ParseCallback: %v, want %v", err, ErrSnapCode)
	}
}

// A refund arriving down the payment webhook would otherwise read as a payment and
// credit an order for money going the other way.
func TestParseCallback_RejectsARefund(t *testing.T) {
	base, _ := apiSaying(t, http.StatusOK, paymentJSON(func(p map[string]any) {
		p["transactionType"] = "refund"
	}))
	g := gatewayTalkingTo(t, base, nil)

	_, err := g.ParseCallback(t.Context(), signed(encodeWebhook(t, paymentJSON(nil)), "test-webhook-key"))
	if !errors.Is(err, ErrNotValidated) {
		t.Errorf("ParseCallback: %v, want %v", err, ErrNotValidated)
	}
}

func TestParseCallback_RejectsAnUnconfirmableNotification(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   any
	}{
		"SnapScan has never heard of the payment": {http.StatusNotFound, map[string]any{"message": "not found"}},
		"the API key was refused":                 {http.StatusUnauthorized, map[string]any{"message": "Unauthorised"}},
		"SnapScan is having a bad afternoon":      {http.StatusInternalServerError, map[string]any{}},
		"the response is not a payment object":    {http.StatusOK, "not json"},
		"the response is somebody else's payment": {http.StatusOK, paymentJSON(func(p map[string]any) { p["id"] = 999 })},
	} {
		t.Run(name, func(t *testing.T) {
			base, _ := apiSaying(t, tc.status, tc.body)
			g := gatewayTalkingTo(t, base, nil)

			_, err := g.ParseCallback(t.Context(), signed(encodeWebhook(t, paymentJSON(nil)), "test-webhook-key"))
			if !errors.Is(err, ErrNotValidated) {
				t.Errorf("ParseCallback: %v, want %v", err, ErrNotValidated)
			}
		})
	}
}

func TestParseCallback_RejectsMalformedBodies(t *testing.T) {
	base, _ := apiSaying(t, http.StatusOK, paymentJSON(nil))
	g := gatewayTalkingTo(t, base, nil)

	for name, body := range map[string][]byte{
		"empty":                    []byte(""),
		"no payload field":         []byte("something=else"),
		"payload is not JSON":      []byte(url.Values{"payload": {"{nope"}}.Encode()),
		"payload names no payment": []byte(url.Values{"payload": {`{"status":"completed"}`}}.Encode()),
		"payload id is zero":       []byte(url.Values{"payload": {`{"id":0}`}}.Encode()),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := g.ParseCallback(t.Context(), signed(body, "test-webhook-key")); !errors.Is(err, ErrMalformed) {
				t.Errorf("ParseCallback: %v, want %v", err, ErrMalformed)
			}
		})
	}

	// Oversized, checked before anything is parsed.
	big := make([]byte, MaxBodyBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if _, err := g.ParseCallback(t.Context(), signed(big, "test-webhook-key")); !errors.Is(err, ErrMalformed) {
		t.Errorf("ParseCallback on an oversized body: %v, want %v", err, ErrMalformed)
	}
}

// SnapScan's vocabulary, mapped once. An unrecognised status stays pending: not
// knowing a payment failed is not the same as knowing it did.
func TestOutcome_MapsSnapScansStatuses(t *testing.T) {
	for status, want := range map[string]payment.Outcome{
		StatusCompleted: payment.OutcomePaid,
		StatusError:     payment.OutcomeFailed,
		StatusPending:   payment.OutcomePending,
		"something new": payment.OutcomePending,
	} {
		if got := outcome(status); got != want {
			t.Errorf("outcome(%q) = %v, want %v", status, got, want)
		}
	}
}

func withAuth(body []byte, header string) payment.Notification {
	h := http.Header{}
	h.Set("Authorization", header)
	return payment.Notification{Body: body, Header: h}
}
