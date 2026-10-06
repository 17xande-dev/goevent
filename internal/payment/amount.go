package payment

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// A gateway talks about money in decimal strings; this store talks about it in
// integer cents. These two functions are the only place the conversion happens,
// and they are here rather than in a gateway package because every gateway needs
// exactly the same thing — and because the format a gateway is sent must not
// change if the format a page displays ever does.

// ErrAmount is returned by ParseAmount for anything that is not a plain amount.
var ErrAmount = errors.New("payment: not a plain decimal amount")

// FormatAmount renders cents as a gateway amount string: two decimal places, no
// thousands separator, no currency symbol.
func FormatAmount(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

// ParseAmount parses a gateway amount string into cents. It is deliberately
// strict — digits, and at most two decimal places — because this figure is
// compared against an order total to decide whether the right amount was paid,
// and a lenient parser there turns a mismatch into a silent acceptance.
func ParseAmount(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrAmount
	}

	whole, frac, hasFrac := strings.Cut(s, ".")
	if !hasFrac {
		frac = "00"
	}
	switch len(frac) {
	case 1:
		frac += "0"
	case 2:
	default:
		return 0, ErrAmount
	}
	if !isDigits(whole) || !isDigits(frac) {
		return 0, ErrAmount
	}

	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || units > (1<<62)/100 {
		return 0, ErrAmount
	}
	cents, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, ErrAmount
	}
	return units*100 + cents, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
