package handler

import (
	"net/http"

	"github.com/17xande-dev/goevent/internal/middleware"
	"github.com/17xande-dev/goevent/internal/validate"
)

// Your profile settings: one page for everything about your own account. Every
// role has a password to change here.

// accountNotices are the notices the account page shows, by code. See
// userNotices for why a notice is looked up rather than echoed.
var accountNotices = map[string]string{}

type accountPage struct {
	page
	Notice string

	// Forced is set when this page was reached by being bounced to it, so it can
	// say why rather than looking like a page the browser wandered onto.
	Forced         bool
	PasswordErrors validate.FormErrors
}

// adminAccount is your profile settings page, and the one admin page every role
// can reach — including an account that has been bounced here and can reach
// nothing else.
func (h *Handler) adminAccount(w http.ResponseWriter, r *http.Request) {
	h.renderAccount(w, r, http.StatusOK, accountPage{Notice: noticeFor(r, accountNotices)})
}

// renderAccount fills in what every render of the account page needs around the
// parts the caller set: the page and the forced flag.
func (h *Handler) renderAccount(w http.ResponseWriter, r *http.Request, status int, v accountPage) {
	user, ok := middleware.AdminUser(r)
	if !ok {
		h.serverError(w, r, errNoAdminUser)
		return
	}
	v.page = h.newPage(r, "Profile settings")
	v.Forced = user.MustChangePassword
	h.render(w, r, status, "admin_account", v)
}

// accountMenu is the public header's account menu: a fragment the public layout
// loads with htmx, so the public pages stay the same for everybody — cacheable,
// and never carrying the admin session, whose cookie is scoped to /admin and so
// reaches only this request.
//
// Nobody signed in is a 204, which htmx is configured not to swap: the
// placeholder stays empty, and a visitor sees nothing.
func (h *Handler) accountMenu(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	if _, ok := middleware.AdminUser(r); !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.render(w, r, http.StatusOK, "account_menu", h.newPage(r, ""))
}
