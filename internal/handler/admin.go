package handler

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/17xande-dev/goevent/internal/auth"
	"github.com/17xande-dev/goevent/internal/blob"
	"github.com/17xande-dev/goevent/internal/config"
	"github.com/17xande-dev/goevent/internal/events"
	"github.com/17xande-dev/goevent/internal/middleware"
	"github.com/17xande-dev/goevent/internal/outbox"
	"github.com/17xande-dev/goevent/internal/payment"
	"github.com/17xande-dev/goevent/internal/registrations"
	"github.com/17xande-dev/mailer"
	"github.com/justinas/nosurf"
)

// Handler holds everything the HTTP layer needs. It is created once at startup
// and is safe for concurrent use.
type Handler struct {
	cfg  config.Config
	log  *slog.Logger
	tmpl *Templates
	// events is what people register for: events, ticket types and questions.
	events *events.Store
	// regs is what people did: registrations, attendees and payments.
	regs *registrations.Store
	// signer makes and checks manage links and ticket codes.
	signer registrations.Signer
	// gateways is every payment provider this deployment has configured, in the
	// order the checkout offers them. A registry rather than one gateway because
	// a deployment may offer more than one, and because the callback route has to
	// resolve a name to the provider that can prove a notification genuine.
	gateways payment.Registry
	mail     mailer.Sender
	outbox   *outbox.Store
	// blob is the public image store event pictures live in.
	blob blob.Storage

	// users is the administrator accounts and their sessions. The handler needs it
	// for the login, sign-out and setup-claim routes; RequireAdmin holds the same
	// store for the lookup it does on every protected request.
	users *auth.Store

	// adminRoutes is every protected route and the permission it names, recorded
	// by RegisterAdmin as it wires them. Written once at startup, read-only
	// afterwards, so it needs no lock.
	adminRoutes []AdminRoute

	// limits are built here from cfg rather than passed in, so that a rate limit is
	// applied on the line that registers the route it protects — the same reasoning
	// as RequireAdmin. A limiter wrapped around a prefix by the caller is one
	// refactor away from silently not covering a new route.
	limits limiters
}

// limiters are the per-surface rate limits. A zero limit means the surface is
// unlimited, which is what the test configuration uses and what an operator gets
// by setting RATE_LIMIT_*_PER_MINUTE=0.
type limiters struct {
	login    middleware.Middleware
	checkout middleware.Middleware
	callback middleware.Middleware
	status   middleware.Middleware
}

// perMinute builds a limiter allowing n requests a minute, or a pass-through when
// n is zero. Burst is a third of the allowance, minimum two, so a person who
// double-clicks is never the one who trips it.
func perMinute(name string, n int, source middleware.ClientIPSource, log *slog.Logger, exceeded http.Handler) middleware.Middleware {
	if n <= 0 {
		log.Warn("rate limiting is disabled for a surface that has one available", "limiter", name)
		return func(next http.Handler) http.Handler { return next }
	}
	return middleware.RateLimit(middleware.RateLimitConfig{
		Name:     name,
		Every:    time.Minute / time.Duration(n),
		Burst:    max(2, n/3),
		Exceeded: exceeded,
	}, source, log)
}

// rateLimited is the page a throttled person sees. The payment callback does not
// use it — a gateway wants a status and a Retry-After, not HTML — which is why
// this is passed per limiter rather than built into the middleware.
func (h *Handler) rateLimited(w http.ResponseWriter, r *http.Request) {
	h.clientError(w, r, http.StatusTooManyRequests, "Too many requests",
		"That came through faster than we allow. Wait a moment and try again — the "+
			"Retry-After header on this response says how long.")
}

// Deps is everything a Handler needs, by name, so that two values of the same
// type can never be swapped by a careless positional call.
type Deps struct {
	Config        config.Config
	Log           *slog.Logger
	Tmpl          *Templates
	Events        *events.Store
	Registrations *registrations.Store
	Signer        registrations.Signer
	Gateways      payment.Registry
	Mail          mailer.Sender
	Outbox        *outbox.Store
	Images        blob.Storage
	Users         *auth.Store
}

func New(d Deps) *Handler {
	cfg, log := d.Config, d.Log
	h := &Handler{
		cfg: cfg, log: log, tmpl: d.Tmpl, events: d.Events, regs: d.Registrations, signer: d.Signer,
		gateways: d.Gateways, mail: d.Mail,
		blob: d.Images, users: d.Users, outbox: d.Outbox,
	}
	// Storage is optional and must be non-nil, so that a caller omitting it gets
	// a refusal with a message rather than a nil panic on the first upload.
	if h.blob == nil {
		h.blob = blob.Unconfigured{}
	}
	page := http.HandlerFunc(h.rateLimited)
	h.limits = limiters{
		login:    perMinute("admin login", cfg.RateLimits.LoginPerMinute, cfg.ClientIPSource, log, page),
		checkout: perMinute("checkout", cfg.RateLimits.CheckoutPerMinute, cfg.ClientIPSource, log, page),
		// No page for the gateway: it is a machine, and the plain status with
		// Retry-After is exactly what it acts on.
		callback: perMinute("payment callback", cfg.RateLimits.CallbackPerMinute, cfg.ClientIPSource, log, nil),
		// The payment-status poll. Cheap per request, but a page left open asks
		// for it all afternoon, so it gets a tier of its own rather than eating
		// the checkout's allowance and locking a registrant out of retrying.
		status: perMinute("payment status", cfg.RateLimits.StatusPerMinute, cfg.ClientIPSource, log, page),
	}
	return h
}

// FirstPartyHandler returns everything that changes state — the admin and the
// registration forms — behind CSRF protection. The admin routes additionally
// require a session; registering is anonymous.
//
// CSRF is scoped to these routes rather than wrapped around the server's whole
// mux, because nosurf sets a token cookie on every response it handles and the
// event pages that only read should stay cookie-free and cacheable. Scoping by
// group is also what makes the payment callback CSRF-exempt: it is not in this
// group at all, rather than being excused by an exempt-path string that has to
// keep matching the route.
//
// The caller mounts this at /admin/ and at RegisterPath.
func (h *Handler) FirstPartyHandler(protect middleware.Middleware) http.Handler {
	mux := http.NewServeMux()
	h.RegisterAdmin(mux, protect)
	h.registerRegistration(mux)
	// This mux is reached only for paths the outer one handed over, so its own
	// catch-all is what stops /admin/nonsense falling back to Go's plain 404
	// while every other unknown URL gets the page.
	mux.HandleFunc("/", h.notFoundFor(mux))
	return h.withCSRF(mux)
}

// withCSRF wraps a handler in nosurf. It is one function so that every
// CSRF-protected group shares a single configuration and a single token pool.
func (h *Handler) withCSRF(next http.Handler) http.Handler {
	csrf := nosurf.New(next)
	csrf.SetBaseCookie(http.Cookie{
		// Path "/" because the protected routes span /admin and the public
		// registration forms, and a cookie has only one path.
		Path:     "/",
		HttpOnly: true,
		Secure:   h.cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   nosurf.MaxAge,
	})
	csrf.SetFailureHandler(http.HandlerFunc(h.csrfFailed))

	// nosurf compares a request's Origin against one it builds from the Host
	// header, and assumes https unless told otherwise — so without this every
	// form on a plain-HTTP deployment is rejected as cross-origin. BaseURL is
	// the right signal rather than r.TLS: behind a TLS-terminating proxy the
	// connection is plain HTTP but the browser's origin is https.
	csrf.SetIsTLSFunc(func(*http.Request) bool { return h.cfg.CookieSecure })
	return csrf
}

// csrfFailed answers a request whose CSRF token was missing or wrong. It is a
// 403 and nothing else: the request was either forged or made with a stale form,
// and neither case should be retried silently.
func (h *Handler) csrfFailed(w http.ResponseWriter, r *http.Request) {
	h.logger(r).Warn("rejected request with a bad CSRF token",
		"method", r.Method, "path", r.URL.Path, "reason", nosurf.Reason(r))

	// For htmx, a reload rather than a page: the token is stale, and reloading is
	// both the explanation and the fix. HX-Refresh is honoured whatever the status,
	// because htmx handles it before it decides whether to swap anything.
	if isHTMX(r) {
		w.Header().Set("HX-Refresh", "true")
		http.Error(w, "the form you submitted has expired; reload the page and try again", http.StatusForbidden)
		return
	}
	h.clientError(w, r, http.StatusForbidden, "That form has expired",
		"Forms are only good for a while, and this one has run out. Reload the page you were on and try again.")
}

// RegisterAdmin wires the admin routes: the login form and the sign-out
// endpoint reachable by anyone, everything else behind protect and the
// permission it names.
//
// Protection is applied here, route by route, rather than by the caller. A
// middleware wrapped around a prefix somewhere else is one refactor away from
// silently no longer covering a new route; a handler registered without protect
// in this list is visible on the line that registers it. Authorisation rides
// along the same way: the permission is an argument to the registration, so
// there is no second place where a route and its role could disagree.
func (h *Handler) RegisterAdmin(mux *http.ServeMux, protect middleware.Middleware) {
	mux.HandleFunc("GET /admin/login", h.adminLoginForm)
	// Rate limited, and only the POST: the form itself is harmless, and limiting a
	// GET would lock an operator out of the page they need to read the message on.
	// argon2id's cost already makes each attempt expensive, but cost is not a limit.
	mux.Handle("POST /admin/login", h.limits.login(http.HandlerFunc(h.adminLogin)))
	mux.HandleFunc("POST /admin/logout", h.adminLogout)
	// The first-account claim, unprotected because there is no account to
	// authenticate as yet — the setup token is the credential. Both routes answer
	// 404 the moment an administrator exists, and the POST shares the login
	// limiter because it too verifies a secret.
	mux.HandleFunc("GET /admin/setup", h.adminSetupForm)
	mux.Handle("POST /admin/setup", h.limits.login(http.HandlerFunc(h.adminSetupClaim)))
	// The public header's account menu. Outside the closure because it
	// serves anybody: a visitor with no session gets an empty 204, not a
	// redirect to the login form — RequireAdmin's htmx answer is a full-page
	// refresh, which on a public page would loop. It reveals nothing but
	// who you are, to you.
	mux.Handle("GET /admin/account/menu", middleware.AttachAdmin(h.users, h.log)(http.HandlerFunc(h.accountMenu)))

	// Registering twice would otherwise record every route twice; the mux would
	// panic first, but a handler mounted on two muxes in a test would not.
	h.adminRoutes = nil

	// admin registers a route behind a session and the permission it needs, and
	// records the pair. The permission is a required argument rather than
	// something a route can leave out: a new route has to say what it is for, and
	// auth.PermRead is how it says "any signed-in administrator".
	admin := func(pattern string, perm auth.Permission, handler http.HandlerFunc) {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			panic("admin route pattern must be \"METHOD /path\": " + pattern)
		}
		h.adminRoutes = append(h.adminRoutes, AdminRoute{Method: method, Pattern: path, Perm: perm})
		mux.Handle(pattern, protect(h.requirePerm(perm, handler)))
	}
	admin("GET /admin/{$}", auth.PermRead, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, adminHome, http.StatusSeeOther)
	})
	// Events, their ticket types and their questions. See admin_events.go. The
	// new-thing forms are events.write rather than read: they exist only to
	// create, and offering one to a role that cannot submit it is a page whose one
	// button is a 403. The edit pages stay read, because they are also where a
	// viewer sees what a thing is.
	admin("GET /admin/events", auth.PermRead, h.adminEventList)
	admin("GET /admin/events/new", auth.PermEventsWrite, h.adminEventNew)
	admin("POST /admin/events", auth.PermEventsWrite, h.adminEventCreate)
	admin("GET /admin/events/{id}", auth.PermRead, h.adminEventShow)
	admin("POST /admin/events/{id}", auth.PermEventsWrite, h.adminEventUpdate)
	admin("POST /admin/events/{id}/status", auth.PermEventsWrite, h.adminEventStatus)
	admin("POST /admin/events/{id}/delete", auth.PermEventsWrite, h.adminEventDelete)
	admin("POST /admin/events/{id}/image", auth.PermEventsWrite, h.adminEventImageUpload)
	admin("POST /admin/events/{id}/image/delete", auth.PermEventsWrite, h.adminEventImageDelete)
	admin("GET /admin/events/{id}/tickets/new", auth.PermEventsWrite, h.adminTicketNew)
	admin("POST /admin/events/{id}/tickets", auth.PermEventsWrite, h.adminTicketCreate)
	admin("GET /admin/events/{id}/tickets/{ticketID}", auth.PermRead, h.adminTicketEdit)
	admin("POST /admin/events/{id}/tickets/{ticketID}", auth.PermEventsWrite, h.adminTicketUpdate)
	admin("POST /admin/events/{id}/tickets/{ticketID}/delete", auth.PermEventsWrite, h.adminTicketDelete)
	admin("GET /admin/events/{id}/questions/new", auth.PermEventsWrite, h.adminQuestionNew)
	admin("POST /admin/events/{id}/questions", auth.PermEventsWrite, h.adminQuestionCreate)
	admin("GET /admin/events/{id}/questions/{questionID}", auth.PermRead, h.adminQuestionEdit)
	admin("POST /admin/events/{id}/questions/{questionID}", auth.PermEventsWrite, h.adminQuestionUpdate)
	admin("POST /admin/events/{id}/questions/{questionID}/delete", auth.PermEventsWrite, h.adminQuestionDelete)
	// An event's attendees as CSV: read, because it is a view of the
	// registrations, and buffered — see adminEventExport.
	admin("GET /admin/events/{id}/attendees.csv", auth.PermRead, h.adminEventExport)
	// Registrations. See admin_registrations.go. Payments recorded here confirm
	// a registration exactly as a gateway's do.
	admin("GET /admin/registrations", auth.PermRead, h.adminRegistrationList)
	admin("GET /admin/registrations/{id}", auth.PermRead, h.adminRegistrationShow)
	admin("POST /admin/registrations/{id}/payments", auth.PermRegistrationsWrite, h.adminRegistrationPay)
	admin("POST /admin/registrations/{id}/cancel", auth.PermRegistrationsWrite, h.adminRegistrationCancel)
	admin("POST /admin/registrations/{id}/attendees/{attendeeID}/cancel", auth.PermRegistrationsWrite, h.adminAttendeeCancel)
	admin("POST /admin/registrations/{id}/resend", auth.PermRegistrationsWrite, h.adminRegistrationResend)
	admin("POST /admin/registrations/{id}/emails/retry", auth.PermRegistrationsWrite, h.adminRegistrationRetryEmail)
	// Administrator accounts. See internal/handler/admin_users.go — accounts are
	// disabled, never deleted, and nobody may change their own role, disable
	// themselves, or reset their own password from these pages.
	admin("GET /admin/users", auth.PermUsersWrite, h.adminUserList)
	admin("GET /admin/users/new", auth.PermUsersWrite, h.adminUserNew)
	admin("POST /admin/users", auth.PermUsersWrite, h.adminUserCreate)
	admin("GET /admin/users/{id}/edit", auth.PermUsersWrite, h.adminUserEdit)
	admin("POST /admin/users/{id}/role", auth.PermUsersWrite, h.adminUserRole)
	admin("POST /admin/users/{id}/disabled", auth.PermUsersWrite, h.adminUserDisabled)
	admin("POST /admin/users/{id}/password", auth.PermUsersWrite, h.adminUserPasswordReset)
	// Your profile settings: PermRead, because every role has a password, and
	// written as accountPath because requirePerm exempts exactly this path from
	// the forced-change bounce. Two strings that had to match would eventually
	// not. The POST is the password change.
	admin("GET "+accountPath, auth.PermRead, h.adminAccount)
	// Rate limited for the same reason the login POST is: it verifies a secret.
	// Inside the session check rather than outside it, so the allowance is spent
	// by signed-in administrators rather than by anyone who can reach the door.
	admin("POST "+accountPath, auth.PermRead, rateLimited(h.limits.login, h.adminPasswordChange))
}

// rateLimited puts a limiter in front of one handler, in the shape the route
// closure takes. The limiters are middleware because most of them wrap a route
// registered directly on the mux; this is the adapter for the ones registered
// through admin().
func rateLimited(limit middleware.Middleware, next http.HandlerFunc) http.HandlerFunc {
	return limit(next).ServeHTTP
}

// page is what every rendered page needs regardless of what it shows. It is
// embedded rather than repeated so that adding something universal — the CSRF
// token was exactly this — is one change, not one per page.
type page struct {
	Title     string
	SiteName  string
	Currency  string
	CSRFToken string

	// BaseURL is the store's own address. Templates need it only where a link has
	// to work from somewhere else — the embedded catalog fragment renders inside
	// another origin's page, where a relative href would point at that origin.
	BaseURL string

	// FontCSSURL is a hosted font service's stylesheet, when one is configured, for
	// the default layout to link. Empty renders no link, which is the default: the
	// bundled theme uses the system font stack. Its origin is in the CSP's style-src
	// by the time it reaches here — config refuses to boot otherwise.
	FontCSSURL string

	// User is the signed-in administrator, and the zero User on every public page.
	// Templates ask Can rather than reading Role, so the answer comes from the
	// same map the routes are gated by.
	User auth.User
}

// Can reports whether the signed-in administrator holds a permission, so a
// template can leave out what their role could not do anyway.
//
// Presentation only. requirePerm is what actually refuses the request, and a
// page that hid a form from somebody who could still post to it would be a
// restriction on nothing but the mouse.
//
// An unknown permission is an error rather than a false, which is the whole
// reason for the second return: a mistyped name in a template would otherwise
// hide a button from everybody, for good, and look like a design decision.
func (p page) Can(perm string) (bool, error) {
	if !auth.Permission(perm).Valid() {
		return false, fmt.Errorf("page.Can: %q is not a permission", perm)
	}
	return p.User.Can(auth.Permission(perm)), nil
}

func (h *Handler) newPage(r *http.Request, title string) page {
	user, _ := middleware.AdminUser(r)
	// No template needs the hash, and every admin page would otherwise carry the
	// signed-in operator's argon2 hash in its render data — one careless
	// {{printf "%+v" .}} away from being on the page.
	user.PasswordHash = ""
	return page{
		Title:     title,
		SiteName:  h.cfg.SiteName,
		Currency:  h.cfg.Currency,
		CSRFToken: nosurf.Token(r),
		BaseURL:   h.cfg.BaseURL,

		FontCSSURL: h.cfg.FontCSSURL,
		// Absent on every page outside RequireAdmin, which is the zero User: it
		// holds no permissions, so a public template asking Can gets false.
		User: user,
	}
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	body, err := h.tmpl.Execute(name, data)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		h.logger(r).Error("writing a page failed", "template", name, "path", r.URL.Path, "error", err)
	}
}

// storeError maps a store error onto a response: missing rows are 404s, and
// anything else is a genuine server fault.
func (h *Handler) storeError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, events.ErrNotFound) || errors.Is(err, registrations.ErrNotFound) {
		h.notFound(w, r)
		return
	}
	h.serverError(w, r, err)
}
