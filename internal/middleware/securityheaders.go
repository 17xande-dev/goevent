package middleware

import (
	"net/http"
	"slices"
	"strings"
)

// Policy is the parts of the Content-Security-Policy that depend on how this
// particular store is deployed. Everything else in the policy is fixed, because
// the templates earn it.
type Policy struct {
	// FrameAncestors are the origins allowed to frame the store — where
	// embedding is permitted to happen. Empty means 'none'.
	FrameAncestors []string

	// FormActions are the external origins a form may post to, beyond the store
	// itself. In practice this is the payment gateway: the checkout hands the
	// shopper to it with a real form submission, and form-action 'self' alone
	// makes the browser block that — silently enough to cost an afternoon.
	FormActions []string

	// ImgSources are the origins images may be loaded from besides this one — the
	// object storage bucket, when one is configured. Empty means 'self' only, which
	// is what a store serving its images from a local directory needs.
	ImgSources []string

	// FontSources are the origins a web font may come from besides this one — a
	// hosted font service, when one is configured. Empty means 'self' only, which
	// is what the default theme's system font stack needs.
	//
	// These land in style-src as well as font-src, and the reason is not obvious:
	// a hosted font service is two fetches, not one. The store links a stylesheet
	// from the service, that stylesheet's @font-face rules name the font files, and
	// the browser fetches those. Allowing only font-src blocks the stylesheet, so
	// nothing ever asks for a font and the directive that was widened is never
	// reached — a failure with no error beyond a console warning.
	//
	// It is one field rather than two because a service's font host being permitted
	// to serve a stylesheet is a smaller over-grant than a second knob that has to
	// be kept in step with this one. What stays closed is the part that matters:
	// script-src is untouched, so a font origin cannot run JavaScript on any page
	// of this store.
	FontSources []string

	// HSTS adds Strict-Transport-Security. Only ever true on an https deployment:
	// sending it over plain HTTP is ignored by browsers, and sending it from a
	// development server on localhost would pin a rule that makes the next plain
	// HTTP project on that port unreachable.
	HSTS bool
}

// SecurityHeaders sets the response headers every page wants.
//
// The Content-Security-Policy is strict because the templates earn it: no inline
// script, and htmx served from the binary rather than a CDN. The only external
// origins are the ones a deployment names — the bucket, the gateway, a font
// service — each in the single directive it needs. Adopters who add an analytics
// tag will need to widen it further, which is the correct direction of travel:
// start closed and open deliberately, one directive at a time.
func SecurityHeaders(p Policy) Middleware {
	frame := "'none'"
	if len(p.FrameAncestors) > 0 {
		frame = strings.Join(p.FrameAncestors, " ")
	}
	formAction := selfPlus(p.FormActions)

	// img-src is 'self' plus the bucket and nothing else. A product image is always
	// bytes this store holds — an object in the bucket, or a file served from this
	// origin — because pasting a URL from the general internet means the picture on a
	// product page belongs to somebody who can change or delete it. That used to be
	// allowed and no longer is, which is what lets this directive be closed.
	//
	// Every directive is now closed to this origin plus, for images, the bucket.
	//
	// style-src is 'self' plus whatever FontSources names, and nothing else. It used
	// to carry 'unsafe-inline', because restyling through TEMPLATE_DIR
	// had no other legal way to apply CSS — there was no stylesheet and no mechanism
	// for an adopter to add one. Both exist now: the theme is a bundled styles.css and
	// STATIC_DIR replaces it. No served template contains a style attribute, so the
	// concession is gone. The remaining inline styles are in email bodies, which no
	// CSP has ever applied to.
	//
	// # If something inline is ever genuinely needed, add a nonce — not 'unsafe-inline'
	//
	// A decision made once, so it is not remade under pressure by whoever hits the
	// first library that injects a <style> or a <script>: the answer is a per-response
	// nonce.
	//
	// 'unsafe-inline' cannot be scoped to the code that asked for it. It is one switch
	// for the whole origin, and the browser cannot tell an intended inline block from
	// an injected one — which is the entire threat it exists to stop. On script-src it
	// would turn any future escaping slip into a live compromise of the admin session:
	// customer-typed text reaches an authenticated page (the address in
	// admin_order.gohtml), html/template is what keeps it inert, and this directive is
	// the backstop for the day something returns template.HTML without escaping first.
	// A nonce keeps that backstop while still letting the store's own inline content
	// run: fresh random value per response, in the header and on the tag, unguessable
	// by an injection.
	//
	// What that would cost here, so the estimate is not rediscovered:
	//
	//   - The value must be minted per request and reach every render's data, which
	//     means every overridable "head" template has to carry it. A theme that forgets
	//     it fails the same silent way an inline block does today.
	//   - htmx takes it as configuration: inlineStyleNonce for the indicator block it
	//     injects, inlineScriptNonce for scripts arriving in swapped fragments — both
	//     set through the htmx-config meta tag, interpolated per response.
	//   - Prefer inlineStyleNonce alone. inlineScriptNonce tells htmx to stamp the
	//     valid nonce onto *every* script in *any* fragment, which downgrades the
	//     guarantee from "the server authorised this block" to "the server authorised
	//     whatever it emitted" — the assumption an escaping bug breaks, and close to
	//     'unsafe-inline' with extra steps.
	//   - Nothing may cache an HTML response, or it serves a stale nonce. Nothing does
	//     today; a page cache added later would have to reckon with this.
	//
	// Two things that look like they need it and do not: styling set through the CSSOM
	// (element.style.x, sheet.insertRule) is not covered by CSP at all, and hx-on /
	// js: filters need 'unsafe-eval' rather than 'unsafe-inline' — which is why the
	// store's htmx-driven code lives in .js files under STATIC_DIR instead.
	imgSrc := selfPlus(p.ImgSources)

	// font-src is stated rather than left to default-src, even when it is only
	// 'self'. Fonts inherited that fallback silently, so widening for a hosted
	// service would have meant a directive appearing where there had been none —
	// and a policy where the interesting decisions are invisible is one nobody
	// audits. See Policy.FontSources for why the same origins reach style-src.
	fontSrc := selfPlus(p.FontSources)
	styleSrc := selfPlus(p.FontSources)

	csp := strings.Join([]string{
		"default-src 'self'",
		"img-src " + imgSrc,
		"font-src " + fontSrc,
		"style-src " + styleSrc,
		"script-src 'self'",
		"form-action " + formAction,
		"base-uri 'none'",
		"object-src 'none'",
		"frame-ancestors " + frame,
	}, "; ")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy", csp)
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			// Nothing here uses a camera, a microphone or a location, and a store
			// that takes card details should not be able to start doing so through
			// an injected iframe.
			h.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=(), payment=()")
			if p.HSTS {
				// Two years, subdomains included. No preload directive: getting onto
				// the preload list is a decision with a slow exit, and it is the
				// operator's to make rather than this project's to make for them.
				h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// selfPlus builds a CSP source list: this origin, then whatever else is allowed.
//
// 'self' is never dropped. Every directive built this way covers something the
// store also serves itself — its own stylesheet, its own images — so an operator
// naming an external origin is adding to the list, never replacing it.
func selfPlus(extra []string) string {
	if len(extra) == 0 {
		return "'self'"
	}
	return "'self' " + strings.Join(extra, " ")
}

// CORS allows the listed origins to fetch a handler cross-origin.
//
// It belongs only on the read-only, cookie-free catalog routes. Nothing it
// guards may depend on a cookie or change state: no credentials are allowed, so
// a permissive origin list here cannot become a way to act as somebody.
func CORS(allowedOrigins []string) Middleware {
	allowAll := slices.Contains(allowedOrigins, "*")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			switch {
			case len(allowedOrigins) == 0 || origin == "":
				// No embedding configured, or a same-origin request: send no
				// CORS headers at all rather than an empty allowance.
			case allowAll:
				w.Header().Set("Access-Control-Allow-Origin", "*")
			case slices.Contains(allowedOrigins, origin):
				w.Header().Set("Access-Control-Allow-Origin", origin)
				// The response varies by Origin, so a shared cache must not
				// serve one embedder's copy to another.
				w.Header().Add("Vary", "Origin")
			default:
				w.Header().Add("Vary", "Origin")
			}

			// Preflights: only the htmx request header needs allowing, and only
			// GET is ever offered.
			if r.Method == http.MethodOptions && origin != "" {
				h := w.Header()
				h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "HX-Request, HX-Current-URL, HX-Target, HX-Trigger")
				h.Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
