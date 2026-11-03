package engine

import (
	"context"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/store"
)

// setDowntime does what the form in the web UI does.
func (h *harness) setDowntime(key string, lasts time.Duration) {
	h.t.Helper()
	err := h.engine.Store.SetDowntime(context.Background(), store.Downtime{
		Key: key, Check: "fake",
		Object:    checks.ObjectRef{Kind: "Pod", Namespace: "media", Name: key},
		Reason:    "Broken",
		CreatedAt: h.now,
		ExpiresAt: h.now.Add(lasts),
	})
	if err != nil {
		h.t.Fatalf("SetDowntime: %v", err)
	}
}

// endDowntime does what the "End" button in the web UI does.
func (h *harness) endDowntime(key string) {
	h.t.Helper()
	ctx := context.Background()
	active, err := h.engine.Store.ActiveDowntimes(ctx, h.now)
	if err != nil {
		h.t.Fatalf("ActiveDowntimes: %v", err)
	}
	for _, d := range active {
		if d.Key == key {
			if _, _, err := h.engine.Store.EndDowntime(ctx, d.ID, h.now); err != nil {
				h.t.Fatalf("EndDowntime: %v", err)
			}
			return
		}
	}
	h.t.Fatalf("no active downtime for %s", key)
}

func (h *harness) downtimeRows() int {
	h.t.Helper()
	keys, err := h.engine.Store.DowntimeKeys(context.Background())
	if err != nil {
		h.t.Fatalf("DowntimeKeys: %v", err)
	}
	return len(keys)
}

// wantSent fails unless exactly this many notifications were sent so far.
func (h *harness) wantSent(want int) {
	h.t.Helper()
	if len(h.notifier.sent) != want {
		h.t.Fatalf("sent %d notifications, want %d: %+v", len(h.notifier.sent), want, h.notifier.sent)
	}
}

// wantLastNew fails unless the last notification reports this key as new and
// nothing else.
func (h *harness) wantLastNew(key string) {
	h.t.Helper()
	got := h.notifier.sent[len(h.notifier.sent)-1]
	if alertKeys(got.New) != key || len(got.Reminders) != 0 || len(got.Resolved) != 0 {
		h.t.Fatalf("last notification = %+v, want only %s as new", got, key)
	}
}

// wantLastResolved fails unless the last notification reports this key as
// resolved and nothing else.
func (h *harness) wantLastResolved(key string) {
	h.t.Helper()
	got := h.notifier.sent[len(h.notifier.sent)-1]
	if alertKeys(got.Resolved) != key || len(got.New) != 0 || len(got.Reminders) != 0 {
		h.t.Fatalf("last notification = %+v, want only %s as resolved", got, key)
	}
}

// firingUnreported returns a harness with problem "a" firing, whose first mail
// failed.
func firingUnreported(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.notifier.fail = true
	_ = h.run(2 * time.Minute)
	h.notifier.fail = false
	return h
}

func TestDowntimeSilencesRemindersAndBringsTheAlarmBack(t *testing.T) {
	h := firingCritical(t)
	h.setDowntime("a", 3*time.Hour)

	h.mustRun(time.Hour)
	h.mustRun(time.Hour)
	h.mustRun(59 * time.Minute)
	h.wantSent(1)

	// The problem was reported before the downtime, so only the cleared marker
	// brings it back: a new mail, and not a reminder.
	h.mustRun(time.Minute)
	h.wantSent(2)
	h.wantLastNew("a")
	if h.downtimeRows() != 0 {
		t.Error("the expired downtime was not removed")
	}

	// The reminder clock runs from this mail, not from the first one.
	h.mustRun(59 * time.Minute)
	h.wantSent(2)
	h.mustRun(time.Minute)
	h.wantSent(3)
	if alertKeys(h.notifier.sent[2].Reminders) != "a" {
		t.Fatalf("sent %+v, want a reminder for a", h.notifier.sent[2])
	}
}

func TestDowntimeOnReportedWarningBringsItBackWithoutReminders(t *testing.T) {
	h := newHarness(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.findings = []checks.Finding{finding("a", checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.wantSent(1)
	h.setDowntime("a", time.Hour)

	h.mustRun(59 * time.Minute)
	h.wantSent(1)
	h.mustRun(time.Minute)
	h.wantSent(2)
	h.wantLastNew("a")

	h.mustRun(2 * time.Hour)
	h.wantSent(2)
}

func TestDowntimeHoldsBackAFirstMailThatFailed(t *testing.T) {
	h := firingUnreported(t)
	h.setDowntime("a", time.Hour)

	h.mustRun(30 * time.Minute)
	h.wantSent(0)

	h.mustRun(30 * time.Minute)
	h.wantSent(1)
	h.wantLastNew("a")

	// Holding the mail back must not use up the later "resolved" mail.
	h.findings = nil
	h.mustRun(time.Minute)
	h.wantSent(2)
	h.wantLastResolved("a")
}

func TestDowntimeFromBeforeTheProblemFires(t *testing.T) {
	h := newHarness(t)
	h.setDowntime("a", time.Hour)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.mustRun(30 * time.Minute)
	h.wantSent(0)
	if h.countRows(string(store.StateFiring)) != 1 {
		t.Error("the problem must keep firing during the downtime")
	}

	h.mustRun(28 * time.Minute)
	h.wantSent(1)
	h.wantLastNew("a")
}

func TestFailedMailAtTheEndOfADowntimeIsRetriedAfterARestart(t *testing.T) {
	h := firingCritical(t)
	h.setDowntime("a", time.Hour)

	h.notifier.fail = true
	if err := h.run(time.Hour); err == nil {
		t.Fatal("RunOnce: want the send error")
	}
	h.notifier.fail = false
	h.restart()

	h.mustRun(30 * time.Second)
	h.wantSent(2)
	h.wantLastNew("a")

	h.mustRun(30 * time.Second)
	h.wantSent(2)
}

func TestProblemThatGoesAwayDuringDowntimeStaysSilent(t *testing.T) {
	h := firingCritical(t)
	h.setDowntime("a", time.Hour)

	h.findings = nil
	h.mustRun(time.Minute)
	h.wantSent(1)

	resolved, err := h.engine.Store.Resolved(context.Background(), "", 10)
	if err != nil || len(resolved) != 1 {
		t.Fatalf("Resolved = %v, %v", resolved, err)
	}
	if resolved[0].ResolvedNotifiedAt == nil {
		t.Error("resolved_notified_at is not set, so the mail would go out later")
	}

	h.restart()
	h.mustRun(2 * time.Hour)
	h.wantSent(1)
	if h.downtimeRows() != 0 {
		t.Error("the expired downtime was not removed")
	}
}

func TestPendingResolvedMailIsDroppedByADowntime(t *testing.T) {
	h := firingCritical(t)
	h.findings = nil
	h.notifier.fail = true
	if err := h.run(time.Minute); err == nil {
		t.Fatal("RunOnce: want the send error")
	}
	h.notifier.fail = false
	h.setDowntime("a", time.Hour)

	h.mustRun(time.Minute)
	h.mustRun(2 * time.Hour)
	h.wantSent(1)
}

func TestAcknowledgedProblemThatGoesAwayDuringDowntimeStaysSilent(t *testing.T) {
	h := firingCritical(t)
	h.acknowledge("a")
	h.setDowntime("a", time.Hour)

	h.findings = nil
	h.mustRun(time.Minute)
	h.mustRun(2 * time.Hour)
	h.wantSent(1)
}

func TestAcknowledgedProblemStillActiveAtTheEndSendsOnlyItsResolution(t *testing.T) {
	h := firingCritical(t)
	h.acknowledge("a")
	h.setDowntime("a", time.Hour)

	h.mustRun(time.Hour)
	h.mustRun(3 * time.Hour)
	h.wantSent(1)
	if h.downtimeRows() != 0 {
		t.Error("the expired downtime was not removed")
	}

	h.findings = nil
	h.mustRun(time.Minute)
	h.wantSent(2)
	h.wantLastResolved("a")
}

func TestAcknowledgedBeforeFirstMailStaysSilentAfterADowntime(t *testing.T) {
	h := firingUnreported(t)
	h.acknowledge("a")
	h.setDowntime("a", time.Hour)

	h.mustRun(time.Hour)
	h.mustRun(time.Hour)
	h.wantSent(0)

	h.findings = nil
	h.mustRun(time.Minute)
	h.wantSent(1)
	h.wantLastResolved("a")
}

func TestEscalationDuringDowntimeIsReportedAtTheEnd(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.wantSent(1)
	h.setDowntime("vol", time.Hour)

	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(30 * time.Minute)
	h.wantSent(1)

	h.mustRun(30 * time.Minute)
	h.wantSent(2)
	h.wantLastNew("vol")
	if got := h.notifier.sent[1].New[0].Severity; got != checks.Critical {
		t.Errorf("severity = %s, want critical", got)
	}
}

func TestFlappingProblemStaysSilentForTheWholeDowntime(t *testing.T) {
	h := firingCritical(t)
	h.setDowntime("a", time.Hour)

	h.findings = nil
	h.mustRun(time.Minute) // resolved

	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(time.Minute)     // pending again
	h.mustRun(2 * time.Minute) // firing again

	h.findings = nil
	h.mustRun(time.Minute) // resolved again
	h.wantSent(1)

	// After the downtime it is a normal new alarm.
	h.mustRun(time.Hour)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(time.Minute)
	h.mustRun(2 * time.Minute)
	h.wantSent(2)
	h.wantLastNew("a")
}

func TestDowntimeSetWhileTheProblemIsResolved(t *testing.T) {
	h := firingCritical(t)
	h.findings = nil
	h.mustRun(time.Minute)
	h.wantSent(2) // the resolved mail, from before the downtime
	h.setDowntime("a", time.Hour)

	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(time.Minute)
	h.mustRun(2 * time.Minute)
	h.findings = nil
	h.mustRun(time.Minute)
	h.wantSent(2)
}

func TestProblemStillFiringAtTheEndOfADowntimeSetWhileResolved(t *testing.T) {
	h := firingCritical(t)
	h.findings = nil
	h.mustRun(time.Minute)
	h.setDowntime("a", time.Hour)

	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(time.Minute)
	h.mustRun(2 * time.Minute)
	h.wantSent(2)

	h.mustRun(time.Hour)
	h.wantSent(3)
	h.wantLastNew("a")
}

func TestDowntimeCoversOnlyItsOwnKey(t *testing.T) {
	h := newHarness(t)
	h.setDowntime("a", time.Hour)
	h.findings = []checks.Finding{finding("a", checks.Critical), finding("a-new-reason", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.wantSent(1)
	h.wantLastNew("a-new-reason")
}

func TestEndingADowntimeEarlyBringsAnActiveProblemBack(t *testing.T) {
	h := firingCritical(t)
	h.setDowntime("a", 10*time.Hour)
	h.mustRun(time.Hour)
	h.wantSent(1)

	h.endDowntime("a")
	h.mustRun(30 * time.Second)
	h.wantSent(2)
	h.wantLastNew("a")
	if h.downtimeRows() != 0 {
		t.Error("the ended downtime was not removed")
	}
}

func TestEndingADowntimeEarlyAfterTheProblemWentAwayStaysSilent(t *testing.T) {
	h := firingCritical(t)
	h.setDowntime("a", 10*time.Hour)
	h.findings = nil
	h.mustRun(time.Minute)

	h.endDowntime("a")
	h.mustRun(30 * time.Second)
	h.mustRun(time.Hour)
	h.wantSent(1)
}

// Nobody can tell whether it went away just before or just after the end, so
// it counts as gone during the downtime.
func TestProblemThatGoesAwayInTheRunOfTheEndStaysSilent(t *testing.T) {
	h := firingCritical(t)
	h.setDowntime("a", time.Hour)
	h.mustRun(59 * time.Minute)

	h.findings = nil
	h.mustRun(time.Minute)
	h.mustRun(time.Minute)
	h.wantSent(1)
}

func TestAcknowledgedProblemThatGoesAwayInTheRunOfTheEndSendsItsResolution(t *testing.T) {
	h := firingCritical(t)
	h.acknowledge("a")
	h.setDowntime("a", time.Hour)
	h.mustRun(59 * time.Minute)

	h.findings = nil
	h.mustRun(time.Minute)
	h.wantSent(2)
	h.wantLastResolved("a")
}

func TestDowntimeOfACheckWithUnknownStateWaitsForTheCheck(t *testing.T) {
	h := firingCritical(t)
	h.setDowntime("a", time.Hour)

	h.skipped = map[string]string{"fake": "kubelet does not answer"}
	h.mustRun(2 * time.Hour)
	h.wantSent(1)
	if h.downtimeRows() != 1 {
		t.Error("the downtime must wait until the check runs again")
	}

	h.skipped = nil
	h.mustRun(30 * time.Second)
	h.wantSent(2)
	h.wantLastNew("a")
	if h.downtimeRows() != 0 {
		t.Error("the expired downtime was not removed")
	}
}

func TestDowntimeSurvivesARestart(t *testing.T) {
	h := firingCritical(t)
	h.setDowntime("a", 3*time.Hour)
	h.mustRun(time.Hour)

	h.restart()
	h.engine.CriticalRepeatInterval = time.Hour
	h.mustRun(time.Hour)
	h.wantSent(1)

	h.mustRun(time.Hour)
	h.wantSent(2)
	h.wantLastNew("a")
}

func TestOtherProblemsStillSendWhileOneIsInADowntime(t *testing.T) {
	h := firingCritical(t)
	h.setDowntime("a", 10*time.Hour)
	h.findings = append(h.findings, finding("b", checks.Critical))
	h.mustRun(time.Minute)
	h.mustRun(2 * time.Minute)
	h.wantSent(2)
	h.wantLastNew("b")
}
