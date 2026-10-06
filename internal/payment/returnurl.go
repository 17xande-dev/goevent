package payment

import "net/url"

// OrderReturnURL binds a browser return to its checkout rather than whichever
// order another tab placed most recently. It carries no authentication token.
func OrderReturnURL(base, orderID string) string {
	if base == "" {
		return ""
	}
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	q := u.Query()
	q.Set("order", orderID)
	u.RawQuery = q.Encode()
	return u.String()
}
