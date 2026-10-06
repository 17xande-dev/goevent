package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func ok(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }

func TestSecurityHeaders(t *testing.T) {
	h := SecurityHeaders(Policy{})(http.HandlerFunc(ok))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/products", nil))

	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'self'",
		"script-src 'self'", // htmx is served from the binary, so no CDN needs allowing
		"object-src 'none'",
		"base-uri 'none'",
		"frame-ancestors 'none'",
		"form-action 'self'",
		// The theme is a bundled stylesheet and STATIC_DIR replaces it, so no served
		// template needs a style attribute and this directive is closed. It carried
		// 'unsafe-inline' until there was a stylesheet to put those rules in.
		"style-src 'self'",
		// Stated rather than inherited from default-src, so that widening it for a
		// hosted font service changes a directive that was already there.
		"font-src 'self'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP is missing %q: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("the CSP has regained an unsafe- directive: %s", csp)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("X-Content-Type-Options is not set")
	}
	if w.Header().Get("Referrer-Policy") == "" {
		t.Error("Referrer-Policy is not set")
	}
	// Nothing here uses a camera, a microphone or a location.
	pp := w.Header().Get("Permissions-Policy")
	for _, want := range []string{"geolocation=()", "camera=()", "microphone=()"} {
		if !strings.Contains(pp, want) {
			t.Errorf("Permissions-Policy is missing %q: %s", want, pp)
		}
	}
	// HSTS is off unless asked for: a browser ignores it over plain HTTP, and
	// sending it from localhost would pin a rule that breaks the next plain-HTTP
	// project on this port.
	if got := w.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("Strict-Transport-Security = %q on a non-HSTS policy", got)
	}
}

func TestSecurityHeaders_HSTSWhenAsked(t *testing.T) {
	h := SecurityHeaders(Policy{HSTS: true})(http.HandlerFunc(ok))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/products", nil))

	hsts := w.Header().Get("Strict-Transport-Security")
	if !strings.Contains(hsts, "max-age=") || !strings.Contains(hsts, "includeSubDomains") {
		t.Errorf("Strict-Transport-Security = %q", hsts)
	}
	// No preload: getting onto that list is a decision with a slow exit, and it is
	// the operator's to make rather than this project's to make for them.
	if strings.Contains(hsts, "preload") {
		t.Errorf("HSTS asks for preload, which this project should not decide: %q", hsts)
	}
}

func TestSecurityHeaders_ImagesComeFromHereOrTheBucketOnly(t *testing.T) {
	// A product image is always bytes this store holds: an object in the bucket, or a
	// file served from this origin. Pasting a URL from the general internet used to be
	// allowed and no longer is, which is what lets this directive be closed.
	h := SecurityHeaders(Policy{
		ImgSources: []string{"https://images.example"},
	})(http.HandlerFunc(ok))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/products", nil))

	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "img-src 'self' https://images.example;") {
		t.Errorf("img-src is not exactly 'self' plus the bucket: %s", csp)
	}
	// No blanket and no data: URIs. An image from anywhere else is refused by the
	// browser as well as by the admin.
	for _, forbidden := range []string{"https:;", "https: ", "data:", "*"} {
		if strings.Contains(csp, "img-src 'self' https://images.example "+forbidden) {
			t.Errorf("img-src still allows %q: %s", forbidden, csp)
		}
	}

	// And with no bucket — a disk-backed store — it is 'self' and nothing else.
	h = SecurityHeaders(Policy{})(http.HandlerFunc(ok))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/products", nil))
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self';") {
		t.Errorf("img-src with no bucket = %s, want exactly 'self'", csp)
	}
}

func TestSecurityHeaders_FontSourcesOpenStyleAndFontOnly(t *testing.T) {
	// A hosted font service is two fetches: the stylesheet declaring the fonts, and
	// the font files it names. Both directives have to name the origins or the
	// widening silently achieves nothing.
	h := SecurityHeaders(Policy{
		FontSources: []string{"https://use.typekit.net", "https://p.typekit.net"},
	})(http.HandlerFunc(ok))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/products", nil))

	csp := w.Header().Get("Content-Security-Policy")
	for _, want := range []string{
		"font-src 'self' https://use.typekit.net https://p.typekit.net;",
		"style-src 'self' https://use.typekit.net https://p.typekit.net;",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP is missing %q: %s", want, csp)
		}
	}

	// The property that makes this a narrow widening: a font service still cannot
	// run JavaScript on any page of this store, the checkout included. If this ever
	// fails, the font knob has grown into a general one.
	if !strings.Contains(csp, "script-src 'self';") {
		t.Errorf("font sources reached script-src: %s", csp)
	}
	// Nor may they be framed, posted to, or serve an image.
	for _, closed := range []string{"img-src 'self';", "form-action 'self';", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, closed) {
			t.Errorf("font sources widened more than style-src and font-src, want %q: %s", closed, csp)
		}
	}
	// And still no concession that cannot be scoped to an origin.
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("the CSP has regained an unsafe- directive: %s", csp)
	}
}

func TestSecurityHeaders_FrameAncestorsFollowEmbedOrigins(t *testing.T) {
	// Embedding the catalog in someone else's page is the point of the feature,
	// so the origins allowed to fetch it must also be allowed to frame it.
	h := SecurityHeaders(Policy{
		FrameAncestors: []string{"https://cms.example", "https://other.example"},
	})(http.HandlerFunc(ok))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/products", nil))

	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors https://cms.example https://other.example") {
		t.Errorf("frame-ancestors does not list the embed origins: %s", csp)
	}
}

func TestSecurityHeaders_FormActionAllowsTheGateway(t *testing.T) {
	// The checkout hands the shopper to the gateway with a real cross-origin form
	// post. form-action 'self' alone makes the browser block it — silently, from
	// the shopper's point of view — so the gateway's origin has to be named.
	h := SecurityHeaders(Policy{
		FormActions: []string{"https://sandbox.payfast.co.za"},
	})(http.HandlerFunc(ok))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/cart/checkout", nil))

	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "form-action 'self' https://sandbox.payfast.co.za") {
		t.Errorf("form-action does not allow the gateway: %s", csp)
	}
}

func TestCORS(t *testing.T) {
	allowed := "https://cms.example"
	h := CORS([]string{allowed})(http.HandlerFunc(ok))

	cases := []struct {
		name, origin, wantAllow string
		wantVary                bool
	}{
		{name: "no origin at all", origin: "", wantAllow: ""},
		{name: "allowed origin", origin: allowed, wantAllow: allowed, wantVary: true},
		{name: "unlisted origin", origin: "https://evil.example", wantAllow: "", wantVary: true},
		// A prefix of an allowed origin is a different origin.
		{name: "lookalike origin", origin: "https://cms.example.evil.test", wantAllow: "", wantVary: true},
		{name: "scheme downgrade", origin: "http://cms.example", wantAllow: "", wantVary: true},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/products", nil)
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		h.ServeHTTP(w, req)

		if got := w.Header().Get("Access-Control-Allow-Origin"); got != tc.wantAllow {
			t.Errorf("%s: Access-Control-Allow-Origin = %q, want %q", tc.name, got, tc.wantAllow)
		}
		if got := w.Header().Get("Vary") != ""; got != tc.wantVary {
			t.Errorf("%s: Vary present = %v, want %v", tc.name, got, tc.wantVary)
		}
		if w.Body.String() != "ok" {
			t.Errorf("%s: the handler did not run", tc.name)
		}
	}
}

func TestCORS_NeverAllowsCredentials(t *testing.T) {
	// The fragments are cookie-free by design. If this header ever appears, a
	// permissive origin list stops being safe.
	for _, origins := range [][]string{{"https://cms.example"}, {"*"}} {
		h := CORS(origins)(http.HandlerFunc(ok))

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/products", nil)
		req.Header.Set("Origin", "https://cms.example")
		h.ServeHTTP(w, req)

		if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
			t.Errorf("origins %v: Access-Control-Allow-Credentials = %q, want it absent", origins, got)
		}
	}
}

func TestCORS_Wildcard(t *testing.T) {
	h := CORS([]string{"*"})(http.HandlerFunc(ok))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/products", nil)
	req.Header.Set("Origin", "https://anyone.example")
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

func TestCORS_UnconfiguredSendsNothing(t *testing.T) {
	h := CORS(nil)(http.HandlerFunc(ok))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/products", nil)
	req.Header.Set("Origin", "https://cms.example")
	h.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q with no origins configured", got)
	}
}

func TestCORS_Preflight(t *testing.T) {
	reached := false
	h := CORS([]string{"https://cms.example"})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/products", nil)
	req.Header.Set("Origin", "https://cms.example")
	req.Header.Set("Access-Control-Request-Headers", "hx-request")
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "GET") {
		t.Errorf("Access-Control-Allow-Methods = %q", got)
	}
	// htmx sends HX-Request, which makes the fetch non-simple; without this the
	// browser refuses before the request is ever made.
	if got := w.Header().Get("Access-Control-Allow-Headers"); !strings.Contains(got, "HX-Request") {
		t.Errorf("Access-Control-Allow-Headers = %q, want HX-Request", got)
	}
	if reached {
		t.Error("the preflight reached the wrapped handler")
	}
}
