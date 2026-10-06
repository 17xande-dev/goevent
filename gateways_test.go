package main

import (
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/config"
)

// gatewayConfig is the smallest config newGateways reads, with both providers
// switched on. Tests turn one off rather than building a config from scratch.
func gatewayConfig() config.Config {
	return config.Config{
		BaseURL:  "https://store.example",
		Currency: "ZAR",
		PayFast: config.PayFast{
			MerchantID:  "10000100",
			MerchantKey: "46f0cd694581a",
			Sandbox:     true,
		},
		SnapScan: config.SnapScan{
			SnapCode:       "shopalot",
			APIKey:         "api-key",
			WebhookAuthKey: "webhook-key",
		},
	}
}

func TestNewGateways_BuildsWhatIsConfigured(t *testing.T) {
	log := slog.New(slog.DiscardHandler)

	for name, tc := range map[string]struct {
		edit func(*config.Config)
		want []string
	}{
		"both":          {func(*config.Config) {}, []string{"payfast", "snapscan"}},
		"payfast only":  {func(c *config.Config) { c.SnapScan = config.SnapScan{} }, []string{"payfast"}},
		"snapscan only": {func(c *config.Config) { c.PayFast = config.PayFast{} }, []string{"snapscan"}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := gatewayConfig()
			tc.edit(&cfg)

			r, err := newGateways(cfg, log)
			if err != nil {
				t.Fatalf("newGateways: %v", err)
			}
			var got []string
			for _, g := range r.All() {
				got = append(got, g.Name())
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("gateways = %q, want %q", got, tc.want)
			}
		})
	}
}

// Unlike a shop, an events deployment can be useful with no gateway at all:
// free events, and cash or EFT an organiser records by hand. It starts, with an
// empty registry that offers no online payment and permits no extra CSP origin.
func TestNewGateways_AllowsNoGatewayAtAll(t *testing.T) {
	cfg := gatewayConfig()
	cfg.PayFast = config.PayFast{}
	cfg.SnapScan = config.SnapScan{}

	reg, err := newGateways(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("newGateways with none configured: %v", err)
	}
	if reg.Len() != 0 {
		t.Errorf("registry has %d gateways, want 0", reg.Len())
	}
	if forms, images := reg.CSP(); len(forms) != 0 || len(images) != 0 {
		t.Errorf("an empty registry widened the CSP: form-action %q, img-src %q", forms, images)
	}
}

// Both providers settle in ZAR only. Discovering that at the first checkout,
// after an order row already exists, is worse than discovering it at boot.
func TestNewGateways_RefusesAMismatchedCurrency(t *testing.T) {
	for name, edit := range map[string]func(*config.Config){
		"payfast":  func(c *config.Config) { c.SnapScan = config.SnapScan{} },
		"snapscan": func(c *config.Config) { c.PayFast = config.PayFast{} },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := gatewayConfig()
			edit(&cfg)
			cfg.Currency = "USD"

			_, err := newGateways(cfg, slog.New(slog.DiscardHandler))
			if err == nil {
				t.Fatal("newGateways accepted a currency no gateway settles in")
			}
			if !strings.Contains(err.Error(), "USD") {
				t.Errorf("the error does not name the configured currency: %v", err)
			}
		})
	}
}

// The CSP is derived from the gateways rather than configured, because a missing
// origin is refused by the browser and nowhere else — no test that does not run
// one can see it. This asserts the derivation; the browser step in the plan
// asserts the effect.
func TestNewGateways_DeclareTheirCSPOrigins(t *testing.T) {
	log := slog.New(slog.DiscardHandler)

	both, err := newGateways(gatewayConfig(), log)
	if err != nil {
		t.Fatalf("newGateways: %v", err)
	}
	forms, images := both.CSP()
	if !slices.Contains(forms, "https://sandbox.payfast.co.za") {
		t.Errorf("form-action = %q, want PayFast's origin: the hand-over is a cross-origin form post", forms)
	}
	if !slices.Contains(images, "https://pos.snapscan.io") {
		t.Errorf("img-src = %q, want SnapScan's origin: the QR code is loaded from it", images)
	}
	for _, src := range slices.Concat(forms, images) {
		// A CSP source carrying a path matches that path exactly and refuses
		// everything beneath it — the bug that cost this project an afternoon
		// over the image bucket.
		if strings.Count(src, "/") != 2 {
			t.Errorf("CSP source %q carries a path", src)
		}
	}

	// And a store without SnapScan adds nothing to img-src, so the policy stays as
	// tight as its deployment allows.
	cfg := gatewayConfig()
	cfg.SnapScan = config.SnapScan{}
	payfastOnly, err := newGateways(cfg, log)
	if err != nil {
		t.Fatalf("newGateways: %v", err)
	}
	if _, images := payfastOnly.CSP(); len(images) != 0 {
		t.Errorf("img-src = %q for a store with no QR gateway", images)
	}
}
