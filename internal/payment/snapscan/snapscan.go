// Package snapscan implements payment.Gateway for SnapScan, the South African
// QR payment app.
//
// It is the second gateway in this store, and the one that made the payment
// interface honest: SnapScan is not a redirect gateway. There is no page to post
// signed fields to. A payment is a URL carrying an order reference and an amount,
// which the shopper opens — on a phone that URL opens the SnapScan app, and on a
// desktop it is a QR code to scan with one. The store then hears about the
// outcome twice over: once from a webhook, and once from an authenticated read of
// SnapScan's own API, which is the only half it actually trusts.
//
// The spec is at https://developer.snapscan.co.za.
//
// # Three things worth knowing before changing this
//
//   - **There is no sandbox.** Any configuration here is live money. That is the
//     opposite of PayFast, whose sandbox is the default, and it is why nothing in
//     this package has a "test mode" knob that could be left on.
//   - **strict=true and the signature do different jobs.** The Secure QR
//     signature fixes the amount; strict additionally refuses a second successful
//     payment against the same id. A store wants both, so both are sent whenever
//     a validation key is configured.
//   - **The webhook proves very little on its own.** SnapScan's own documentation
//     calls it "an unauthenticated event stream" and points at the API for
//     certainty. The HMAC is necessary and not sufficient: see webhook.go.
package snapscan

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/17xande-dev/goevent/internal/payment"
)

// SnapScan settles in ZAR and nothing else.
const Currency = "ZAR"

// DefaultBaseURL is where both the QR URLs and the merchant API live.
const DefaultBaseURL = "https://pos.snapscan.io"

// defaultQRSize is the rendered QR image's edge in pixels. SnapScan accepts
// 50–500 and defaults to 125, which is too small to scan comfortably from across
// a desk.
const defaultQRSize = 260

// Errors from ParseCallback. Each names one failed check, so the rejection log
// says which one — "the signature did not match" and "SnapScan says that payment
// is still pending" are very different events to be looking at.
var (
	ErrMalformed    = errors.New("snapscan: notification body is not a SnapScan payload")
	ErrSignature    = errors.New("snapscan: notification signature does not match")
	ErrNotValidated = errors.New("snapscan: SnapScan's API did not confirm the notification")
	ErrSnapCode     = errors.New("snapscan: notification is for another merchant's snap code")

	// ErrCurrency and ErrAmount are refusals to *send* a payment.
	ErrCurrency = errors.New("snapscan: SnapScan settles in ZAR only")
	ErrAmount   = errors.New("snapscan: amount must be a positive number of cents")
)

// Config is everything the gateway needs.
type Config struct {
	// SnapCode identifies the merchant, and is the segment in every QR URL.
	SnapCode string
	// APIKey authenticates reads of the merchant API. It is sent as the username
	// of an HTTP Basic credential with an empty password, which is what
	// SnapScan's documentation specifies.
	APIKey string
	// WebhookAuthKey is the shared secret the notification's HMAC is computed
	// with. Without it nothing about a notification can be believed at all, so
	// the server refuses to start when it is missing.
	WebhookAuthKey string
	// ValidationKey enables the Secure QR Payload signature, which stops a
	// shopper editing the amount or the reference between the page and the scan.
	// It is optional because SnapScan enables the feature per account on
	// request; without it, strict=true still fixes a minimum and blocks a repeat
	// payment on the same id.
	ValidationKey string

	// SuccessURL and FailURL are where SnapScan returns the shopper's browser.
	// Both are informational: this store believes the callback, not the return.
	SuccessURL string
	FailURL    string

	// QRSize is the rendered QR image's edge in pixels, 50–500.
	QRSize int

	// BaseURL overrides https://pos.snapscan.io. Only tests set it.
	BaseURL string

	// HTTPClient is used for the API confirmation call. A nil client gets one
	// with a timeout, which the default client does not have.
	HTTPClient *http.Client

	Log *slog.Logger
}

// Gateway is a configured SnapScan client. It is safe for concurrent use.
type Gateway struct {
	cfg     Config
	log     *slog.Logger
	client  *http.Client
	base    string
	origin  string
	qrSize  int
	apiBase string
}

// New validates the configuration and returns a gateway. Everything it can check
// up front it checks at startup, because the first time this configuration is
// otherwise exercised is a real shopper trying to pay.
func New(cfg Config) (*Gateway, error) {
	var missing []string
	for _, f := range []struct{ name, value string }{
		{"snap code", cfg.SnapCode},
		{"API key", cfg.APIKey},
		// Not optional, and deliberately so: a notification with nothing to
		// check its HMAC against is an unauthenticated request that can mark
		// orders paid.
		{"webhook authentication key", cfg.WebhookAuthKey},
		{"success URL", cfg.SuccessURL},
		{"fail URL", cfg.FailURL},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("snapscan: missing configuration: %s", strings.Join(missing, ", "))
	}
	// The snap code is a path segment in every QR URL, and a URL built from an
	// unexpected one fails at the shopper rather than here.
	if strings.ContainsAny(cfg.SnapCode, "/?#& ") {
		return nil, fmt.Errorf("snapscan: snap code %q contains a character that cannot appear in a URL path", cfg.SnapCode)
	}

	base := strings.TrimSuffix(cfg.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("snapscan: base URL %q is not absolute", base)
	}

	size := cfg.QRSize
	if size == 0 {
		size = defaultQRSize
	}
	if size < 50 || size > 500 {
		return nil, fmt.Errorf("snapscan: QR size %d is outside SnapScan's range of 50–500", size)
	}

	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	client := cfg.HTTPClient
	if client == nil {
		// SnapScan retries a notification we fail to answer, so a slow
		// confirmation should give up rather than hold the request open.
		client = &http.Client{Timeout: 15 * time.Second}
	}

	g := &Gateway{
		cfg:     cfg,
		log:     log,
		client:  client,
		base:    base,
		origin:  u.Scheme + "://" + u.Host,
		qrSize:  size,
		apiBase: base + "/merchant/api/v1",
	}

	if cfg.ValidationKey == "" {
		// Not fatal: the feature is enabled per account on request, and strict
		// mode covers most of the same ground. Worth saying out loud, though,
		// because the difference is whether the amount is cryptographically
		// fixed or merely a floor.
		log.Warn("snapscan: no validation key configured; QR amounts are enforced by strict mode only. " +
			"Ask SnapScan support to enable the Secure QR Payload feature and set SNAPSCAN_VALIDATION_KEY")
	}
	// Said at startup because the PayFast section of this project trains the
	// opposite expectation: PAYFAST_SANDBOX defaults to true, and there is no
	// equivalent here.
	log.Info("snapscan: enabled — SnapScan has no sandbox, so these payments are real", "snap_code", cfg.SnapCode)
	return g, nil
}

func (g *Gateway) Name() string { return "snapscan" }

func (g *Gateway) Label() string { return "SnapScan" }

func (g *Gateway) Currency() string { return Currency }

// CSP: the hand-over is a link, not a form post, so nothing is needed in
// form-action — a top-level navigation is not governed by it. The QR image is
// served by SnapScan, so img-src is. Scheme and host only: a CSP source carrying
// a path matches that path exactly and refuses everything beneath it.
func (g *Gateway) CSP() payment.CSPOrigins {
	return payment.CSPOrigins{ImgSrc: g.origin}
}

// Handover builds the payment URL and the URL of its QR image.
//
// Both carry the same parameters, because they are the same payment: the link is
// for the device reading the page, and the image is for a phone that is not that
// device.
func (g *Gateway) Handover(r payment.Request) (payment.Handover, error) {
	if r.Currency != Currency {
		return payment.Handover{}, fmt.Errorf("%w, not %s", ErrCurrency, r.Currency)
	}
	if r.AmountCents <= 0 {
		return payment.Handover{}, ErrAmount
	}
	if r.PaymentID == "" {
		return payment.Handover{}, errors.New("snapscan: request has no order id")
	}

	// SnapScan takes the amount in integer cents, not as a decimal string —
	// which is why payment.FormatAmount, used at every other gateway boundary in
	// this project, is deliberately absent here.
	amount := strconv.FormatInt(r.AmountCents, 10)

	// What identifies the payment, and what both the link and the QR image carry.
	q := url.Values{}
	q.Set("id", r.PaymentID)
	q.Set("amount", amount)
	// strict does what the signature does not: it refuses a second successful
	// payment against the same id, and refuses an amount below the one asked
	// for. Sent whether or not a validation key is configured.
	q.Set("strict", "true")
	if g.cfg.ValidationKey != "" {
		q.Set("signature", Sign(g.cfg.ValidationKey, r.AmountCents, r.PaymentID))
	}

	// The link additionally says where to put the shopper's browser afterwards.
	link := url.Values{}
	maps.Copy(link, q)
	link.Set("s_url", payment.ReturnURL(g.cfg.SuccessURL, r.PaymentID))
	link.Set("f_url", payment.ReturnURL(g.cfg.FailURL, r.PaymentID))
	action := g.base + "/qr/" + g.cfg.SnapCode + "?" + link.Encode()

	// The image is the same payment with a format suffix on the snap code — and
	// deliberately *without* the redirect URLs.
	//
	// ⚠ This is not tidiness. SnapScan's image endpoint answers 403 with an HTML
	// body when s_url or f_url is present, and a browser then refuses the HTML as
	// an image (Chrome: ERR_BLOCKED_BY_ORB) and shows a broken code with nothing
	// in the page but a console entry. curl sees a 403 and the markup looks
	// perfect, so only a real browser finds this.
	//
	// Nothing is lost by dropping them: a scanned code is paid in an app on
	// another device, where there is no browser to send anywhere.
	qrQuery := url.Values{}
	maps.Copy(qrQuery, q)
	qrQuery.Set("snap_code_size", strconv.Itoa(g.qrSize))
	image := g.base + "/qr/" + g.cfg.SnapCode + ".svg?" + qrQuery.Encode()

	return payment.Handover{
		Kind:       payment.HandoverLink,
		Action:     action,
		QRImageURL: image,
	}, nil
}

// Sign is the Secure QR Payload signature: a lowercase HMAC-SHA256 hex digest of
// "key||amount||id", with the validation key as both the HMAC secret and the
// first element of the message.
//
// The key appearing on both sides is not a mistake in this code — it is what
// SnapScan's reference implementation does, and a signature is only right if it
// is computed the way the verifier computes it. The amount is the integer cents
// exactly as they appear in the URL; a mismatch of one character between the two
// makes SnapScan refuse the payment in front of the shopper.
func Sign(validationKey string, amountCents int64, orderID string) string {
	msg := validationKey + "||" + strconv.FormatInt(amountCents, 10) + "||" + orderID
	mac := hmac.New(sha256.New, []byte(validationKey))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}
