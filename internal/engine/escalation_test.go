package engine

import (
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// firingWarning brings a problem that can get worse to the point where it was
// reported as a warning.
func firingWarning(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	if len(h.notifier.sent) != 1 {
		t.Fatalf("sent %d notifications, want the first warning", len(h.notifier.sent))
	}
	return h
}

func TestFlappingBetweenWarningAndCriticalSendsNothing(t *testing.T) {
	h := firingWarning(t)

	for i := 0; i < 20; i++ {
		severity := checks.Critical
		if i%2 == 1 {
			severity = checks.Warning
		}
		h.findings = []checks.Finding{escalating(severity)}
		h.mustRun(30 * time.Second)
	}
	if len(h.notifier.sent) != 1 {
		t.Errorf("sent %d notifications while flapping, want only the first warning", len(h.notifier.sent))
	}
}

func TestEscalationIsReportedOnceItHasLastedAlertAfter(t *testing.T) {
	h := firingWarning(t)

	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(30 * time.Second)
	h.mustRun(119 * time.Second)
	if len(h.notifier.sent) != 1 {
		t.Fatalf("sent %d notifications just below ALERT_AFTER, want 1", len(h.notifier.sent))
	}

	h.mustRun(time.Second)
	if len(h.notifier.sent) != 2 || len(h.notifier.sent[1].New) != 1 {
		t.Fatalf("sent = %+v, want the escalation as new", h.notifier.sent)
	}
	if got := h.notifier.sent[1].New[0].Severity; got != checks.Critical {
		t.Errorf("severity = %s, want critical", got)
	}

	h.mustRun(time.Hour)
	if len(h.notifier.sent) != 2 {
		t.Errorf("sent %d notifications, want the escalation only once", len(h.notifier.sent))
	}
}

func TestEscalationWaitSurvivesARestart(t *testing.T) {
	h := firingWarning(t)

	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(0)
	h.restart()
	h.mustRun(2 * time.Minute)
	if len(h.notifier.sent) != 2 {
		t.Errorf("sent %d notifications, want the escalation after the restart", len(h.notifier.sent))
	}
}

func TestNoEscalationDelayWithoutAlertAfter(t *testing.T) {
	h := newHarness(t)
	h.engine.AlertAfter = 0
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(0)

	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(30 * time.Second)
	if len(h.notifier.sent) != 2 {
		t.Errorf("sent %d notifications, want the escalation at once", len(h.notifier.sent))
	}
}

func TestNoReminderWhileAnEscalationWaits(t *testing.T) {
	h := firingWarning(t)
	h.engine.CriticalRepeatInterval = time.Minute

	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(90 * time.Second) // the warning was reported 90 s ago
	if len(h.notifier.sent) != 1 {
		t.Errorf("sent %d notifications, want no reminder for what was only a warning", len(h.notifier.sent))
	}
}

// A target for critical problems only has no marker yet. It must not hear about
// the critical state before the others do.
func TestCriticalOnlyTargetWaitsForTheEscalationToo(t *testing.T) {
	h := newHarness(t)
	pager := h.addTarget("ntfy")
	h.engine.MinSeverity = map[string]checks.Severity{"ntfy": checks.Critical}
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)

	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(30 * time.Second)
	if len(pager.sent) != 0 {
		t.Fatalf("critical-only target got %+v before ALERT_AFTER", pager.sent)
	}
	h.mustRun(2 * time.Minute)
	if len(pager.sent) != 1 || len(pager.sent[0].New) != 1 {
		t.Errorf("critical-only target got %+v, want the problem once", pager.sent)
	}
}
