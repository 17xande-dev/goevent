package middleware

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// ClientIPSource names where the address a request came from is read from.
//
// This is a statement of fact about the deployment rather than a preference: the
// right value is decided by whatever sits in front of the server, and a wrong one
// fails quietly in both directions — too trusting and every client can invent an
// address and a rate-limit bucket to itself, not trusting enough and the whole
// internet shares one bucket while the payment callback's source-IP check rejects
// every genuine notification.
type ClientIPSource string

const (
	// ClientIPRemote reads r.RemoteAddr, which the server observed itself and
	// which therefore cannot be lied about. Correct when nothing is in front.
	ClientIPRemote ClientIPSource = "remote"

	// ClientIPForwarded reads the leftmost X-Forwarded-For entry.
	//
	// **The proxy must be configured to replace X-Forwarded-For, not append to
	// it.** The leftmost entry is the original client for every proxy that
	// overwrites the header — and is whatever the client typed for one that
	// appends (nginx's $proxy_add_x_forwarded_for does append; Caddy replaces,
	// unless trusted_proxies is configured). The alternative, reading the
	// rightmost entry, is safe against that but wrong on every platform that puts
	// a load balancer of its own in the chain, which is most managed ones.
	ClientIPForwarded ClientIPSource = "forwarded"

	// ClientIPCloudflare reads CF-Connecting-IP, which Cloudflare sets to the
	// address it accepted the connection from. Unlike X-Forwarded-For it holds
	// exactly one address rather than a chain, so there is no entry to choose.
	//
	// Correct only where Cloudflare is the *sole* way in — a tunnel, or an origin
	// firewalled to Cloudflare's ranges. Any route that reaches this server
	// without passing through Cloudflare can set the header to anything it likes,
	// and this mode will believe it.
	//
	// X-Forwarded-For is deliberately not the fallback: Cloudflare appends to that
	// header rather than replacing it, so its leftmost entry is client-supplied.
	// Falling back to it would give away exactly what this mode exists to protect.
	ClientIPCloudflare ClientIPSource = "cloudflare"
)

// ParseClientIPSource validates a CLIENT_IP_SOURCE setting. It lives here rather
// than in config so that the values and their meanings stay next to the code that
// acts on them.
func ParseClientIPSource(s string) (ClientIPSource, error) {
	switch src := ClientIPSource(strings.TrimSpace(s)); src {
	case ClientIPRemote, ClientIPForwarded, ClientIPCloudflare:
		return src, nil
	default:
		return "", fmt.Errorf("must be one of %s, %s or %s; got %q",
			ClientIPRemote, ClientIPForwarded, ClientIPCloudflare, s)
	}
}

// ClientIP returns the address a request came from, read as source says to.
//
// Every mode falls back to r.RemoteAddr when its header is missing or empty: a
// header that is not there means the request did not arrive the way the
// deployment claims it does, and RemoteAddr is the one value on a request that
// cannot be forged.
//
// Where this matters most is the payment callback's source-IP check, and there it
// is worth knowing that the check is defence in depth: a forged notification also
// has to carry a valid signature and be confirmed by the gateway's own servers,
// and spoofing an IP does neither.
func ClientIP(r *http.Request, source ClientIPSource) string {
	switch source {
	case ClientIPForwarded:
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			first, _, _ := strings.Cut(forwarded, ",")
			if ip := strings.TrimSpace(first); ip != "" {
				return unbracket(ip)
			}
		}
	case ClientIPCloudflare:
		if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
			return unbracket(ip)
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// No port, which happens in tests and with some listeners.
		return unbracket(strings.TrimSpace(r.RemoteAddr))
	}
	return host
}

// unbracket strips the brackets an IPv6 address may arrive wrapped in, since
// netip.ParseAddr wants the bare form.
func unbracket(ip string) string {
	return strings.TrimSuffix(strings.TrimPrefix(ip, "["), "]")
}
