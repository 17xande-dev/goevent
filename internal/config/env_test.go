package config

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/env/envtest"
)

func TestEnvironmentDocumentation(t *testing.T) {
	recorder := envtest.NewRecorder(nil)
	// Required settings are intentionally missing; the loader must still read
	// every setting, even those used only by an optional feature.
	_, _ = load(recorder.Lookup)
	documented := envtest.EnvExample(t, "../../.env.example")
	// The example also configures the test runner, which is not a server read.
	documented = slices.DeleteFunc(documented, func(key string) bool {
		return key == "TEST_DATABASE_URL"
	})
	envtest.Documented(t, ".env.example", recorder.Keys(), documented)
}

func TestLoad_ReportsProblemsWithoutEchoingValues(t *testing.T) {
	setRequired(t)
	t.Setenv("BLOB_ENDPOINT", "localhost:9000")
	const marker = "zq7-private-input"
	keys := []string{
		"BASE_URL", "LOG_LEVEL", "LOG_FORMAT", "CLIENT_IP_SOURCE",
		"EMBED_ORIGINS", "FONT_ORIGINS", "FONT_CSS_URL", "PAYFAST_NOTIFY_URL",
		"BLOB_PUBLIC_BASE_URL", "TEMPLATE_DIR", "STATIC_DIR", "SMTP_TLS", "SMTP_PORT",
	}
	for _, key := range keys {
		t.Setenv(key, marker)
	}
	_, err := Load()
	if err == nil {
		t.Fatal("invalid configuration was accepted")
	}
	if strings.Contains(err.Error(), marker) {
		t.Error("an error echoed a configuration value")
	}
	for _, key := range keys {
		if !strings.Contains(err.Error(), key+":") {
			t.Errorf("no problem reported for %s", key)
		}
	}
}

func TestEmptyIntegersUseDefaults(t *testing.T) {
	for _, tc := range []struct {
		key  string
		got  func(Config) int
		want int
	}{
		{"SESSION_TTL_HOURS", func(c Config) int { return int(c.SessionTTL / time.Hour) }, 24},
		{"SHUTDOWN_TIMEOUT_SECONDS", func(c Config) int { return int(c.ShutdownTimeout / time.Second) }, 15},
		{"RATE_LIMIT_LOGIN_PER_MINUTE", func(c Config) int { return c.RateLimits.LoginPerMinute }, 10},
		{"RATE_LIMIT_CHECKOUT_PER_MINUTE", func(c Config) int { return c.RateLimits.CheckoutPerMinute }, 20},
		{"RATE_LIMIT_CALLBACK_PER_MINUTE", func(c Config) int { return c.RateLimits.CallbackPerMinute }, 120},
		{"RATE_LIMIT_STATUS_PER_MINUTE", func(c Config) int { return c.RateLimits.StatusPerMinute }, 30},
		{"SMTP_PORT", func(c Config) int { return c.SMTP.Port }, 587},
	} {
		t.Run(tc.key, func(t *testing.T) {
			setRequired(t)
			t.Setenv(tc.key, "")
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if got := tc.got(cfg); got != tc.want {
				t.Errorf("got %d, want the default %d", got, tc.want)
			}
		})
	}
}

func TestComposeAndExampleValues(t *testing.T) {
	// Copy the server environment literals and interpolation defaults from the
	// Compose files, and active server settings from their example env files.
	// Where deployment examples leave required credentials blank, fill them
	// with test fixtures as an operator would fill in their own credentials.
	for _, tc := range []struct {
		name   string
		values map[string]string
	}{
		{"compose.yaml", map[string]string{
			"DATABASE_URL":           "postgres://goevent:goevent@postgres:5432/goevent?sslmode=disable",
			"PORT":                   "8080",
			"BASE_URL":               "http://localhost:8081",
			"SITE_NAME":              "goevent",
			"CURRENCY":               "ZAR",
			"LOG_LEVEL":              "debug",
			"SETUP_TOKEN":            "",
			"PAYFAST_MERCHANT_ID":    "10000100",
			"PAYFAST_MERCHANT_KEY":   "46f0cd694581a",
			"PAYFAST_PASSPHRASE":     "jt7NOE43FZPn",
			"PAYFAST_SANDBOX":        "true",
			"PAYFAST_ALLOWED_CIDRS":  "any",
			"PAYFAST_NOTIFY_URL":     "",
			"SMTP_HOST":              "mailpit",
			"SMTP_PORT":              "1025",
			"SMTP_TLS":               "none",
			"EMAIL_FROM":             "events@goevent.example",
			"NOTIFY_EMAIL":           "organiser@goevent.example",
			"SECRET_KEY":             strings.Repeat("ab", 32),
			"IMAGE_DIR":              "/images",
			"BLOB_ENDPOINT":          "",
			"BLOB_BUCKET":            "",
			"BLOB_ACCESS_KEY_ID":     "",
			"BLOB_SECRET_ACCESS_KEY": "",
			"BLOB_PUBLIC_BASE_URL":   "",
			"TEMPLATE_DIR":           "/theme/templates",
			"STATIC_DIR":             "/theme/static",
			"THEME_RELOAD":           "true",
		}},
		{".env.example", map[string]string{
			"PORT":                     "8080",
			"BASE_URL":                 "http://localhost:8080",
			"LOG_LEVEL":                "info",
			"LOG_FORMAT":               "json",
			"SHUTDOWN_TIMEOUT_SECONDS": "15",
			"DATABASE_URL":             "postgres://goevent:goevent@localhost:5433/goevent?sslmode=disable",
			"SITE_NAME":                "goevent",
			"CURRENCY":                 "ZAR",
			"PAYFAST_MERCHANT_ID":      "10000100",
			"PAYFAST_MERCHANT_KEY":     "46f0cd694581a",
			"PAYFAST_PASSPHRASE":       "jt7NOE43FZPn",
			"PAYFAST_SANDBOX":          "true",
			"SECRET_KEY":               strings.Repeat("ab", 32),
			"SMTP_HOST":                "localhost",
			"SMTP_PORT":                "1026",
			"SMTP_TLS":                 "none",
			"SMTP_USERNAME":            "",
			"SMTP_PASSWORD":            "",
			"EMAIL_FROM":               "events@example.com",
			"NOTIFY_EMAIL":             "organiser@example.com",
			"IMAGE_DIR":                ".local/images",
		}},
		{"deploy/standard", map[string]string{
			"DATABASE_URL":           "postgres://goevent:fixture@postgres:5432/goevent?sslmode=disable",
			"BASE_URL":               "https://events.example.com",
			"CLIENT_IP_SOURCE":       "forwarded",
			"SITE_NAME":              "Your Organisation",
			"CURRENCY":               "ZAR",
			"SECRET_KEY":             strings.Repeat("cd", 32),
			"PAYFAST_SANDBOX":        "true",
			"PAYFAST_MERCHANT_ID":    "10000100",
			"PAYFAST_MERCHANT_KEY":   "46f0cd694581a",
			"PAYFAST_PASSPHRASE":     "jt7NOE43FZPn",
			"BLOB_ENDPOINT":          "<account-id>.r2.cloudflarestorage.com",
			"BLOB_REGION":            "auto",
			"BLOB_USE_TLS":           "true",
			"BLOB_BUCKET":            "goevent-images",
			"BLOB_ACCESS_KEY_ID":     "fixture-id",
			"BLOB_SECRET_ACCESS_KEY": "fixture-key",
			"BLOB_PUBLIC_BASE_URL":   "https://images.example.com",
			"SMTP_HOST":              "smtp.example.com",
			"SMTP_PORT":              "587",
			"SMTP_TLS":               "starttls",
			"SMTP_USERNAME":          "",
			"SMTP_PASSWORD":          "",
			"EMAIL_FROM":             "events@example.com",
			"NOTIFY_EMAIL":           "",
		}},
		{"deploy/tunnel", map[string]string{
			"DATABASE_URL":           "postgres://goevent:fixture@postgres:5432/goevent?sslmode=disable",
			"BASE_URL":               "https://events.example.com",
			"CLIENT_IP_SOURCE":       "cloudflare",
			"SITE_NAME":              "Your Organisation",
			"CURRENCY":               "ZAR",
			"SECRET_KEY":             strings.Repeat("cd", 32),
			"PAYFAST_SANDBOX":        "true",
			"PAYFAST_MERCHANT_ID":    "10000100",
			"PAYFAST_MERCHANT_KEY":   "46f0cd694581a",
			"PAYFAST_PASSPHRASE":     "jt7NOE43FZPn",
			"GRAPH_TENANT_ID":        "fixture-tenant",
			"GRAPH_CLIENT_ID":        "fixture-client",
			"GRAPH_CLIENT_SECRET":    "fixture-secret",
			"EMAIL_FROM":             "events@example.com",
			"NOTIFY_EMAIL":           "",
			"BLOB_ENDPOINT":          "<account-id>.r2.cloudflarestorage.com",
			"BLOB_REGION":            "auto",
			"BLOB_USE_TLS":           "true",
			"BLOB_BUCKET":            "goevent-images",
			"BLOB_ACCESS_KEY_ID":     "fixture-id",
			"BLOB_SECRET_ACCESS_KEY": "fixture-key",
			"BLOB_PUBLIC_BASE_URL":   "https://images.example.com",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := envtest.NewRecorder(tc.values)
			lookup := func(key string) (string, bool) {
				value, ok := recorder.Lookup(key)
				// Use the source of the Compose bind mount, since the unit
				// test runs on the host and /theme only exists in the image.
				if key == "TEMPLATE_DIR" || key == "STATIC_DIR" {
					if suffix, mounted := strings.CutPrefix(value, "/theme/"); mounted {
						return "../../theme/" + suffix, ok
					}
				}
				return value, ok
			}
			if _, err := load(lookup); err != nil {
				t.Fatal(err)
			}
			read := recorder.Keys()
			for key := range tc.values {
				if !slices.Contains(read, key) {
					t.Errorf("%s was not parsed", key)
				}
			}
		})
	}
}
