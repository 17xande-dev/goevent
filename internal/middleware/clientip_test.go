package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseClientIPSource(t *testing.T) {
	for _, in := range []string{"remote", "forwarded", "cloudflare", "  cloudflare  "} {
		if _, err := ParseClientIPSource(in); err != nil {
			t.Errorf("ParseClientIPSource(%q): %v", in, err)
		}
	}

	// A near-miss is refused rather than falling back to a default, because
	// every value here changes which client the rate limits and the payment
	// callback's source-IP check see.
	for _, in := range []string{"", "cloudlfare", "Cloudflare", "true", "xff"} {
		if _, err := ParseClientIPSource(in); err == nil {
			t.Errorf("ParseClientIPSource(%q) was accepted", in)
		}
	}
}

// ipRequest builds a request from remoteAddr carrying whatever headers a caller
// wants to claim, which is the whole point of these tests: the headers are
// attacker-controlled input unless something in front is replacing them.
func ipRequest(remoteAddr string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestClientIP(t *testing.T) {
	const (
		peer   = "203.0.113.7:54321"
		claim  = "198.51.100.1"
		real   = "192.0.2.50"
		peerIP = "203.0.113.7"
	)

	tests := []struct {
		name    string
		source  ClientIPSource
		remote  string
		headers map[string]string
		want    string
	}{
		{
			name:   "remote ignores every header",
			source: ClientIPRemote,
			remote: peer,
			headers: map[string]string{
				"X-Forwarded-For":  claim,
				"CF-Connecting-IP": claim,
			},
			want: peerIP,
		},
		{
			name:    "forwarded takes the leftmost entry",
			source:  ClientIPForwarded,
			remote:  peer,
			headers: map[string]string{"X-Forwarded-For": real + ", 10.0.0.1, 10.0.0.2"},
			want:    real,
		},
		{
			name:    "forwarded trims whitespace",
			source:  ClientIPForwarded,
			remote:  peer,
			headers: map[string]string{"X-Forwarded-For": "  " + real + "  , 10.0.0.1"},
			want:    real,
		},
		{
			name:    "forwarded falls back to the peer when the header is absent",
			source:  ClientIPForwarded,
			remote:  peer,
			headers: nil,
			want:    peerIP,
		},
		{
			name:    "forwarded falls back to the peer when the header is empty",
			source:  ClientIPForwarded,
			remote:  peer,
			headers: map[string]string{"X-Forwarded-For": "   "},
			want:    peerIP,
		},
		{
			name:    "forwarded ignores CF-Connecting-IP",
			source:  ClientIPForwarded,
			remote:  peer,
			headers: map[string]string{"CF-Connecting-IP": claim},
			want:    peerIP,
		},
		{
			name:    "cloudflare reads CF-Connecting-IP",
			source:  ClientIPCloudflare,
			remote:  peer,
			headers: map[string]string{"CF-Connecting-IP": real},
			want:    real,
		},
		{
			// The reason this mode exists. Cloudflare appends to X-Forwarded-For,
			// so its leftmost entry is whatever the client sent — falling back to
			// it would hand away exactly what CF-Connecting-IP is being read for.
			name:   "cloudflare never falls back to X-Forwarded-For",
			source: ClientIPCloudflare,
			remote: peer,
			headers: map[string]string{
				"X-Forwarded-For": claim + ", " + real,
			},
			want: peerIP,
		},
		{
			name:   "cloudflare prefers CF-Connecting-IP over a claimed X-Forwarded-For",
			source: ClientIPCloudflare,
			remote: peer,
			headers: map[string]string{
				"X-Forwarded-For":  claim,
				"CF-Connecting-IP": real,
			},
			want: real,
		},
		{
			name:    "cloudflare falls back to the peer when the header is empty",
			source:  ClientIPCloudflare,
			remote:  peer,
			headers: map[string]string{"CF-Connecting-IP": "  "},
			want:    peerIP,
		},
		{
			name:    "an IPv6 peer loses its brackets",
			source:  ClientIPRemote,
			remote:  "[2001:db8::1]:54321",
			headers: nil,
			want:    "2001:db8::1",
		},
		{
			name:    "an IPv6 forwarded entry loses its brackets",
			source:  ClientIPForwarded,
			remote:  peer,
			headers: map[string]string{"X-Forwarded-For": "[2001:db8::2]"},
			want:    "2001:db8::2",
		},
		{
			name:    "an IPv6 CF-Connecting-IP arrives bare",
			source:  ClientIPCloudflare,
			remote:  peer,
			headers: map[string]string{"CF-Connecting-IP": "2001:db8::3"},
			want:    "2001:db8::3",
		},
		{
			// httptest and some listeners hand over an address with no port.
			name:    "a peer with no port is still an address",
			source:  ClientIPRemote,
			remote:  "203.0.113.9",
			headers: nil,
			want:    "203.0.113.9",
		},
		{
			// An unset source is not a valid configuration — config refuses it at
			// boot — but if one ever reached here it must not read headers.
			name:    "an unknown source reads nothing but the peer",
			source:  ClientIPSource("nonsense"),
			remote:  peer,
			headers: map[string]string{"X-Forwarded-For": claim, "CF-Connecting-IP": claim},
			want:    peerIP,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClientIP(ipRequest(tt.remote, tt.headers), tt.source); got != tt.want {
				t.Errorf("ClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}
