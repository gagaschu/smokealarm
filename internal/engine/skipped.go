package engine

import (
	"maps"
	"slices"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// SkippedCheck is a check that does not run, with the reason to show.
type SkippedCheck struct {
	Name   string
	Reason string
	// KeepProblems leaves the open problems of the check as they are, because
	// not running it says nothing about them (a missing permission, for
	// example). Without it they count as gone.
	KeepProblems bool
}

// splitSkipped separates the checks that can run from those the snapshot
// reports as unable to run this time (check name -> reason). It also logs
// when that set changes, so a permanent problem is not repeated every run.
func (e *Engine) splitSkipped(snap *checks.Snapshot) ([]checks.Check, map[string]string) {
	var runnable []checks.Check
	skipped := map[string]string{}
	for _, c := range e.Checks {
		if reason, found := skipReason(snap, c.Name); found {
			skipped[c.Name] = reason
			continue
		}
		runnable = append(runnable, c)
	}

	for name, reason := range skipped {
		if e.previousSkipped[name] != reason {
			e.Logger.Warn("check skipped", "check", name, "reason", reason)
		}
	}
	for name := range e.previousSkipped {
		if _, stillSkipped := skipped[name]; !stillSkipped {
			e.Logger.Info("check runs again", "check", name)
		}
	}
	e.previousSkipped = skipped
	return runnable, skipped
}

// skipReason says why a check cannot run in this run. Without any fresh data no
// check has anything to say, except the one that reports exactly that.
func skipReason(snap *checks.Snapshot, check string) (string, bool) {
	if snap.APIProblem != nil && snap.APIProblem.Blind && check != checks.CheckAPIConnection {
		return "the Kubernetes API cannot be read, the data is out of date", true
	}
	reason, found := snap.Skipped[check]
	return reason, found
}

// skippedNames returns the names of the skipped checks, sorted.
func skippedNames(skipped map[string]string) []string {
	return slices.Sorted(maps.Keys(skipped))
}

// checksWithUnknownState names the checks that say nothing about their problems
// in this run: those the snapshot skipped, and those skipped for the whole
// process with KeepProblems.
func (e *Engine) checksWithUnknownState(runSkipped map[string]string) []string {
	names := skippedNames(runSkipped)
	for _, c := range e.SkippedChecks {
		if c.KeepProblems {
			names = append(names, c.Name)
		}
	}
	return names
}
