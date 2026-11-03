package web

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(ready func() error) *Server {
	return &Server{Ready: ready, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func get(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealthz(t *testing.T) {
	// Liveness must not depend on readiness.
	rec := get(t, newTestServer(func() error { return errors.New("not ready") }), "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name       string
		ready      func() error
		wantStatus int
		wantBody   string
	}{
		{"ready", func() error { return nil }, http.StatusOK, "ok"},
		{"not ready", func() error { return errors.New("caches not synced") }, http.StatusServiceUnavailable, "caches not synced"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, newTestServer(tt.ready), "/readyz")
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if !strings.Contains(rec.Body.String(), tt.wantBody) {
				t.Errorf("body = %q, want it to contain %q", rec.Body.String(), tt.wantBody)
			}
		})
	}
}
