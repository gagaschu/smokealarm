package web

import (
	"net/http"

	"github.com/gagaschu/smokealarm/internal/auth"
)

// oidcName is what the login button says it logs in with.
func (s *Server) oidcName() string {
	if s.OIDC == nil {
		return ""
	}
	if s.OIDCName == "" {
		return "OIDC"
	}
	return s.OIDCName
}

// oidcStart sends the browser to the identity provider. If the provider cannot
// be reached, the login page says so; the password form still works.
func (s *Server) oidcStart(w http.ResponseWriter, r *http.Request) {
	authURL, err := s.OIDC.Begin(w, r)
	if err != nil {
		s.Logger.Error("OIDC login could not start", "client", clientIP(r), "error", err)
		s.render(w, http.StatusServiceUnavailable, "login",
			s.loginPageData("Login with "+s.oidcName()+" is not available right now."))
		return
	}
	http.Redirect(w, r, authURL, http.StatusSeeOther)
}

// oidcCallback is where the provider sends the browser back. The page only says
// that the login failed; the reason is in the log, because it can tell an
// attacker something (for example that an address is on the allowlist).
func (s *Server) oidcCallback(w http.ResponseWriter, r *http.Request) {
	client := clientIP(r)
	username, err := s.OIDC.Finish(w, r)
	if err != nil {
		s.Logger.Warn("OIDC login failed", "client", client, "reason", err)
		s.render(w, http.StatusUnauthorized, "login",
			s.loginPageData("Login with "+s.oidcName()+" failed."))
		return
	}
	if err := s.Sessions.Start(w, r, username, auth.MethodOIDC); err != nil {
		s.serverError(w, "start session", err)
		return
	}
	s.Logger.Info("login succeeded", "client", client, "username", username, "method", auth.MethodOIDC)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
