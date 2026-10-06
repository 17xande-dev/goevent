package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRequestID(t *testing.T) {
	cases := map[string]struct {
		headers map[string]string
		want    string // "" means "generated, so just check the shape"
	}{
		"generates one when there is nothing to adopt": {
			headers: nil,
		},
		"adopts Cloud Trace, without the span": {
			headers: map[string]string{"X-Cloud-Trace-Context": "105445aa7843bc8bf206b12000100000/1;o=1"},
			want:    "105445aa7843bc8bf206b12000100000",
		},
		"adopts Cloud Trace with no span at all": {
			headers: map[string]string{"X-Cloud-Trace-Context": "105445aa7843bc8bf206b12000100000"},
			want:    "105445aa7843bc8bf206b12000100000",
		},
		"adopts X-Request-Id": {
			headers: map[string]string{"X-Request-Id": "from-the-proxy"},
			want:    "from-the-proxy",
		},
		"prefers Cloud Trace, because that is what the platform files logs under": {
			headers: map[string]string{
				"X-Cloud-Trace-Context": "abc123/1;o=1",
				"X-Request-Id":          "ignored",
			},
			want: "abc123",
		},
		// A client controls this header, and it ends up in a log line and on a page.
		// Anything that would let it forge a line break or inject markup is dropped
		// rather than escaped, because there is no reason for an id to contain it.
		"strips anything that is not an id": {
			headers: map[string]string{"X-Request-Id": "line\nbreak <script> and spaces"},
			want:    "linebreakscriptandspaces",
		},
		"an entirely unusable id is replaced rather than left empty": {
			headers: map[string]string{"X-Request-Id": "!!!"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var seen string
			h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = RequestIDFrom(r.Context())
			}))

			// httptest.NewRequest rather than a real client, because Go's client
			// refuses to send a header containing a newline — which is exactly the
			// case worth checking.
			req := httptest.NewRequest("GET", "/", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			switch {
			case tc.want != "":
				if seen != tc.want {
					t.Errorf("id = %q, want %q", seen, tc.want)
				}
			default:
				if len(seen) != 16 {
					t.Errorf("generated id = %q, want 16 hex characters", seen)
				}
			}
			// The response always names the request, whether the id was adopted or
			// minted: it is what a customer can quote back.
			if got := rec.Header().Get(Header); got != seen {
				t.Errorf("echoed %q but the handler saw %q", got, seen)
			}
		})
	}
}

func TestRequestID_UniquePerRequest(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	seen := make(map[string]bool, 100)
	for range 100 {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		id := rec.Header().Get(Header)
		if seen[id] {
			t.Fatalf("id %q was issued twice", id)
		}
		seen[id] = true
	}
}

func TestRequestIDFrom_WithoutTheMiddleware(t *testing.T) {
	// A handler called directly, or a test server without the middleware, gets an
	// empty string rather than a panic — the reference is then simply absent from
	// the page.
	if got := RequestIDFrom(httptest.NewRequest("GET", "/", nil).Context()); got != "" {
		t.Errorf("id = %q, want empty", got)
	}
}
