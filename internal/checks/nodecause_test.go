package checks

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func namedNode(name string, ready corev1.ConditionStatus, annotations map[string]string) *corev1.Node {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations}}
	if ready != "" {
		node.Status.Conditions = []corev1.NodeCondition{nodeCond(corev1.NodeReady, ready)}
	}
	return node
}

func podOn(namespace, name, node string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

func selectorFor(labels map[string]string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: labels}
}

func nodeCauseSnapshot() *Snapshot {
	app := map[string]string{"app": "sonarr"}
	return &Snapshot{
		Now: testNow,
		Nodes: []*corev1.Node{
			namedNode("pi-1", corev1.ConditionTrue, nil),
			namedNode("pi-2", corev1.ConditionUnknown, nil),
			namedNode("pi-test", corev1.ConditionFalse, map[string]string{IgnoreAnnotation: "true"}),
			namedNode("pi-new", "", nil),
		},
		Pods: []*corev1.Pod{
			podOn("media", "jellyfin", "pi-1", nil),
			podOn("media", "sonarr-a", "pi-1", app),
			podOn("media", "sonarr-b", "pi-2", app),
			podOn("media", "waiting", "", nil),
			podOn("media", "on-test", "pi-test", map[string]string{"app": "test"}),
			podOn("media", "on-new", "pi-new", nil),
			podOn("other", "radarr", "pi-2", map[string]string{"app": "radarr"}),
		},
		Deployments: []*appsv1.Deployment{
			{ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "sonarr"}, Spec: appsv1.DeploymentSpec{Selector: selectorFor(app)}},
			{ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "radarr"}, Spec: appsv1.DeploymentSpec{Selector: selectorFor(map[string]string{"app": "radarr"})}},
			{ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "everything"}, Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{}}},
			{
				// Two of three replicas missing, but only one pod on the down node.
				ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "big"},
				Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(3), Selector: selectorFor(app)},
				Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
			},
		},
		StatefulSets: []*appsv1.StatefulSet{
			{ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "db"}, Spec: appsv1.StatefulSetSpec{Selector: selectorFor(map[string]string{"app": "other"})}},
		},
		DaemonSets: []*appsv1.DaemonSet{
			{
				ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "agent"},
				Spec:       appsv1.DaemonSetSpec{Selector: selectorFor(map[string]string{"app": "test"})},
				Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, NumberAvailable: 2},
			},
			{
				// The rollout fails on the healthy nodes too: more is missing
				// than the down node explains.
				ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "broken-rollout"},
				Spec:       appsv1.DaemonSetSpec{Selector: selectorFor(map[string]string{"app": "test"})},
				Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, NumberAvailable: 0},
			},
		},
	}
}

func TestApplyNodeCauses(t *testing.T) {
	tests := []struct {
		name string
		ref  ObjectRef
		want string
	}{
		{"pod on a ready node", ObjectRef{KindPod, "media", "jellyfin"}, ""},
		{"pod on a down node", ObjectRef{KindPod, "other", "radarr"}, "pi-2"},
		{"pod without a node", ObjectRef{KindPod, "media", "waiting"}, ""},
		{"pod on an ignored down node", ObjectRef{KindPod, "media", "on-test"}, "pi-test"},
		{"pod on a node without a Ready condition", ObjectRef{KindPod, "media", "on-new"}, ""},
		{"pod not in the snapshot", ObjectRef{KindPod, "media", "gone"}, ""},
		{"deployment with one pod on a down node", ObjectRef{KindDeployment, "media", "sonarr"}, "pi-2"},
		{"pods of the selector in another namespace", ObjectRef{KindDeployment, "media", "radarr"}, ""},
		{"empty selector", ObjectRef{KindDeployment, "media", "everything"}, ""},
		{"selector matches nothing", ObjectRef{KindStatefulSet, "media", "db"}, ""},
		{"daemonset missing only its pod on the down node", ObjectRef{KindDaemonSet, "media", "agent"}, "pi-test"},
		{"daemonset missing more than the down node explains", ObjectRef{KindDaemonSet, "media", "broken-rollout"}, ""},
		{"deployment missing more than the down node explains", ObjectRef{KindDeployment, "media", "big"}, ""},
		{"workload not in the snapshot", ObjectRef{KindDeployment, "media", "gone"}, ""},
		{"the node itself", ObjectRef{KindNode, "", "pi-2"}, ""},
		{"other kinds never", ObjectRef{KindPVC, "other", "radarr"}, ""},
	}
	snap := nodeCauseSnapshot()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := newFinding("test", Warning, tt.ref, "Reason", "message")
			out := ApplyNodeCauses(snap, []Finding{in}, DownNodes(snap))
			if len(out) != 1 {
				t.Fatalf("got %d findings, want 1", len(out))
			}
			if out[0].CausedBy != tt.want {
				t.Errorf("CausedBy = %q, want %q", out[0].CausedBy, tt.want)
			}
			if out[0].Key != in.Key {
				t.Errorf("key changed from %q to %q", in.Key, out[0].Key)
			}
		})
	}
}

func TestApplyNodeCausesWithoutDownNodes(t *testing.T) {
	snap := &Snapshot{
		Nodes: []*corev1.Node{namedNode("pi-1", corev1.ConditionTrue, nil)},
		Pods:  []*corev1.Pod{podOn("media", "jellyfin", "pi-1", nil)},
	}
	in := []Finding{newFinding("test", Critical, ObjectRef{KindPod, "media", "jellyfin"}, "Reason", "message")}
	if out := ApplyNodeCauses(snap, in, DownNodes(snap)); out[0].CausedBy != "" {
		t.Errorf("CausedBy = %q, want none", out[0].CausedBy)
	}
}

func TestApplyNodeCausesHoldsNothingBackWithoutDownNodes(t *testing.T) {
	snap := nodeCauseSnapshot() // pi-2 is down, but the engine passes no down nodes
	in := []Finding{newFinding("test", Critical, ObjectRef{KindPod, "other", "radarr"}, "Reason", "message")}
	if out := ApplyNodeCauses(snap, in, nil); out[0].CausedBy != "" {
		t.Errorf("CausedBy = %q, want none", out[0].CausedBy)
	}
}

func TestDownNodes(t *testing.T) {
	got := DownNodes(nodeCauseSnapshot())
	if len(got) != 2 || !got["pi-2"] || !got["pi-test"] {
		t.Errorf("DownNodes = %v, want pi-2 and pi-test", got)
	}
}
