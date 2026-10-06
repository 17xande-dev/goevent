package payment

import "net/url"

// ReturnURL binds a browser return to its own payment rather than whichever
// one another tab started most recently. It carries no authentication token.
func ReturnURL(base, paymentID string) string {
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	q.Set("payment", paymentID)
	u.RawQuery = q.Encode()
	return u.String()
}
