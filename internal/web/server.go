// Package web serves the HTTP endpoints: the health probes, the login, and
// the pages (overview, history, status). Pages are rendered on the server
// with html/template; there is no JavaScript.
package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/gagaschu/smokealarm/internal/auth"
	"github.com/gagaschu/smokealarm/internal/engine"
	"github.com/gagaschu/smokealarm/internal/store"
)

// assets holds the templates and the stylesheet. go:embed needs a
// package-level variable; it is never modified.
//
//go:embed templates static
var assets embed.FS

// Server bundles the handlers and their dependencies.
type Server struct {
	// Ready returns nil when the service can do its job, e.g. the informer
	// caches are synced and the database answers. The reason in the error is
	// shown on /readyz.
	Ready func(ctx context.Context) error

	Store    *store.Store
	Login    auth.PasswordLogin
	Sessions *auth.Sessions
	Limiter  *auth.RateLimiter

	// Status returns what the engine knows about itself.
	Status func() engine.Status

	Now    func() time.Time
	Logger *slog.Logger

	pages map[string]*template.Template // page name -> layout + page
}

// Handler parses the templates and returns the router. It fails if a
// template is broken, so that shows up at startup and not on the first visit.
func (s *Server) Handler() (http.Handler, error) {
	s.pages = map[string]*template.Template{}
	for _, name := range []string{"login", "overview", "history", "status"} {
		page, err := template.ParseFS(assets, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}
		s.pages[name] = page
	}
	static, err := fs.Sub(assets, "static")
	if err != nil {
		return nil, fmt.Errorf("open static files: %w", err)
	}

	mux := http.NewServeMux()
	// Probes and static files need no login.
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))

	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.loginSubmit)

	// Everything below needs a valid session.
	protected := func(handler http.HandlerFunc) http.Handler {
		return s.Sessions.Require(handler)
	}
	mux.Handle("POST /logout", protected(s.logout))
	mux.Handle("GET /{$}", protected(s.overview))
	mux.Handle("GET /history", protected(s.history))
	mux.Handle("GET /status", protected(s.status))

	return securityHeaders(mux), nil
}

// securityHeaders adds headers that make browsers stricter. The pages need
// no inline scripts or styles, so the policy can be tight.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// healthz is the liveness probe: if we can answer, the process is alive.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = fmt.Fprintln(w, "ok") // a failed write to the client cannot be handled
}

// readyz is the readiness probe: 503 until the service is usable.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.Ready(r.Context()); err != nil {
		s.Logger.Debug("not ready", "reason", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = fmt.Fprintln(w, "ok") // a failed write to the client cannot be handled
}
