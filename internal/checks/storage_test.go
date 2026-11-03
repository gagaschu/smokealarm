package checks

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func storageOptions() Options {
	return Options{PVCPendingAfter: 5 * time.Minute}
}

func newPVC(phase corev1.PersistentVolumeClaimPhase, age time.Duration, class *string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "media",
			Name:              "data",
			CreationTimestamp: metav1.NewTime(testNow.Add(-age)),
		},
		Spec:   corev1.PersistentVolumeClaimSpec{StorageClassName: class},
		Status: corev1.PersistentVolumeClaimStatus{Phase: phase},
	}
}

func newClass(name string, mode storagev1.VolumeBindingMode, isDefault bool) *storagev1.StorageClass {
	class := &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: name},
		VolumeBindingMode: &mode,
	}
	if isDefault {
		class.Annotations = map[string]string{defaultClassAnnotation: "true"}
	}
	return class
}

func strPtr(s string) *string { return &s }

func TestPVCPendingThreshold(t *testing.T) {
	const key = "pvc-pending/PersistentVolumeClaim/media/data/PVCPending"
	tests := []struct {
		name string
		pvc  *corev1.PersistentVolumeClaim
		want []string
	}{
		{"pending 4 minutes", newPVC(corev1.ClaimPending, 4*time.Minute, strPtr("nfs")), nil},
		{"pending 6 minutes", newPVC(corev1.ClaimPending, 6*time.Minute, strPtr("nfs")), []string{key}},
		{"bound", newPVC(corev1.ClaimBound, time.Hour, strPtr("nfs")), nil},
		{"lost", newPVC(corev1.ClaimLost, time.Hour, strPtr("nfs")), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &Snapshot{Now: testNow, PVCs: []*corev1.PersistentVolumeClaim{tt.pvc}}
			wantFindings(t, PVCPending(snap, storageOptions()), tt.want, Warning)
		})
	}
}

func TestPVCPendingMessageUsesEvent(t *testing.T) {
	event := &corev1.Event{
		Type: corev1.EventTypeWarning, Reason: "ProvisioningFailed", Message: "storageclass not found",
		InvolvedObject: corev1.ObjectReference{Kind: "PersistentVolumeClaim", Namespace: "media", Name: "data"},
		LastTimestamp:  metav1.NewTime(testNow.Add(-time.Minute)),
	}
	snap := &Snapshot{
		Now:    testNow,
		PVCs:   []*corev1.PersistentVolumeClaim{newPVC(corev1.ClaimPending, 10*time.Minute, strPtr("nope"))},
		Events: []*corev1.Event{event},
	}
	got := PVCPending(snap, storageOptions())
	want := "pending for 10m0s: ProvisioningFailed: storageclass not found"
	if len(got) != 1 || got[0].Message != want {
		t.Errorf("got %+v, want message %q", got, want)
	}
}

func TestPVCPendingWaitForFirstConsumer(t *testing.T) {
	classes := []*storagev1.StorageClass{
		newClass("local-path", storagev1.VolumeBindingWaitForFirstConsumer, true),
		newClass("nfs", storagev1.VolumeBindingImmediate, false),
	}
	tests := []struct {
		name  string
		class *string
		want  int
	}{
		{"named WaitForFirstConsumer class", strPtr("local-path"), 0},
		{"named Immediate class", strPtr("nfs"), 1},
		{"no class name uses the default (WaitForFirstConsumer)", nil, 0},
		{"unknown class", strPtr("does-not-exist"), 1},
		{"empty class name means no class", strPtr(""), 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &Snapshot{
				Now:            testNow,
				PVCs:           []*corev1.PersistentVolumeClaim{newPVC(corev1.ClaimPending, time.Hour, tt.class)},
				StorageClasses: classes,
			}
			if got := len(PVCPending(snap, storageOptions())); got != tt.want {
				t.Errorf("got %d findings, want %d", got, tt.want)
			}
		})
	}
}

func TestPVCPendingNoDefaultClass(t *testing.T) {
	// Without any default class a claim without a class name has no
	// WaitForFirstConsumer exception.
	snap := &Snapshot{
		Now:            testNow,
		PVCs:           []*corev1.PersistentVolumeClaim{newPVC(corev1.ClaimPending, time.Hour, nil)},
		StorageClasses: []*storagev1.StorageClass{newClass("local-path", storagev1.VolumeBindingWaitForFirstConsumer, false)},
	}
	if got := len(PVCPending(snap, storageOptions())); got != 1 {
		t.Errorf("got %d findings, want 1", got)
	}
}

func TestPVCPendingRespectsIgnore(t *testing.T) {
	annotated := newPVC(corev1.ClaimPending, time.Hour, strPtr("nfs"))
	annotated.Annotations = map[string]string{IgnoreAnnotation: "true"}

	ignoredNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "media", Annotations: map[string]string{IgnoreAnnotation: "true"},
	}}
	plain := func() []*corev1.PersistentVolumeClaim {
		return []*corev1.PersistentVolumeClaim{newPVC(corev1.ClaimPending, time.Hour, strPtr("nfs"))}
	}

	tests := []struct {
		name string
		snap *Snapshot
		opts Options
		want int
	}{
		{"not ignored", &Snapshot{PVCs: plain()}, storageOptions(), 1},
		{"object annotation", &Snapshot{PVCs: []*corev1.PersistentVolumeClaim{annotated}}, storageOptions(), 0},
		{"namespace annotation", &Snapshot{PVCs: plain(), Namespaces: []*corev1.Namespace{ignoredNamespace}}, storageOptions(), 0},
		{"namespace in env list", &Snapshot{PVCs: plain()}, Options{PVCPendingAfter: 5 * time.Minute, IgnoredNamespaces: []string{"media"}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.snap.Now = testNow
			if got := len(PVCPending(tt.snap, tt.opts)); got != tt.want {
				t.Errorf("got %d findings, want %d", got, tt.want)
			}
		})
	}
}

const gib = int64(1) << 30

func TestPVCUsageThresholds(t *testing.T) {
	const capacity = 1000
	tests := []struct {
		name       string
		used       int64
		wantReason string
		wantSev    Severity
	}{
		{"empty", 0, "", ""},
		{"84 percent", 840, "", ""},
		{"exactly 85 percent", 850, "", ""},
		{"just above 85 percent", 851, "VolumeAlmostFull", Warning},
		{"90 percent", 900, "VolumeAlmostFull", Warning},
		{"exactly 95 percent", 950, "VolumeAlmostFull", Warning},
		{"just above 95 percent", 951, "VolumeFull", Critical},
		{"completely full", 1000, "VolumeFull", Critical},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := &Snapshot{Now: testNow, VolumeUsage: map[string]VolumeUsage{
				"media/data": {UsedBytes: tt.used, CapacityBytes: capacity},
			}}
			got := PVCUsage(snap, testOptions())

			if tt.wantReason == "" {
				if len(got) != 0 {
					t.Fatalf("got %+v, want none", got)
				}
				return
			}
			wantKey := "pvc-usage/PersistentVolumeClaim/media/data/" + tt.wantReason
			if len(got) != 1 || got[0].Key != wantKey || got[0].Severity != tt.wantSev {
				t.Errorf("got %+v, want %s %s", got, tt.wantSev, wantKey)
			}
		})
	}
}

func TestPVCUsageMessage(t *testing.T) {
	snap := &Snapshot{Now: testNow, VolumeUsage: map[string]VolumeUsage{
		"media/data": {UsedBytes: 9*gib + gib/2 + 1, CapacityBytes: 10 * gib},
	}}
	got := PVCUsage(snap, testOptions())
	want := "95.0% full (9.5 GiB of 10.0 GiB used)"
	if len(got) != 1 || got[0].Message != want {
		t.Errorf("got %+v, want message %q", got, want)
	}
}

func TestPVCUsageSkipsInvalidEntries(t *testing.T) {
	snap := &Snapshot{Now: testNow, VolumeUsage: map[string]VolumeUsage{
		"media/zero-capacity": {UsedBytes: 10, CapacityBytes: 0},
		"no-slash":            {UsedBytes: 99, CapacityBytes: 100},
	}}
	if got := PVCUsage(snap, testOptions()); len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

func TestPVCUsageOutputOrderIsStable(t *testing.T) {
	full := VolumeUsage{UsedBytes: 99, CapacityBytes: 100}
	snap := &Snapshot{Now: testNow, VolumeUsage: map[string]VolumeUsage{
		"b/two": full, "a/one": full, "c/three": full,
	}}
	got := PVCUsage(snap, testOptions())
	if len(got) != 3 || got[0].Object.Namespace != "a" || got[1].Object.Namespace != "b" || got[2].Object.Namespace != "c" {
		t.Errorf("findings are not sorted by volume: %+v", got)
	}
}

func TestPVCUsageRespectsIgnore(t *testing.T) {
	full := map[string]VolumeUsage{"media/data": {UsedBytes: 99, CapacityBytes: 100}}
	annotated := newPVC(corev1.ClaimBound, time.Hour, nil)
	annotated.Annotations = map[string]string{IgnoreAnnotation: "true"}
	ignoredNamespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "media", Annotations: map[string]string{IgnoreAnnotation: "true"},
	}}

	tests := []struct {
		name string
		snap *Snapshot
		opts Options
		want int
	}{
		{"not ignored", &Snapshot{VolumeUsage: full}, testOptions(), 1},
		{"claim annotation", &Snapshot{VolumeUsage: full, PVCs: []*corev1.PersistentVolumeClaim{annotated}}, testOptions(), 0},
		{"namespace annotation", &Snapshot{VolumeUsage: full, Namespaces: []*corev1.Namespace{ignoredNamespace}}, testOptions(), 0},
		{"namespace in env list", &Snapshot{VolumeUsage: full}, Options{IgnoredNamespaces: []string{"media"}}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.snap.Now = testNow
			if got := len(PVCUsage(tt.snap, tt.opts)); got != tt.want {
				t.Errorf("got %d findings, want %d", got, tt.want)
			}
		})
	}
}

func TestFormatBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{5 * 1024 * 1024, "5.0 MiB"},
		{10 * gib, "10.0 GiB"},
		{3 * gib * 1024, "3.0 TiB"},
	}
	for _, tt := range tests {
		if got := formatBytes(tt.n); got != tt.want {
			t.Errorf("formatBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}
