package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/notify"
)

// Every rule of the notifications applies to every target alike. These tests
// use two targets: h.notifier ("fake") and a second one ("second").

// wantNotifications fails unless the target got exactly these notifications,
// each written as "new:keys/reminders:keys/resolved:keys".
func wantNotifications(t *testing.T, n *fakeNotifier, want ...string) {
	t.Helper()
	var got []string
	for _, sent := range n.sent {
		got = append(got, summarize(sent))
	}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("%s got %q, want %q", n.Name(), got, want)
	}
}

func summarize(n notify.Notification) string {
	return "new:" + alertKeys(n.New) + "/reminders:" + alertKeys(n.Reminders) + "/resolved:" + alertKeys(n.Resolved)
}

func twoTargets(t *testing.T) (*harness, *fakeNotifier) {
	t.Helper()
	h := newHarness(t)
	second := h.addTarget("second")
	return h, second
}

func TestEveryTargetGetsTheSameNotification(t *testing.T) {
	h, second := twoTargets(t)
	h.findings = []checks.Finding{finding("a", checks.Critical), finding("b", checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.findings = nil
	h.mustRun(time.Minute)

	for _, n := range []*fakeNotifier{h.notifier, second} {
		wantNotifications(t, n, "new:a,b/reminders:/resolved:", "new:/reminders:/resolved:a,b")
	}
}

func TestFailedTargetIsRetriedWithoutDuplicatesElsewhere(t *testing.T) {
	h, second := twoTargets(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)

	second.fail = true
	if err := h.run(2 * time.Minute); err == nil || !strings.Contains(err.Error(), "second (test)") {
		t.Fatalf("RunOnce = %v, want the error of the second target", err)
	}
	_ = h.run(30 * time.Second) // still down: the first target hears nothing new
	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:")
	if second.attempts != 2 {
		t.Fatalf("second target tried %d times, want 2", second.attempts)
	}

	// Also after a restart only the failed target is tried again.
	h.restart()
	second.fail = false
	h.mustRun(30 * time.Second)
	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:")
	wantNotifications(t, second, "new:a/reminders:/resolved:")

	status := h.engine.Status().Targets
	if len(status) != 2 || status[1].LastError != "" || status[1].LastSentAt.IsZero() {
		t.Errorf("targets = %+v, want both fine", status)
	}
}

func TestStatusShowsTheErrorOfEachTarget(t *testing.T) {
	h, second := twoTargets(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	second.fail = true
	_ = h.run(2 * time.Minute)

	status := h.engine.Status().Targets
	if status[0].LastError != "" || !status[0].LastSentAt.Equal(h.now) {
		t.Errorf("first target = %+v, want sent without error", status[0])
	}
	if !strings.Contains(status[1].LastError, "smtp server unreachable") || !status[1].LastErrorAt.Equal(h.now) {
		t.Errorf("second target = %+v, want its error", status[1])
	}
}

func TestEveryTargetHasItsOwnReminderClock(t *testing.T) {
	h, second := twoTargets(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	second.fail = true
	_ = h.run(2 * time.Minute) // the first target is told now
	second.fail = false
	h.mustRun(30 * time.Minute) // the second one half an hour later

	h.mustRun(31 * time.Minute) // one hour after the first report
	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:", "new:/reminders:a/resolved:")
	wantNotifications(t, second, "new:a/reminders:/resolved:")

	h.mustRun(30 * time.Minute) // one hour after the second report
	wantNotifications(t, second, "new:a/reminders:/resolved:", "new:/reminders:a/resolved:")
	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:", "new:/reminders:a/resolved:")
}

// Phase 9: an acknowledgement drops a first message that has not gone out yet,
// and the resolution is reported anyway. With several targets that holds for
// each of them.
func TestAcknowledgedBeforeOneTargetWasToldDropsOnlyThatMessage(t *testing.T) {
	h, second := twoTargets(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	second.fail = true
	_ = h.run(2 * time.Minute)
	second.fail = false
	h.acknowledge("a")

	h.mustRun(time.Minute)
	wantNotifications(t, second)

	h.findings = nil
	h.mustRun(time.Minute)
	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:", "new:/reminders:/resolved:a")
	wantNotifications(t, second, "new:/reminders:/resolved:a")
}

func TestAcknowledgedProblemGetsNoReminderAtAnyTarget(t *testing.T) {
	h, second := twoTargets(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.acknowledge("a")
	h.mustRun(3 * time.Hour)

	for _, n := range []*fakeNotifier{h.notifier, second} {
		wantNotifications(t, n, "new:a/reminders:/resolved:")
	}
}

func TestDowntimeSilencesEveryTargetAndEndsForAll(t *testing.T) {
	h, second := twoTargets(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.setDowntime("a", 3*time.Hour)

	h.mustRun(2 * time.Hour) // no reminder during the downtime
	h.mustRun(time.Hour)     // the downtime ends: new again, everywhere
	for _, n := range []*fakeNotifier{h.notifier, second} {
		wantNotifications(t, n, "new:a/reminders:/resolved:", "new:a/reminders:/resolved:")
	}
}

func TestProblemGoneDuringDowntimeStaysSilentEverywhere(t *testing.T) {
	h, second := twoTargets(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.setDowntime("a", time.Hour)
	h.findings = nil
	h.mustRun(time.Minute)
	h.mustRun(2 * time.Hour)

	for _, n := range []*fakeNotifier{h.notifier, second} {
		wantNotifications(t, n, "new:a/reminders:/resolved:")
	}
}

func TestEscalationReachesEveryTarget(t *testing.T) {
	h, second := twoTargets(t)
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(time.Minute)
	h.mustRun(2 * time.Minute)

	for _, n := range []*fakeNotifier{h.notifier, second} {
		wantNotifications(t, n, "new:vol/reminders:/resolved:", "new:vol/reminders:/resolved:")
		if got := n.sent[1].New[0].Severity; got != checks.Critical {
			t.Errorf("%s: escalation reported as %s, want critical", n.Name(), got)
		}
	}
}

func TestNewTargetHearsWhatIsFiringButNotTheHistory(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("old", checks.Critical), finding("gone", checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.findings = []checks.Finding{finding("old", checks.Critical), finding("waiting", checks.Critical)}
	h.mustRun(time.Minute) // "gone" is resolved and reported
	h.notifier.fail = true
	_ = h.run(2 * time.Minute) // "waiting" fires, but its first message fails
	h.notifier.fail = false

	added := h.addTarget("second")
	h.mustRun(30 * time.Second)
	wantNotifications(t, added, "new:old,waiting/reminders:/resolved:")

	h.findings = nil
	h.mustRun(time.Minute)
	wantNotifications(t, added, "new:old,waiting/reminders:/resolved:", "new:/reminders:/resolved:old,waiting")
}

// A problem that ended before the new target's first run is none of its
// business, even if the others have not been told about the end yet.
func TestNewTargetDoesNotHearTheEndOfWhatItNeverHeardOf(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.findings = nil
	h.notifier.fail = true
	_ = h.run(time.Minute) // the end of "a" is not reported yet
	h.notifier.fail = false

	added := h.addTarget("second")
	h.mustRun(30 * time.Second)
	wantNotifications(t, added)
}

// When what failed is over, no message is waiting anymore, so the status must
// not go on claiming that sending fails.
func TestStatusForgetsTheErrorWhenNothingIsLeftToSend(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.notifier.fail = true
	_ = h.run(2 * time.Minute) // "a" fires, but its first message fails
	if status := h.engine.Status().Targets[0]; status.LastError == "" {
		t.Fatalf("target = %+v, want its error", status)
	}

	h.findings = nil
	h.mustRun(time.Minute) // "a" ends before anybody heard about it: silent
	if status := h.engine.Status().Targets[0]; status.LastError != "" || !status.LastErrorAt.IsZero() {
		t.Errorf("target = %+v, want no error", status)
	}
}
