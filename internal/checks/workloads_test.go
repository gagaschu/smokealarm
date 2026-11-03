package checks

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func int32Ptr(n int32) *int32 { return &n }

func workloadMeta() metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: "media", Name: "web"}
}

func TestWorkloadReplicas(t *testing.T) {
	tests := []struct {
		name        string
		desired     *int32
		available   int32
		wantReason  string
		wantSeverty Severity
		wantMessage string
	}{
		{"all available", int32Ptr(3), 3, "", "", ""},
		{"more available than desired (rollout)", int32Ptr(3), 4, "", "", ""},
		{"one missing", int32Ptr(3), 2, "ReplicasUnavailable", Warning, "2/3 replicas available"},
		{"none available", int32Ptr(3), 0, "NoReplicasAvailable", Critical, "0/3 replicas available"},
		{"scaled to zero", int32Ptr(0), 0, "", "", ""},
		{"replicas unset means one", nil, 0, "NoReplicasAvailable", Critical, "0/1 replicas available"},
		{"replicas unset, one available", nil, 1, "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &Snapshot{
				Deployments: []*appsv1.Deployment{{
					ObjectMeta: workloadMeta(),
					Spec:       appsv1.DeploymentSpec{Replicas: tt.desired},
					Status:     appsv1.DeploymentStatus{AvailableReplicas: tt.available},
				}},
				StatefulSets: []*appsv1.StatefulSet{{
					ObjectMeta: workloadMeta(),
					Spec:       appsv1.StatefulSetSpec{Replicas: tt.desired},
					Status:     appsv1.StatefulSetStatus{AvailableReplicas: tt.available},
				}},
			}
			got := WorkloadReplicas(snap, testOptions())

			if tt.wantReason == "" {
				if len(got) != 0 {
					t.Fatalf("got %+v, want no findings", got)
				}
				return
			}
			// One finding for the Deployment and one for the StatefulSet.
			if len(got) != 2 {
				t.Fatalf("got %d findings %+v, want 2", len(got), got)
			}
			for _, f := range got {
				if f.Reason != tt.wantReason || f.Severity != tt.wantSeverty || f.Message != tt.wantMessage {
					t.Errorf("got %+v, want reason %s, severity %s, message %q",
						f, tt.wantReason, tt.wantSeverty, tt.wantMessage)
				}
			}
			if got[0].Key != "workload-replicas/Deployment/media/web/replicas" {
				t.Errorf("unexpected key %q", got[0].Key)
			}
		})
	}
}

func TestWorkloadReplicasDaemonSet(t *testing.T) {
	tests := []struct {
		name      string
		desired   int32
		available int32
		want      string
	}{
		{"all nodes covered", 3, 3, ""},
		{"one node missing", 3, 2, "ReplicasUnavailable"},
		{"no pod available", 3, 0, "NoReplicasAvailable"},
		{"no nodes match", 0, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &Snapshot{DaemonSets: []*appsv1.DaemonSet{{
				ObjectMeta: workloadMeta(),
				Status:     appsv1.DaemonSetStatus{DesiredNumberScheduled: tt.desired, NumberAvailable: tt.available},
			}}}
			got := WorkloadReplicas(snap, testOptions())
			if tt.want == "" && len(got) != 0 {
				t.Fatalf("got %+v, want none", got)
			}
			if tt.want != "" && (len(got) != 1 || got[0].Reason != tt.want || got[0].Object.Kind != "DaemonSet") {
				t.Fatalf("got %+v, want one DaemonSet finding with reason %s", got, tt.want)
			}
		})
	}
}

func TestWorkloadReplicasRespectsIgnore(t *testing.T) {
	annotated := workloadMeta()
	annotated.Annotations = map[string]string{IgnoreAnnotation: "true"}

	ignoredNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "media", Annotations: map[string]string{IgnoreAnnotation: "true"},
	}}
	broken := func(meta metav1.ObjectMeta) []*appsv1.Deployment {
		return []*appsv1.Deployment{{ObjectMeta: meta, Spec: appsv1.DeploymentSpec{Replicas: int32Ptr(2)}}}
	}

	tests := []struct {
		name string
		snap *Snapshot
		opts Options
		want int
	}{
		{"not ignored", &Snapshot{Deployments: broken(workloadMeta())}, testOptions(), 1},
		{"object annotation", &Snapshot{Deployments: broken(annotated)}, testOptions(), 0},
		{"namespace annotation", &Snapshot{Deployments: broken(workloadMeta()), Namespaces: []*corev1.Namespace{ignoredNamespace}}, testOptions(), 0},
		{"namespace in env list", &Snapshot{Deployments: broken(workloadMeta())}, Options{IgnoredNamespaces: []string{"media"}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(WorkloadReplicas(tt.snap, tt.opts)); got != tt.want {
				t.Errorf("got %d findings, want %d", got, tt.want)
			}
		})
	}
}

// A workload that gets worse is still the same problem: the key must not
// change with the severity, otherwise the engine would see one problem
// disappear and a new one start.
func TestWorkloadReplicasKeyIsStableAcrossSeverities(t *testing.T) {
	keyFor := func(available int32) (string, Severity) {
		snap := &Snapshot{Deployments: []*appsv1.Deployment{{
			ObjectMeta: workloadMeta(),
			Spec:       appsv1.DeploymentSpec{Replicas: int32Ptr(3)},
			Status:     appsv1.DeploymentStatus{AvailableReplicas: available},
		}}}
		got := WorkloadReplicas(snap, testOptions())
		if len(got) != 1 {
			t.Fatalf("available %d: got %+v, want one finding", available, got)
		}
		return got[0].Key, got[0].Severity
	}

	warnKey, warnSeverity := keyFor(1)
	critKey, critSeverity := keyFor(0)
	if warnSeverity != Warning || critSeverity != Critical {
		t.Fatalf("severities = %s, %s; want warning, critical", warnSeverity, critSeverity)
	}
	if warnKey != critKey {
		t.Errorf("key changed with the severity: %q vs %q", warnKey, critKey)
	}
}
