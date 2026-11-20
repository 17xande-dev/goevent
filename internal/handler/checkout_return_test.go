package handler

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestCheckoutReturn_RejectsCookiesThatDoNotOwnThePayment(t *testing.T) {
	s := newApp(t)
	e, adult, _ := published(t, s, 10)
	checkout := func() *http.Cookie {
		t.Helper()
		form := registerForm(t, s, e, map[string]int{adult.ID: 1})
		res, body := submit(t, s, e, form)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("checkout = %d:\n%s", res.StatusCode, excerpt(body))
		}
		for _, c := range res.Cookies() {
			if c.Name == regCookie {
				return c
			}
		}
		t.Fatal("checkout did not set the registration cookie")
		return nil
	}

	checkout()
	id := s.gateway.Requests()[0].Reference
	reg := onlyRegistration(t, s)
	other := checkout()
	otherID, otherToken, ok := strings.Cut(other.Value, ".")
	if !ok || otherID == reg.ID || !s.signer.CheckManage(otherID, otherToken) {
		t.Fatal("second checkout did not issue a valid cookie for a different registration")
	}
	if res := callback(t, s, id, "COMPLETE", 45000); res.StatusCode != http.StatusOK {
		t.Fatalf("callback = %d", res.StatusCode)
	}

	for _, visitor := range []struct {
		name, cookie string
	}{
		{"valid cookie for another registration", other.Value},
		{"forged token for this registration", reg.ID + "." + otherToken},
		{"valid token with the wrong registration ID", otherID + "." + s.signer.ManageToken(reg.ID)},
	} {
		t.Run(visitor.name, func(t *testing.T) {
			for _, key := range []string{"ref", "payment"} {
				for _, page := range []string{"success", "cancel"} {
					t.Run(key+"/"+page, func(t *testing.T) {
						req := newRequest(t, s.srv, http.MethodGet, "/checkout/"+page+"?"+key+"="+id, nil)
						// Do not use the checkout browser's jar: send exactly the cookie under test.
						req.AddCookie(&http.Cookie{Name: regCookie, Value: visitor.cookie})
						res, err := http.DefaultClient.Do(req)
						if err != nil {
							t.Fatal(err)
						}
						defer res.Body.Close()
						body, err := io.ReadAll(res.Body)
						if err != nil {
							t.Fatal(err)
						}
						html := string(body)
						want := "Payment received"
						if page == "cancel" {
							want = "Nothing was taken for"
						}
						// The public reference shows we found the requested payment, not a generic
						// fallback page. Only the signed link and contact email are private.
						if res.StatusCode != http.StatusOK || !strings.Contains(html, want) ||
							!strings.Contains(html, reg.Reference) {
							t.Fatalf("return page = %d, missing the payment's public details:\n%s", res.StatusCode, excerpt(html))
						}
						if strings.Contains(html, "/r/") || strings.Contains(html, reg.ContactEmail) {
							t.Error("return page exposed the private manage link or contact email")
						}
					})
				}
			}
		})
	}
}
