package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/goevent/internal/middleware"
)

// setRequired sets every required var to something valid, so each test can
// break exactly one of them and know which failure it is asserting on.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/goevent")
	t.Setenv("SECRET_KEY", strings.Repeat("ab", 32))
	// PayFast's own published sandbox credentials — see .env.example. A gateway
	// is optional, but this is the configuration most deployments have.
	t.Setenv("PAYFAST_MERCHANT_ID", "10000100")
	t.Setenv("PAYFAST_MERCHANT_KEY", "46f0cd694581a")
	// Images and mail are required too. IMAGE_DIR is the cheapest of the two image
	// backends and needs nothing running, which is why it is the one used here.
	t.Setenv("IMAGE_DIR", t.TempDir())
	t.Setenv("SMTP_HOST", "localhost")
	t.Setenv("EMAIL_FROM", "events@example.com")
}

// A deployment with no payment gateway is valid: free events, and cash or EFT
// recorded by hand.
func TestLoad_NoGatewayIsAllowed(t *testing.T) {
	setRequired(t)
	t.Setenv("PAYFAST_MERCHANT_ID", "")
	t.Setenv("PAYFAST_MERCHANT_KEY", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load with no gateway: %v", err)
	}
	if c.PayFast.Configured() || c.SnapScan.Configured() {
		t.Error("a gateway reads as configured with none set")
	}
}

func TestLoad_RequiresSecrets(t *testing.T) {
	// Each required var, named in the error, so a misconfigured deployment says
	// what is missing instead of failing later and less clearly.
	for _, key := range []string{
		"DATABASE_URL", "SMTP_HOST", "EMAIL_FROM", "SECRET_KEY",
		// Only once PayFast is switched on by its merchant id, which setRequired does.
		"PAYFAST_MERCHANT_KEY",
	} {
		setRequired(t)
		t.Setenv(key, "")

		_, err := Load()
		if err == nil {
			t.Errorf("%s unset: expected an error, got nil", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s unset: error %q does not name it", key, err)
		}
	}
}

func TestLoad_Defaults(t *testing.T) {
	setRequired(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Port != "8080" {
		t.Errorf("Port = %q, want 8080", c.Port)
	}
	if c.Currency != "ZAR" {
		t.Errorf("Currency = %q, want ZAR", c.Currency)
	}
	if c.BaseURL != "http://localhost:8080" {
		t.Errorf("BaseURL = %q, want http://localhost:8080", c.BaseURL)
	}
	if c.SessionTTL != 24*time.Hour {
		t.Errorf("SessionTTL = %s, want 24h", c.SessionTTL)
	}
	// No admin credential in the environment at all: the first account is claimed
	// at /admin/setup, and there is nothing to sign a cookie with because a
	// session is a row.
	if c.SetupToken != "" {
		t.Errorf("SetupToken = %q with SETUP_TOKEN unset, want empty", c.SetupToken)
	}
	// A plain-HTTP BaseURL cannot use Secure cookies, or local development
	// could never sign in.
	if c.CookieSecure {
		t.Error("CookieSecure is set for an http:// BaseURL")
	}
}

func TestLoad_RejectsMalformedEmailQueueKey(t *testing.T) {
	for _, value := range []string{"short", strings.Repeat("zz", 32), strings.Repeat("ab", 31)} {
		setRequired(t)
		t.Setenv("SECRET_KEY", value)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "SECRET_KEY") {
			t.Fatalf("key accepted: %v", err)
		}
	}
}

func TestLoad_TrimsTrailingSlashFromBaseURL(t *testing.T) {
	setRequired(t)
	t.Setenv("BASE_URL", "https://events.example.com/")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.BaseURL != "https://events.example.com" {
		t.Errorf("BaseURL = %q, want https://events.example.com", c.BaseURL)
	}
	// HTTPS deployments always want Secure cookies, so this is derived rather
	// than being one more thing to forget.
	if !c.CookieSecure {
		t.Error("CookieSecure is not set for an https:// BaseURL")
	}
}

func TestLoad_RejectsBadLogLevel(t *testing.T) {
	setRequired(t)
	t.Setenv("LOG_LEVEL", "verbose")

	if _, err := Load(); err == nil {
		t.Fatal("expected an error for an unknown LOG_LEVEL, got nil")
	}
}

func TestLoad_SetupToken(t *testing.T) {
	setRequired(t)

	// Long enough is passed through verbatim: it is compared against a hash, not
	// decoded, so the server must not reinterpret what an operator supplied.
	token := strings.Repeat("t", MinSetupTokenLen)
	t.Setenv("SETUP_TOKEN", "  "+token+"  ")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SetupToken != token {
		t.Errorf("SetupToken = %q, want the trimmed %q", c.SetupToken, token)
	}

	// A short one is refused rather than accepted: while it is unclaimed it is
	// the credential for the whole admin area, and a guessable one is worse than
	// no automated bootstrap at all.
	t.Setenv("SETUP_TOKEN", strings.Repeat("t", MinSetupTokenLen-1))
	if _, err := Load(); err == nil {
		t.Error("a SETUP_TOKEN one character short of the minimum was accepted")
	} else if !strings.Contains(err.Error(), "SETUP_TOKEN") {
		t.Errorf("error %q does not name SETUP_TOKEN", err)
	}
}

func TestLoad_EmbedOrigins(t *testing.T) {
	setRequired(t)

	// Unset means no embedding, which is the right default for a site only ever
	// browsed on its own domain.
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AllowsEmbedding() {
		t.Error("embedding is allowed with EMBED_ORIGINS unset")
	}

	t.Setenv("EMBED_ORIGINS", " https://cms.example , https://www.cms.example:8443 ")
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"https://cms.example", "https://www.cms.example:8443"}
	if len(c.EmbedOrigins) != len(want) {
		t.Fatalf("EmbedOrigins = %v, want %v", c.EmbedOrigins, want)
	}
	for i := range want {
		if c.EmbedOrigins[i] != want[i] {
			t.Errorf("EmbedOrigins[%d] = %q, want %q", i, c.EmbedOrigins[i], want[i])
		}
	}
	if !c.AllowsEmbedding() {
		t.Error("AllowsEmbedding() is false with two origins configured")
	}

	t.Setenv("EMBED_ORIGINS", "*")
	if c, err = Load(); err != nil || len(c.EmbedOrigins) != 1 || c.EmbedOrigins[0] != "*" {
		t.Errorf("wildcard: %v, %v", c.EmbedOrigins, err)
	}

	// These are compared literally against an Origin header, so anything that
	// could never match one is a misconfiguration worth failing on now.
	for _, bad := range []string{"cms.example", "https://cms.example/", "https://cms.example/embed", "://nope"} {
		setRequired(t)
		t.Setenv("EMBED_ORIGINS", bad)
		if _, err := Load(); err == nil {
			t.Errorf("EMBED_ORIGINS %q was accepted", bad)
		}
	}
}

func TestLoad_FontOrigins(t *testing.T) {
	setRequired(t)

	// Unset is the default and the closed position: the bundled theme uses the
	// system font stack, so no origin needs allowing and no <link> is rendered.
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.FontOrigins) != 0 || c.FontCSSURL != "" {
		t.Errorf("fonts are configured by default: %v, %q", c.FontOrigins, c.FontCSSURL)
	}

	// The Typekit shape: two hosts, because the kit's stylesheet and the font files
	// it points at are served from different ones.
	t.Setenv("FONT_ORIGINS", " https://use.typekit.net , https://p.typekit.net ")
	t.Setenv("FONT_CSS_URL", "https://use.typekit.net/abc1def.css")
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"https://use.typekit.net", "https://p.typekit.net"}
	if len(c.FontOrigins) != len(want) {
		t.Fatalf("FontOrigins = %v, want %v", c.FontOrigins, want)
	}
	for i := range want {
		if c.FontOrigins[i] != want[i] {
			t.Errorf("FontOrigins[%d] = %q, want %q", i, c.FontOrigins[i], want[i])
		}
	}
	if c.FontCSSURL != "https://use.typekit.net/abc1def.css" {
		t.Errorf("FontCSSURL = %q", c.FontCSSURL)
	}

	// A stylesheet the CSP would block is refused at boot. In a browser the only
	// symptom is a console warning and a page in the fallback font, which is a
	// genuinely slow thing to work out.
	setRequired(t)
	t.Setenv("FONT_ORIGINS", "https://p.typekit.net")
	t.Setenv("FONT_CSS_URL", "https://use.typekit.net/abc1def.css")
	if _, err := Load(); err == nil {
		t.Error("FONT_CSS_URL on an origin FONT_ORIGINS does not list was accepted")
	}

	// A wildcard font source would let any origin serve a stylesheet to every page
	// including the checkout. The embed list may say "*"; this one may not.
	setRequired(t)
	t.Setenv("FONT_CSS_URL", "")
	t.Setenv("FONT_ORIGINS", "*")
	if _, err := Load(); err == nil {
		t.Error(`FONT_ORIGINS="*" was accepted`)
	}

	// Same literal-comparison rule as the embed origins: a CSP source carrying a
	// path matches that path exactly, so a font list entry with one is a mistake.
	for _, bad := range []string{"use.typekit.net", "https://use.typekit.net/", "https://use.typekit.net/abc1def.css"} {
		setRequired(t)
		t.Setenv("FONT_CSS_URL", "")
		t.Setenv("FONT_ORIGINS", bad)
		if _, err := Load(); err == nil {
			t.Errorf("FONT_ORIGINS %q was accepted", bad)
		}
	}

	// And a relative stylesheet URL has no origin to check against the policy.
	setRequired(t)
	t.Setenv("FONT_ORIGINS", "https://use.typekit.net")
	t.Setenv("FONT_CSS_URL", "/fonts/kit.css")
	if _, err := Load(); err == nil {
		t.Error("a relative FONT_CSS_URL was accepted")
	}
}

func TestLoad_PayFast(t *testing.T) {
	setRequired(t)

	// Sandbox is on unless explicitly turned off: the wrong default here takes
	// real money during somebody's first afternoon with the project.
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.PayFast.Sandbox {
		t.Error("PayFast.Sandbox is false by default")
	}
	if c.PayFast.AllowAnySourceIP {
		t.Error("the notification source-IP check is disabled by default")
	}
	if c.PayFast.AllowedCIDRs != nil {
		t.Errorf("AllowedCIDRs = %v with the var unset, want nil so the gateway's defaults apply", c.PayFast.AllowedCIDRs)
	}

	t.Setenv("PAYFAST_SANDBOX", "false")
	// Real credentials alongside it: turning the sandbox off while still using
	// PayFast's published demo merchant id is refused. See
	// TestLoad_RefusesRealPaymentsWithDemoCredentials.
	t.Setenv("PAYFAST_MERCHANT_ID", "20000200")
	t.Setenv("PAYFAST_ALLOWED_CIDRS", " 10.0.0.0/8 , 192.168.0.0/16 ")
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PayFast.Sandbox {
		t.Error("PAYFAST_SANDBOX=false did not turn the sandbox off")
	}
	want := []string{"10.0.0.0/8", "192.168.0.0/16"}
	if len(c.PayFast.AllowedCIDRs) != len(want) {
		t.Fatalf("AllowedCIDRs = %v, want %v", c.PayFast.AllowedCIDRs, want)
	}
	for i := range want {
		if c.PayFast.AllowedCIDRs[i] != want[i] {
			t.Errorf("AllowedCIDRs[%d] = %q, want %q", i, c.PayFast.AllowedCIDRs[i], want[i])
		}
	}

	// Disabling the check is spelled out, so it cannot happen by leaving something
	// blank.
	t.Setenv("PAYFAST_ALLOWED_CIDRS", "any")
	if c, err = Load(); err != nil || !c.PayFast.AllowAnySourceIP {
		t.Errorf("PAYFAST_ALLOWED_CIDRS=any: AllowAnySourceIP = %v, err = %v", c.PayFast.AllowAnySourceIP, err)
	}

	// The notify URL is the one PayFast's servers must reach, so a relative one
	// is a misconfiguration that would only show up as a missing notification.
	for _, bad := range []string{"/payments/payfast/callback", "notify.example.com"} {
		setRequired(t)
		t.Setenv("PAYFAST_NOTIFY_URL", bad)
		if _, err := Load(); err == nil {
			t.Errorf("PAYFAST_NOTIFY_URL %q was accepted", bad)
		}
	}
}

func TestLoad_Blob(t *testing.T) {
	setRequired(t)

	// Unconfigured is a complete, working site: object storage is optional.
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Blob.Configured() {
		t.Error("object storage is configured with no BLOB_ENDPOINT")
	}

	blobEnv := func(t *testing.T) {
		t.Helper()
		setRequired(t)
		// The two image backends are mutually exclusive and setRequired picks the
		// directory, so a test about object storage has to put that down first.
		t.Setenv("IMAGE_DIR", "")
		t.Setenv("BLOB_ENDPOINT", "localhost:9000")
		t.Setenv("BLOB_BUCKET", "goevent-images")
		t.Setenv("BLOB_ACCESS_KEY_ID", "key")
		t.Setenv("BLOB_SECRET_ACCESS_KEY", "secret")
		t.Setenv("BLOB_PUBLIC_BASE_URL", "http://localhost:9000/goevent-images")
	}

	blobEnv(t)
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Blob.Configured() {
		t.Fatal("a full BLOB_* set was not recognised")
	}
	// "auto" is what R2 wants, and GCS and MinIO ignore it.
	if c.Blob.Region != "auto" {
		t.Errorf("Region = %q, want auto", c.Blob.Region)
	}
	if !c.Blob.UseTLS {
		t.Error("TLS is off by default; it should have to be turned off deliberately")
	}
	// A trailing slash must not survive into image URLs.
	t.Setenv("BLOB_PUBLIC_BASE_URL", "https://images.example/")
	if c, err = Load(); err != nil || c.Blob.PublicBaseURL != "https://images.example" {
		t.Errorf("PublicBaseURL = %q, %v", c.Blob.PublicBaseURL, err)
	}

	// A partial configuration fails at boot, not at the first upload — and the error
	// names what is missing.
	for _, key := range []string{
		"BLOB_BUCKET", "BLOB_ACCESS_KEY_ID", "BLOB_SECRET_ACCESS_KEY", "BLOB_PUBLIC_BASE_URL",
	} {
		blobEnv(t)
		t.Setenv(key, "")

		_, err := Load()
		if err == nil {
			t.Errorf("%s unset was accepted", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s unset: error %q does not name it", key, err)
		}
	}

	// And the other direction: credentials with no endpoint means uploads are off
	// while somebody clearly intended them on.
	setRequired(t)
	t.Setenv("BLOB_BUCKET", "goevent-images")
	t.Setenv("BLOB_ACCESS_KEY_ID", "key")
	if _, err := Load(); err == nil {
		t.Error("BLOB_* without BLOB_ENDPOINT was accepted, so uploads would be silently off")
	}

	blobEnv(t)
	t.Setenv("BLOB_PUBLIC_BASE_URL", "/images")
	if _, err := Load(); err == nil {
		t.Error("a relative BLOB_PUBLIC_BASE_URL was accepted")
	}
}

func TestBlob_PublicOrigin(t *testing.T) {
	// The CSP source has to be the origin alone. A source carrying a path matches
	// that path *exactly*, so listing the bucket base would permit the bucket root
	// and refuse every image beneath it — which is invisible outside a browser,
	// because the image itself still returns 200.
	cases := map[string]string{
		"http://localhost:9000/goevent-images": "http://localhost:9000",
		"https://images.example":               "https://images.example",
		"https://images.example/":              "https://images.example",
		"https://pub-abc.r2.dev/bucket/nested": "https://pub-abc.r2.dev",
		"not a url":                            "",
		"":                                     "",
	}
	for base, want := range cases {
		got := Blob{PublicBaseURL: base}.PublicOrigin()
		if got != want {
			t.Errorf("Blob{%q}.PublicOrigin() = %q, want %q", base, got, want)
		}
		if strings.Count(got, "/") > 2 {
			t.Errorf("PublicOrigin() = %q still carries a path", got)
		}
	}
}

func TestLoad_ClientIPSource(t *testing.T) {
	setRequired(t)

	// remote by default: believing a header without a proxy in front that sets it
	// lets a client claim any source IP, which is one of the checks on the
	// payment callback.
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ClientIPSource != middleware.ClientIPRemote {
		t.Errorf("default ClientIPSource = %q, want %q", c.ClientIPSource, middleware.ClientIPRemote)
	}

	for _, want := range []middleware.ClientIPSource{
		middleware.ClientIPRemote,
		middleware.ClientIPForwarded,
		middleware.ClientIPCloudflare,
	} {
		t.Setenv("CLIENT_IP_SOURCE", string(want))
		if c, err = Load(); err != nil || c.ClientIPSource != want {
			t.Errorf("CLIENT_IP_SOURCE=%s: got %q, %v", want, c.ClientIPSource, err)
		}
	}

	// A typo is refused rather than silently falling back, because every value
	// here is load-bearing for the rate limits and the callback's source-IP check.
	t.Setenv("CLIENT_IP_SOURCE", "cloudlfare")
	if _, err = Load(); err == nil {
		t.Error("a misspelt CLIENT_IP_SOURCE was accepted")
	}
}

func TestLoad_TrustProxyIPIsRefused(t *testing.T) {
	setRequired(t)

	// The variable it replaced must not be ignored: a deployment that still sets
	// it would fall back to remote, so every registrant would share one rate-limit
	// bucket and the payment callback would reject every genuine notification.
	// Both fail quietly, so the boot does not.
	for _, v := range []string{"true", "false"} {
		t.Setenv("TRUST_PROXY_IP", v)
		_, err := Load()
		if err == nil {
			t.Fatalf("TRUST_PROXY_IP=%s was accepted", v)
		}
		if !strings.Contains(err.Error(), "CLIENT_IP_SOURCE") {
			t.Errorf("TRUST_PROXY_IP=%s error does not name the replacement: %v", v, err)
		}
	}
}

func TestLoad_SessionTTL(t *testing.T) {
	setRequired(t)
	t.Setenv("SESSION_TTL_HOURS", "8")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.SessionTTL != 8*time.Hour {
		t.Errorf("SessionTTL = %s, want 8h", c.SessionTTL)
	}

	for _, bad := range []string{"0", "-1", "eight", ""} {
		setRequired(t)
		t.Setenv("SESSION_TTL_HOURS", bad)
		if _, err := Load(); err == nil {
			t.Errorf("SESSION_TTL_HOURS %q was accepted", bad)
		}
	}
}

// LoadTool is what cmd/seed and the server's migration modes use. The point of
// it is what it does *not* require: a migration job gets the database URL and
// none of the secrets, so these assertions are the security property, not a
// convenience.
func TestLoadTool_NeedsOnlyDatabaseURL(t *testing.T) {
	for _, key := range []string{
		"SETUP_TOKEN", "PAYFAST_MERCHANT_ID", "PAYFAST_MERCHANT_KEY",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/goevent")

	c, err := LoadTool()
	if err != nil {
		t.Fatalf("LoadTool with only DATABASE_URL: %v", err)
	}
	if c.DatabaseURL == "" {
		t.Error("DatabaseURL is empty")
	}
	if c.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want the info default", c.LogLevel)
	}
}

// writeSecret puts contents in a file of its own and returns the path, the way a
// Compose secret arrives under /run/secrets.
func writeSecret(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(contents), 0o400); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_SecretFromFile(t *testing.T) {
	setRequired(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("PAYFAST_MERCHANT_KEY", "")

	// A trailing newline is what `pass show` and most editors leave, and CRLF is
	// what a file written on Windows does; both belong to the file, not the value.
	t.Setenv("DATABASE_URL_FILE", writeSecret(t, "postgres://u:from-file@db:5432/goevent\n"))
	t.Setenv("PAYFAST_MERCHANT_KEY_FILE", writeSecret(t, "key-from-file\r\n"))

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := "postgres://u:from-file@db:5432/goevent"; c.DatabaseURL != want {
		t.Errorf("DatabaseURL = %q, want %q", c.DatabaseURL, want)
	}
	if want := "key-from-file"; c.PayFast.MerchantKey != want {
		t.Errorf("MerchantKey = %q, want %q", c.PayFast.MerchantKey, want)
	}
}

func TestLoad_SecretKeepsInnerWhitespace(t *testing.T) {
	setRequired(t)
	t.Setenv("PAYFAST_PASSPHRASE", "")
	// Only trailing newlines go. A passphrase is the account's choice, and one
	// with a space in it must survive — trimming it would sign every payment
	// with the wrong key.
	t.Setenv("PAYFAST_PASSPHRASE_FILE", writeSecret(t, "a pass phrase\n"))

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := "a pass phrase"; c.PayFast.Passphrase != want {
		t.Errorf("Passphrase = %q, want %q", c.PayFast.Passphrase, want)
	}
}

func TestLoad_SecretFileSatisfiesRequired(t *testing.T) {
	setRequired(t)
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_URL_FILE", writeSecret(t, "postgres://u:p@db:5432/goevent"))

	// DATABASE_URL is required; arriving by file is arriving.
	if _, err := Load(); err != nil {
		t.Fatalf("a required secret supplied by file was reported missing: %v", err)
	}
}

func TestLoad_SecretValueAndFileBothSetIsRefused(t *testing.T) {
	setRequired(t)
	t.Setenv("PAYFAST_MERCHANT_KEY_FILE", writeSecret(t, "other-key"))

	// Which one wins would be a guess, and guessing wrong about a merchant key
	// fails every payment. Refused, naming both.
	_, err := Load()
	if err == nil {
		t.Fatal("PAYFAST_MERCHANT_KEY and PAYFAST_MERCHANT_KEY_FILE both set was accepted")
	}
	for _, want := range []string{"PAYFAST_MERCHANT_KEY", "PAYFAST_MERCHANT_KEY_FILE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %s: %v", want, err)
		}
	}
}

func TestLoad_EmptyValueBesideFileIsNotAConflict(t *testing.T) {
	setRequired(t)
	// The development compose file supplies these as ${KEY:-}: present, empty.
	// That must not read as a clash with a KEY_FILE next to it.
	t.Setenv("PAYFAST_MERCHANT_KEY", "")
	t.Setenv("PAYFAST_MERCHANT_KEY_FILE", writeSecret(t, "key-from-file"))

	c, err := Load()
	if err != nil {
		t.Fatalf("empty KEY beside KEY_FILE was refused: %v", err)
	}
	if c.PayFast.MerchantKey != "key-from-file" {
		t.Errorf("MerchantKey = %q, want the file's value", c.PayFast.MerchantKey)
	}
}

func TestLoad_EmptyFileVarFallsBackToValue(t *testing.T) {
	setRequired(t)
	t.Setenv("PAYFAST_MERCHANT_KEY_FILE", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.PayFast.MerchantKey != "46f0cd694581a" {
		t.Errorf("MerchantKey = %q, want the plain value when KEY_FILE is empty", c.PayFast.MerchantKey)
	}
}

func TestLoad_UnreadableSecretFileIsRefused(t *testing.T) {
	setRequired(t)
	t.Setenv("SMTP_PASSWORD_FILE", filepath.Join(t.TempDir(), "not-there"))

	// A secret that failed to arrive is a boot failure naming the setting, not an
	// empty password discovered when the first receipt fails to send.
	_, err := Load()
	if err == nil {
		t.Fatal("an unreadable SMTP_PASSWORD_FILE was accepted")
	}
	if !strings.Contains(err.Error(), "SMTP_PASSWORD_FILE") {
		t.Errorf("error does not name SMTP_PASSWORD_FILE: %v", err)
	}
}

func TestLoad_EverySecretKeyAcceptsAFile(t *testing.T) {
	// Guards the list itself: each credential must actually be read through the
	// resolver, so adding a key to secretKeys and forgetting to switch its read
	// over is caught here rather than in production. The value is distinctive
	// enough that finding it anywhere in the loaded config proves it arrived.
	for _, key := range secretKeys {
		t.Run(key, func(t *testing.T) {
			setRequired(t)
			// Features whose credentials are only read when the feature is on.
			t.Setenv("SNAPSCAN_SNAP_CODE", "shop")
			t.Setenv("SNAPSCAN_API_KEY", "api")
			t.Setenv("SNAPSCAN_WEBHOOK_AUTH_KEY", "hook")
			t.Setenv("BLOB_ENDPOINT", "localhost:9000")
			t.Setenv("BLOB_BUCKET", "images")
			t.Setenv("BLOB_ACCESS_KEY_ID", "id")
			t.Setenv("BLOB_SECRET_ACCESS_KEY", "secret")
			t.Setenv("BLOB_PUBLIC_BASE_URL", "http://localhost:9000/images")
			t.Setenv("IMAGE_DIR", "")
			t.Setenv("SMTP_USERNAME", "events@example.com")

			marker := "via-file-7f3a9c"
			if key == "SECRET_KEY" {
				marker = strings.Repeat("cd", 32)
			}
			value := marker
			switch key {
			case "DATABASE_URL":
				value = "postgres://u:" + marker + "@db:5432/goevent"
			case "SETUP_TOKEN":
				value = marker + strings.Repeat("x", MinSetupTokenLen)
			case "SMTP_OAUTH_CLIENT_SECRET":
				// XOAUTH2 needs all three of its settings and refuses a password
				// alongside them.
				t.Setenv("SMTP_OAUTH_TENANT_ID", "tenant")
				t.Setenv("SMTP_OAUTH_CLIENT_ID", "client")
				t.Setenv("SMTP_PASSWORD", "")
			case "GRAPH_CLIENT_SECRET":
				t.Setenv("GRAPH_TENANT_ID", "tenant")
				t.Setenv("GRAPH_CLIENT_ID", "client")
			}
			t.Setenv(key, "")
			t.Setenv(key+"_FILE", writeSecret(t, value+"\n"))

			c, err := Load()
			if err != nil {
				t.Fatalf("Load with %s_FILE: %v", key, err)
			}
			if got := fmt.Sprintf("%+v", c); !strings.Contains(got, marker) {
				t.Errorf("%s_FILE was accepted but its value never reached the config", key)
			}
		})
	}
}

func TestLoadTool_DatabaseURLFromFile(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DATABASE_URL_FILE", writeSecret(t, "postgres://u:from-file@db:5432/goevent\n"))
	// A secret the tool never reads must not stop it: a migration job is handed
	// the database URL and nothing else, so a merchant key's file that was never
	// mounted into it is none of its business.
	t.Setenv("PAYFAST_MERCHANT_KEY_FILE", filepath.Join(t.TempDir(), "not-mounted"))

	c, err := LoadTool()
	if err != nil {
		t.Fatalf("LoadTool: %v", err)
	}
	if want := "postgres://u:from-file@db:5432/goevent"; c.DatabaseURL != want {
		t.Errorf("DatabaseURL = %q, want %q", c.DatabaseURL, want)
	}
}

func TestLoadTool_RequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	_, err := LoadTool()
	if err == nil {
		t.Fatal("DATABASE_URL unset: expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Errorf("error %q does not name DATABASE_URL", err)
	}
}

// A bad LOG_LEVEL is rejected on both paths. It was once checked only in Load,
// which meant a typo'd level was a startup failure for the server and silently
// accepted by every tool.
func TestLoadTool_RejectsBadLogLevel(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/goevent")
	t.Setenv("LOG_LEVEL", "verbose")

	if _, err := LoadTool(); err == nil {
		t.Error("LOG_LEVEL=verbose was accepted")
	}
}

func TestLoad_RefusesRealPaymentsWithDemoCredentials(t *testing.T) {
	// The mistake this catches is the second half of a two-step one: a site is
	// found to have been quietly running against the sandbox, somebody sets
	// PAYFAST_SANDBOX=false, and does not notice that the merchant id came from
	// .env.example too. Every payment would then be signed with a key printed in
	// PayFast's own documentation.
	setRequired(t) // leaves PAYFAST_MERCHANT_ID at the published sandbox id
	t.Setenv("PAYFAST_SANDBOX", "false")

	_, err := Load()
	if err == nil {
		t.Fatal("real payments were accepted with the demo merchant id")
	}
	// The message has to name the variable and the value, because the person
	// reading it has just been told their deployment will not start.
	for _, want := range []string{"PAYFAST_SANDBOX", "PAYFAST_MERCHANT_ID", "10000100"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %s: %v", want, err)
		}
	}

	// The same credentials are fine while the sandbox is on — that is the whole
	// point of shipping them.
	t.Setenv("PAYFAST_SANDBOX", "true")
	if _, err := Load(); err != nil {
		t.Errorf("the demo credentials were refused in sandbox mode: %v", err)
	}

	// And real credentials with the sandbox off are the ordinary production case.
	t.Setenv("PAYFAST_SANDBOX", "false")
	t.Setenv("PAYFAST_MERCHANT_ID", "20000200")
	if _, err := Load(); err != nil {
		t.Errorf("real credentials with the sandbox off were refused: %v", err)
	}
}

// setOAuth configures a complete app registration, so each case below can break
// exactly one part of it.
func setOAuth(t *testing.T) {
	t.Helper()
	t.Setenv("SMTP_USERNAME", "events@example.com")
	t.Setenv("SMTP_OAUTH_TENANT_ID", "tenant-id")
	t.Setenv("SMTP_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("SMTP_OAUTH_CLIENT_SECRET", "client-secret")
}

func TestLoad_SMTPOAuth(t *testing.T) {
	setRequired(t)
	setOAuth(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.SMTP.OAuth.Configured() {
		t.Fatalf("OAuth = %+v, want it configured", c.SMTP.OAuth)
	}
	if c.SMTP.OAuth.TenantID != "tenant-id" || c.SMTP.OAuth.ClientID != "client-id" ||
		c.SMTP.OAuth.ClientSecret != "client-secret" {
		t.Errorf("OAuth = %+v, want the configured registration", c.SMTP.OAuth)
	}

	// Absent entirely is the ordinary case, not an error: a relay reached with a
	// password, or none at all, is still supported.
	t.Setenv("SMTP_OAUTH_TENANT_ID", "")
	t.Setenv("SMTP_OAUTH_CLIENT_ID", "")
	t.Setenv("SMTP_OAUTH_CLIENT_SECRET", "")
	c, err = Load()
	if err != nil {
		t.Fatalf("Load with no OAuth: %v", err)
	}
	if c.SMTP.OAuth.Configured() {
		t.Error("an empty environment produced a configured registration")
	}
}

// A half-configured registration is a boot failure. The alternative is a server
// that starts, takes a registration, and only then discovers it cannot
// authenticate — with the registrant's tickets in the message it failed to send.
func TestLoad_SMTPOAuthMustBeComplete(t *testing.T) {
	for _, missing := range []string{
		"SMTP_OAUTH_TENANT_ID", "SMTP_OAUTH_CLIENT_ID", "SMTP_OAUTH_CLIENT_SECRET",
	} {
		t.Run(missing, func(t *testing.T) {
			setRequired(t)
			setOAuth(t)
			t.Setenv(missing, "")

			_, err := Load()
			if err == nil {
				t.Fatalf("a registration missing %s was accepted", missing)
			}
			if !strings.Contains(err.Error(), "must be set together") {
				t.Errorf("error = %v, want it to say the three go together", err)
			}
		})
	}
}

func TestLoad_SMTPOAuthNeedsAUsername(t *testing.T) {
	setRequired(t)
	setOAuth(t)
	t.Setenv("SMTP_USERNAME", "")

	_, err := Load()
	if err == nil {
		t.Fatal("OAuth with no username was accepted")
	}
	if !strings.Contains(err.Error(), "SMTP_USERNAME") {
		t.Errorf("error = %v, want it to name SMTP_USERNAME", err)
	}
}

// Refused rather than resolved by precedence: both being set means somebody has
// a belief about which one is in use, and a silent winner would leave a stale
// secret in the environment looking live.
func TestLoad_SMTPOAuthAndPasswordConflict(t *testing.T) {
	setRequired(t)
	setOAuth(t)
	t.Setenv("SMTP_PASSWORD", "hunter2")

	_, err := Load()
	if err == nil {
		t.Fatal("a password and an app registration were both accepted")
	}
	if !strings.Contains(err.Error(), "SMTP_PASSWORD") {
		t.Errorf("error = %v, want it to name SMTP_PASSWORD", err)
	}
}

// setGraph configures a complete Graph app registration, so each case below can
// break exactly one part of it.
func setGraph(t *testing.T) {
	t.Helper()
	t.Setenv("GRAPH_TENANT_ID", "tenant-id")
	t.Setenv("GRAPH_CLIENT_ID", "client-id")
	t.Setenv("GRAPH_CLIENT_SECRET", "client-secret")
	t.Setenv("EMAIL_FROM", "events@example.com")
}

// A Graph-only deployment sets no SMTP_HOST at all, and that has to satisfy the
// mail requirement on its own — EMAIL_FROM without a host is what Graph looks
// like, not a half-configured relay.
func TestLoad_GraphOnlySatisfiesTheMailRequirement(t *testing.T) {
	t.Setenv("SECRET_KEY", strings.Repeat("ab", 32))
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/goevent")
	t.Setenv("PAYFAST_MERCHANT_ID", "10000100")
	t.Setenv("PAYFAST_MERCHANT_KEY", "46f0cd694581a")
	t.Setenv("IMAGE_DIR", t.TempDir())
	setGraph(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Graph.Configured() {
		t.Fatalf("Graph = %+v, want it configured", c.Graph)
	}
	if c.SMTP.Configured() {
		t.Error("SMTP reported configured with no SMTP_HOST set")
	}
}

// Half-configured Graph is a boot failure. The alternative is a server that
// starts, takes a registration, and only then discovers it cannot send.
func TestLoad_GraphMustBeComplete(t *testing.T) {
	for _, missing := range []string{
		"GRAPH_TENANT_ID", "GRAPH_CLIENT_ID", "GRAPH_CLIENT_SECRET", "EMAIL_FROM",
	} {
		t.Run(missing, func(t *testing.T) {
			setRequired(t)
			setGraph(t)
			// setRequired already set SMTP_HOST/EMAIL_FROM; clear SMTP_HOST so
			// this exercises the Graph-only shape, then break one Graph var.
			t.Setenv("SMTP_HOST", "")
			t.Setenv(missing, "")

			_, err := Load()
			if err == nil {
				t.Fatalf("a registration missing %s was accepted", missing)
			}
			if !strings.Contains(err.Error(), "must all be set") &&
				!strings.Contains(err.Error(), "are required") {
				t.Errorf("error = %v, want it to name the missing requirement", err)
			}
		})
	}
}

// SMTP_HOST with no EMAIL_FROM is still a mistake even though Graph makes
// EMAIL_FROM-without-a-host acceptable: a relay with no sender is not a working
// configuration either way.
func TestLoad_SMTPHostNeedsEmailFrom(t *testing.T) {
	t.Setenv("SECRET_KEY", strings.Repeat("ab", 32))
	t.Setenv("DATABASE_URL", "postgres://u:p@localhost:5432/goevent")
	t.Setenv("PAYFAST_MERCHANT_ID", "10000100")
	t.Setenv("PAYFAST_MERCHANT_KEY", "46f0cd694581a")
	t.Setenv("IMAGE_DIR", t.TempDir())
	t.Setenv("SMTP_HOST", "localhost")

	_, err := Load()
	if err == nil {
		t.Fatal("SMTP_HOST with no EMAIL_FROM was accepted")
	}
	if !strings.Contains(err.Error(), "EMAIL_FROM") {
		t.Errorf("error = %v, want it to name EMAIL_FROM", err)
	}
}
