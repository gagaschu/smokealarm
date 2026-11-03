// Package web serves the HTTP endpoints. In this phase that is only the
// health probes; the UI and login follow in a later phase.
package web

import (
	"fmt"
	"log/slog"
	"net/http"
)

// Server bundles the handlers and their dependencies.
type Server struct {
	// Ready returns nil when the service can do its job, e.g. the informer
	// caches are synced. The reason in the error is shown on /readyz.
	Ready  func() error
	Logger *slog.Logger
}

// Handler returns the router.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	return mux
}

// healthz is the liveness probe: if we can answer, the process is alive.
func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	_, _ = fmt.Fprintln(w, "ok") // a failed write to the client cannot be handled
}

// readyz is the readiness probe: 503 until the service is usable.
func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if err := s.Ready(); err != nil {
		s.Logger.Debug("not ready", "reason", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	_, _ = fmt.Fprintln(w, "ok") // a failed write to the client cannot be handled
}
