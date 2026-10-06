package events

import (
	"testing"
)

func TestParsePrice(t *testing.T) {
	valid := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"149", 14900},
		{"149.9", 14990},
		{"149.99", 14999},
		{"1,149.99", 114999},
		{" 149.99 ", 14999},
		{"0.05", 5},
	}
	for _, tc := range valid {
		got, err := ParsePrice(tc.in)
		if err != nil {
			t.Errorf("ParsePrice(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParsePrice(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}

	// Three decimals is the interesting one: silently rounding a price the
	// operator typed is worse than refusing it.
	invalid := []string{"", "  ", "abc", "-5", "1.999", "1.", ".99", "R149.99", "1 2 . 3 4x"}
	for _, in := range invalid {
		if got, err := ParsePrice(in); err == nil {
			t.Errorf("ParsePrice(%q) = %d, want an error", in, got)
		}
	}
}

func TestFormatPrice(t *testing.T) {
	cases := map[int64]string{
		0:      "0.00",
		5:      "0.05",
		50:     "0.50",
		14900:  "149.00",
		114999: "1149.99",
	}
	for cents, want := range cases {
		if got := FormatPrice(cents); got != want {
			t.Errorf("FormatPrice(%d) = %q, want %q", cents, got, want)
		}
	}
}

func TestPriceRoundTrips(t *testing.T) {
	for _, cents := range []int64{0, 1, 99, 100, 14999, 999_999_99} {
		got, err := ParsePrice(FormatPrice(cents))
		if err != nil {
			t.Fatalf("ParsePrice(FormatPrice(%d)): %v", cents, err)
		}
		if got != cents {
			t.Errorf("round trip of %d gave %d", cents, got)
		}
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"The Quiet Machine":     "the-quiet-machine",
		"  Spaced  Out  ":       "spaced-out",
		"Ampersands & Symbols!": "ampersands-symbols",
		"Already-a-slug":        "already-a-slug",
		"Numbers 123":           "numbers-123",
		"!!!":                   "",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
