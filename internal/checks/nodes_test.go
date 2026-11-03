package checks

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newNode(conditions ...corev1.NodeCondition) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "pi-1"},
		Status:     corev1.NodeStatus{Conditions: conditions},
	}
}

func nodeCond(t corev1.NodeConditionType, s corev1.ConditionStatus) corev1.NodeCondition {
	return corev1.NodeCondition{
		Type:               t,
		Status:             s,
		LastTransitionTime: metav1.NewTime(testNow.Add(-10 * time.Minute)),
	}
}

func TestNodeNotReady(t *testing.T) {
	tests := []struct {
		name string
		node *corev1.Node
		want []string
	}{
		{"ready", newNode(nodeCond(corev1.NodeReady, corev1.ConditionTrue)), nil},
		{"not ready", newNode(nodeCond(corev1.NodeReady, corev1.ConditionFalse)), []string{"node-not-ready/Node//pi-1/NotReady"}},
		{"unknown", newNode(nodeCond(corev1.NodeReady, corev1.ConditionUnknown)), []string{"node-not-ready/Node//pi-1/NotReady"}},
		{"no ready condition yet", newNode(), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &Snapshot{Now: testNow, Nodes: []*corev1.Node{tt.node}}
			wantFindings(t, NodeNotReady(snap, testOptions()), tt.want, Critical)
		})
	}
}

func TestNodeNotReadyMessage(t *testing.T) {
	cond := nodeCond(corev1.NodeReady, corev1.ConditionUnknown)
	cond.Message = "Kubelet stopped posting node status."
	snap := &Snapshot{Now: testNow, Nodes: []*corev1.Node{newNode(cond)}}

	got := NodeNotReady(snap, testOptions())
	want := "node is not ready for 10m0s (status Unknown): Kubelet stopped posting node status."
	if len(got) != 1 || got[0].Message != want {
		t.Errorf("got %+v, want message %q", got, want)
	}
}

func TestNodePressure(t *testing.T) {
	tests := []struct {
		name string
		node *corev1.Node
		want []string
	}{
		{"no pressure", newNode(
			nodeCond(corev1.NodeMemoryPressure, corev1.ConditionFalse),
			nodeCond(corev1.NodeDiskPressure, corev1.ConditionFalse),
			nodeCond(corev1.NodePIDPressure, corev1.ConditionFalse)), nil},
		{"memory", newNode(nodeCond(corev1.NodeMemoryPressure, corev1.ConditionTrue)),
			[]string{"node-pressure/Node//pi-1/MemoryPressure"}},
		{"disk", newNode(nodeCond(corev1.NodeDiskPressure, corev1.ConditionTrue)),
			[]string{"node-pressure/Node//pi-1/DiskPressure"}},
		{"pid", newNode(nodeCond(corev1.NodePIDPressure, corev1.ConditionTrue)),
			[]string{"node-pressure/Node//pi-1/PIDPressure"}},
		{"memory and disk are separate findings", newNode(
			nodeCond(corev1.NodeMemoryPressure, corev1.ConditionTrue),
			nodeCond(corev1.NodeDiskPressure, corev1.ConditionTrue)),
			[]string{"node-pressure/Node//pi-1/MemoryPressure", "node-pressure/Node//pi-1/DiskPressure"}},
		{"unknown status is not pressure", newNode(nodeCond(corev1.NodeMemoryPressure, corev1.ConditionUnknown)), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &Snapshot{Now: testNow, Nodes: []*corev1.Node{tt.node}}
			wantFindings(t, NodePressure(snap, testOptions()), tt.want, Warning)
		})
	}
}

func TestNodeChecksRespectIgnore(t *testing.T) {
	node := newNode(
		nodeCond(corev1.NodeReady, corev1.ConditionFalse),
		nodeCond(corev1.NodeMemoryPressure, corev1.ConditionTrue))
	snap := &Snapshot{Now: testNow, Nodes: []*corev1.Node{node}}

	if len(NodeNotReady(snap, testOptions())) != 1 || len(NodePressure(snap, testOptions())) != 1 {
		t.Fatal("expected one finding from each check before ignoring")
	}

	node.Annotations = map[string]string{IgnoreAnnotation: "true"}
	if got := NodeNotReady(snap, testOptions()); len(got) != 0 {
		t.Errorf("NodeNotReady reported %+v for an ignored node", got)
	}
	if got := NodePressure(snap, testOptions()); len(got) != 0 {
		t.Errorf("NodePressure reported %+v for an ignored node", got)
	}
}
