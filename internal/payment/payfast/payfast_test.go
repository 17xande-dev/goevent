package payfast

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/payment"
)

// testConfig is a gateway configured with PayFast's own published sandbox
// credentials and passphrase. They are public — they appear in PayFast's
// documentation and in every tutorial — so there is nothing here to keep secret.
func testConfig() Config {
	return Config{
		MerchantID:  "10000100",
		MerchantKey: "46f0cd694581a",
		Passphrase:  "jt7NOE43FZPn",
		Sandbox:     true,
		ReturnURL:   "https://events.example/checkout/success",
		CancelURL:   "https://events.example/checkout/cancel",
		NotifyURL:   "https://events.example/payments/payfast/callback",
	}
}

func testGateway(t *testing.T, edit func(*Config)) *Gateway {
	t.Helper()
	cfg := testConfig()
	if edit != nil {
		edit(&cfg)
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

func testRequest() payment.Request {
	return payment.Request{
		PaymentID:   "3f2504e0-4f89-41d3-9a0c-0305e82c3301",
		AmountCents: 29900,
		Currency:    "ZAR",
		ItemName:    "Family Camp 2026 K7Q-4MZ",
		NameFirst:   "Jane",
		NameLast:    "Doe",
		Email:       "jane@example.com",
	}
}

// TestPayFast_SignatureMatchesKnownVector pins the signature against a fixed
// vector, in two halves, because a mismatched signature otherwise says nothing
// about which part was wrong.
//
// The parameter string is spelled out literally: it is the part with the field
// order, the encoding and the passphrase in it, and reading it is how anyone
// checks this implementation against PayFast's own signature tool at
// https://developers.payfast.co.za. The digest is MD5 of exactly those bytes,
// computed independently of this code.
//
// **Before taking real money, put this string through PayFast's signature tool and
// confirm the digest.** That is a step nobody can do from a test suite, and the
// consequence of skipping it is every payment being rejected.
func TestPayFast_SignatureMatchesKnownVector(t *testing.T) {
	const (
		wantString = "merchant_id=10000100&merchant_key=46f0cd694581a" +
			"&return_url=https%3A%2F%2Fevents.example%2Fcheckout%2Fsuccess%3Fpayment%3D3f2504e0-4f89-41d3-9a0c-0305e82c3301" +
			"&cancel_url=https%3A%2F%2Fevents.example%2Fcheckout%2Fcancel%3Fpayment%3D3f2504e0-4f89-41d3-9a0c-0305e82c3301" +
			"&notify_url=https%3A%2F%2Fevents.example%2Fpayments%2Fpayfast%2Fcallback" +
			"&name_first=Jane&name_last=Doe&email_address=jane%40example.com" +
			"&m_payment_id=3f2504e0-4f89-41d3-9a0c-0305e82c3301" +
			"&amount=299.00&item_name=Family+Camp+2026+K7Q-4MZ" +
			"&passphrase=jt7NOE43FZPn"
		// md5sum of wantString, computed outside Go.
		wantDigest = "5e1aec2ca5c2229b446a4b8351cf5775"
	)

	g := testGateway(t, nil)
	h, err := g.Handover(testRequest())
	fields := h.Fields
	if err != nil {
		t.Fatalf("Handover: %v", err)
	}

	// Everything except the signature is what was signed.
	signed := fields[:len(fields)-1]
	if got := signatureString(signed, g.cfg.Passphrase); got != wantString {
		t.Errorf("parameter string:\n got %s\nwant %s", got, wantString)
	}
	if got := sign(signed, g.cfg.Passphrase); got != wantDigest {
		t.Errorf("signature = %s, want %s", got, wantDigest)
	}

	last := fields[len(fields)-1]
	if last.Name != "signature" {
		t.Fatalf("the last field is %q, want signature — PayFast expects it last", last.Name)
	}
	if last.Value != wantDigest {
		t.Errorf("submitted signature = %s, want %s", last.Value, wantDigest)
	}
}

func TestPayFast_SignatureOrderIsSubmissionOrderNotAlphabetical(t *testing.T) {
	// Sorting the fields is the single most common way to get this wrong: PayFast
	// signs them in the order they were submitted. If the two ever agree, this test
	// is not testing anything, so it asserts they differ.
	fields := []payment.Field{
		{Name: "merchant_id", Value: "10000100"},
		{Name: "amount", Value: "299.00"},
	}
	sorted := []payment.Field{fields[1], fields[0]}

	if sign(fields, "") == sign(sorted, "") {
		t.Fatal("reordering the fields did not change the signature, so order is not being honoured")
	}
	if got, want := signatureString(fields, ""), "merchant_id=10000100&amount=299.00"; got != want {
		t.Errorf("parameter string = %q, want %q", got, want)
	}
}

func TestPayFast_URLEncodeIsPHPs(t *testing.T) {
	// PHP's urlencode, which is what the signature is defined in terms of. The
	// tilde is the one that catches people: Go's url.QueryEscape leaves it alone,
	// PHP escapes it, and one character is a failed signature.
	cases := map[string]string{
		"Jane Doe":              "Jane+Doe",
		"jane@example.com":      "jane%40example.com",
		"https://a.example/b":   "https%3A%2F%2Fa.example%2Fb",
		"a~b":                   "a%7Eb",
		"a-b_c.d":               "a-b_c.d",
		"299.00":                "299.00",
		"Grüße":                 "Gr%C3%BC%C3%9Fe", // UTF-8, byte by byte
		"a+b":                   "a%2Bb",           // a literal plus is not a space
		"50%":                   "50%25",
		"quote\"and'apostrophe": "quote%22and%27apostrophe",
	}
	for in, want := range cases {
		if got := urlencode(in); got != want {
			t.Errorf("urlencode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPayFast_HandoverOmitsBlankFields(t *testing.T) {
	// PayFast excludes blank fields when it verifies a signature, so a blank field
	// that is submitted anyway is a mismatch. Rather than sign one set and submit
	// another, blanks are not submitted at all.
	g := testGateway(t, nil)

	req := testRequest()
	req.NameLast = "" // a shopper with a one-word name
	h, err := g.Handover(req)
	fields := h.Fields
	if err != nil {
		t.Fatalf("Handover: %v", err)
	}

	for _, f := range fields {
		if f.Value == "" {
			t.Errorf("field %q was submitted with an empty value", f.Name)
		}
		if f.Name == "name_last" {
			t.Error("name_last was submitted despite being blank")
		}
	}
	// And the signature covers exactly the fields that are being sent.
	signed := fields[:len(fields)-1]
	if got, want := fields[len(fields)-1].Value, sign(signed, g.cfg.Passphrase); got != want {
		t.Errorf("the signature does not cover the submitted fields: %s != %s", got, want)
	}
}

func TestPayFast_HandoverTargetsTheRightHost(t *testing.T) {
	sandbox := testGateway(t, nil)
	h, err := sandbox.Handover(testRequest())
	action := h.Action
	if err != nil {
		t.Fatalf("Handover: %v", err)
	}
	if action != "https://sandbox.payfast.co.za/eng/process" {
		t.Errorf("sandbox action = %q", action)
	}
	// The CSP has to name this origin, or the browser blocks the hand-over.
	if sandbox.CSP().FormAction != "https://sandbox.payfast.co.za" {
		t.Errorf("sandbox CSP form-action = %q", sandbox.CSP().FormAction)
	}

	live := testGateway(t, func(c *Config) { c.Sandbox = false })
	h, err = live.Handover(testRequest())
	action = h.Action
	if err != nil {
		t.Fatalf("Handover: %v", err)
	}
	if action != "https://www.payfast.co.za/eng/process" {
		t.Errorf("live action = %q", action)
	}
	if live.CSP().FormAction != "https://www.payfast.co.za" {
		t.Errorf("live CSP form-action = %q", live.CSP().FormAction)
	}
}

func TestPayFast_RefusesWhatPayFastWouldReject(t *testing.T) {
	g := testGateway(t, nil)

	cases := []struct {
		name string
		edit func(*payment.Request)
		want error
	}{
		{
			// PayFast settles in ZAR. Sending it anything else produces a payment
			// in the wrong currency rather than an error, which is worse.
			name: "another currency",
			edit: func(r *payment.Request) { r.Currency = "USD" },
			want: ErrCurrency,
		},
		{
			name: "below the minimum",
			edit: func(r *payment.Request) { r.AmountCents = MinAmountCents - 1 },
			want: ErrAmount,
		},
		{
			name: "zero",
			edit: func(r *payment.Request) { r.AmountCents = 0 },
			want: ErrAmount,
		},
	}
	for _, tc := range cases {
		req := testRequest()
		tc.edit(&req)
		if _, err := g.Handover(req); !errors.Is(err, tc.want) {
			t.Errorf("%s: error = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestPayFast_TruncatesItemName(t *testing.T) {
	g := testGateway(t, nil)

	req := testRequest()
	req.ItemName = strings.Repeat("x", itemNameMaxLen+50)
	h, err := g.Handover(req)
	fields := h.Fields
	if err != nil {
		t.Fatalf("Handover: %v", err)
	}

	for _, f := range fields {
		if f.Name == "item_name" && len(f.Value) != itemNameMaxLen {
			t.Errorf("item_name is %d bytes, want %d", len(f.Value), itemNameMaxLen)
		}
	}

	// Truncation cuts on a rune boundary: an item name is not worth emitting
	// invalid UTF-8 over.
	if got := truncate(strings.Repeat("é", 60), 51); !isValidUTF8(got) {
		t.Errorf("truncate produced invalid UTF-8: %q", got)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

func TestPayFast_NewRequiresConfiguration(t *testing.T) {
	// Every one of these is checked at startup rather than at the first checkout,
	// because the first checkout is a real shopper.
	cases := map[string]func(*Config){
		"no merchant id":  func(c *Config) { c.MerchantID = "" },
		"no merchant key": func(c *Config) { c.MerchantKey = "" },
		"no notify URL":   func(c *Config) { c.NotifyURL = "" },
		"no return URL":   func(c *Config) { c.ReturnURL = "" },
		"no cancel URL":   func(c *Config) { c.CancelURL = "" },
		"bad CIDR":        func(c *Config) { c.AllowedCIDRs = []string{"197.97.145.144"} },
	}
	for name, edit := range cases {
		cfg := testConfig()
		edit(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestPayFast_DefaultCIDRsParse(t *testing.T) {
	// The published ranges are a default in code, so a typo in one of them would
	// otherwise only surface when a real notification was rejected.
	g := testGateway(t, nil)
	if len(g.allowed) != len(DefaultAllowedCIDRs) {
		t.Fatalf("%d parsed prefixes, want %d", len(g.allowed), len(DefaultAllowedCIDRs))
	}
}

// TestPayFast_URLEncodeMatchesPHPForEveryByte pins the encoding against PHP's
// rule stated directly, rather than against whatever urlencode happens to do.
//
// It exists because urlencode delegates to url.QueryEscape and patches a single
// character. That is only correct while the stdlib's idea of an unreserved
// character stays where it is, and a change there would otherwise surface as a
// rejected payment rather than a failed test.
func TestPayFast_URLEncodeMatchesPHPForEveryByte(t *testing.T) {
	// PHP: alphanumerics and -_. pass through, space becomes +, everything else
	// becomes %XX in uppercase hex.
	php := func(c byte) string {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
			return string(c)
		case c == ' ':
			return "+"
		default:
			return fmt.Sprintf("%%%02X", c)
		}
	}
	for i := range 256 {
		c := byte(i)
		if got, want := urlencode(string([]byte{c})), php(c); got != want {
			t.Errorf("urlencode(%q) = %q, want %q", string([]byte{c}), got, want)
		}
	}

	// Bytes in company, since the escaping of one must not depend on the last.
	// 0x7E next to a percent-escape is the pairing the tilde patch could get
	// wrong: %7E must never be produced by rewriting an escape's own text.
	for _, s := range []string{"~", "~~", "a~b", "%7E", "~%7E~", "ü~ü", " ~ "} {
		var want strings.Builder
		for i := 0; i < len(s); i++ {
			want.WriteString(php(s[i]))
		}
		if got := urlencode(s); got != want.String() {
			t.Errorf("urlencode(%q) = %q, want %q", s, got, want.String())
		}
	}
}
