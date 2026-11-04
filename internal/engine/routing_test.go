package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
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
