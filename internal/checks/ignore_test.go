package checks

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsIgnored(t *testing.T) {
	annotated := map[string]string{IgnoreAnnotation: "true"}
	snap := &Snapshot{Namespaces: []*corev1.Namespace{
		{ObjectMeta: metav1.ObjectMeta{Name: "quiet", Annotations: annotated}},
		{ObjectMeta: metav1.ObjectMeta{Name: "loud"}},
	}}
	opts := Options{IgnoredNamespaces: []string{"kube-system"}}

	tests := []struct {
		name string
		meta metav1.ObjectMeta
		want bool
	}{
		{"plain object", metav1.ObjectMeta{Namespace: "loud", Name: "a"}, false},
		{"object annotation", metav1.ObjectMeta{Namespace: "loud", Name: "a", Annotations: annotated}, true},
		{"annotation with other value", metav1.ObjectMeta{Namespace: "loud", Name: "a", Annotations: map[string]string{IgnoreAnnotation: "false"}}, false},
		{"namespace annotation", metav1.ObjectMeta{Namespace: "quiet", Name: "a"}, true},
		{"namespace on env list", metav1.ObjectMeta{Namespace: "kube-system", Name: "a"}, true},
		{"unknown namespace", metav1.ObjectMeta{Namespace: "gone", Name: "a"}, false},
		{"cluster-scoped object", metav1.ObjectMeta{Name: "node-1"}, false},
		{"cluster-scoped object with annotation", metav1.ObjectMeta{Name: "node-1", Annotations: annotated}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isIgnored(snap, opts, tt.meta); got != tt.want {
				t.Errorf("isIgnored = %v, want %v", got, tt.want)
			}
		})
	}
}
