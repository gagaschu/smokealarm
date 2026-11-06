package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase 14: a target can be limited to critical problems, and the targets
// annotation can send a problem to some targets only. These tests use two
// targets: h.notifier ("fake") and "second".

func withTargets(f checks.Finding, targets ...string) checks.Finding {
	f.Targets = targets
	return f
}

func TestCriticalOnlyTargetHearsOnlyCriticalProblems(t *testing.T) {
	h, second := twoTargets(t)
	h.engine.MinSeverity = map[string]checks.Severity{"second": checks.Critical}
	h.findings = []checks.Finding{finding("a", checks.Critical), finding("b", checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.findings = nil
	h.mustRun(time.Minute)

	wantNotifications(t, h.notifier, "new:a,b/reminders:/resolved:", "new:/reminders:/resolved:a,b")
	wantNotifications(t, second, "new:a/reminders:/resolved:", "new:/reminders:/resolved:a")
}

func TestCriticalOnlyTargetHearsAnEscalationAndItsEnd(t *testing.T) {
	h, second := twoTargets(t)
	h.engine.MinSeverity = map[string]checks.Severity{"second": checks.Critical}
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	wantNotifications(t, second)

	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(time.Minute)
	wantNotifications(t, second) // not before it has lasted ALERT_AFTER
	h.mustRun(2 * time.Minute)
	wantNotifications(t, second, "new:vol/reminders:/resolved:")

	// Better again, then gone: it heard the start, so it hears the end.
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(time.Minute)
	h.findings = nil
	h.mustRun(time.Minute)
	wantNotifications(t, second, "new:vol/reminders:/resolved:", "new:/reminders:/resolved:vol")
	wantNotifications(t, h.notifier, "new:vol/reminders:/resolved:", "new:vol/reminders:/resolved:",
		"new:/reminders:/resolved:vol")
}

func TestTargetsAnnotationLimitsTheTargets(t *testing.T) {
	h, second := twoTargets(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.findings = []checks.Finding{withTargets(finding("a", checks.Critical), "second"), finding("b", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.mustRun(time.Hour) // reminders follow the annotation too
	h.findings = nil
	h.mustRun(time.Minute)

	wantNotifications(t, h.notifier, "new:b/reminders:/resolved:", "new:/reminders:b/resolved:",
		"new:/reminders:/resolved:b")
	wantNotifications(t, second, "new:a,b/reminders:/resolved:", "new:/reminders:a,b/resolved:",
		"new:/reminders:/resolved:a,b")
}

func TestChangedTargetsAnnotationAppliesToTheOpenProblem(t *testing.T) {
	h, second := twoTargets(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.findings = []checks.Finding{withTargets(finding("a", checks.Critical), "fake")}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)

	// Moved to the second target: it hears the problem as new, the first one
	// gets no more reminders, but hears the end, because it heard the start.
	h.findings = []checks.Finding{withTargets(finding("a", checks.Critical), "second")}
	h.mustRun(time.Minute)
	h.mustRun(time.Hour)
	h.findings = nil
	h.mustRun(time.Minute)

	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:", "new:/reminders:/resolved:a")
	wantNotifications(t, second, "new:a/reminders:/resolved:", "new:/reminders:a/resolved:",
		"new:/reminders:/resolved:a")
}

func TestTargetsAnnotationWithoutConfiguredTargetGoesEverywhere(t *testing.T) {
	h, second := twoTargets(t)
	h.findings = []checks.Finding{withTargets(finding("a", checks.Critical), "slak")}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.mustRun(time.Minute)

	for _, n := range []*fakeNotifier{h.notifier, second} {
		wantNotifications(t, n, "new:a/reminders:/resolved:")
	}
	if got := strings.Count(h.logs.String(), `which is not a configured notification target`); got != 1 {
		t.Errorf("warning logged %d times, want once:\n%s", got, h.logs)
	}
}

func TestUnknownNameNextToAConfiguredTargetIsIgnored(t *testing.T) {
	h, second := twoTargets(t)
	h.findings = []checks.Finding{withTargets(finding("a", checks.Critical), "slak", "second")}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)

	wantNotifications(t, h.notifier)
	wantNotifications(t, second, "new:a/reminders:/resolved:")
	if !strings.Contains(h.logs.String(), `\"slak\"`) {
		t.Errorf("want a warning about slak, log:\n%s", h.logs)
	}
}

// Acknowledged before the first message went out: the end is reported, but
// only where the problem is meant to go.
func TestAcknowledgedBeforeFirstMessageEndsOnlyWhereRouted(t *testing.T) {
	h, second := twoTargets(t)
	h.findings = []checks.Finding{withTargets(finding("a", checks.Critical), "fake")}
	h.mustRun(0)
	h.notifier.fail = true
	_ = h.run(2 * time.Minute)
	h.notifier.fail = false
	h.acknowledge("a")
	h.findings = nil
	h.mustRun(time.Minute)

	wantNotifications(t, h.notifier, "new:/reminders:/resolved:a")
	wantNotifications(t, second)
}

func TestNamespaceAnnotationsRouteAndRaiseTheSeverity(t *testing.T) {
	h, second := twoTargets(t)
	h.engine.MinSeverity = map[string]checks.Severity{"second": checks.Critical}
	h.namespaces = []*corev1.Namespace{{ObjectMeta: metav1.ObjectMeta{Name: "media", Annotations: map[string]string{
		checks.TargetsAnnotation:  "second",
		checks.SeverityAnnotation: "critical",
	}}}}
	h.findings = []checks.Finding{finding("a", checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)

	wantNotifications(t, h.notifier)
	wantNotifications(t, second, "new:a/reminders:/resolved:")
	if got := second.sent[0].New[0]; got.Severity != checks.Critical || len(got.Targets) != 1 {
		t.Errorf("alert = %+v, want critical for target second", got.Finding)
	}
}

func TestInvalidSeverityAnnotationIsLoggedOnce(t *testing.T) {
	h := newHarness(t)
	h.namespaces = []*corev1.Namespace{{ObjectMeta: metav1.ObjectMeta{Name: "media", Annotations: map[string]string{
		checks.SeverityAnnotation: "loud",
	}}}}
	h.findings = []checks.Finding{finding("a", checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)

	if got := strings.Count(h.logs.String(), "annotation ignored"); got != 1 {
		t.Errorf("warning logged %d times, want once:\n%s", got, h.logs)
	}
	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:")
}

func TestStatusShowsCriticalOnlyTargets(t *testing.T) {
	h, _ := twoTargets(t)
	h.engine.MinSeverity = map[string]checks.Severity{"second": checks.Critical}
	targets := h.engine.Status().Targets
	if targets[0].CriticalOnly || !targets[1].CriticalOnly {
		t.Errorf("targets = %+v, want only the second one critical only", targets)
	}
}

// Review finding 1: an escalation must not take the end away from a target
// that heard the start but is no longer meant for the problem.
func TestEscalationKeepsTheEndForATargetTheProblemMovedAwayFrom(t *testing.T) {
	h, second := twoTargets(t)
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute) // both hear the warning

	moved := escalating(checks.Critical)
	moved.Targets = []string{"second"}
	h.findings = []checks.Finding{moved}
	h.mustRun(time.Minute) // now critical, and only for the second target
	h.mustRun(2 * time.Minute)
	h.findings = nil
	h.mustRun(time.Minute)

	wantNotifications(t, h.notifier, "new:vol/reminders:/resolved:", "new:/reminders:/resolved:vol")
	wantNotifications(t, second, "new:vol/reminders:/resolved:", "new:vol/reminders:/resolved:",
		"new:/reminders:/resolved:vol")
}

// Review finding 2: the end of a downtime must not take the end away from a
// critical-only target while the problem is only a warning.
func TestDowntimeEndKeepsTheEndForACriticalOnlyTarget(t *testing.T) {
	h, second := twoTargets(t)
	h.engine.MinSeverity = map[string]checks.Severity{"second": checks.Critical}
	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute) // both hear it as critical
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(time.Minute)
	h.setDowntime("vol", time.Hour)
	h.mustRun(2 * time.Hour) // the downtime ends while it is a warning
	h.findings = nil
	h.mustRun(time.Minute)

	wantNotifications(t, second, "new:vol/reminders:/resolved:", "new:/reminders:/resolved:vol")
	wantNotifications(t, h.notifier, "new:vol/reminders:/resolved:", "new:vol/reminders:/resolved:",
		"new:/reminders:/resolved:vol")
}

// The same for a targets annotation that moved away during the downtime.
func TestDowntimeEndKeepsTheEndForATargetTheProblemMovedAwayFrom(t *testing.T) {
	h, second := twoTargets(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.setDowntime("a", time.Hour)
	h.findings = []checks.Finding{withTargets(finding("a", checks.Critical), "second")}
	h.mustRun(2 * time.Hour)
	h.findings = nil
	h.mustRun(time.Minute)

	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:", "new:/reminders:/resolved:a")
	wantNotifications(t, second, "new:a/reminders:/resolved:", "new:a/reminders:/resolved:",
		"new:/reminders:/resolved:a")
}

// Review finding 3: a warning that reaches no target is allowed, but logged.
func TestWarningThatReachesNoTargetIsLoggedOnce(t *testing.T) {
	h, _ := twoTargets(t)
	h.engine.MinSeverity = map[string]checks.Severity{"second": checks.Critical}
	h.namespaces = []*corev1.Namespace{{ObjectMeta: metav1.ObjectMeta{Name: "media", Annotations: map[string]string{
		checks.TargetsAnnotation: "second",
	}}}}
	h.findings = []checks.Finding{finding("a", checks.Warning), finding("b", checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.mustRun(time.Minute)

	wantNotifications(t, h.notifier)
	if got := strings.Count(h.logs.String(), `msg="problems reach no notification target"`); got != 1 {
		t.Errorf("logged %d times, want once for the namespace:\n%s", got, h.logs)
	}
	if !strings.Contains(h.logs.String(), "Namespace media") {
		t.Errorf("want the namespace named as the source:\n%s", h.logs)
	}
}

// Review finding 5: a warning names where the annotation is, once per process,
// also when the problem goes away for a run and comes back.
func TestAnnotationWarningNamesTheSourceOnce(t *testing.T) {
	h := newHarness(t)
	h.namespaces = []*corev1.Namespace{{ObjectMeta: metav1.ObjectMeta{Name: "media", Annotations: map[string]string{
		checks.TargetsAnnotation: "slak",
	}}}}
	h.findings = []checks.Finding{finding("a", checks.Critical), finding("b", checks.Critical)}
	h.mustRun(0)
	h.findings = nil
	h.mustRun(time.Minute)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(time.Minute)

	logs := h.logs.String()
	if got := strings.Count(logs, "annotation ignored"); got != 1 {
		t.Errorf("logged %d times, want once:\n%s", got, logs)
	}
	if !strings.Contains(logs, "Namespace media") || strings.Contains(logs, "Pod media/a: ") {
		t.Errorf("want the namespace as the source, not the pod:\n%s", logs)
	}
}

// Review finding 4: the web UI shows who really hears about a problem.
func TestAudienceIsWhatTheSendingDoes(t *testing.T) {
	h, _ := twoTargets(t)
	h.engine.MinSeverity = map[string]checks.Severity{"second": checks.Critical}
	tests := []struct {
		name    string
		finding checks.Finding
		want    string
		wantAll bool
	}{
		{"critical, no annotation", finding("a", checks.Critical), "fake (test),second (test)", true},
		{"warning, critical-only target left out", finding("a", checks.Warning), "fake (test)", false},
		{"annotation", withTargets(finding("a", checks.Critical), "second"), "second (test)", false},
		{"warning for a critical-only target", withTargets(finding("a", checks.Warning), "second"), "", false},
		{"typo goes everywhere", withTargets(finding("a", checks.Critical), "slak"), "fake (test),second (test)", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			targets, all := h.engine.Audience(store.Record{Finding: tt.finding})
			if strings.Join(targets, ",") != tt.want || all != tt.wantAll {
				t.Errorf("Audience = %v, %v, want %q, %v", targets, all, tt.want, tt.wantAll)
			}
		})
	}
}

// Issue #26 finding 2: after a downtime that ended while the problem was meant
// for the second target only, the first target keeps its marker, and the
// summary of the problem is empty while the second target fails. An escalation
// must still reach the first target once the annotation is gone.
func TestEscalationReachesATargetWhoseMarkerOutlivedTheSummary(t *testing.T) {
	h, second := twoTargets(t)
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute) // both hear the warning

	h.findings = []checks.Finding{withTargets(escalating(checks.Warning), "second")}
	h.setDowntime("vol", time.Hour)
	h.mustRun(time.Minute)
	second.fail = true
	_ = h.run(time.Hour) // the downtime ends; telling the second target fails

	h.findings = []checks.Finding{escalating(checks.Critical)}
	_ = h.run(time.Minute) // critical, and for every target again
	_ = h.run(2 * time.Minute)

	wantNotifications(t, h.notifier, "new:vol/reminders:/resolved:", "new:vol/reminders:/resolved:")
}
