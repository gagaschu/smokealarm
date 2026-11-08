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

func TestSwitchingOnWaitsForUnknownChecks(t *testing.T) {
	h, second := heardByBoth(t)
	h.mute("second")
	h.requestUnmute("second")
	h.skipped = map[string]string{"fake": "kubelet stats unavailable"}
	h.mustRun(time.Minute)

	mutes, err := h.engine.Store.TargetMutes(context.Background())
	if err != nil {
		t.Fatalf("TargetMutes: %v", err)
	}
	if m, found := mutes["second"]; !found || m.UnmuteRequestedAt == nil {
		t.Fatalf("mutes = %+v, want second still off with a pending request", mutes)
	}
	wantNotifications(t, second, "new:a/reminders:/resolved:")

	h.skipped = nil
	h.mustRun(time.Minute)
	h.wantMuted("second", false)
	wantNotifications(t, second, "new:a/reminders:/resolved:", "new:a/reminders:/resolved:")
}

// A pvc-usage problem whose volume was not measured in this run (a node is
// down) may be gone, so switching on waits as for a check whose state is
// unknown.
func TestSwitchingOnWaitsForAnUnmeasuredVolume(t *testing.T) {
	h, second := twoTargets(t)
	vol := finding("vol", checks.Critical)
	vol.Check = "pvc-usage"
	h.findings = []checks.Finding{vol}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.mute("second")
	h.requestUnmute("second")

	h.volumesPartial = true // vol is not in the (empty) measurement,
	h.findings = nil        // so the check cannot report it
	h.mustRun(time.Minute)
	h.wantMuted("second", true)
	wantNotifications(t, second, "new:vol/reminders:/resolved:")

	h.volumesPartial = false
	h.findings = []checks.Finding{vol}
	h.mustRun(time.Minute)
	h.wantMuted("second", false)
	wantNotifications(t, second, "new:vol/reminders:/resolved:", "new:vol/reminders:/resolved:")
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
