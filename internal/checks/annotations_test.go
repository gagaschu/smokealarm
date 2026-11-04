package checks

import (
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestApplyAnnotations(t *testing.T) {
	meta := func(namespace, name string, annotations map[string]string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Namespace: namespace, Name: name, Annotations: annotations}
	}
	snap := &Snapshot{
		Namespaces: []*corev1.Namespace{
			{ObjectMeta: meta("", "media", map[string]string{TargetsAnnotation: "slack", SeverityAnnotation: "warning"})},
			{ObjectMeta: meta("", "plain", nil)},
		},
		Pods: []*corev1.Pod{
			{ObjectMeta: meta("media", "jellyfin", nil)},
			{ObjectMeta: meta("media", "loud", map[string]string{SeverityAnnotation: "critical"})},
			{ObjectMeta: meta("media", "own-targets", map[string]string{TargetsAnnotation: " Mail, ntfy ,mail,"})},
			{ObjectMeta: meta("media", "blank", map[string]string{TargetsAnnotation: " , "})},
			{ObjectMeta: meta("plain", "typo", map[string]string{SeverityAnnotation: "hgih"})},
			{ObjectMeta: meta("plain", "upper", map[string]string{SeverityAnnotation: "CRITICAL"})},
		},
		Deployments:  []*appsv1.Deployment{{ObjectMeta: meta("plain", "web", map[string]string{TargetsAnnotation: "teams"})}},
		Nodes:        []*corev1.Node{{ObjectMeta: meta("", "pi-1", map[string]string{TargetsAnnotation: "ntfy"})}},
		Certificates: []Certificate{{Meta: meta("plain", "tls", map[string]string{SeverityAnnotation: "warning"})}},
		Applications: []Application{{Meta: meta("plain", "app", map[string]string{TargetsAnnotation: "webhook"})}},
	}

	tests := []struct {
		name         string
		ref          ObjectRef
		severity     Severity
		wantSeverity Severity
		wantTargets  []string
	}{
		{"namespace annotations", ObjectRef{"Pod", "media", "jellyfin"}, Critical, Warning, []string{"slack"}},
		{"object severity wins, namespace targets stay", ObjectRef{"Pod", "media", "loud"}, Warning, Critical, []string{"slack"}},
		{"object targets win, cleaned up", ObjectRef{"Pod", "media", "own-targets"}, Critical, Warning, []string{"mail", "ntfy"}},
		{"empty list counts as not set", ObjectRef{"Pod", "media", "blank"}, Critical, Warning, []string{"slack"}},
		{"invalid severity keeps the check's", ObjectRef{"Pod", "plain", "typo"}, Critical, Critical, nil},
		{"severity in upper case", ObjectRef{"Pod", "plain", "upper"}, Warning, Critical, nil},
		{"workload", ObjectRef{"Deployment", "plain", "web"}, Warning, Warning, []string{"teams"}},
		{"same name, other kind", ObjectRef{"StatefulSet", "plain", "web"}, Warning, Warning, nil},
		{"cluster-scoped object", ObjectRef{"Node", "", "pi-1"}, Critical, Critical, []string{"ntfy"}},
		{"certificate", ObjectRef{"Certificate", "plain", "tls"}, Critical, Warning, nil},
		{"application", ObjectRef{"Application", "plain", "app"}, Critical, Critical, []string{"webhook"}},
		{"object not in the snapshot, namespace counts", ObjectRef{"Pod", "media", "gone"}, Critical, Warning, []string{"slack"}},
		{"no annotations", ObjectRef{"Pod", "plain", "other"}, Critical, Critical, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := newFinding("test", tt.severity, tt.ref, "Reason", "message")
			out, _ := ApplyAnnotations(snap, []Finding{in})
			if len(out) != 1 {
				t.Fatalf("got %d findings, want 1", len(out))
			}
			got := out[0]
			if got.Severity != tt.wantSeverity {
				t.Errorf("severity = %s, want %s", got.Severity, tt.wantSeverity)
			}
			if !slices.Equal(got.Targets, tt.wantTargets) {
				t.Errorf("targets = %v, want %v", got.Targets, tt.wantTargets)
			}
			if got.Key != in.Key {
				t.Errorf("key changed from %q to %q", in.Key, got.Key)
			}
		})
	}
}

func TestApplyAnnotationsReportsInvalidSeverity(t *testing.T) {
	snap := &Snapshot{Pods: []*corev1.Pod{{ObjectMeta: metav1.ObjectMeta{
		Namespace: "plain", Name: "typo", Annotations: map[string]string{SeverityAnnotation: "hgih"},
	}}}}
	findings := []Finding{
		newFinding("test", Critical, ObjectRef{"Pod", "plain", "typo"}, "A", "message"),
		newFinding("test", Critical, ObjectRef{"Pod", "plain", "other"}, "A", "message"),
	}
	_, problems := ApplyAnnotations(snap, findings)
	if len(problems) != 1 {
		t.Fatalf("problems = %v, want one", problems)
	}
	if !strings.Contains(problems[0], "Pod plain/typo") || !strings.Contains(problems[0], `"hgih"`) {
		t.Errorf("problem = %q, want object and value", problems[0])
	}
}

func TestParseTargets(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{" , ,", nil},
		{"slack", []string{"slack"}},
		{"Slack, NTFY ,slack", []string{"ntfy", "slack"}},
	}
	for _, tt := range tests {
		if got := ParseTargets(tt.in); !slices.Equal(got, tt.want) {
			t.Errorf("ParseTargets(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
