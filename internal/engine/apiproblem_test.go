package engine

import (
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

const apiAlarmKey = "api-connection/APIServer//kubernetes/APIConnectionFailing"

// withAPICheck adds the real check to a harness that has problem "a" reported.
func withAPICheck(t *testing.T) *harness {
	t.Helper()
	h := firingCritical(t)
	h.engine.Checks = append(h.engine.Checks, checks.Check{Name: checks.CheckAPIConnection, Run: checks.APIConnection})
	return h
}

func TestBlindRunKeepsProblemsAndRaisesTheAlarm(t *testing.T) {
	h := withAPICheck(t)
	runs := h.checkRuns

	// Everything is stale. The checks would find nothing, which means nothing.
	h.apiProblem = &checks.APIProblem{Since: h.now, Blind: true, Error: "connection refused"}
	h.findings = nil
	h.mustRun(30 * time.Second)
	if h.checkRuns != runs {
		t.Errorf("the other checks ran %d times, want not at all", h.checkRuns-runs)
	}
	if h.countRows("resolved") != 0 {
		t.Error("a problem was resolved without any data")
	}

	h.mustRun(2 * time.Minute)
	h.wantSent(2)
	h.wantLastNew(apiAlarmKey)
	for _, c := range h.engine.Status().Checks {
		if c.Name == "fake" && (c.Active || c.Reason == "") {
			t.Errorf("status of the other check = %+v, want the reason it is paused", c)
		}
	}

	// Reminders only for the alarm itself, not for what nobody can see.
	h.mustRun(2 * time.Hour)
	h.wantSent(3)
	if got := alertKeys(h.notifier.sent[2].Reminders); got != apiAlarmKey {
		t.Errorf("reminders = %q, want only the alarm", got)
	}

	// Back: the old problem is judged by fresh data again, and the alarm ends.
	h.apiProblem = nil
	h.mustRun(30 * time.Second)
	if alerts := h.notifier.sent[len(h.notifier.sent)-1].Resolved; len(alerts) != 2 {
		t.Errorf("resolved = %+v, want the old problem and the alarm", alerts)
	}
}

func TestPartialFailureOnlyRaisesTheAlarm(t *testing.T) {
	h := withAPICheck(t)
	runs := h.checkRuns

	h.apiProblem = &checks.APIProblem{Since: h.now, Resources: []string{"certificates"}, Error: "forbidden"}
	h.mustRun(30 * time.Second)
	if h.checkRuns != runs+1 {
		t.Errorf("the other checks ran %d times, want once", h.checkRuns-runs)
	}
	if h.engine.Status().APIProblem == nil {
		t.Error("the status does not show the problem")
	}

	h.apiProblem = nil
	h.mustRun(30 * time.Second)
	if h.engine.Status().APIProblem != nil {
		t.Error("the status still shows the problem")
	}
}
