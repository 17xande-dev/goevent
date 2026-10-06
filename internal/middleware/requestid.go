package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
)

// Request IDs exist so that a person and a log file can talk about the same
// request.
//
// Without one, an error page can only say "something went wrong" and a log line
// can only say "something went wrong at 14:32" — and with two shoppers on the site
// there is no way to tell which line belongs to which of them. The id is put in
// every log line for the request, echoed in a response header, and printed on the
// error page as a reference, so "I got an error, it said 7f3a9c2e" is enough to
// find the failure.
//
// It is not a trace and it is not metrics. It is the smallest thing that makes
// stdout logging usable without a log service in front of it.

type ctxKey int

const requestIDKey ctxKey = iota

// Header is where the id is echoed, and one of the places it is read from.
const Header = "X-Request-Id"

// cloudTraceHeader is set by Google Cloud Run on every request, formatted
// "TRACE_ID/SPAN_ID;o=1". Adopting its trace id rather than minting our own means
// the store's logs and the platform's own request logs name the same request, which
// is the difference between one search and two.
const cloudTraceHeader = "X-Cloud-Trace-Context"

// RequestID gives every request an id, adopting one from the platform or the
// client when there is one and generating it otherwise.
//
// It must be the outermost middleware: anything wrapped outside it — including
// the rate limiter, which logs — would run before the id exists and log without
// one.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := incoming(r)
		if id == "" {
			id = generate()
		}

		// Echoed so that a customer's devtools screenshot, or a curl -i, carries the
		// reference without anyone having to reach the error page for it.
		w.Header().Set(Header, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey, id)))
	})
}

// incoming reads an id the request already carries. Cloud Trace first, because on
// a platform that sets it that is the id everything else will be filed under.
//
// A client-supplied X-Request-Id is trusted only as far as it is used: it names a
// log line and nothing more, grants nothing, and is truncated and stripped of
// anything that would make a log line hard to read. A forged one can confuse a
// search and that is all.
func incoming(r *http.Request) string {
	if trace := r.Header.Get(cloudTraceHeader); trace != "" {
		// "TRACE_ID/SPAN_ID;o=1" — the part before the slash is the trace.
		if id, _, ok := strings.Cut(trace, "/"); ok {
			return clean(id)
		}
		return clean(trace)
	}
	return clean(r.Header.Get(Header))
}

// clean keeps the id to characters that are safe to put in a header, a log line
// and an HTML page, and short enough to read aloud.
func clean(s string) string {
	const maxLen = 64
	var b strings.Builder
	for _, r := range s {
		if len(b.String()) >= maxLen {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	return b.String()
}

// generate mints an id. Eight bytes is short enough to read over the phone and
// far more than enough to tell one request from another in a day's logs.
//
// crypto/rand rather than math/rand, not for secrecy — the id protects nothing —
// but because it cannot fail in a way that returns duplicates, and there is no
// seeding to get wrong.
func generate() string {
	var b [8]byte
	rand.Read(b[:]) // documented never to fail since Go 1.24
	return hex.EncodeToString(b[:])
}

// RequestIDFrom returns the id carried by a request's context, or "" when the
// middleware did not run — a test server or a direct handler call, where an empty
// reference is better than a panic.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}
