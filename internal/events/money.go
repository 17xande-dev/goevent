package events

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Money is integer cents everywhere, never float: a float total rounded
// differently from a gateway's amount string is a real and hard-to-find class
// of payment bug.

// ErrPrice is returned by ParsePrice for anything that is not a plain amount.
var ErrPrice = errors.New("events: price must be an amount like 149.99")

// FormatPrice renders cents as a plain decimal amount, with no currency symbol
// — the symbol or code is presentation, and comes from config.
func FormatPrice(cents int64) string {
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

// ParsePrice converts a human-typed amount into cents. It accepts "149",
// "149.9", "149.99" and "1,149.99", and rejects anything else — including more
// than two decimal places, which is more likely a typo than an intent to round.
func ParsePrice(s string) (int64, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", "")
	s = strings.ReplaceAll(s, " ", "")
	if s == "" {
		return 0, ErrPrice
	}
	if strings.HasPrefix(s, "-") {
		return 0, ErrPrice
	}

	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" || !isDigits(whole) {
		return 0, ErrPrice
	}
	if hasFrac {
		if frac == "" || len(frac) > 2 || !isDigits(frac) {
			return 0, ErrPrice
		}
		if len(frac) == 1 {
			frac += "0"
		}
	} else {
		frac = "00"
	}

	units, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || units > (1<<62)/100 {
		return 0, ErrPrice
	}
	cents, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, ErrPrice
	}
	return units*100 + cents, nil
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
