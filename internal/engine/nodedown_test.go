package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/notify"
	"github.com/gagaschu/smokealarm/internal/store"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Phase 15: a node that is down is the alarm; the problems it causes are held
// back while it is down.

// nodeHarness has the node pi-2 with the pods "a" and "b" on it, and "c" on
// pi-1. The node starts ready.
func nodeHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.nodes = []*corev1.Node{readyNode("pi-1", corev1.ConditionTrue), readyNode("pi-2", corev1.ConditionTrue)}
	h.pods = []*corev1.Pod{podOn("a", "pi-2"), podOn("b", "pi-2"), podOn("c", "pi-1"), podOn("waiting", "")}
	h.enableNodeCheck()
	return h
}

// enableNodeCheck adds node-not-ready to the checks that run. Consequences are
// held back only while it runs; the fake check still returns all findings.
func (h *harness) enableNodeCheck() {
	h.engine.Checks = append(h.engine.Checks, checks.Check{
		Name: checks.CheckNodeNotReady,
		Run:  func(*checks.Snapshot, checks.Options) []checks.Finding { return nil },
	})
}

func readyNode(name string, status corev1.ConditionStatus) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}}},
	}
}

func podOn(name, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: name},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

// setNode changes the Ready condition of a node in the snapshot.
func (h *harness) setNode(name string, status corev1.ConditionStatus) {
	for i, n := range h.nodes {
		if n.Name == name {
			h.nodes[i] = readyNode(name, status)
		}
	}
}

// nodeDown is the NotReady finding of a node, as the check reports it.
func nodeDown(name string) checks.Finding {
	return checks.Finding{
		Key: "node-down-" + name, Check: checks.CheckNodeNotReady, Severity: checks.Critical,
		Object: checks.ObjectRef{Kind: checks.KindNode, Name: name},
		Reason: "NotReady", Message: "node is not ready",
	}
}

func affected(alert notify.Alert) string {
	var keys []string
	for _, a := range alert.Affected {
		keys = append(keys, a.Object.Name+":"+a.Reason)
	}
	return strings.Join(keys, ",")
}

func (h *harness) record(key string) store.Record {
	h.t.Helper()
	open, err := h.engine.Store.OpenRecords(context.Background())
	if err != nil {
		h.t.Fatalf("OpenRecords: %v", err)
	}
	for _, r := range open {
		if r.Key == key {
			return r
		}
	}
	h.t.Fatalf("no open record %s", key)
	return store.Record{}
}

func TestNodeDownHoldsBackItsConsequences(t *testing.T) {
	h := nodeHarness(t)
	h.setNode("pi-2", corev1.ConditionUnknown)
	h.findings = []checks.Finding{nodeDown("pi-2"), finding("a", checks.Critical), finding("c", checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	wantNotifications(t, h.notifier, "new:node-down-pi-2,c/reminders:/resolved:")
	if got := affected(h.notifier.sent[0].New[0]); got != "a:Broken" {
		t.Errorf("affected = %q, want a:Broken", got)
	}

	// Long after ALERT_AFTER it still waits for the node.
	h.mustRun(time.Hour)
	wantNotifications(t, h.notifier, "new:node-down-pi-2,c/reminders:/resolved:")
	if r := h.record("a"); r.State != store.StatePending || r.CausedBy != "pi-2" {
		t.Errorf("a: state %s, caused by %q; want pending, pi-2", r.State, r.CausedBy)
	}
}

func TestLateConsequenceSendsNothingAndShowsInTheReminder(t *testing.T) {
	h := nodeHarness(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.setNode("pi-2", corev1.ConditionUnknown)
	h.findings = []checks.Finding{nodeDown("pi-2")}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)

	// The eviction, minutes later.
	h.findings = []checks.Finding{nodeDown("pi-2"), finding("a", checks.Critical), finding("b", checks.Warning)}
	h.mustRun(5 * time.Minute)
	h.mustRun(5 * time.Minute)
	wantNotifications(t, h.notifier, "new:node-down-pi-2/reminders:/resolved:")

	h.mustRun(time.Hour)
	wantNotifications(t, h.notifier, "new:node-down-pi-2/reminders:/resolved:", "new:/reminders:node-down-pi-2/resolved:")
	if got := affected(h.notifier.sent[1].Reminders[0]); got != "a:Broken,b:Broken" {
		t.Errorf("affected = %q, want a and b", got)
	}
}

func TestNodeBackAndConsequencesRecoverSendsOnlyTheEnd(t *testing.T) {
	h := nodeHarness(t)
	h.setNode("pi-2", corev1.ConditionUnknown)
	h.findings = []checks.Finding{nodeDown("pi-2"), finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.mustRun(10 * time.Minute)

	// Back; the pod needs a minute to become ready.
	h.setNode("pi-2", corev1.ConditionTrue)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(time.Minute)
	h.mustRun(time.Minute)
	h.findings = nil
	h.mustRun(30 * time.Second)

	wantNotifications(t, h.notifier, "new:node-down-pi-2/reminders:/resolved:", "new:/reminders:/resolved:node-down-pi-2")
	if n := h.countRows(""); n != 1 {
		t.Errorf("%d rows, want only the node in the history", n)
	}
}

func TestConsequenceThatLastsAfterTheNodeIsBackIsReported(t *testing.T) {
	h := nodeHarness(t)
	h.setNode("pi-2", corev1.ConditionUnknown)
	h.findings = []checks.Finding{nodeDown("pi-2"), finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.mustRun(10 * time.Minute)
	firstSeen := h.record("a").FirstSeen

	h.setNode("pi-2", corev1.ConditionTrue)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(time.Minute) // node resolved; the clock of a starts again
	h.mustRun(time.Minute + 59*time.Second)
	wantNotifications(t, h.notifier, "new:node-down-pi-2/reminders:/resolved:", "new:/reminders:/resolved:node-down-pi-2")

	h.mustRun(time.Second)
	wantNotifications(t, h.notifier, "new:node-down-pi-2/reminders:/resolved:", "new:/reminders:/resolved:node-down-pi-2",
		"new:a/reminders:/resolved:")
	if got := h.notifier.sent[2].New[0].Since; !got.Equal(firstSeen) {
		t.Errorf("since = %v, want the real start %v", got, firstSeen)
	}
}

// Review of phase 15: a problem that the down node no longer explains is a
// problem of its own, even while the node is still down.
func TestConsequenceThatNoLongerDependsOnTheNodeIsReported(t *testing.T) {
	h := nodeHarness(t)
	h.setNode("pi-2", corev1.ConditionUnknown)
	h.findings = []checks.Finding{nodeDown("pi-2"), finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)

	h.pods = []*corev1.Pod{podOn("a", "pi-1")}
	h.mustRun(time.Minute) // released: the clock starts again
	h.mustRun(time.Minute + 59*time.Second)
	wantNotifications(t, h.notifier, "new:node-down-pi-2/reminders:/resolved:")
	h.mustRun(time.Second)
	wantNotifications(t, h.notifier, "new:node-down-pi-2/reminders:/resolved:", "new:a/reminders:/resolved:")
}

// Review of phase 15: without node-not-ready nobody would hear about the node,
// so nothing is held back.
func TestNothingIsHeldBackWithoutTheNodeCheck(t *testing.T) {
	h := newHarness(t) // only the fake check runs
	h.nodes = []*corev1.Node{readyNode("pi-2", corev1.ConditionUnknown)}
	h.pods = []*corev1.Pod{podOn("a", "pi-2")}
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:")
}

func TestHeldBackProblemsAreReleasedWhenTheNodeCheckIsSkipped(t *testing.T) {
	h := nodeHarness(t)
	h.setNode("pi-2", corev1.ConditionUnknown)
	h.findings = []checks.Finding{nodeDown("pi-2"), finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)

	h.skipped = map[string]string{checks.CheckNodeNotReady: "no data"}
	h.mustRun(time.Minute)
	h.mustRun(2 * time.Minute)
	wantNotifications(t, h.notifier, "new:node-down-pi-2/reminders:/resolved:", "new:a/reminders:/resolved:")
}

func TestIgnoredNodeStillHoldsBackItsConsequences(t *testing.T) {
	h := nodeHarness(t)
	h.setNode("pi-2", corev1.ConditionFalse)
	// The check reports nothing for an ignored node.
	h.findings = []checks.Finding{finding("a", checks.Critical), finding("b", checks.Warning)}
	h.mustRun(0)
	h.mustRun(time.Hour)
	wantNotifications(t, h.notifier)
}

func TestPodWithoutANodeIsReportedAsUsual(t *testing.T) {
	h := nodeHarness(t)
	h.setNode("pi-2", corev1.ConditionUnknown)
	h.findings = []checks.Finding{nodeDown("pi-2"), finding("waiting", checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	wantNotifications(t, h.notifier, "new:node-down-pi-2,waiting/reminders:/resolved:")
}

func TestProblemFiringBeforeTheNodeWentDownIsNotTouched(t *testing.T) {
	h := nodeHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)

	h.setNode("pi-2", corev1.ConditionUnknown)
	h.findings = []checks.Finding{nodeDown("pi-2"), finding("a", checks.Critical)}
	h.mustRun(time.Minute)
	h.mustRun(2 * time.Minute)
	h.setNode("pi-2", corev1.ConditionTrue)
	h.findings = nil
	h.mustRun(time.Minute)

	wantNotifications(t, h.notifier, "new:a/reminders:/resolved:", "new:node-down-pi-2/reminders:/resolved:",
		"new:/reminders:/resolved:node-down-pi-2,a")
	if got := affected(h.notifier.sent[1].New[0]); got != "" {
		t.Errorf("affected = %q, want none: a was reported on its own", got)
	}
}

func TestHeldBackProblemFiresNotAtOnceWithAlertAfterZero(t *testing.T) {
	h := nodeHarness(t)
	h.engine.AlertAfter = 0
	h.setNode("pi-2", corev1.ConditionUnknown)
	h.findings = []checks.Finding{nodeDown("pi-2"), finding("a", checks.Critical)}
	h.mustRun(0)
	wantNotifications(t, h.notifier, "new:node-down-pi-2/reminders:/resolved:")
}

func TestAffectedListFollowsTheRouting(t *testing.T) {
	h, second := twoTargets(t)
	h.enableNodeCheck()
	h.nodes = []*corev1.Node{readyNode("pi-2", corev1.ConditionUnknown)}
	h.pods = []*corev1.Pod{podOn("a", "pi-2"), podOn("b", "pi-2")}
	h.engine.MinSeverity = map[string]checks.Severity{"second": checks.Critical}
	h.findings = []checks.Finding{
		nodeDown("pi-2"),
		finding("a", checks.Warning),
		withTargets(finding("b", checks.Critical), "second"),
	}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)

	if got := affected(h.notifier.sent[0].New[0]); got != "a:Broken" {
		t.Errorf("fake: affected = %q, want only a", got)
	}
	if got := affected(second.sent[0].New[0]); got != "b:Broken" {
		t.Errorf("second: affected = %q, want only b", got)
	}
}

func TestNodeDownComesFirst(t *testing.T) {
	alerts := []notify.Alert{
		{Finding: finding("a", checks.Critical)},
		{Finding: nodeDown("pi-2")},
		{Finding: finding("b", checks.Warning)},
	}
	sortAlerts(alerts)
	if got := alertKeys(alerts); got != "node-down-pi-2,a,b" {
		t.Errorf("order = %s, want the node first", got)
	}
}
