// Package payfast implements payment.Gateway for PayFast, the gateway most South
// African stores use.
//
// There is no PayFast SDK for Go, and this package is deliberately not one: it
// covers exactly the two directions this store needs — sending a shopper to pay,
// and authenticating the notification that comes back — over about three hundred
// lines of net/http. The spec is at https://developers.payfast.co.za.
//
// # The signature
//
// Both directions are MD5 over a string of `name=urlencode(value)` pairs joined
// by `&`, with `&passphrase=urlencode(passphrase)` appended when the account has
// a salt passphrase configured. Three details in that sentence are where every
// implementation goes wrong, so they are stated plainly:
//
//   - **The order is the order the fields were submitted in, not alphabetical.**
//     Sorting produces a signature PayFast rejects. This is why payment.Field is
//     a slice of name/value pairs rather than a map anywhere near a signature.
//   - **urlencode is PHP's**, which every reference implementation uses and which
//     the signature is therefore defined in terms of. It differs from Go's
//     url.QueryEscape in one character, and one character is enough to fail.
//   - **Outgoing and incoming disagree about empty values.** A blank field is
//     excluded from the signature when building the redirect form, and *included*
//     when verifying a notification. This is not a symmetry that was overlooked:
//     it is what PayFast's own reference code does in each direction, and the
//     signature has to be computed the way the sender computed it. Building the
//     form sidesteps the asymmetry by not submitting blank fields at all.
package payfast

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/17xande-dev/goevent/internal/payment"
)

// PayFast is ZAR-only, and quietly sending it anything else produces a payment
// in the wrong currency rather than an error.
const Currency = "ZAR"

// MinAmountCents is PayFast's documented minimum transaction. Enforcing it here
// turns a rejection on the gateway's own page — after a pending order already
// exists — into a message on the checkout form.
const MinAmountCents = 500

// itemNameMaxLen is PayFast's limit on item_name. A longer value is truncated
// rather than rejected: the description is cosmetic, and refusing a sale over it
// would be absurd.
const itemNameMaxLen = 100

// DefaultAllowedCIDRs are PayFast's published source ranges for notifications,
// as of 2026-08. They are a *default* and not a constant on purpose: PayFast has
// changed them before, and an operator should be able to add a range by setting
// an environment variable rather than waiting for a release of this project.
//
// Verify against https://support.payfast.co.za/portal/en/kb/articles/what-ip-addresses-does-payfast-use
// when a notification is rejected with ErrSourceIP.
var DefaultAllowedCIDRs = []string{
	"197.97.145.144/28",
	"41.74.179.192/27",
	"102.216.36.0/28",
	"102.216.36.128/28",
	"144.126.193.139/32",
}

// Errors from ParseCallback. Each names one failed check, so the rejection log
// says which one — "the signature did not match" and "that IP is not PayFast"
// are very different events to be looking at.
var (
	ErrMalformed    = errors.New("payfast: notification body is not form-encoded")
	ErrSignature    = errors.New("payfast: notification signature does not match")
	ErrSourceIP     = errors.New("payfast: notification came from an IP outside the allowlist")
	ErrNotValidated = errors.New("payfast: PayFast did not confirm the notification as valid")
	ErrMerchant     = errors.New("payfast: notification is for another merchant")

	// ErrCurrency and ErrAmount are refusals to *send* a payment.
	ErrCurrency = errors.New("payfast: PayFast settles in ZAR only")
	ErrAmount   = fmt.Errorf("payfast: amount is below PayFast's minimum of %s", payment.FormatAmount(MinAmountCents))
)

// Config is everything the gateway needs. The three URLs are absolute and
// public: NotifyURL especially, since PayFast's servers have to reach it — which
// on a laptop means a tunnel, not localhost.
type Config struct {
	MerchantID  string
	MerchantKey string
	// Passphrase is the account's "salt passphrase". It must match the value in
	// the PayFast dashboard exactly: set on one side only, every signature
	// fails.
	Passphrase string
	Sandbox    bool

	ReturnURL string
	CancelURL string
	NotifyURL string

	// AllowedCIDRs restricts where a notification may come from. Empty means
	// DefaultAllowedCIDRs.
	AllowedCIDRs []string
	// AllowAnySourceIP disables the source-IP check. It exists because the
	// sandbox does not necessarily notify from the published production ranges,
	// so testing against it otherwise fails on a check that is not the one being
	// tested. It is logged loudly at startup, and it is never right in
	// production: the check is one of the four things standing between a forged
	// notification and a fulfilled order.
	AllowAnySourceIP bool

	// HTTPClient is used for the server-to-server validation call. A nil client
	// gets one with a timeout, which the default client does not have.
	HTTPClient *http.Client

	// ValidateURL overrides the server-to-server validation endpoint. Only tests
	// set it.
	ValidateURL string

	Log *slog.Logger
}

// Gateway is a configured PayFast client. It is safe for concurrent use.
type Gateway struct {
	cfg         Config
	log         *slog.Logger
	client      *http.Client
	allowed     []netip.Prefix
	processURL  string
	validateURL string
	origin      string
}

// New validates the configuration and returns a gateway. Everything it can check
// up front it checks at startup, because the first time this configuration is
// otherwise exercised is a real shopper trying to pay.
func New(cfg Config) (*Gateway, error) {
	var missing []string
	for _, f := range []struct{ name, value string }{
		{"merchant id", cfg.MerchantID},
		{"merchant key", cfg.MerchantKey},
		{"return URL", cfg.ReturnURL},
		{"cancel URL", cfg.CancelURL},
		{"notify URL", cfg.NotifyURL},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("payfast: missing configuration: %s", strings.Join(missing, ", "))
	}

	cidrs := cfg.AllowedCIDRs
	if len(cidrs) == 0 {
		cidrs = DefaultAllowedCIDRs
	}
	allowed := make([]netip.Prefix, 0, len(cidrs))
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("payfast: allowed CIDR %q: %w", c, err)
		}
		allowed = append(allowed, p)
	}

	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	client := cfg.HTTPClient
	if client == nil {
		// PayFast retries a notification we fail to answer, so a slow validation
		// call should give up rather than hold the request open.
		client = &http.Client{Timeout: 15 * time.Second}
	}

	host := "www.payfast.co.za"
	if cfg.Sandbox {
		host = "sandbox.payfast.co.za"
	}
	origin := "https://" + host

	g := &Gateway{
		cfg:         cfg,
		log:         log,
		client:      client,
		allowed:     allowed,
		processURL:  origin + "/eng/process",
		validateURL: origin + "/eng/query/validate",
		origin:      origin,
	}
	if cfg.ValidateURL != "" {
		g.validateURL = cfg.ValidateURL
	}

	if cfg.Sandbox {
		log.Warn("payfast: sandbox mode — no real money moves", "host", host)
	}
	if cfg.Passphrase == "" {
		// Not fatal: an account without a salt passphrase is a valid, if weaker,
		// configuration, and PayFast will reject a signature that includes one
		// when the account has none.
		log.Warn("payfast: no passphrase configured; set one in the PayFast dashboard and in PAYFAST_PASSPHRASE")
	}
	if cfg.AllowAnySourceIP {
		log.Warn("payfast: source-IP check on notifications is DISABLED")
	}
	return g, nil
}

func (g *Gateway) Name() string { return "payfast" }

func (g *Gateway) Label() string { return "Card, EFT or Instant EFT (PayFast)" }

func (g *Gateway) Currency() string { return Currency }

// CSP: the hand-over is a real cross-origin form post, so PayFast's origin has to
// be in form-action or the browser blocks the submit. It loads no images.
func (g *Gateway) CSP() payment.CSPOrigins {
	return payment.CSPOrigins{FormAction: g.origin}
}

// Handover returns PayFast's process URL and the fields to post to it.
//
// The field order below is the order they are signed in and the order they must
// be submitted in. Blank values are dropped entirely rather than submitted empty:
// PayFast excludes blank fields when it verifies, so submitting one that was not
// signed — or signing one that is not submitted — is a signature mismatch.
func (g *Gateway) Handover(r payment.Request) (payment.Handover, error) {
	if r.Currency != Currency {
		return payment.Handover{}, fmt.Errorf("%w, not %s", ErrCurrency, r.Currency)
	}
	if r.AmountCents < MinAmountCents {
		return payment.Handover{}, ErrAmount
	}
	if r.PaymentID == "" {
		return payment.Handover{}, errors.New("payfast: request has no order id")
	}

	fields := make([]payment.Field, 0, 12)
	add := func(name, value string) {
		if value = strings.TrimSpace(value); value != "" {
			fields = append(fields, payment.Field{Name: name, Value: value})
		}
	}

	add("merchant_id", g.cfg.MerchantID)
	add("merchant_key", g.cfg.MerchantKey)
	add("return_url", payment.ReturnURL(g.cfg.ReturnURL, r.PaymentID))
	add("cancel_url", payment.ReturnURL(g.cfg.CancelURL, r.PaymentID))
	add("notify_url", g.cfg.NotifyURL)
	add("name_first", r.NameFirst)
	add("name_last", r.NameLast)
	add("email_address", r.Email)
	// m_payment_id is our order id coming back on the notification, and the only
	// thing that ties a payment to an order.
	add("m_payment_id", r.PaymentID)
	add("amount", payment.FormatAmount(r.AmountCents))
	add("item_name", truncate(r.ItemName, itemNameMaxLen))

	fields = append(fields, payment.Field{Name: "signature", Value: sign(fields, g.cfg.Passphrase)})
	return payment.Handover{Kind: payment.HandoverPostForm, Action: g.processURL, Fields: fields}, nil
}

// sign is the signature, in both directions: MD5 of the parameter string.
//
// The caller decides which fields are in the slice and in what order, because
// that is exactly what differs between building a form and verifying a
// notification.
func sign(fields []payment.Field, passphrase string) string {
	sum := md5.Sum([]byte(signatureString(fields, passphrase)))
	return hex.EncodeToString(sum[:])
}

// signatureString builds what gets hashed: name=urlencode(value) joined by &, in
// the order given, with the passphrase appended.
//
// It is a separate function because it is the part that goes wrong. A mismatched
// signature says nothing about which of the order, the encoding or the passphrase
// was responsible, so the tests assert on this string directly and the hash is
// then only arithmetic.
func signatureString(fields []payment.Field, passphrase string) string {
	var b strings.Builder
	for i, f := range fields {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(f.Name)
		b.WriteByte('=')
		b.WriteString(urlencode(f.Value))
	}
	if passphrase != "" {
		if b.Len() > 0 {
			b.WriteByte('&')
		}
		b.WriteString("passphrase=")
		b.WriteString(urlencode(strings.TrimSpace(passphrase)))
	}
	return b.String()
}

// urlencode is PHP's urlencode, because that is the function every PayFast
// reference implementation uses and therefore the one the signature is defined
// in terms of: alphanumerics and -_. pass through, a space becomes '+', and
// everything else becomes %XX with uppercase hex.
//
// url.QueryEscape is that function in every respect but one: Go follows RFC 3986
// and treats '~' as unreserved, PHP escapes it to %7E. So the stdlib does the
// encoding and the one disagreement is patched afterwards, which is safe because
// the only literal '~' that can appear in QueryEscape's output came from a
// literal '~' in the input — everything else is already %XX, and '~' cannot
// occur inside a percent-escape.
//
// Getting this wrong is worth one character of care in both directions. Outgoing,
// a tilde in a name or a return URL produces a form PayFast rejects; incoming, it
// produces ErrSignature on a real payment that has already been taken, which is
// indistinguishable in the log from a forgery.
// TestPayFast_URLEncodeMatchesPHPForEveryByte pins the behaviour byte by byte.
func urlencode(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "~", "%7E")
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	// Cut on a rune boundary, so a truncated description is not invalid UTF-8.
	for max > 0 && !isRuneStart(s[max]) {
		max--
	}
	return s[:max]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
