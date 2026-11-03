package checks

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var testNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func testOptions() Options {
	return Options{RestartThreshold: 5, PodPendingAfter: 5 * time.Minute}
}

// newPod builds a pod in namespace "media" named "jellyfin".
func newPod(status corev1.PodStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "media",
			Name:              "jellyfin",
			CreationTimestamp: metav1.NewTime(testNow.Add(-time.Hour)),
		},
		Status: status,
	}
}

func waiting(reason, message string) corev1.ContainerState {
	return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}}
}

func terminatedAgo(ago time.Duration, reason string, exitCode int32) *corev1.ContainerStateTerminated {
	return &corev1.ContainerStateTerminated{
		Reason:     reason,
		ExitCode:   exitCode,
		FinishedAt: metav1.NewTime(testNow.Add(-ago)),
	}
}

func withContainer(cs corev1.ContainerStatus) corev1.PodStatus {
	return corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{cs}}
}

// wantFindings compares findings by key and severity only; messages are
// checked separately where they matter.
func wantFindings(t *testing.T, got []Finding, wantKeys []string, wantSeverity Severity) {
	t.Helper()
	if len(got) != len(wantKeys) {
		t.Fatalf("got %d findings %+v, want %d", len(got), got, len(wantKeys))
	}
	for i, f := range got {
		if f.Key != wantKeys[i] {
			t.Errorf("finding %d key = %q, want %q", i, f.Key, wantKeys[i])
		}
		if f.Severity != wantSeverity {
			t.Errorf("finding %d severity = %q, want %q", i, f.Severity, wantSeverity)
		}
	}
}

func TestPodCrashLoop(t *testing.T) {
	crashing := corev1.ContainerStatus{
		Name:                 "app",
		RestartCount:         7,
		State:                waiting(reasonCrashLoop, "back-off"),
		LastTerminationState: corev1.ContainerState{Terminated: terminatedAgo(time.Minute, "Error", 137)},
	}
	healthy := corev1.ContainerStatus{Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}

	tests := []struct {
		name string
		pod  *corev1.Pod
		want []string
	}{
		{"crashing container", newPod(withContainer(crashing)), []string{"pod-crashloop/Pod/media/jellyfin/CrashLoopBackOff"}},
		{"healthy container", newPod(withContainer(healthy)), nil},
		{"crashing init container", newPod(corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{crashing}}),
			[]string{"pod-crashloop/Pod/media/jellyfin/CrashLoopBackOff"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &Snapshot{Now: testNow, Pods: []*corev1.Pod{tt.pod}}
			wantFindings(t, PodCrashLoop(snap, testOptions()), tt.want, Critical)
		})
	}
}

func TestPodCrashLoopMessage(t *testing.T) {
	pod := newPod(withContainer(corev1.ContainerStatus{
		Name:                 "app",
		RestartCount:         7,
		State:                waiting(reasonCrashLoop, ""),
		LastTerminationState: corev1.ContainerState{Terminated: terminatedAgo(time.Minute, "Error", 137)},
	}))
	got := PodCrashLoop(&Snapshot{Now: testNow, Pods: []*corev1.Pod{pod}}, testOptions())
	want := "container app keeps crashing (7 restarts), last exit code 137 (Error)"
	if len(got) != 1 || got[0].Message != want {
		t.Errorf("got %+v, want message %q", got, want)
	}
}

func TestPodImagePull(t *testing.T) {
	const key = "pod-image-pull/Pod/media/jellyfin/ImagePullBackOff"
	tests := []struct {
		name   string
		reason string
		want   []string
	}{
		{"ImagePullBackOff", reasonImagePull, []string{key}},
		{"ErrImagePull uses the same key", reasonErrImage, []string{key}},
		{"other waiting reason", "ContainerCreating", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := newPod(withContainer(corev1.ContainerStatus{Name: "app", Image: "x/y", State: waiting(tt.reason, "pull failed")}))
			snap := &Snapshot{Now: testNow, Pods: []*corev1.Pod{pod}}
			wantFindings(t, PodImagePull(snap, testOptions()), tt.want, Critical)
		})
	}
}

func TestPodOOMKilled(t *testing.T) {
	const key = "pod-oomkilled/Pod/media/jellyfin/OOMKilled"
	tests := []struct {
		name string
		last *corev1.ContainerStateTerminated
		want []string
	}{
		{"killed 14 minutes ago", terminatedAgo(14*time.Minute, reasonOOMKilled, 137), []string{key}},
		{"killed 16 minutes ago", terminatedAgo(16*time.Minute, reasonOOMKilled, 137), nil},
		{"other termination reason", terminatedAgo(time.Minute, "Error", 1), nil},
		{"never terminated", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := newPod(withContainer(corev1.ContainerStatus{
				Name:                 "app",
				LastTerminationState: corev1.ContainerState{Terminated: tt.last},
			}))
			snap := &Snapshot{Now: testNow, Pods: []*corev1.Pod{pod}}
			wantFindings(t, PodOOMKilled(snap, testOptions()), tt.want, Warning)
		})
	}
}

func TestPodRestarts(t *testing.T) {
	const key = "pod-restarts/Pod/media/jellyfin/TooManyRestarts"
	tests := []struct {
		name     string
		restarts int32
		lastAgo  time.Duration
		want     []string
	}{
		{"at the threshold", 5, time.Minute, nil},
		{"above the threshold", 6, time.Minute, []string{key}},
		{"last restart 59 minutes ago", 6, 59 * time.Minute, []string{key}},
		{"last restart 61 minutes ago", 6, 61 * time.Minute, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := newPod(withContainer(corev1.ContainerStatus{
				Name:                 "app",
				RestartCount:         tt.restarts,
				LastTerminationState: corev1.ContainerState{Terminated: terminatedAgo(tt.lastAgo, "Error", 1)},
			}))
			snap := &Snapshot{Now: testNow, Pods: []*corev1.Pod{pod}}
			wantFindings(t, PodRestarts(snap, testOptions()), tt.want, Warning)
		})
	}
}

func pendingPod(age time.Duration) *corev1.Pod {
	pod := newPod(corev1.PodStatus{Phase: corev1.PodPending})
	pod.CreationTimestamp = metav1.NewTime(testNow.Add(-age))
	return pod
}

func TestPodPending(t *testing.T) {
	const key = "pod-pending/Pod/media/jellyfin/PodPending"
	tests := []struct {
		name string
		pod  *corev1.Pod
		want []string
	}{
		{"pending 4 minutes", pendingPod(4 * time.Minute), nil},
		{"pending 6 minutes", pendingPod(6 * time.Minute), []string{key}},
		{"running pod", newPod(corev1.PodStatus{Phase: corev1.PodRunning}), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &Snapshot{Now: testNow, Pods: []*corev1.Pod{tt.pod}}
			wantFindings(t, PodPending(snap, testOptions()), tt.want, Warning)
		})
	}
}

func TestPodPendingMessageUsesSchedulerCondition(t *testing.T) {
	pod := pendingPod(10 * time.Minute)
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Reason: "Unschedulable", Message: "0/3 nodes are available",
	}}
	got := PodPending(&Snapshot{Now: testNow, Pods: []*corev1.Pod{pod}}, testOptions())
	want := "pending for 10m0s: Unschedulable: 0/3 nodes are available"
	if len(got) != 1 || got[0].Message != want {
		t.Errorf("got %+v, want message %q", got, want)
	}
}

func TestPodPendingMessageFallsBackToEvent(t *testing.T) {
	older := &corev1.Event{
		Type: corev1.EventTypeWarning, Reason: "Old", Message: "old news",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "media", Name: "jellyfin"},
		LastTimestamp:  metav1.NewTime(testNow.Add(-5 * time.Minute)),
	}
	newer := &corev1.Event{
		Type: corev1.EventTypeWarning, Reason: "FailedMount", Message: "volume missing",
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "media", Name: "jellyfin"},
		LastTimestamp:  metav1.NewTime(testNow.Add(-time.Minute)),
	}
	snap := &Snapshot{Now: testNow, Pods: []*corev1.Pod{pendingPod(10 * time.Minute)}, Events: []*corev1.Event{older, newer}}
	got := PodPending(snap, testOptions())
	want := "pending for 10m0s: FailedMount: volume missing"
	if len(got) != 1 || got[0].Message != want {
		t.Errorf("got %+v, want message %q", got, want)
	}
}

func TestPodPendingSkipsImagePullProblems(t *testing.T) {
	pod := pendingPod(10 * time.Minute)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: waiting(reasonImagePull, "nope")}}
	snap := &Snapshot{Now: testNow, Pods: []*corev1.Pod{pod}}

	if got := PodPending(snap, testOptions()); len(got) != 0 {
		t.Errorf("pending check reported %+v, want nothing", got)
	}
	if got := PodImagePull(snap, testOptions()); len(got) != 1 {
		t.Errorf("image pull check reported %d findings, want 1", len(got))
	}
}

// Every pod check must respect the ignore rules.
func TestPodChecksRespectIgnore(t *testing.T) {
	crashing := corev1.ContainerStatus{
		Name:                 "app",
		RestartCount:         9,
		State:                waiting(reasonCrashLoop, ""),
		LastTerminationState: corev1.ContainerState{Terminated: terminatedAgo(time.Minute, reasonOOMKilled, 137)},
	}
	build := func() *corev1.Pod {
		pod := pendingPod(10 * time.Minute)
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{crashing}
		return pod
	}

	ignoredPod := build()
	ignoredPod.Annotations = map[string]string{IgnoreAnnotation: "true"}

	ignoredNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "media", Annotations: map[string]string{IgnoreAnnotation: "true"},
	}}

	tests := []struct {
		name string
		snap *Snapshot
		opts Options
		want int // findings per check, when not ignored
	}{
		{"not ignored", &Snapshot{Now: testNow, Pods: []*corev1.Pod{build()}}, testOptions(), 1},
		{"pod annotation", &Snapshot{Now: testNow, Pods: []*corev1.Pod{ignoredPod}}, testOptions(), 0},
		{"namespace annotation", &Snapshot{Now: testNow, Pods: []*corev1.Pod{build()}, Namespaces: []*corev1.Namespace{ignoredNamespace}}, testOptions(), 0},
		{"namespace in env list", &Snapshot{Now: testNow, Pods: []*corev1.Pod{build()}},
			Options{RestartThreshold: 5, PodPendingAfter: 5 * time.Minute, IgnoredNamespaces: []string{"media"}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Crash loop, OOM and restarts all fire for this pod. Pending
			// does not (the container status has no pull problem but the
			// pod is Pending), so check them individually.
			for _, check := range []Check{{Name: "crashloop", Run: PodCrashLoop}, {Name: "oom", Run: PodOOMKilled}, {Name: "restarts", Run: PodRestarts}, {Name: "pending", Run: PodPending}} {
				if got := len(check.Run(tt.snap, tt.opts)); got != tt.want {
					t.Errorf("%s: got %d findings, want %d", check.Name, got, tt.want)
				}
			}
		})
	}
}
