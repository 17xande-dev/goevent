// Package config loads all runtime configuration from the environment.
//
// Everything the server needs comes from env vars, so the same binary and image
// run unchanged on a VM, in Compose, or on a managed container platform.
package config

import (
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/17xande-dev/goevent/internal/middleware"
	"github.com/17xande-dev/mailer"
)

// Config is the fully resolved configuration for one server process.
type Config struct {
	// Server
	Port            string
	BaseURL         string
	ShutdownTimeout time.Duration

	// Storage
	DatabaseURL string

	// Site presentation. No organisation-specific defaults live in the code;
	// adopters set these.
	SiteName string
	Currency string

	// TemplateDir, when set, overlays same-named templates from disk over the
	// embedded defaults, so adopters can restyle without forking.
	TemplateDir string

	// StaticDir is TemplateDir's counterpart for assets: a file there shadows a
	// bundled one of the same name, and a new name is served too. Replacing the logo
	// is dropping a logo.svg into it — restyling without a way to supply an image
	// would be half a feature.
	StaticDir string

	// ThemeReload re-reads TemplateDir and StaticDir on every request instead of
	// once at startup, so editing a theme file and refreshing the page is enough to
	// see it. It is for writing a theme and nothing else: it costs a parse of every
	// template and a read of every asset per request, and it turns a typo in a
	// template into a 500 at request time rather than a boot failure. Off unless
	// THEME_RELOAD says otherwise; the compose stack sets it.
	ThemeReload bool

	// SessionTTL is how long a sign-in lasts. A session is a row in
	// admin_sessions and a cookie carrying a random token; there is no secret to
	// configure, because there is nothing signed to verify — see internal/auth.
	SessionTTL time.Duration

	// SetupToken is the one-time token that may claim the first administrator
	// account, supplied rather than generated.
	//
	// It exists for an automated deploy, which cannot read a token out of a log
	// line and paste it into a form. Empty is the ordinary case: the server
	// generates one on first boot against an empty admin_users and prints it.
	// Either way it is stored only as a hash and is spent by the first claim.
	SetupToken string

	// PayFast and SnapScan are the payment gateways' configuration. They are flat
	// structs here rather than the gateway packages' own Configs so that config
	// depends on no gateway: main assembles them, which is also where a gateway is
	// chosen.
	//
	// Either or both may be left unset. With neither, the deployment runs free
	// events and registrations an organiser marks paid by hand (cash or EFT);
	// both set means the registrant picks at checkout.
	PayFast  PayFast
	SnapScan SnapScan

	// SMTP is how transactional mail leaves. It is required, unless Graph is
	// configured instead — see the refusal in Load, and the reason recorded
	// there.
	SMTP SMTP

	// Graph, when configured, sends mail through the Microsoft Graph API
	// instead of SMTP. It takes precedence over SMTP when both are set — see
	// newMailer in main.go.
	Graph Graph

	// NotifyEmail is where a copy of each confirmed registration goes — the
	// event organiser. Empty means the registrant's confirmation is the only mail
	// sent, and the organiser finds registrations in the admin instead.
	NotifyEmail string
	// SecretKey is the server's one secret, 32 bytes as hex. Separate keys are
	// derived from it for encrypting the email queue and for signing manage
	// links and ticket codes. Changing it invalidates every link and ticket
	// already sent and strands any queued email, so keep it — and back it up
	// with the database.
	SecretKey string

	// Blob is object storage for event images.
	Blob Blob

	// ImageDir stores event images in a local directory served by this server,
	// for a site that wants no object storage at all. Mutually exclusive with Blob:
	// two configured backends would leave "which one wins" to be guessed.
	//
	// An event image is only ever a bucket object or a file here. Pasting a URL
	// from the general internet used to be allowed and no longer is: those bytes
	// belong to somebody else, who can change or delete them, and an event page
	// with a broken image is worse than one with none.
	ImageDir string

	// RateLimits are the per-IP limits on the three surfaces worth protecting.
	// Defaults are deliberately loose enough that no real registrant or operator
	// meets one — a limit that fires on ordinary use gets turned off.
	RateLimits RateLimits

	// ClientIPSource says where the address a request came from is read from:
	// the connection itself, X-Forwarded-For, or Cloudflare's CF-Connecting-IP.
	// It describes what is actually in front of the server, and naming a header
	// nothing is setting lets a client claim any IP it likes — which the payment
	// callback's source-IP check and the per-IP rate limits both rely on. See
	// middleware.ClientIPSource for what each value means.
	ClientIPSource middleware.ClientIPSource

	// CookieSecure is derived from BaseURL rather than configured separately:
	// an HTTPS deployment always wants Secure cookies, and localhost
	// development cannot use them.
	CookieSecure bool

	// ShowErrorDetail puts the underlying error on the error page, instead of only
	// a reference to find in the logs. Derived from the same signal as
	// CookieSecure, deliberately: "is this production" should have one answer
	// rather than three, and this is already the question HSTS and the CSRF
	// cookie's TLS mode are decided by.
	//
	// The detail is the Go error string, never a stack trace. That string names
	// tables, columns and constraints, which is reconnaissance for anybody probing
	// the site, so it is off the moment BaseURL is https — and an http deployment
	// is one this project already treats as not-production, since it gets neither
	// Secure cookies nor HSTS.
	ShowErrorDetail bool

	// EmbedOrigins are the origins allowed to frame the public event pages, for
	// dropping them into a page hosted elsewhere. Empty means no other origin may,
	// which is the right default for a site that is only ever browsed on its own
	// domain.
	EmbedOrigins []string

	// FontOrigins are the origins a web font may be loaded from besides this one.
	// Empty — the default — means the CSP stays closed and the theme uses the
	// system font stack.
	//
	// This opens *two* CSP directives, which is worth knowing before setting it: a
	// hosted font service serves a stylesheet that declares the fonts and then the
	// font files that stylesheet points at, so the origins land in both style-src
	// and font-src. Allowing the fonts without the stylesheet declaring them
	// half-works, which is a slow thing to diagnose. It does not open script-src:
	// a font service still cannot run JavaScript on the checkout page, which is
	// the property that makes this a narrow widening rather than a general one.
	FontOrigins []string

	// FontCSSURL is a stylesheet the default layout links from its <head>, for the
	// hosted-font case where the service gives you a CSS URL — an Adobe Fonts kit,
	// typically. Empty means no such link is rendered at all.
	//
	// Its origin must appear in FontOrigins, checked at boot: a link the CSP then
	// blocks fails silently apart from a console warning, and the page renders in
	// the fallback font with nothing to suggest why.
	//
	// Deliberately a CSS URL and not a script: a font service's JavaScript loader
	// needs script-src widened, connect-src opened for its config fetch, and a
	// nonce for the inline snippet and the inline <style> it injects. That is a far
	// larger concession than this one, and this project does not offer it.
	FontCSSURL string

	// LogLevel is one of debug, info, warn, error.
	LogLevel string

	// LogFormat is "json" or "gcp". Both are JSON on stdout; "gcp" renames two
	// keys — level to severity, msg to message — because that is what Google Cloud
	// Logging reads. Without it every line files as DEFAULT severity, so
	// `severity>=ERROR` matches nothing and alerting on the error rate silently
	// never fires.
	//
	// Opt-in rather than automatic: the rename is a Google convention, and this
	// project does not assume anybody's platform.
	LogFormat string
}

// MinSetupTokenLen is the shortest SETUP_TOKEN accepted. The token is, for as
// long as it is unclaimed, the credential for the whole admin area, so it is held
// to the length 32 random bytes reach in base64 rather than to anything a person
// would type.
const MinSetupTokenLen = 32

// payFastSandboxMerchantID is the merchant id PayFast publishes in its own
// documentation for testing, and therefore the one every copy of this project's
// .env.example and compose.yaml carries. It is a constant here so that "still on
// the demo credentials" is something the config can recognise.
const payFastSandboxMerchantID = "10000100"

// PayFast is what the PayFast gateway needs from the environment. The merchant
// id switches it on and the key is then required: half a credential is a
// configuration mistake, and discovering that at the first checkout is worse
// than discovering it at boot.
//
// Notification URLs are derived from BaseURL rather than configured, with one
// override — NotifyURL — because that is the one PayFast's own servers have to
// reach, which on a development machine means a tunnel's hostname and not
// localhost.
type PayFast struct {
	MerchantID  string
	MerchantKey string
	Passphrase  string
	Sandbox     bool

	NotifyURL string

	// AllowedCIDRs overrides PayFast's published source ranges. It is
	// configuration rather than a constant because PayFast has changed its ranges
	// before, and adding one should not need a release of this project.
	AllowedCIDRs []string
	// AllowAnySourceIP disables the source-IP check entirely. See
	// PAYFAST_ALLOWED_CIDRS=any in .env.example: it is for testing against the
	// sandbox and never right in production.
	AllowAnySourceIP bool
}

// Configured reports whether PayFast is switched on. The merchant id is the
// signal, and the key is then required — half a credential is a configuration
// mistake, not a decision to run without the gateway.
func (p PayFast) Configured() bool { return p.MerchantID != "" }

// SnapScan is what the SnapScan gateway needs from the environment.
//
// SnapScan is South Africa's QR payment app: the registrant opens a URL this server
// builds, on a phone through the app or on a desktop by scanning the QR code it
// renders to.
//
// Two absences are deliberate. There is no sandbox setting, because SnapScan has
// no sandbox — configuring this at all means real money, which is the reverse of
// PAYFAST_SANDBOX's safe default. And there is no notify URL, because SnapScan's
// support configures the webhook address on the merchant account rather than
// reading it from each payment; it must point at BASE_URL + the callback route,
// which on a development machine means a tunnel.
type SnapScan struct {
	// SnapCode identifies the merchant. Its presence is what enables the gateway.
	SnapCode string
	// APIKey reads payments back from SnapScan's merchant API, which is how a
	// notification is confirmed.
	APIKey string
	// WebhookAuthKey is the shared secret a notification's HMAC is computed with.
	// Required whenever the gateway is enabled: without it, nothing about a
	// notification can be checked, and the callback route is the one thing that
	// can mark a registration paid.
	WebhookAuthKey string
	// ValidationKey enables the Secure QR Payload signature, which SnapScan
	// switches on per account on request. Optional — see snapscan.Config.
	ValidationKey string
}

// Configured reports whether SnapScan is switched on.
func (s SnapScan) Configured() bool { return s.SnapCode != "" }

// Blob is object storage for event images, against anything speaking the S3
// API — Cloudflare R2, Google Cloud Storage in interoperability mode, or MinIO.
//
// PublicBaseURL is separate from Endpoint and cannot be derived from it: the
// address a bucket is written through and the address it is read from are
// routinely different — R2 writes to <account>.r2.cloudflarestorage.com and reads
// from a custom domain — and only the operator knows the second one.
type Blob struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string

	// Region is "auto" for R2. GCS and MinIO ignore it.
	Region string
	UseTLS bool

	PublicBaseURL string
}

// Configured reports whether object storage is set up.
func (b Blob) Configured() bool { return b.Endpoint != "" }

// PublicOrigin is the scheme and host images are served from, with any path
// stripped — which is what a Content-Security-Policy source has to be.
//
// This is not fussiness. A CSP source whose path does not end in "/" must match a
// URL's path *exactly*, so listing "http://host:9000/bucket" permits that one URL
// and refuses "http://host:9000/bucket/events/x.jpg" — every actual image. MinIO
// and any path-style bucket URL hit this, and the failure is invisible outside a
// browser: the image returns 200 to curl, the markup is correct, and the page shows
// a broken image with a console warning nothing else surfaces.
func (b Blob) PublicOrigin() string {
	u, err := url.Parse(b.PublicBaseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// Load has already rejected this, so reaching here means the value changed
		// underneath us. An empty source is safe: it permits nothing.
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// ImagesEnabled reports whether an event can have an image at all — by upload to
// object storage or to a local directory. With neither, the admin says so rather
// than offering a form that could only fail.
func (c Config) ImagesEnabled() bool { return c.Blob.Configured() || c.ImageDir != "" }

// RateLimits holds the per-IP limits. Each is a number of requests per minute
// with a burst; see middleware.RateLimit for what the two mean together.
type RateLimits struct {
	// LoginPerMinute guards the admin password against brute force. Low, because
	// an operator signs in once.
	LoginPerMinute int
	// CheckoutPerMinute guards registration. Loose, because refusing a real
	// registrant costs a registration and double-clicking is normal.
	CheckoutPerMinute int
	// CallbackPerMinute guards the payment callback, which is unauthenticated and
	// makes the server POST to the gateway for every request it accepts. Generous:
	// a throttled notification is retried, but throttling a busy site's genuine
	// traffic delays real payments.
	CallbackPerMinute int
	// StatusPerMinute guards the checkout's payment-status poll, which a QR
	// hand-over page asks for every few seconds while the registrant pays on their
	// phone. It reads one payment by its id and costs a single indexed
	// query, so the allowance is roughly "twice what an open page asks for".
	StatusPerMinute int
}

// SMTP is the mail relay's configuration. Username and Password may be empty, for
// a relay that authenticates by network address — mailpit in development being the
// case that matters here.
type SMTP struct {
	Host     string
	Port     int
	Username string
	Password string

	// From is the sender address. A relay usually rejects a From it does not
	// consider itself responsible for, so this has to be on a domain it accepts.
	From    string
	ReplyTo string

	// TLS is "starttls" (the default, correct for port 587), "tls" (implicit, for
	// 465) or "none" (development only).
	TLS string

	// OAuth, when configured, replaces Password with an XOAUTH2 access token
	// fetched per send. It is how a Microsoft Exchange Online mailbox is reached
	// now that Basic Auth for SMTP client submission is going away.
	OAuth SMTPOAuth
}

// SMTPOAuth is an Entra ID app registration, used to obtain an access token for
// XOAUTH2. All three parts are needed or none of them are.
type SMTPOAuth struct {
	TenantID     string
	ClientID     string
	ClientSecret string
}

// Configured reports whether a token can be obtained.
func (o SMTPOAuth) Configured() bool {
	return o.TenantID != "" && o.ClientID != "" && o.ClientSecret != ""
}

// Configured reports whether mail can actually be sent. Both a host and a From
// address are needed: a relay with no sender is not a working configuration, and
// half-configured is the case worth catching at startup.
func (s SMTP) Configured() bool { return s.Host != "" && s.From != "" }

// Graph is an Entra ID app registration used to send mail through the
// Microsoft Graph API instead of SMTP. It is a separate path from SMTP.OAuth:
// the two need different app permissions (Mail.Send versus SMTP.SendAsApp) and
// different token audiences, and are not interchangeable.
type Graph struct {
	TenantID     string
	ClientID     string
	ClientSecret string
	// From is the mailbox to send as. Kept separate from SMTP.From rather than
	// shared, since a deployment choosing Graph has no reason to have set SMTP
	// up at all.
	From string
}

// Configured reports whether Graph can actually be used. All four are needed:
// three for the token and From for the mailbox, and half-configured is the case
// worth catching at startup rather than at the first registration.
func (g Graph) Configured() bool {
	return g.TenantID != "" && g.ClientID != "" && g.ClientSecret != "" && g.From != ""
}

// AllowsEmbedding reports whether any other origin may frame the public event pages.
func (c Config) AllowsEmbedding() bool { return len(c.EmbedOrigins) > 0 }

// Load reads configuration from the environment, applying defaults and
// returning an error listing every missing or malformed required value.
func Load() (Config, error) {
	sec, err := loadSecrets(secretKeys...)
	if err != nil {
		return Config{}, err
	}
	c := Config{
		Port:            env("PORT", "8080"),
		BaseURL:         strings.TrimRight(env("BASE_URL", "http://localhost:8080"), "/"),
		DatabaseURL:     sec.get("DATABASE_URL"),
		SiteName:        env("SITE_NAME", "goevent"),
		Currency:        env("CURRENCY", "ZAR"),
		TemplateDir:     os.Getenv("TEMPLATE_DIR"),
		StaticDir:       strings.TrimSpace(os.Getenv("STATIC_DIR")),
		ThemeReload:     boolEnv("THEME_RELOAD", false),
		LogLevel:        env("LOG_LEVEL", "info"),
		LogFormat:       env("LOG_FORMAT", "json"),
		SetupToken:      strings.TrimSpace(sec.get("SETUP_TOKEN")),
		SessionTTL:      24 * time.Hour,
		ShutdownTimeout: 15 * time.Second,
		RateLimits: RateLimits{
			LoginPerMinute:    10,
			CheckoutPerMinute: 20,
			CallbackPerMinute: 120,
			StatusPerMinute:   30,
		},
		NotifyEmail: strings.TrimSpace(os.Getenv("NOTIFY_EMAIL")),
		SecretKey:   strings.TrimSpace(sec.get("SECRET_KEY")),
		ImageDir:    strings.TrimSpace(os.Getenv("IMAGE_DIR")),
		Blob: Blob{
			Endpoint:      strings.TrimSpace(os.Getenv("BLOB_ENDPOINT")),
			Bucket:        strings.TrimSpace(os.Getenv("BLOB_BUCKET")),
			AccessKey:     os.Getenv("BLOB_ACCESS_KEY_ID"),
			SecretKey:     sec.get("BLOB_SECRET_ACCESS_KEY"),
			Region:        env("BLOB_REGION", "auto"),
			UseTLS:        boolEnv("BLOB_USE_TLS", true),
			PublicBaseURL: strings.TrimRight(strings.TrimSpace(os.Getenv("BLOB_PUBLIC_BASE_URL")), "/"),
		},
		SMTP: SMTP{
			Host:     strings.TrimSpace(os.Getenv("SMTP_HOST")),
			Username: os.Getenv("SMTP_USERNAME"),
			Password: sec.get("SMTP_PASSWORD"),
			From:     strings.TrimSpace(os.Getenv("EMAIL_FROM")),
			ReplyTo:  strings.TrimSpace(os.Getenv("EMAIL_REPLY_TO")),
			TLS:      env("SMTP_TLS", "starttls"),
			OAuth: SMTPOAuth{
				TenantID:     strings.TrimSpace(os.Getenv("SMTP_OAUTH_TENANT_ID")),
				ClientID:     strings.TrimSpace(os.Getenv("SMTP_OAUTH_CLIENT_ID")),
				ClientSecret: sec.get("SMTP_OAUTH_CLIENT_SECRET"),
			},
		},
		Graph: Graph{
			TenantID:     strings.TrimSpace(os.Getenv("GRAPH_TENANT_ID")),
			ClientID:     strings.TrimSpace(os.Getenv("GRAPH_CLIENT_ID")),
			ClientSecret: sec.get("GRAPH_CLIENT_SECRET"),
			From:         strings.TrimSpace(os.Getenv("EMAIL_FROM")),
		},
		PayFast: PayFast{
			MerchantID:  os.Getenv("PAYFAST_MERCHANT_ID"),
			MerchantKey: sec.get("PAYFAST_MERCHANT_KEY"),
			Passphrase:  sec.get("PAYFAST_PASSPHRASE"),
			// Sandbox defaults to true: the wrong default here takes real money
			// from a real card during somebody's first afternoon with the project.
			//
			// The cost of that default is the mirror failure — a production
			// deployment that never sets it takes no money and looks fine — so
			// anything that deploys this is expected to set it explicitly, and
			// the Compose files in deploy/ require it. See the check further
			// down for the half-done version of turning it off.
			Sandbox:   boolEnv("PAYFAST_SANDBOX", true),
			NotifyURL: strings.TrimSpace(os.Getenv("PAYFAST_NOTIFY_URL")),
		},
		SnapScan: SnapScan{
			SnapCode:       strings.TrimSpace(os.Getenv("SNAPSCAN_SNAP_CODE")),
			APIKey:         strings.TrimSpace(sec.get("SNAPSCAN_API_KEY")),
			WebhookAuthKey: strings.TrimSpace(sec.get("SNAPSCAN_WEBHOOK_AUTH_KEY")),
			ValidationKey:  strings.TrimSpace(sec.get("SNAPSCAN_VALIDATION_KEY")),
		},
	}
	c.CookieSecure = strings.HasPrefix(c.BaseURL, "https://")
	// One signal, three uses: Secure cookies, HSTS, and whether an error page may
	// say what actually went wrong.
	c.ShowErrorDetail = !c.CookieSecure

	var missing []string
	if c.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	if c.SecretKey == "" {
		missing = append(missing, "SECRET_KEY")
	}
	// Each gateway's credentials are required only when that gateway is switched
	// on, so a SnapScan-only store needs no PayFast account and the reverse.
	if c.PayFast.Configured() && c.PayFast.MerchantKey == "" {
		missing = append(missing, "PAYFAST_MERCHANT_KEY")
	}
	if c.SnapScan.Configured() {
		if c.SnapScan.APIKey == "" {
			missing = append(missing, "SNAPSCAN_API_KEY")
		}
		// Not optional. A notification with nothing to check its signature
		// against is an unauthenticated request that can mark registrations paid, and
		// the callback route is unauthenticated by definition — a payment
		// provider cannot be given a session or a CSRF token.
		if c.SnapScan.WebhookAuthKey == "" {
			missing = append(missing, "SNAPSCAN_WEBHOOK_AUTH_KEY")
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("config: required env vars not set: %s", strings.Join(missing, ", "))
	}
	if key, err := hex.DecodeString(c.SecretKey); err != nil || len(key) != 32 {
		return Config{}, errors.New("config: SECRET_KEY must be 64 hexadecimal characters; generate with openssl rand -hex 32")
	}

	// There is no admin credential in the environment any more: accounts live in
	// admin_users and the first one is claimed at /admin/setup. What can be
	// supplied is the token that authorises that claim, and a short one is a
	// guessable credential for the whole admin area — so it is bounded here,
	// where the message can say so, rather than accepted and regretted.
	if c.SetupToken != "" && len(c.SetupToken) < MinSetupTokenLen {
		return Config{}, fmt.Errorf("config: SETUP_TOKEN is %d characters, want at least %d "+
			"(generate one with `openssl rand -base64 32`)", len(c.SetupToken), MinSetupTokenLen)
	}

	if h, ok := os.LookupEnv("SESSION_TTL_HOURS"); ok {
		n, err := strconv.Atoi(h)
		if err != nil || n <= 0 {
			return Config{}, fmt.Errorf("config: SESSION_TTL_HOURS must be a positive integer, got %q", h)
		}
		c.SessionTTL = time.Duration(n) * time.Hour
	}

	if d, ok := os.LookupEnv("SHUTDOWN_TIMEOUT_SECONDS"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return Config{}, fmt.Errorf("config: SHUTDOWN_TIMEOUT_SECONDS must be a non-negative integer, got %q", d)
		}
		c.ShutdownTimeout = time.Duration(n) * time.Second
	}

	if err := checkLogLevel(c.LogLevel); err != nil {
		return Config{}, err
	}
	if err := checkLogFormat(c.LogFormat); err != nil {
		return Config{}, err
	}

	// TRUST_PROXY_IP was a bool meaning "believe X-Forwarded-For". It cannot
	// describe a Cloudflare deployment, where the trustworthy address is in
	// CF-Connecting-IP and X-Forwarded-For is appended to rather than replaced.
	//
	// Refused rather than ignored: silently falling back to the default would
	// leave a deployment that had set it reading RemoteAddr instead — which is
	// the proxy — so every registrant would share one rate-limit bucket and the
	// payment callback's source-IP check would reject every genuine
	// notification. Both fail quietly, which is what a boot refusal is for.
	if _, ok := os.LookupEnv("TRUST_PROXY_IP"); ok {
		return Config{}, errors.New(
			"config: TRUST_PROXY_IP has been replaced by CLIENT_IP_SOURCE — " +
				"use CLIENT_IP_SOURCE=forwarded for what TRUST_PROXY_IP=true did, " +
				"CLIENT_IP_SOURCE=cloudflare behind Cloudflare, or unset it for remote")
	}
	clientIPSource, sourceErr := middleware.ParseClientIPSource(
		env("CLIENT_IP_SOURCE", string(middleware.ClientIPRemote)))
	if sourceErr != nil {
		return Config{}, fmt.Errorf("config: CLIENT_IP_SOURCE: %w", sourceErr)
	}
	c.ClientIPSource = clientIPSource

	// Taking real money with the demo credentials is a contradiction, and it is
	// the mistake that follows naturally from the other one: somebody discovers
	// their site has been quietly running against the sandbox, sets
	// PAYFAST_SANDBOX=false, and does not realise the merchant id came from
	// .env.example too. Every payment would then be signed with a key published
	// in PayFast's own documentation.
	//
	// Refused at boot, where it costs one message, rather than at the first
	// checkout, where it costs a registrant.
	if c.PayFast.Configured() && !c.PayFast.Sandbox && c.PayFast.MerchantID == payFastSandboxMerchantID {
		return Config{}, fmt.Errorf(
			"config: PAYFAST_SANDBOX is false but PAYFAST_MERCHANT_ID is still %s, "+
				"which is PayFast's published sandbox merchant id — set your own "+
				"credentials, or leave PAYFAST_SANDBOX=true", payFastSandboxMerchantID)
	}

	// "*" is allowed here and nowhere else: the fragments these origins may fetch
	// are cookie-free and read-only, so a permissive list cannot become a way to
	// act as somebody.
	c.EmbedOrigins, err = parseOrigins("EMBED_ORIGINS", true)
	if err != nil {
		return Config{}, err
	}

	// Whereas a wildcard font source would let any origin serve a stylesheet to
	// every page of the site, the checkout included — the opposite of what this
	// directive is for. So: list them.
	c.FontOrigins, err = parseOrigins("FONT_ORIGINS", false)
	if err != nil {
		return Config{}, err
	}

	if c.FontCSSURL = strings.TrimSpace(os.Getenv("FONT_CSS_URL")); c.FontCSSURL != "" {
		u, err := url.Parse(c.FontCSSURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return Config{}, fmt.Errorf("config: FONT_CSS_URL %q must be an absolute URL", c.FontCSSURL)
		}
		// The stylesheet's own origin has to be allowed to serve it. Refused at boot
		// rather than in the browser, where the only symptom is a console warning and
		// a page rendered in the fallback font.
		origin := u.Scheme + "://" + u.Host
		if !slices.Contains(c.FontOrigins, origin) {
			return Config{}, fmt.Errorf(
				"config: FONT_CSS_URL is on %s, which FONT_ORIGINS does not list — "+
					"the CSP would block the stylesheet; add %s to FONT_ORIGINS", origin, origin)
		}
	}

	// The limits are configurable because the right number depends on a site's
	// traffic, and 0 means "no limit on this surface" — spelled out rather than
	// implied by an empty value, since switching a protection off should be
	// something an operator typed.
	for _, l := range []struct {
		key string
		dst *int
	}{
		{"RATE_LIMIT_LOGIN_PER_MINUTE", &c.RateLimits.LoginPerMinute},
		{"RATE_LIMIT_CHECKOUT_PER_MINUTE", &c.RateLimits.CheckoutPerMinute},
		{"RATE_LIMIT_CALLBACK_PER_MINUTE", &c.RateLimits.CallbackPerMinute},
		{"RATE_LIMIT_STATUS_PER_MINUTE", &c.RateLimits.StatusPerMinute},
	} {
		if v, ok := os.LookupEnv(l.key); ok {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return Config{}, fmt.Errorf("config: %s must be a non-negative integer, got %q", l.key, v)
			}
			*l.dst = n
		}
	}

	// Mail is validated whenever any of it is set, so a half-configured relay is a
	// boot failure rather than a receipt that silently never arrives.
	c.SMTP.Port = 587
	if p, ok := os.LookupEnv("SMTP_PORT"); ok {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return Config{}, fmt.Errorf("config: SMTP_PORT must be a port number, got %q", p)
		}
		c.SMTP.Port = n
	}
	if _, err := mailer.ParseTLSPolicy(c.SMTP.TLS); err != nil {
		return Config{}, fmt.Errorf("config: SMTP_TLS: %w", err)
	}
	// From set without Host is not an error on its own: it is what a Graph-only
	// deployment looks like, since EMAIL_FROM serves both transports. Host
	// without From is still a mistake — a relay with no sender is not a working
	// configuration.
	if c.SMTP.Host != "" && c.SMTP.From == "" {
		return Config{}, fmt.Errorf(
			"config: EMAIL_FROM is required when SMTP_HOST is set")
	}
	// Graph is all or nothing, for the same reason XOAUTH2 is below: a
	// half-configured app registration would boot, then fail to send on the
	// first confirmed registration. Checked before the general mail requirement below, so a
	// deployment that got partway through setting Graph up gets the specific
	// error rather than the generic "nothing is configured" one.
	graph := c.Graph
	if !graph.Configured() && (graph.TenantID != "" || graph.ClientID != "" || graph.ClientSecret != "") {
		return Config{}, fmt.Errorf(
			"config: GRAPH_TENANT_ID, GRAPH_CLIENT_ID, GRAPH_CLIENT_SECRET and EMAIL_FROM " +
				"must all be set to send through Graph")
	}

	// Mail is REQUIRED. The confirmation email is the only place a registrant is
	// given their tickets and the link to their registration — so a deployment
	// with no mail server does not merely drop a receipt, it takes money for
	// tickets nobody can then present at the door. Graph satisfies it just as
	// SMTP does.
	if !c.SMTP.Configured() && !c.Graph.Configured() {
		return Config{}, fmt.Errorf(
			"config: SMTP_HOST and EMAIL_FROM (or GRAPH_TENANT_ID, GRAPH_CLIENT_ID, " +
				"GRAPH_CLIENT_SECRET and EMAIL_FROM) are required — a confirmation carries " +
				"the attendee's tickets, which exist nowhere else")
	}
	if c.NotifyEmail != "" && !c.SMTP.Configured() && !c.Graph.Configured() {
		return Config{}, fmt.Errorf(
			"config: NOTIFY_EMAIL is set but no mail transport is configured, " +
				"so the notification could never be sent")
	}

	// XOAUTH2 is all or nothing. A half-configured app registration would boot,
	// then fail to authenticate against Exchange on the first registration — which
	// is the worst possible moment to discover a missing environment variable.
	oauth := c.SMTP.OAuth
	if !oauth.Configured() && (oauth.TenantID != "" || oauth.ClientID != "" || oauth.ClientSecret != "") {
		return Config{}, fmt.Errorf(
			"config: SMTP_OAUTH_TENANT_ID, SMTP_OAUTH_CLIENT_ID and SMTP_OAUTH_CLIENT_SECRET " +
				"must be set together")
	}
	if oauth.Configured() {
		// XOAUTH2 authenticates as a named mailbox, so the username is not
		// optional the way it is for a relay that authenticates by address.
		if c.SMTP.Username == "" {
			return Config{}, fmt.Errorf(
				"config: SMTP_USERNAME is required with SMTP OAuth — XOAUTH2 authenticates " +
					"as a named mailbox")
		}
		// Refused rather than resolved by precedence. Both being set means
		// somebody has a belief about which one is in use, and a silent winner
		// would leave a stale secret sitting in the environment looking live.
		if c.SMTP.Password != "" {
			return Config{}, fmt.Errorf(
				"config: SMTP_PASSWORD and SMTP OAuth are both set, so which one authenticates " +
					"would be a guess; unset SMTP_PASSWORD")
		}
	}

	// One image backend at most. Both configured would leave which one wins to be
	// guessed, and the guess would be wrong half the time.
	if c.Blob.Configured() && c.ImageDir != "" {
		return Config{}, fmt.Errorf(
			"config: BLOB_ENDPOINT and IMAGE_DIR are both set; event images come from one or the other")
	}
	// And at least one. Otherwise the admin's upload form has to either exist or
	// explain itself on every event page — a half-feature carried by every
	// deployment that forgot a variable.
	//
	// The bar is deliberately low: IMAGE_DIR is one path and needs nothing running.
	// Refusing to boot over a setting that cheap costs an adopter a line of config
	// and saves them a public site that looks broken.
	if !c.ImagesEnabled() {
		return Config{}, fmt.Errorf(
			"config: event images are required — set IMAGE_DIR for a local directory, " +
				"or the BLOB_* variables for object storage")
	}

	// Object storage is all-or-nothing: a partial configuration would fail at the
	// first upload with whichever piece is missing, which is a worse place to find
	// out than at boot.
	if c.Blob.Configured() {
		var missing []string
		for _, f := range []struct{ name, value string }{
			{"BLOB_BUCKET", c.Blob.Bucket},
			{"BLOB_ACCESS_KEY_ID", c.Blob.AccessKey},
			{"BLOB_SECRET_ACCESS_KEY", c.Blob.SecretKey},
			{"BLOB_PUBLIC_BASE_URL", c.Blob.PublicBaseURL},
		} {
			if strings.TrimSpace(f.value) == "" {
				missing = append(missing, f.name)
			}
		}
		if len(missing) > 0 {
			return Config{}, fmt.Errorf(
				"config: BLOB_ENDPOINT is set, so these are required too: %s", strings.Join(missing, ", "))
		}
		u, err := url.Parse(c.Blob.PublicBaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return Config{}, fmt.Errorf(
				"config: BLOB_PUBLIC_BASE_URL %q must be an absolute URL", c.Blob.PublicBaseURL)
		}
	} else if c.Blob.Bucket != "" || c.Blob.AccessKey != "" || c.Blob.PublicBaseURL != "" {
		return Config{}, fmt.Errorf(
			"config: BLOB_* variables are set but BLOB_ENDPOINT is not, so uploads would be off")
	}

	// "any" is spelled out rather than being an empty list, so disabling a
	// security check is something an operator typed on purpose.
	switch cidrs := strings.TrimSpace(os.Getenv("PAYFAST_ALLOWED_CIDRS")); cidrs {
	case "":
	case "any":
		c.PayFast.AllowAnySourceIP = true
	default:
		for _, cidr := range strings.Split(cidrs, ",") {
			if cidr = strings.TrimSpace(cidr); cidr != "" {
				c.PayFast.AllowedCIDRs = append(c.PayFast.AllowedCIDRs, cidr)
			}
		}
	}

	if c.PayFast.NotifyURL != "" {
		u, err := url.Parse(c.PayFast.NotifyURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return Config{}, fmt.Errorf("config: PAYFAST_NOTIFY_URL %q must be an absolute URL", c.PayFast.NotifyURL)
		}
	}

	for _, d := range []struct{ key, path string }{
		{"TEMPLATE_DIR", c.TemplateDir},
		{"STATIC_DIR", c.StaticDir},
	} {
		if d.path == "" {
			continue
		}
		if fi, err := os.Stat(d.path); err != nil {
			return Config{}, fmt.Errorf("config: %s %q: %w", d.key, d.path, err)
		} else if !fi.IsDir() {
			return Config{}, fmt.Errorf("config: %s %q is not a directory", d.key, d.path)
		}
	}

	return c, nil
}

// parseOrigins reads a comma-separated origin list from the environment.
//
// Both lists it serves — embed origins and font origins — are compared literally
// by the browser, against the Origin header in one case and as a CSP source
// expression in the other. Neither tolerates a trailing slash or a path, so both
// are refused here where the message can say why, rather than in a browser where
// the symptom is a request that simply does not happen.
func parseOrigins(key string, allowWildcard bool) ([]string, error) {
	var out []string
	for _, origin := range strings.Split(os.Getenv(key), ",") {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		if origin == "*" {
			if !allowWildcard {
				return nil, fmt.Errorf("config: %s does not accept \"*\" — list the origins", key)
			}
			out = append(out, origin)
			continue
		}
		u, err := url.Parse(origin)
		if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" {
			return nil, fmt.Errorf("config: %s entry %q must be scheme://host[:port] with no path", key, origin)
		}
		out = append(out, origin)
	}
	return out, nil
}

// LoadTool loads only the database settings, for the migration commands, which
// need no payment or mail credentials.
func LoadTool() (Config, error) {
	// Tool jobs must not require unrelated secret files to be mounted.
	sec, err := loadSecrets("DATABASE_URL")
	if err != nil {
		return Config{}, err
	}
	c := Config{
		DatabaseURL: sec.get("DATABASE_URL"),
		LogLevel:    env("LOG_LEVEL", "info"),
		LogFormat:   env("LOG_FORMAT", "json"),
	}
	if c.DatabaseURL == "" {
		return Config{}, fmt.Errorf("config: required env vars not set: DATABASE_URL")
	}
	if err := checkLogLevel(c.LogLevel); err != nil {
		return Config{}, err
	}
	if err := checkLogFormat(c.LogFormat); err != nil {
		return Config{}, err
	}
	return c, nil
}

func checkLogFormat(format string) error {
	switch format {
	case "json", "gcp":
		return nil
	default:
		return fmt.Errorf("config: LOG_FORMAT must be json or gcp; got %q", format)
	}
}

func checkLogLevel(level string) error {
	switch level {
	case "debug", "info", "warn", "error":
		return nil
	default:
		return fmt.Errorf("config: LOG_LEVEL must be one of debug, info, warn, error; got %q", level)
	}
}

// secretKeys are the settings that are credentials, and so may arrive as a file
// instead of a value: KEY_FILE names a file whose contents are KEY. That keeps
// them out of the process environment — which `docker inspect` shows, and which
// a container's /proc/<pid>/environ exposes to anything running as its user —
// and lets a deployment mount them as Compose secrets rather than write them
// into a compose file. Identifiers that sit next to them (a merchant id, an
// access key id, a snap code) are not on this list: they are not secret.
var secretKeys = []string{
	"SECRET_KEY",
	"DATABASE_URL",
	"SETUP_TOKEN",
	"PAYFAST_MERCHANT_KEY",
	"PAYFAST_PASSPHRASE",
	"SNAPSCAN_API_KEY",
	"SNAPSCAN_WEBHOOK_AUTH_KEY",
	"SNAPSCAN_VALIDATION_KEY",
	"SMTP_PASSWORD",
	"SMTP_OAUTH_CLIENT_SECRET",
	"GRAPH_CLIENT_SECRET",
	"BLOB_SECRET_ACCESS_KEY",
}

// secrets holds the values read from KEY_FILE for the keys that had one.
type secrets map[string]string

// loadSecrets reads KEY_FILE for each key that has one, so that a file which is
// missing or unreadable is a boot failure naming the setting, rather than an
// empty credential discovered at the first checkout.
//
// "Set" means non-empty on both sides. The development compose file supplies
// most of these as `${KEY:-}`, i.e. present but empty, and that must not read as
// a conflict with a KEY_FILE alongside it.
//
// Trailing newlines are stripped, and nothing else is: `pass show` and most
// editors end a file with one, and a shell's $(< file) drops them the same way —
// the convention the official Postgres image's *_FILE variables follow.
func loadSecrets(keys ...string) (secrets, error) {
	s := make(secrets)
	for _, key := range keys {
		path := os.Getenv(key + "_FILE")
		if path == "" {
			continue
		}
		if os.Getenv(key) != "" {
			return nil, fmt.Errorf("config: %s and %s_FILE are both set; set one", key, key)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: %s_FILE: %w", key, err)
		}
		s[key] = strings.TrimRight(string(b), "\r\n")
	}
	return s, nil
}

// get returns key's value from its file if it had one, else from the
// environment.
func (s secrets) get(key string) string {
	if v, ok := s[key]; ok {
		return v
	}
	return os.Getenv(key)
}

// boolEnv reads a flag. Only the obvious spellings count as true, and anything
// else is false — a typo turning a safety default off silently is worse than a
// typo being ignored.
func boolEnv(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
