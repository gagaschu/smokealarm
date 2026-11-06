package engine

import (
	"sort"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// CheckStatus is one line of the check list on the status page.
type CheckStatus struct {
	Name   string
	Active bool
	Reason string // why the check is not active; empty if it is
}

// TargetStatus is one line of the target list on the status page.
type TargetStatus struct {
	Name        string // Notifier.Name
	Description string // Notifier.Description, without secrets
	// CriticalOnly says that the target hears only about critical problems
	// (see Engine.MinSeverity).
	CriticalOnly bool
	LastSentAt   time.Time
	LastError    string    // empty if the last send worked
	LastErrorAt  time.Time // zero if there is no error
}

// Status is what the engine reports about itself, for the status page.
type Status struct {
	LastRun      time.Time // zero before the first run has finished
	LastRunError string    // empty if the last run had no errors
	// APIProblem says that the data of the last run may be out of date. Nil if
	// the connection to the API is fine.
	APIProblem *checks.APIProblem
	Checks     []CheckStatus
	Targets    []TargetStatus // in the order of Engine.Notifiers
}

// Status returns a copy of the current status.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.status
	s.Checks = e.checkStatuses()
	for _, n := range e.Notifiers {
		target := e.targets[n.Name()]
		target.Name = n.Name()
		target.Description = n.Description()
		target.CriticalOnly = e.MinSeverity[n.Name()] == checks.Critical
		s.Targets = append(s.Targets, target)
	}
	return s
}

// checkStatuses lists the active and the skipped checks, sorted by name.
func (e *Engine) checkStatuses() []CheckStatus {
	var statuses []CheckStatus
	for _, c := range e.Checks {
		if reason, skipped := e.runSkipped[c.Name]; skipped {
			statuses = append(statuses, CheckStatus{Name: c.Name, Reason: reason})
			continue
		}
		statuses = append(statuses, CheckStatus{Name: c.Name, Active: true})
	}
	for _, c := range e.SkippedChecks {
		statuses = append(statuses, CheckStatus{Name: c.Name, Reason: c.Reason})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	return statuses
}

// setRunResult remembers when the last run finished, which checks were
// skipped, how it went, and whether its data was up to date.
func (e *Engine) setRunResult(finished time.Time, skipped map[string]string, apiProblem *checks.APIProblem, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status.LastRun = finished
	e.runSkipped = skipped
	e.status.APIProblem = apiProblem
	e.status.LastRunError = ""
	if err != nil {
		e.status.LastRunError = err.Error()
	}
}

// setNotifyResult remembers whether the last send to a target worked. The
// error is cleared by the next success or when nothing is left to send (see
// clearNotifyError), so the status shows the current situation and not an old
// incident.
func (e *Engine) setNotifyResult(target string, err error, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.targets == nil {
		e.targets = map[string]TargetStatus{}
	}
	status := e.targets[target]
	if err == nil {
		status.LastSentAt = now
		status.LastError = ""
		status.LastErrorAt = time.Time{}
	} else {
		status.LastError = err.Error()
		status.LastErrorAt = now
	}
	e.targets[target] = status
}

// clearNotifyError removes the error of a target that has nothing left to send:
// what failed is over or acknowledged, so no message is waiting anymore.
func (e *Engine) clearNotifyError(target string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if status, found := e.targets[target]; found {
		status.LastError = ""
		status.LastErrorAt = time.Time{}
		e.targets[target] = status
	}
}
