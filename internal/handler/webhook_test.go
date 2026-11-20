package handler

import (
	"crypto/md5" // PayFast's ITN signature format.
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/17xande-dev/goevent/internal/config"
	"github.com/17xande-dev/goevent/internal/middleware"
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/17xande-dev/payment"
	"github.com/17xande-dev/payment/payfast"
)

func TestPayFastCallback_UsesBareClientIPFromTrustedHeader(t *testing.T) {
	validate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "VALID")
	}))
	defer validate.Close()

	const merchantID, passphrase, gatewayRef = "10000100", "test-passphrase", "1089250"
	gateway, err := payfast.New(payfast.Config{
		MerchantID: merchantID, MerchantKey: "test-key", Passphrase: passphrase, Sandbox: true,
		ReturnURL: "https://events.example/checkout/success",
		CancelURL: "https://events.example/checkout/cancel",
		NotifyURL: "https://events.example/payments/payfast/callback",
		// Neither the connection peer nor an address with a port passes this check.
		AllowedCIDRs: []string{"197.97.145.150/32"}, ValidateURL: validate.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, header, value string
		source              middleware.ClientIPSource
		wantPaid            bool
	}{
		{"forwarded", "X-Forwarded-For", "197.97.145.150:4711, 10.0.0.1", middleware.ClientIPForwarded, true},
		{"cloudflare", "CF-Connecting-IP", "197.97.145.150:4711", middleware.ClientIPCloudflare, true},
		{"outside allowlist", "X-Forwarded-For", "198.51.100.7:4711", middleware.ClientIPForwarded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newAppWith(t, []payment.Gateway{gateway}, func(cfg *config.Config) {
				cfg.ClientIPSource = tc.source
			})
			e, adult, _ := published(t, s, 10)
			form := registerForm(t, s, e, map[string]int{adult.ID: 1})
			form.Set("method", gateway.Name())
			res, handover := submit(t, s, e, form)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("PayFast checkout = %d:\n%s", res.StatusCode, excerpt(handover))
			}
			id := between(t, handover, `name="m_payment_id" value="`, `"`)
			fields := "m_payment_id=" + id + "&pf_payment_id=" + gatewayRef +
				"&payment_status=COMPLETE&amount_gross=450.00&merchant_id=" + merchantID
			digest := md5.Sum([]byte(fields + "&passphrase=" + passphrase))
			body := fmt.Sprintf("%s&signature=%x", fields, digest)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
				s.srv.URL+"/payments/payfast/callback", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set(tc.header, tc.value)
			res, err = http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("ITN response = %d, want 200", res.StatusCode)
			}

			// Even a rejected ITN receives 200. A settled payment proves the
			// middleware's bare IP made it through to PayFast's allowlist check.
			p, err := s.regs.Payment(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantPaid {
				if p.Status != "paid" || p.GatewayRef != gatewayRef ||
					onlyRegistration(t, s).Status != registrations.StatusConfirmed {
					t.Errorf("accepted ITN did not settle the registration: payment = %+v", p)
				}
			} else if p.Status != "pending" {
				t.Errorf("ITN from outside the allowlist changed payment to %q", p.Status)
			}
		})
	}
}
