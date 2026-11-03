package checks

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func appMeta() metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: "argocd", Name: "media"}
}

func TestArgoCDHealth(t *testing.T) {
	tests := []struct {
		health string
		want   []string
	}{
		{"Healthy", nil},
		{"Progressing", nil},
		{"Suspended", nil},
		{"Unknown", nil},
		{"", nil},
		{"Degraded", []string{"argocd-health/Application/argocd/media/Degraded"}},
		{"Missing", []string{"argocd-health/Application/argocd/media/Missing"}},
	}
	for _, tt := range tests {
		t.Run("health "+tt.health, func(t *testing.T) {
			snap := &Snapshot{Now: testNow, Applications: []Application{{Meta: appMeta(), HealthStatus: tt.health}}}
			wantFindings(t, ArgoCDHealth(snap, testOptions()), tt.want, Critical)
		})
	}
}

func TestArgoCDHealthMessage(t *testing.T) {
	snap := &Snapshot{Now: testNow, Applications: []Application{{Meta: appMeta(), HealthStatus: "Degraded"}}}
	got := ArgoCDHealth(snap, testOptions())
	if len(got) != 1 || got[0].Message != "application health is Degraded" || got[0].Object.Kind != "Application" {
		t.Errorf("got %+v", got)
	}
}

func TestArgoCDSync(t *testing.T) {
	tests := []struct {
		sync string
		want []string
	}{
		{"Synced", nil},
		{"Unknown", nil},
		{"", nil},
		{"OutOfSync", []string{"argocd-sync/Application/argocd/media/OutOfSync"}},
	}
	for _, tt := range tests {
		t.Run("sync "+tt.sync, func(t *testing.T) {
			snap := &Snapshot{Now: testNow, Applications: []Application{{Meta: appMeta(), SyncStatus: tt.sync}}}
			wantFindings(t, ArgoCDSync(snap, testOptions()), tt.want, Warning)
		})
	}
}

func TestArgoCDChecksRespectIgnore(t *testing.T) {
	annotated := appMeta()
	annotated.Annotations = map[string]string{IgnoreAnnotation: "true"}
	ignoredNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "argocd", Annotations: map[string]string{IgnoreAnnotation: "true"},
	}}
	apps := func(meta metav1.ObjectMeta) []Application {
		return []Application{{Meta: meta, HealthStatus: "Degraded", SyncStatus: "OutOfSync"}}
	}

	tests := []struct {
		name string
		snap *Snapshot
		opts Options
		want int
	}{
		{"not ignored", &Snapshot{Applications: apps(appMeta())}, testOptions(), 1},
		{"object annotation", &Snapshot{Applications: apps(annotated)}, testOptions(), 0},
		{"namespace annotation", &Snapshot{Applications: apps(appMeta()), Namespaces: []*corev1.Namespace{ignoredNamespace}}, testOptions(), 0},
		{"namespace in env list", &Snapshot{Applications: apps(appMeta())}, Options{IgnoredNamespaces: []string{"argocd"}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.snap.Now = testNow
			if got := len(ArgoCDHealth(tt.snap, tt.opts)); got != tt.want {
				t.Errorf("ArgoCDHealth: %d findings, want %d", got, tt.want)
			}
			if got := len(ArgoCDSync(tt.snap, tt.opts)); got != tt.want {
				t.Errorf("ArgoCDSync: %d findings, want %d", got, tt.want)
			}
		})
	}
}
