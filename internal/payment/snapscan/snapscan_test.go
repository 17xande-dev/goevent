package snapscan

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/payment"
)

func testGateway(t *testing.T, edit func(*Config)) *Gateway {
	t.Helper()
	cfg := Config{
		SnapCode:       "shopalot",
		APIKey:         "test-api-key",
		WebhookAuthKey: "test-webhook-key",
		SuccessURL:     "https://store.example/cart/checkout/success",
		FailURL:        "https://store.example/cart/checkout/cancel",
	}
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
		PaymentID:   "5f1e4a2c-0000-4000-8000-00000000abcd",
		AmountCents: 12550,
		Currency:    "ZAR",
		ItemName:    "Shopalot order 5F1E4A2C",
		Email:       "buyer@example.com",
	}
}

// The QR URL is the whole payment: an id that comes back as merchantReference, an
// amount SnapScan enforces, and the two URLs the shopper returns through.
func TestHandover_BuildsThePaymentURL(t *testing.T) {
	g := testGateway(t, nil)

	h, err := g.Handover(testRequest())
	if err != nil {
		t.Fatalf("Handover: %v", err)
	}
	if !h.IsLink() {
		t.Fatalf("Kind = %v, want a link hand-over", h.Kind)
	}

	u, err := url.Parse(h.Action)
	if err != nil {
		t.Fatalf("parse action %q: %v", h.Action, err)
	}
	if u.Host != "pos.snapscan.io" || u.Path != "/qr/shopalot" {
		t.Errorf("action = %q, want the snap code's QR path", h.Action)
	}

	q := u.Query()
	// Cents, not a decimal string: this is the one gateway boundary in the project
	// that does not go through payment.FormatAmount, and sending "125.50" here
	// would ask for R1.25.
	if q.Get("amount") != "12550" {
		t.Errorf("amount = %q, want integer cents", q.Get("amount"))
	}
	if q.Get("id") != testRequest().PaymentID {
		t.Errorf("id = %q, want the order id", q.Get("id"))
	}
	// strict is what refuses a second payment against the same order and an
	// amount below the one asked for.
	if q.Get("strict") != "true" {
		t.Errorf("strict = %q, want true", q.Get("strict"))
	}
	if q.Get("s_url") != "https://store.example/cart/checkout/success?payment="+testRequest().PaymentID {
		t.Errorf("s_url = %q", q.Get("s_url"))
	}
	if q.Get("f_url") != "https://store.example/cart/checkout/cancel?payment="+testRequest().PaymentID {
		t.Errorf("f_url = %q", q.Get("f_url"))
	}
	// No validation key configured, so no signature — an empty one would be
	// refused by SnapScan in front of the shopper.
	if q.Has("signature") {
		t.Errorf("signature = %q with no validation key configured", q.Get("signature"))
	}
}

// The QR image must lead to the same payment as the link, or a shopper scanning
// and a shopper tapping pay different things.
func TestHandover_QRImageMatchesTheLink(t *testing.T) {
	g := testGateway(t, nil)

	h, err := g.Handover(testRequest())
	if err != nil {
		t.Fatalf("Handover: %v", err)
	}

	img, err := url.Parse(h.QRImageURL)
	if err != nil {
		t.Fatalf("parse QR image URL %q: %v", h.QRImageURL, err)
	}
	if img.Path != "/qr/shopalot.svg" {
		t.Errorf("QR image path = %q, want the .svg form", img.Path)
	}
	if got := img.Query().Get("snap_code_size"); got != "260" {
		t.Errorf("snap_code_size = %q, want the configured size", got)
	}

	link, _ := url.Parse(h.Action)
	// The payment itself must be identical, or a shopper scanning and a shopper
	// tapping pay different things.
	for _, k := range []string{"id", "amount", "strict", "signature"} {
		if img.Query().Get(k) != link.Query().Get(k) {
			t.Errorf("%s differs between the image and the link: %q vs %q",
				k, img.Query().Get(k), link.Query().Get(k))
		}
	}

	// The redirect URLs must NOT be on the image. SnapScan's image endpoint
	// answers 403 with an HTML body when they are present, and a browser refuses
	// that as an image — a broken QR code that curl and every handler test call a
	// 200. Found in a browser, kept honest here.
	for _, k := range []string{"s_url", "f_url"} {
		if img.Query().Has(k) {
			t.Errorf("the QR image URL carries %s, which makes SnapScan answer 403", k)
		}
		if !link.Query().Has(k) {
			t.Errorf("the payment link is missing %s", k)
		}
	}
}

// Verified against SnapScan's own TypeScript sample: HMAC-SHA256 over
// "key||amount||id" with the key as the secret, lowercase hex. A signature built
// any other way is refused at the shopper, not here, so it is pinned by value.
func TestSign_MatchesSnapScansReferenceImplementation(t *testing.T) {
	// SnapScan's own sample values, and the digest they produce. Pinned by value
	// because there is no sandbox to discover a wrong one against: the first time
	// a bad signature is noticed is a customer being refused at the till.
	const want = "92f0244c9fbbfab97c3938f3ee6bf507970ab1f143dad20f276e054f9adbb6c4"

	got := Sign("my-validation-key", 10050, "ORDER-001")
	if got != want {
		t.Errorf("Sign = %q, want %q", got, want)
	}
	if got != strings.ToLower(got) {
		t.Errorf("signature %q is not lowercase; SnapScan compares hex digests as sent", got)
	}
}

// The message is what goes wrong, so it is asserted directly: the key appears
// both as the secret and as the message's first element, and the amount is the
// integer cents exactly as the URL carries them.
func TestSign_IsStableAndDependsOnEveryPart(t *testing.T) {
	base := Sign("k", 1000, "ORDER-1")

	if base != Sign("k", 1000, "ORDER-1") {
		t.Error("the same inputs produced two different signatures")
	}
	for name, got := range map[string]string{
		"a different key":    Sign("k2", 1000, "ORDER-1"),
		"a different amount": Sign("k", 1001, "ORDER-1"),
		"a different id":     Sign("k", 1000, "ORDER-2"),
	} {
		if got == base {
			t.Errorf("%s produced the same signature", name)
		}
	}
}

// With a validation key the URL carries a signature, and it signs the values the
// URL actually carries — a mismatch of one character makes SnapScan refuse the
// payment in front of the shopper.
func TestHandover_SignsWhenAValidationKeyIsConfigured(t *testing.T) {
	g := testGateway(t, func(c *Config) { c.ValidationKey = "my-validation-key" })

	h, err := g.Handover(testRequest())
	if err != nil {
		t.Fatalf("Handover: %v", err)
	}
	u, _ := url.Parse(h.Action)
	q := u.Query()

	want := Sign("my-validation-key", 12550, testRequest().PaymentID)
	if q.Get("signature") != want {
		t.Errorf("signature = %q, want %q", q.Get("signature"), want)
	}
	// strict stays on: the signature fixes the amount, and only strict stops the
	// same order being paid twice.
	if q.Get("strict") != "true" {
		t.Error("strict was dropped when a signature was added")
	}
}

func TestHandover_Refusals(t *testing.T) {
	g := testGateway(t, nil)

	for name, tc := range map[string]struct {
		edit func(*payment.Request)
		want error
	}{
		"another currency": {func(r *payment.Request) { r.Currency = "USD" }, ErrCurrency},
		"zero amount":      {func(r *payment.Request) { r.AmountCents = 0 }, ErrAmount},
		"negative amount":  {func(r *payment.Request) { r.AmountCents = -100 }, ErrAmount},
	} {
		t.Run(name, func(t *testing.T) {
			r := testRequest()
			tc.edit(&r)
			if _, err := g.Handover(r); !errors.Is(err, tc.want) {
				t.Errorf("Handover: %v, want %v", err, tc.want)
			}
		})
	}
}

// The CSP has to carry the origin the QR image is loaded from, and must not carry
// a path: a CSP source with one matches that path exactly and refuses every image
// beneath it — invisible outside a browser.
func TestCSP_NamesTheImageOriginOnly(t *testing.T) {
	c := testGateway(t, nil).CSP()

	if c.ImgSrc != "https://pos.snapscan.io" {
		t.Errorf("img-src = %q", c.ImgSrc)
	}
	if strings.Count(c.ImgSrc, "/") != 2 {
		t.Errorf("img-src %q carries a path", c.ImgSrc)
	}
	// A link is a top-level navigation, which form-action does not govern.
	if c.FormAction != "" {
		t.Errorf("form-action = %q, want nothing: this gateway posts no form", c.FormAction)
	}
}

// Missing configuration has to fail at boot. The webhook key especially: without
// it a notification cannot be authenticated at all, and the callback route is the
// only thing that can mark an order paid.
func TestNew_RefusesIncompleteConfiguration(t *testing.T) {
	for name, edit := range map[string]func(*Config){
		"no snap code":   func(c *Config) { c.SnapCode = "" },
		"no API key":     func(c *Config) { c.APIKey = "" },
		"no webhook key": func(c *Config) { c.WebhookAuthKey = "" },
		"no success URL": func(c *Config) { c.SuccessURL = "" },
		"no fail URL":    func(c *Config) { c.FailURL = "" },
		"snap code with a slash": func(c *Config) {
			c.SnapCode = "shop/alot"
		},
		"QR size out of range": func(c *Config) { c.QRSize = 10 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Config{
				SnapCode:       "shopalot",
				APIKey:         "k",
				WebhookAuthKey: "w",
				SuccessURL:     "https://store.example/s",
				FailURL:        "https://store.example/f",
			}
			edit(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("New accepted the configuration")
			}
		})
	}
}
