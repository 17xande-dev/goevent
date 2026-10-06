package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// newUserForm is a valid create-administrator submission: the simplest write
// in the admin, and so the one these tests forge.
func newUserForm(email string) url.Values {
	return url.Values{
		"email":            {email},
		"role":             {"viewer"},
		"password":         {"a perfectly long password"},
		"password_confirm": {"a perfectly long password"},
	}
}

// accounts counts administrators, which is how these tests see whether a
// forged request wrote anything.
func accounts(t *testing.T, s *app) int {
	t.Helper()
	n, err := s.users.Count(t.Context())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	return n
}

func TestCSRF_RejectsPostWithoutAToken(t *testing.T) {
	s := setupApp(t)

	// Every state-changing admin route, because a form that forgot its hidden
	// field is exactly the kind of gap that goes unnoticed until it is abused.
	routes := s.handler.AdminProtectedRoutes()
	var posts int
	for _, rt := range append(routes, AdminRoute{Method: http.MethodPost, Pattern: "/admin/login"},
		AdminRoute{Method: http.MethodPost, Pattern: "/admin/logout"}) {
		if rt.Method != http.MethodPost {
			continue
		}
		posts++
		// An explicit empty token stops the helper from supplying a real one.
		form := newUserForm("forged@example.com")
		form.Set("csrf_token", "")
		res, _ := post(t, s.srv, rt.TestPath(), form)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s without a token = %d, want 403", rt.TestPath(), res.StatusCode)
		}
	}
	if posts < 5 {
		t.Fatalf("only %d POST routes checked; the route list is not loading", posts)
	}
	if n := accounts(t, s); n != 1 {
		t.Errorf("%d accounts; a request without a CSRF token wrote something", n)
	}
}

func TestCSRF_RejectsATokenFromAnotherSession(t *testing.T) {
	victim := setupApp(t)
	attacker := newApp(t)

	// nosurf ties the submitted token to the client's own cookie, so a token
	// minted for one visitor is useless to another.
	form := newUserForm("forged@example.com")
	form.Set("csrf_token", csrfToken(t, attacker.srv))
	res, _ := post(t, victim.srv, "/admin/users", form)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("POST with another session's token = %d, want 403", res.StatusCode)
	}
	if n := accounts(t, victim); n != 1 {
		t.Errorf("%d accounts; the forged request wrote something", n)
	}
}

// The origin check has three inputs and short-circuits on the first, so a test
// that always sends Sec-Fetch-Site never exercises the other two. nosurf assumes
// https unless configured otherwise, which made a plain-HTTP deployment reject
// every form while the tests passed — hence this.
func TestCSRF_AcceptsSameOriginPostByOriginOrRefererAlone(t *testing.T) {
	cases := map[string]func(r *http.Request, origin string){
		"Origin only":  func(r *http.Request, origin string) { r.Header.Set("Origin", origin) },
		"Referer only": func(r *http.Request, origin string) { r.Header.Set("Referer", origin+"/admin/users/new") },
	}

	for name, setHeaders := range cases {
		s := setupApp(t)

		form := newUserForm("new@example.com")
		form.Set("csrf_token", csrfToken(t, s.srv))
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.srv.URL+"/admin/users",
			strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		setHeaders(req, s.srv.URL)

		res, body := do(t, s.srv, req)
		if res.StatusCode != http.StatusSeeOther {
			t.Errorf("%s: POST = %d, want 303: %s", name, res.StatusCode, excerpt(body))
			continue
		}
		if n := accounts(t, s); n != 2 {
			t.Errorf("%s: %d accounts; the accepted request did not write", name, n)
		}
	}
}

func TestCSRF_RejectsACrossOriginPost(t *testing.T) {
	s := setupApp(t)

	// nosurf checks where the request came from as well as its token. A valid
	// token submitted from another origin — the shape of a real CSRF attempt if
	// a token ever leaked — must still be refused.
	form := newUserForm("forged@example.com")
	form.Set("csrf_token", csrfToken(t, s.srv))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, s.srv.URL+"/admin/users",
		strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://not-this-site.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")

	res, _ := do(t, s.srv, req)
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST with a valid token = %d, want 403", res.StatusCode)
	}
	if n := accounts(t, s); n != 1 {
		t.Errorf("%d accounts; the cross-origin request wrote something", n)
	}
}

func TestCSRF_TokenIsInEveryAdminForm(t *testing.T) {
	s := setupApp(t)
	other := mustAccount(t, s, "other@example.com", testPassword, "viewer")

	// Count forms against tokens: some pages carry several, and one form
	// missing its field would otherwise hide behind the others.
	pages := []string{"/admin/users", "/admin/users/new", "/admin/users/" + other.ID + "/edit", "/admin/account"}
	var forms int
	for _, path := range pages {
		_, body := get(t, s.srv, path)
		n := strings.Count(body, "<form ")
		tokens := strings.Count(body, `name="csrf_token"`)
		if n == 0 {
			t.Errorf("%s has no forms; the check is not testing anything", path)
		}
		if tokens != n {
			t.Errorf("%s has %d forms but %d CSRF fields", path, n, tokens)
		}
		forms += n
	}
	if forms < len(pages) {
		t.Fatalf("only %d forms across %d pages", forms, len(pages))
	}
}

func TestCSRF_CookieIsScopedAndLocked(t *testing.T) {
	// A fresh client, because nosurf only sets the cookie when the request
	// arrives without a valid one.
	s := newApp(t)

	res, _ := get(t, s.srv, "/admin/login")
	var found bool
	for _, c := range res.Cookies() {
		if c.Name == "csrf_token" {
			found = true
			// Path "/" because the protected group spans /admin and the public
			// registration forms.
			if c.Path != "/" {
				t.Errorf("the CSRF cookie has Path %q, want /", c.Path)
			}
			if !c.HttpOnly {
				t.Error("the CSRF cookie is not HttpOnly")
			}
			if c.SameSite != http.SameSiteLaxMode {
				t.Errorf("the CSRF cookie has SameSite %v, want Lax", c.SameSite)
			}
		}
	}
	if !found {
		t.Fatal("no csrf_token cookie was set on the login page")
	}
}
