package api

import (
	"errors"
	"net"
	"net/http"
	"net/url"

	"github.com/trevorswanson/composecd/internal/auth"
)

// sameOrigin rejects cross-site form posts to the sign-in endpoints (login
// CSRF): a browser sends Origin on POSTs, and it must name this server.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		// Not a browser form (or a very old one): nothing to forge.
		return o == ""
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// login signs in from the status page's form: a local user's name and
// password, or an empty name and an API token (or the admin token).
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-site sign-in refused", http.StatusForbidden)
		return
	}
	p, err := s.Auth.Login(r.Context(), r.PostFormValue("user"), r.PostFormValue("password"), clientIP(r))
	switch {
	case errors.Is(err, auth.ErrLocked):
		http.Redirect(w, r, "/?error=locked", http.StatusSeeOther)
		return
	case err != nil:
		if !errors.Is(err, auth.ErrUnauthenticated) {
			s.Log.Error("signing in", "error", err)
		}
		http.Redirect(w, r, "/?error=failed", http.StatusSeeOther)
		return
	}
	s.startSession(w, r, p)
}

func (s *server) startSession(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	cookie, err := s.Auth.StartSession(p, auth.SecureRequest(r))
	if err != nil {
		s.Log.Error("starting a session", "error", err)
		http.Error(w, "could not start a session", http.StatusInternalServerError)
		return
	}
	s.Log.Info("signed in", "user", p.Name, "role", p.Role, "via", p.Via)
	http.SetCookie(w, cookie)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	p, err := s.Auth.Identify(r)
	if err == nil {
		if err := auth.CheckCSRF(r, p); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
	}
	http.SetCookie(w, s.Auth.Logout(p))
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) oidcBegin(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.OIDC.Begin(w, r); err != nil {
		s.Log.Error("starting an OIDC sign-in", "error", err)
		http.Redirect(w, r, "/?error=oidc", http.StatusSeeOther)
	}
}

func (s *server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	p, err := s.Auth.OIDC.Finish(w, r)
	if err != nil {
		s.Log.Warn("OIDC sign-in failed", "error", err)
		code := "oidc"
		if errors.Is(err, auth.ErrForbidden) {
			code = "forbidden"
		}
		http.Redirect(w, r, "/?error="+code, http.StatusSeeOther)
		return
	}
	s.startSession(w, r, p)
}
