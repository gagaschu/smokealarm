package engine

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/notify"
	"github.com/gagaschu/smokealarm/internal/store"
)

// routedTo says whether the target is meant to hear about a problem: its
// severity is high enough for the target, and the targets annotation, if any,
// names the kind of the target.
func (e *Engine) routedTo(target notify.Notifier, rec store.Record) bool {
	if e.MinSeverity[target.Name()] == checks.Critical && rec.Severity != checks.Critical {
		return false
	}
	return e.listedTarget(rec.Targets, target)
}

// Audience says who hears about a problem, for the web UI: the descriptions of
// the targets it is meant for, and whether that is every target. It uses the
// same rule as the sending, so a typo in an annotation (which goes to every
// target) or a warning for critical-only targets shows what really happens.
// The web server calls it; it reads only fields that do not change after the
// start.
func (e *Engine) Audience(rec store.Record) (targets []string, all bool) {
	for _, n := range e.Notifiers {
		if e.routedTo(n, rec) {
			targets = append(targets, n.Description())
		}
	}
	return targets, len(targets) == len(e.Notifiers)
}

// routedNamesNow returns the targets the open problem with this key is meant for
// in this run. It runs before the findings are saved, so the severity and the
// targets come from the current finding, if there is one (found).
func (e *Engine) routedNamesNow(open map[string]store.Record, f checks.Finding, found bool, key string) []string {
	r, exists := open[key]
	if !exists {
		return nil
	}
	if found {
		r.Severity, r.Targets = f.Severity, f.Targets
	}
	return e.routedNames(r)
}

// routedNames returns the names of the targets the problem is meant for.
func (e *Engine) routedNames(rec store.Record) []string {
	var names []string
	for _, n := range e.Notifiers {
		if e.routedTo(n, rec) {
			names = append(names, n.Name())
		}
	}
	return names
}

// listedTarget says whether the target is one of the listed kinds. A list that
// names no configured target at all (a typo, a target that was removed) counts
// as no list, so a mistake in an annotation never makes an alarm silent.
func (e *Engine) listedTarget(listed []string, target notify.Notifier) bool {
	if len(listed) == 0 || slices.Contains(listed, notify.Kind(target.Name())) {
		return true
	}
	return !slices.ContainsFunc(listed, e.configuredKind)
}

// warnAboutAnnotations logs annotation values that cannot be used (an invalid
// severity, target names that are not configured) and targets lists that send
// a problem to no target at all (only critical-only targets, for a warning).
// Each sentence names the object or namespace that carries the annotation and
// is logged once, not in every run (see warnOnce).
func (e *Engine) warnAboutAnnotations(findings []checks.Finding, severityProblems []string, now time.Time) {
	for _, problem := range severityProblems {
		e.warnOnce("annotation ignored", problem, now)
	}
	for _, f := range findings {
		if len(f.Targets) == 0 {
			continue
		}
		for _, name := range f.Targets {
			if !e.configuredKind(name) {
				e.warnOnce("annotation ignored", fmt.Sprintf("%s: %s lists %q, which is not a configured notification target",
					f.TargetsFrom, checks.TargetsAnnotation, name), now)
			}
		}
		if len(e.routedNames(store.Record{Finding: f})) == 0 {
			e.warnOnce("problems reach no notification target", fmt.Sprintf(
				"%s: its %s problems reach no notification target: %s %q lists only targets that get critical problems only",
				f.TargetsFrom, f.Severity, checks.TargetsAnnotation, strings.Join(f.Targets, ",")), now)
		}
	}
	e.forgetOldWarnings(now)
}

// annotationWarningMemory is how long a warning that no longer occurs is
// remembered. A warning names the object that carries the annotation, and for
// a pod template that is a new pod after every rollout, so the set must forget.
// A problem that is gone for a few runs and comes back is not logged again.
const annotationWarningMemory = time.Hour

// warnOnce logs a warning the first time this process sees it, and notes when
// it was last seen.
func (e *Engine) warnOnce(msg, problem string, now time.Time) {
	if e.loggedAnnotationWarnings == nil {
		e.loggedAnnotationWarnings = map[string]time.Time{}
	}
	if _, logged := e.loggedAnnotationWarnings[problem]; !logged {
		e.Logger.Warn(msg, "problem", problem)
	}
	e.loggedAnnotationWarnings[problem] = now
}

// forgetOldWarnings drops the warnings not seen for annotationWarningMemory.
func (e *Engine) forgetOldWarnings(now time.Time) {
	for problem, seen := range e.loggedAnnotationWarnings {
		if now.Sub(seen) >= annotationWarningMemory {
			delete(e.loggedAnnotationWarnings, problem)
		}
	}
}

// configuredKind says whether a target of this kind is configured.
func (e *Engine) configuredKind(kind string) bool {
	for _, n := range e.Notifiers {
		if notify.Kind(n.Name()) == kind {
			return true
		}
	}
	return false
}
