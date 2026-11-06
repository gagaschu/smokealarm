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
	// Templates names the user's message templates the target uses, e.g.
	// "subject.tmpl, slack.chat.tmpl" or "built-in"; empty for targets
	// without templates.
	Templates string
	// TemplateError says that the last message went out with the built-in
	// template, because a user template failed. Empty if it did not.
	TemplateError   string
	TemplateErrorAt time.Time // zero if there is no template error
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
		target.Templates = e.TemplateFiles[n.Name()]
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

// setTemplateResult remembers whether the last message to a target went out
// with the built-in template because a user template failed (err is a
// notify.FallbackError) or with the user's templates (err is nil). Unlike a
// send error it says nothing about delivery: the message went out either way.
func (e *Engine) setTemplateResult(target string, err error, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.targets == nil {
		e.targets = map[string]TargetStatus{}
	}
	status := e.targets[target]
	if err == nil {
		status.TemplateError = ""
		status.TemplateErrorAt = time.Time{}
	} else {
		status.TemplateError = err.Error()
		status.TemplateErrorAt = now
	}
	e.targets[target] = status
}

// setNotifyResult remembers whether the last send to a target worked. The
// error is cleared by the next success or when none of the failed messages is
// owed anymore (see settleFailed), so the status shows the current situation
// and not an old incident.
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
