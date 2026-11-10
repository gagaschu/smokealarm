package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// These tests use two targets (see twoTargets) and switch the second one off
// and on, as the web UI does.

// mute does what the "switch off" button in the web UI does.
func (h *harness) mute(name string) {
	h.t.Helper()
	if _, err := h.engine.Store.MuteTarget(context.Background(), name, h.now); err != nil {
		h.t.Fatalf("MuteTarget(%s): %v", name, err)
	}
}

// requestUnmute does what the "switch on" button in the web UI does; the
// engine finishes it in its next run.
func (h *harness) requestUnmute(name string) {
	h.t.Helper()
	changed, err := h.engine.Store.RequestUnmute(context.Background(), name, h.now)
	if err != nil || !changed {
		h.t.Fatalf("RequestUnmute(%s) = %v, %v, want true, nil", name, changed, err)
	}
}

// wantMuted fails unless the target is off (muted) or not.
func (h *harness) wantMuted(name string, want bool) {
	h.t.Helper()
	mutes, err := h.engine.Store.TargetMutes(context.Background())
	if err != nil {
		h.t.Fatalf("TargetMutes: %v", err)
	}
	if _, got := mutes[name]; got != want {
		h.t.Fatalf("target %s muted = %v, want %v (%+v)", name, got, want, mutes)
	}
}

// heardByBoth makes "a" fire and be reported to both targets.
func heardByBoth(t *testing.T) (*harness, *fakeNotifier) {
	t.Helper()
	h, second := twoTargets(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	for _, n := range []*fakeNotifier{h.notifier, second} {
		wantNotifications(t, n, "new:a/reminders:/resolved:")
	}
	return h, second
}

func TestMutedTargetSendsNothingWhileOthersDo(t *testing.T) {
	h, second := twoTargets(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.engine.ResolveAfter = 5 * time.Minute
	h.mute("second")

	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute) // fires
	h.mustRun(time.Hour)       // reminder
	h.findings = nil
	h.mustRun(time.Minute)     // gone, waits for ResolveAfter
	h.mustRun(5 * time.Minute) // resolved

	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:", "new:/reminders:a/resolved:", "new:/reminders:/resolved:a")
	wantNotifications(t, second)
	h.wantMuted("second", true)
}

func TestSwitchingOnReportsFiringProblemsAsNew(t *testing.T) {
	h, second := heardByBoth(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.mute("second")
	h.mustRun(time.Hour) // the first target gets its reminder, the second nothing

	h.requestUnmute("second")
	h.mustRun(30 * time.Minute)
	h.wantMuted("second", false)
	wantNotifications(t, second, "new:a/reminders:/resolved:", "new:a/reminders:/resolved:")

	h.mustRun(30 * time.Minute) // the reminder clock runs from the new message
	wantNotifications(t, second, "new:a/reminders:/resolved:", "new:a/reminders:/resolved:")
	h.mustRun(30 * time.Minute)
	wantNotifications(t, second, "new:a/reminders:/resolved:", "new:a/reminders:/resolved:", "new:/reminders:a/resolved:")
	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:", "new:/reminders:a/resolved:", "new:/reminders:a/resolved:")
}

func TestProblemThatEndedWhileMutedStaysSilent(t *testing.T) {
	h, second := heardByBoth(t)
	h.mute("second")
	h.findings = nil
	h.mustRun(time.Minute) // resolved

	h.requestUnmute("second")
	h.mustRun(time.Minute)
	h.mustRun(time.Minute)
	h.wantMuted("second", false)
	wantNotifications(t, second, "new:a/reminders:/resolved:")
	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:", "new:/reminders:/resolved:a")
}

// The switch-on is finished after saveState, so a problem that is resolved in
// that same run counts as ended while the target was off.
func TestProblemEndingInTheRunOfSwitchingOnStaysSilent(t *testing.T) {
	h, second := heardByBoth(t)
	h.engine.ResolveAfter = 0
	h.mute("second")
	h.requestUnmute("second")
	h.findings = nil
	h.mustRun(time.Minute)

	h.wantMuted("second", false)
	wantNotifications(t, second, "new:a/reminders:/resolved:")
	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:", "new:/reminders:/resolved:a")
}

// With RESOLVE_AFTER the problem is only gone when the target is switched on,
// and resolved later. It ended while the target was off, as at the end of a
// downtime: its end stays silent, and if it comes back it is new.
func TestProblemGoneWhileMutedAndResolvedAfterSwitchingOnStaysSilent(t *testing.T) {
	h, second := heardByBoth(t)
	h.engine.ResolveAfter = 5 * time.Minute
	h.mute("second")
	h.findings = nil
	h.requestUnmute("second")
	h.mustRun(time.Minute) // gone, waits for ResolveAfter; switched on
	h.wantMuted("second", false)
	h.mustRun(5 * time.Minute) // resolved
	wantNotifications(t, second, "new:a/reminders:/resolved:")
	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:", "new:/reminders:/resolved:a")

	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(time.Minute)
	h.mustRun(2 * time.Minute) // fires again
	wantNotifications(t, second, "new:a/reminders:/resolved:", "new:a/reminders:/resolved:")
}

// The same, but the problem comes back before RESOLVE_AFTER is over: it is
// reported to the target as new, like after a downtime.
func TestProblemGoneWhileMutedThatComesBackIsNew(t *testing.T) {
	h, second := heardByBoth(t)
	h.engine.ResolveAfter = 5 * time.Minute
	h.mute("second")
	h.findings = nil
	h.requestUnmute("second")
	h.mustRun(time.Minute) // gone; switched on
	wantNotifications(t, second, "new:a/reminders:/resolved:")

	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(time.Minute) // back before ResolveAfter
	wantNotifications(t, second, "new:a/reminders:/resolved:", "new:a/reminders:/resolved:")
	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:")
}

func TestAcknowledgedProblemStaysSilentAfterSwitchingOn(t *testing.T) {
	h, second := heardByBoth(t)
	h.acknowledge("a")
	h.mute("second")
	h.requestUnmute("second")
	h.mustRun(time.Minute)
	h.wantMuted("second", false)
	wantNotifications(t, second, "new:a/reminders:/resolved:")

	h.findings = nil
	h.mustRun(time.Minute)
	wantNotifications(t, second, "new:a/reminders:/resolved:", "new:/reminders:/resolved:a")
}

func TestDowntimeWinsOverSwitchingOn(t *testing.T) {
	h, second := heardByBoth(t)
	h.setDowntime("a", time.Hour)
	h.mute("second")
	h.requestUnmute("second")
	h.mustRun(time.Minute)
	h.wantMuted("second", false)
	wantNotifications(t, second, "new:a/reminders:/resolved:")

	h.mustRun(time.Hour) // the downtime ends
	wantNotifications(t, second, "new:a/reminders:/resolved:", "new:a/reminders:/resolved:")
}

// A check that never runs in this process keeps its problems (KeepProblems), so
// its state is unknown in every run. Switching on must not wait for it.
func TestSwitchingOnDoesNotWaitForACheckThatIsSkippedForGood(t *testing.T) {
	h, second := heardByBoth(t)
	h.engine.SkippedChecks = []SkippedCheck{{Name: "certs", Reason: "no permission", KeepProblems: true}}
	h.mute("second")
	h.requestUnmute("second")
	h.mustRun(time.Minute)

	h.wantMuted("second", false)
	wantNotifications(t, second, "new:a/reminders:/resolved:", "new:a/reminders:/resolved:")
}

// A problem heard before the mute whose check state is unknown at switch-on
// may be gone, so it is left as it was: it keeps its marker and is not sent
// as new, neither in that run nor once the check runs again.
func TestSwitchingOnLeavesAProblemOfAnUnknownCheckAsItWas(t *testing.T) {
	h, second := heardByBoth(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.mute("second")
	h.requestUnmute("second")
	h.skipped = map[string]string{"fake": "kubelet stats unavailable"}
	h.mustRun(time.Minute)
	h.wantMuted("second", false)
	wantNotifications(t, second, "new:a/reminders:/resolved:")

	h.skipped = nil
	h.mustRun(time.Minute)
	wantNotifications(t, second, "new:a/reminders:/resolved:")
}

// A pvc-usage problem whose volume was not measured in this run (a node is
// down) may be gone, so it is left as it was. It does not hold back the
// switch-on: another problem that fires is reported as new in the same run.
func TestSwitchingOnLeavesAnUnmeasuredVolumeAsItWas(t *testing.T) {
	h, second := twoTargets(t)
	h.engine.CriticalRepeatInterval = time.Hour
	vol := finding("vol", checks.Critical)
	vol.Check = "pvc-usage"
	h.findings = []checks.Finding{vol, finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	wantNotifications(t, second, "new:a,vol/reminders:/resolved:")
	h.mute("second")
	h.requestUnmute("second")

	h.volumesPartial = true                                      // vol is not in the (empty) measurement,
	h.findings = []checks.Finding{finding("a", checks.Critical)} // so the check cannot report it
	h.mustRun(time.Minute)
	h.wantMuted("second", false)
	wantNotifications(t, second, "new:a,vol/reminders:/resolved:", "new:a/reminders:/resolved:")
}

// A run in which the findings could not be saved does not finish a switch-on:
// a problem that ended in that run would still count as firing, and the target
// would hear it as new and then as resolved. The next run finishes it.
func TestSwitchingOnWaitsForARunThatSavedItsFindings(t *testing.T) {
	h, second := heardByBoth(t)
	h.engine.ResolveAfter = 0
	vol := finding("vol", checks.Critical)
	vol.Check = "pvc-usage"
	h.mute("second")
	h.findings = []checks.Finding{finding("a", checks.Critical), vol}
	h.mustRun(time.Minute) // vol is new, not firing yet
	h.requestUnmute("second")

	// a ends, but saving fails: vol is not measured, so its open problem is
	// set aside, and inserting it again breaks the unique key. A real check
	// never reports an unmeasured volume; this only makes the save fail.
	h.findings = []checks.Finding{vol}
	h.volumesPartial = true
	if err := h.run(time.Minute); err == nil {
		t.Fatal("RunOnce = nil, want the save to fail")
	}
	h.wantMuted("second", true)

	h.findings = nil
	h.volumesPartial = false
	h.mustRun(time.Minute)
	h.wantMuted("second", false)
	wantNotifications(t, second, "new:a/reminders:/resolved:")
}

// A problem the target heard, whose targets annotation then no longer names
// it, is not told again at switch-on. As at the end of a downtime, it keeps
// "heard the start" and hears the end, even if that end came while it was off
// (gone, waiting for RESOLVE_AFTER).
func TestHeardButNoLongerRoutedProblemThatEndedWhileMutedIsStillResolvedToTheTarget(t *testing.T) {
	h, second := heardByBoth(t)
	h.engine.ResolveAfter = 5 * time.Minute
	h.findings = []checks.Finding{withTargets(finding("a", checks.Critical), "fake")}
	h.mustRun(time.Minute)
	h.mute("second")
	h.findings = nil
	h.mustRun(time.Minute) // gone, waits for ResolveAfter
	h.requestUnmute("second")
	h.mustRun(time.Minute)
	h.wantMuted("second", false)
	wantNotifications(t, second, "new:a/reminders:/resolved:")

	h.mustRun(5 * time.Minute) // resolved
	wantNotifications(t, second, "new:a/reminders:/resolved:", "new:/reminders:/resolved:a")
}

// The failed message is held back by a downtime, so without the explicit
// forgetting the error would stay (see settleFailed).
func TestSwitchingOnForgetsTheLastError(t *testing.T) {
	h, second := twoTargets(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	second.fail = true
	_ = h.run(2 * time.Minute) // the first message to second fails
	if status := h.engine.Status().Targets[1]; status.LastError == "" {
		t.Fatalf("second = %+v, want its error", status)
	}

	h.setDowntime("a", time.Hour)
	h.mute("second")
	h.requestUnmute("second")
	h.mustRun(time.Minute)
	h.wantMuted("second", false)
	if status := h.engine.Status().Targets[1]; status.LastError != "" || !status.LastErrorAt.IsZero() {
		t.Errorf("second = %+v, want no error after switching on", status)
	}
}

func TestMuteSurvivesARestart(t *testing.T) {
	h, second := twoTargets(t)
	h.mute("second")
	h.restart()

	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:")
	wantNotifications(t, second)
}

func TestSwitchingOnIsLoggedWithTheDescription(t *testing.T) {
	h, _ := heardByBoth(t)
	h.mute("second")
	h.requestUnmute("second")
	h.mustRun(time.Minute)
	if want := `msg="notification target switched on" target="second (test)"`; !strings.Contains(h.logs.String(), want) {
		t.Errorf("logs = %s, want %s", h.logs, want)
	}
}
