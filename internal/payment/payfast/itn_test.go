package payfast

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/payment"
)

// validIP is inside PayFast's published ranges (197.97.145.144/28).
const validIP = "197.97.145.150"

// itnFieldsFor returns the fields of a notification in the order PayFast sends
// them, including the empty ones — which matters, because those are part of the
// signature on the way in.
func itnFieldsFor(paymentID string, amount string) []payment.Field {
	return []payment.Field{
		{Name: "m_payment_id", Value: paymentID},
		{Name: "pf_payment_id", Value: "1089250"},
		{Name: "payment_status", Value: StatusComplete},
		{Name: "item_name", Value: "Family Camp 2026 K7Q-4MZ"},
		{Name: "item_description", Value: ""}, // deliberately empty
		{Name: "amount_gross", Value: amount},
		{Name: "amount_fee", Value: "-6.90"},
		{Name: "amount_net", Value: "292.10"},
		{Name: "custom_str1", Value: ""},
		{Name: "name_first", Value: "Jane"},
		{Name: "name_last", Value: "Doe"},
		{Name: "email_address", Value: "jane@example.com"},
		{Name: "merchant_id", Value: "10000100"},
	}
}

// encodeITN writes fields as a form-encoded body, in order, the way PayFast does.
// signWith is the passphrase used for the signature it appends; passing a
// different one to the gateway is how the bad-signature case is built.
func encodeITN(fields []payment.Field, signWith string) []byte {
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(f.Name)
		b.WriteByte('=')
		b.WriteString(urlencode(f.Value))
		b.WriteByte('&')
	}
	b.WriteString("signature=")
	b.WriteString(sign(fields, signWith))
	return []byte(b.String())
}

// validatorSaying starts a stand-in for PayFast's /eng/query/validate endpoint and
// records what was posted to it.
func validatorSaying(t *testing.T, answer string) (url string, received *string) {
	t.Helper()

	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read, _ := io.ReadAll(r.Body)
		body = string(read)
		w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &body
}

// notify wraps a body and a source IP the way the handler does, so a test reads
// the same shape the server passes in.
func notify(body []byte, sourceIP string) payment.Notification {
	return payment.Notification{Body: body, SourceIP: sourceIP}
}

func TestITN_ValidNotification(t *testing.T) {
	validate, received := validatorSaying(t, "VALID")
	g := testGateway(t, func(c *Config) { c.ValidateURL = validate })

	paymentID := "3f2504e0-4f89-41d3-9a0c-0305e82c3301"
	fields := itnFieldsFor(paymentID, "299.00")
	body := encodeITN(fields, g.cfg.Passphrase)

	cb, err := g.ParseCallback(t.Context(), notify(body, validIP))
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}

	if cb.PaymentID != paymentID {
		t.Errorf("PaymentID = %q, want %q", cb.PaymentID, paymentID)
	}
	if cb.Ref != "1089250" {
		t.Errorf("Ref = %q, want the pf_payment_id", cb.Ref)
	}
	if cb.Status != StatusComplete || !cb.Paid() {
		t.Errorf("Status = %q, Paid = %v", cb.Status, cb.Paid())
	}
	// Both forms of the amount: the string for the audit trail, the cents for
	// comparing against the order.
	if cb.Amount != "299.00" || cb.AmountCents != 29900 {
		t.Errorf("Amount = %q, AmountCents = %d", cb.Amount, cb.AmountCents)
	}
	if string(cb.Raw) != string(body) {
		t.Error("Raw is not the body as received, so a dispute has nothing to look at")
	}

	// The confirmation must go back byte for byte. Re-serialising a parsed form
	// reorders fields and re-encodes values, and PayFast then disagrees with it.
	if *received != string(body) {
		t.Errorf("the validation call sent different bytes:\n got %s\nwant %s", *received, body)
	}
}

func TestITN_TemporaryVerificationFailureCanRetry(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		g := testGateway(t, func(c *Config) { c.ValidateURL = srv.URL })
		body := encodeITN(itnFieldsFor("3f2504e0-4f89-41d3-9a0c-0305e82c3301", "299.00"), g.cfg.Passphrase)
		_, err := g.ParseCallback(t.Context(), notify(body, validIP))
		if !errors.Is(err, payment.ErrRetryable) {
			t.Errorf("HTTP %d: %v", code, err)
		}
		srv.Close()
		_, err = g.ParseCallback(t.Context(), notify(body, validIP))
		if !errors.Is(err, payment.ErrRetryable) {
			t.Errorf("connection failure: %v", err)
		}
	}
	endpoint, _ := validatorSaying(t, "INVALID")
	g := testGateway(t, func(c *Config) { c.ValidateURL = endpoint })
	body := encodeITN(itnFieldsFor("3f2504e0-4f89-41d3-9a0c-0305e82c3301", "299.00"), g.cfg.Passphrase)
	_, err := g.ParseCallback(t.Context(), notify(body, validIP))
	if err == nil || errors.Is(err, payment.ErrRetryable) {
		t.Fatalf("explicit rejection: %v", err)
	}
	endpoint, _ = validatorSaying(t, "<html>Temporarily unavailable</html>")
	g = testGateway(t, func(c *Config) { c.ValidateURL = endpoint })
	_, err = g.ParseCallback(t.Context(), notify(body, validIP))
	if !errors.Is(err, payment.ErrRetryable) {
		t.Fatalf("unexpected HTTP 200 body was treated as a permanent rejection: %v", err)
	}
}

func TestITN_IncludesEmptyValuesInTheSignature(t *testing.T) {
	// The asymmetry that costs people an afternoon: building the redirect form
	// *excludes* blank fields, verifying a notification *includes* them. This test
	// pins the incoming direction by signing the same fields with the blanks
	// dropped and requiring that to be rejected.
	validate, _ := validatorSaying(t, "VALID")
	g := testGateway(t, func(c *Config) { c.ValidateURL = validate })

	fields := itnFieldsFor("3f2504e0-4f89-41d3-9a0c-0305e82c3301", "299.00")
	var nonEmpty []payment.Field
	for _, f := range fields {
		if f.Value != "" {
			nonEmpty = append(nonEmpty, f)
		}
	}
	if len(nonEmpty) == len(fields) {
		t.Fatal("the fixture has no empty fields, so this test proves nothing")
	}

	// The body carries every field, but the signature covers only the non-empty
	// ones — which is what a "skip blanks in both directions" implementation would
	// compute, and PayFast does not.
	var b strings.Builder
	for _, f := range fields {
		b.WriteString(f.Name + "=" + urlencode(f.Value) + "&")
	}
	b.WriteString("signature=" + sign(nonEmpty, g.cfg.Passphrase))

	if _, err := g.ParseCallback(t.Context(), notify([]byte(b.String()), validIP)); !errors.Is(err, ErrSignature) {
		t.Errorf("error = %v, want ErrSignature", err)
	}
}

func TestITN_RejectsBadSignature(t *testing.T) {
	validate, received := validatorSaying(t, "VALID")
	g := testGateway(t, func(c *Config) { c.ValidateURL = validate })

	fields := itnFieldsFor("3f2504e0-4f89-41d3-9a0c-0305e82c3301", "299.00")

	cases := map[string][]byte{
		// Somebody who does not know the passphrase.
		"wrong passphrase": encodeITN(fields, "not-the-passphrase"),
		// A tampered amount, signed as it was before the tampering.
		"tampered amount": []byte(strings.Replace(
			string(encodeITN(fields, g.cfg.Passphrase)), "amount_gross=299.00", "amount_gross=1.00", 1)),
		"no signature at all": []byte("m_payment_id=x&merchant_id=10000100"),
		"empty signature":     []byte("m_payment_id=x&merchant_id=10000100&signature="),
	}
	for name, body := range cases {
		_, err := g.ParseCallback(t.Context(), notify(body, validIP))
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		if !errors.Is(err, ErrSignature) && !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: error = %v, want ErrSignature or ErrMalformed", name, err)
		}
	}

	// And the signature is checked before the network call, so a forgery never
	// costs a round trip to PayFast.
	if *received != "" {
		t.Error("a notification with a bad signature was sent to PayFast for validation")
	}
}

func TestITN_RejectsUnknownIP(t *testing.T) {
	validate, _ := validatorSaying(t, "VALID")
	g := testGateway(t, func(c *Config) { c.ValidateURL = validate })
	body := encodeITN(itnFieldsFor("3f2504e0-4f89-41d3-9a0c-0305e82c3301", "299.00"), g.cfg.Passphrase)

	for _, ip := range []string{"198.51.100.7", "197.97.145.143", "197.97.145.160", "", "not-an-ip"} {
		if _, err := g.ParseCallback(t.Context(), notify(body, ip)); !errors.Is(err, ErrSourceIP) {
			t.Errorf("source IP %q: error = %v, want ErrSourceIP", ip, err)
		}
	}

	// Every published range is accepted, including the single-address one.
	for _, ip := range []string{"197.97.145.144", "197.97.145.159", "41.74.179.200", "102.216.36.5", "144.126.193.139"} {
		if _, err := g.ParseCallback(t.Context(), notify(body, ip)); err != nil {
			t.Errorf("source IP %q was rejected: %v", ip, err)
		}
	}
}

func TestITN_AllowAnySourceIPSkipsTheCheck(t *testing.T) {
	// The escape hatch for testing against the sandbox, which does not necessarily
	// notify from the published production ranges.
	validate, _ := validatorSaying(t, "VALID")
	g := testGateway(t, func(c *Config) {
		c.ValidateURL = validate
		c.AllowAnySourceIP = true
	})
	body := encodeITN(itnFieldsFor("3f2504e0-4f89-41d3-9a0c-0305e82c3301", "299.00"), g.cfg.Passphrase)

	if _, err := g.ParseCallback(t.Context(), notify(body, "203.0.113.9")); err != nil {
		t.Errorf("ParseCallback with the check disabled: %v", err)
	}
}

func TestITN_RequiresPayFastToConfirmIt(t *testing.T) {
	body := encodeITN(itnFieldsFor("3f2504e0-4f89-41d3-9a0c-0305e82c3301", "299.00"), testConfig().Passphrase)

	for _, answer := range []string{"INVALID", "", "VALID-ish", "Something went wrong"} {
		validate, _ := validatorSaying(t, answer)
		g := testGateway(t, func(c *Config) { c.ValidateURL = validate })

		if _, err := g.ParseCallback(t.Context(), notify(body, validIP)); !errors.Is(err, ErrNotValidated) {
			t.Errorf("PayFast answering %q: error = %v, want ErrNotValidated", answer, err)
		}
	}

	// A validation endpoint that is down is not a rejection either: the
	// notification may well be genuine, it is simply unconfirmed, and PayFast's
	// retry gets another go.
	unreachable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer unreachable.Close()
	g := testGateway(t, func(c *Config) { c.ValidateURL = unreachable.URL })
	if _, err := g.ParseCallback(t.Context(), notify(body, validIP)); !errors.Is(err, ErrNotValidated) {
		t.Errorf("a failing validator: error = %v, want ErrNotValidated", err)
	}
}

func TestITN_RejectsAnotherMerchant(t *testing.T) {
	validate, _ := validatorSaying(t, "VALID")
	g := testGateway(t, func(c *Config) { c.ValidateURL = validate })

	fields := itnFieldsFor("3f2504e0-4f89-41d3-9a0c-0305e82c3301", "299.00")
	for i := range fields {
		if fields[i].Name == "merchant_id" {
			fields[i].Value = "10000101"
		}
	}
	// Correctly signed, from a valid IP, confirmed by PayFast — and still not ours.
	body := encodeITN(fields, g.cfg.Passphrase)

	if _, err := g.ParseCallback(t.Context(), notify(body, validIP)); !errors.Is(err, ErrMerchant) {
		t.Errorf("error = %v, want ErrMerchant", err)
	}
}

func TestITN_UnpaidStatusesAreNotPaid(t *testing.T) {
	validate, _ := validatorSaying(t, "VALID")
	g := testGateway(t, func(c *Config) { c.ValidateURL = validate })

	for _, status := range []string{"FAILED", "PENDING", "CANCELLED", "complete", "COMPLETED", ""} {
		fields := itnFieldsFor("3f2504e0-4f89-41d3-9a0c-0305e82c3301", "299.00")
		for i := range fields {
			if fields[i].Name == "payment_status" {
				fields[i].Value = status
			}
		}

		cb, err := g.ParseCallback(t.Context(), notify(encodeITN(fields, g.cfg.Passphrase), validIP))
		if err != nil {
			t.Fatalf("status %q: ParseCallback: %v", status, err)
		}
		// Only the exact string COMPLETE means the money is taken. Anything else,
		// including a near miss, is recorded and not credited.
		if cb.Paid() {
			t.Errorf("status %q was treated as paid", status)
		}
		if cb.Status != status {
			t.Errorf("Status = %q, want the gateway's own %q", cb.Status, status)
		}
	}
}

func TestITN_RejectsMalformedBodies(t *testing.T) {
	validate, _ := validatorSaying(t, "VALID")
	g := testGateway(t, func(c *Config) { c.ValidateURL = validate })

	cases := map[string][]byte{
		"empty":            {},
		"not form encoded": []byte("%%%&&&"),
		"oversized":        []byte(strings.Repeat("a=b&", MaxBodyBytes)),
	}
	for name, body := range cases {
		if _, err := g.ParseCallback(t.Context(), notify(body, validIP)); err == nil {
			t.Errorf("%s body was accepted", name)
		}
	}

	// A well-signed notification whose amount is not a plain amount: the figure is
	// compared against a payment's amount, so anything unparseable has to be a refusal
	// rather than a zero.
	fields := itnFieldsFor("3f2504e0-4f89-41d3-9a0c-0305e82c3301", "R 299,00")
	if _, err := g.ParseCallback(t.Context(), notify(encodeITN(fields, g.cfg.Passphrase), validIP)); !errors.Is(err, ErrMalformed) {
		t.Errorf("an unparseable amount: error = %v, want ErrMalformed", err)
	}
}

func TestITN_IgnoresFieldsAfterTheSignature(t *testing.T) {
	// PayFast puts the signature last and its reference implementation stops there,
	// so anything appended after it was not signed and is not read.
	validate, _ := validatorSaying(t, "VALID")
	g := testGateway(t, func(c *Config) { c.ValidateURL = validate })

	fields := itnFieldsFor("3f2504e0-4f89-41d3-9a0c-0305e82c3301", "299.00")
	body := append(encodeITN(fields, g.cfg.Passphrase), []byte("&amount_gross=1.00&merchant_id=evil")...)

	cb, err := g.ParseCallback(t.Context(), notify(body, validIP))
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if cb.AmountCents != 29900 {
		t.Errorf("AmountCents = %d; a field appended after the signature was read", cb.AmountCents)
	}
}

func TestParseAmount(t *testing.T) {
	good := map[string]int64{
		"299.00": 29900,
		"299.0":  29900,
		"299":    29900,
		"0.05":   5,
		"0":      0,
		" 5.00 ": 500,
	}
	for in, want := range good {
		got, err := payment.ParseAmount(in)
		if err != nil || got != want {
			t.Errorf("ParseAmount(%q) = %d, %v; want %d", in, got, err, want)
		}
	}

	// Strict, because this figure decides whether the right amount was paid.
	for _, in := range []string{"", "R299.00", "299.000", "1,299.00", "-5.00", "abc", "299.", "2 99"} {
		if got, err := payment.ParseAmount(in); err == nil {
			t.Errorf("ParseAmount(%q) = %d, want an error", in, got)
		}
	}
}
