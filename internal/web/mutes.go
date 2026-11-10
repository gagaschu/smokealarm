package web

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gagaschu/smokealarm/internal/engine"
)

// mutedTargetView is a notification target that is switched off, for the note
// on the overview.
type mutedTargetView struct {
	Description string
	Since       string // muted_at in the time zone of the pages
	Unmuting    bool   // switch-on requested, not finished yet
}

// mutedTargets lists the configured targets that are switched off, in the
// order of the targets. A row for a target that is no longer configured (the
// user removed it from the settings) is not shown: nothing can be sent to it.
func (s *Server) mutedTargets(ctx context.Context, targets []engine.TargetStatus) ([]mutedTargetView, error) {
	mutes, err := s.Store.TargetMutes(ctx)
	if err != nil {
		return nil, fmt.Errorf("load target mutes: %w", err)
	}
	var views []mutedTargetView
	for _, t := range targets {
		m, off := mutes[t.Name]
		if !off {
			continue
		}
		views = append(views, mutedTargetView{
			Description: t.Description,
			Since:       m.MutedAt.In(s.zone()).Format(timeFormat),
			Unmuting:    m.UnmuteRequestedAt != nil,
		})
	}
	return views, nil
}

// findTarget returns the configured target with the name from the URL. The
// log-only fallback cannot be switched off: it is the only trace of a problem
// when nothing else is set up.
func (s *Server) findTarget(name string) (engine.TargetStatus, bool) {
	if name == "log" {
		return engine.TargetStatus{}, false
	}
	for _, t := range s.Status().Targets {
		if t.Name == name {
			return t, true
		}
	}
	return engine.TargetStatus{}, false
}

// muteTarget switches a target off. It also cancels a switch-on that the
// engine has not finished yet.
func (s *Server) muteTarget(w http.ResponseWriter, r *http.Request) {
	target, ok := s.findTarget(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	changed, err := s.Store.MuteTarget(r.Context(), target.Name, s.Now())
	if err != nil {
		s.serverError(w, "switch notification target off", err)
		return
	}
	if changed {
		s.Logger.Info("notification target switched off", "target", target.Description)
	}
	http.Redirect(w, r, "/status", http.StatusSeeOther)
}

// unmuteTarget only asks to switch a target on. The engine finishes it in its
// next run, because only it knows what the target missed while it was off.
func (s *Server) unmuteTarget(w http.ResponseWriter, r *http.Request) {
	target, ok := s.findTarget(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	changed, err := s.Store.RequestUnmute(r.Context(), target.Name, s.Now())
	if err != nil {
		s.serverError(w, "request to switch notification target on", err)
		return
	}
	if changed {
		s.Logger.Info("switching notification target on requested", "target", target.Description)
	}
	http.Redirect(w, r, "/status", http.StatusSeeOther)
}
