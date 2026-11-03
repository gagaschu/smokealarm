package kube

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestTrimObjectStripsUnusedFields(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:          "p",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
			Annotations: map[string]string{
				lastAppliedAnnotation: "{...big json...}",
				"keep":                "me",
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{Name: "data"}},
			Containers: []corev1.Container{{
				Name:         "app",
				Image:        "nginx",
				Env:          []corev1.EnvVar{{Name: "SECRET", Value: "x"}},
				EnvFrom:      []corev1.EnvFromSource{{Prefix: "p"}},
				VolumeMounts: []corev1.VolumeMount{{Name: "data"}},
			}},
			InitContainers: []corev1.Container{{Name: "init", Env: []corev1.EnvVar{{Name: "A"}}}},
		},
	}

	if _, err := trimObject(pod); err != nil {
		t.Fatalf("trimObject: %v", err)
	}

	if len(pod.ManagedFields) != 0 {
		t.Error("managedFields not removed")
	}
	if _, found := pod.Annotations[lastAppliedAnnotation]; found {
		t.Error("last-applied annotation not removed")
	}
	if pod.Annotations["keep"] != "me" {
		t.Error("other annotations must stay")
	}
	app := pod.Spec.Containers[0]
	if pod.Spec.Volumes != nil || app.Env != nil || app.EnvFrom != nil || app.VolumeMounts != nil {
		t.Errorf("pod spec not trimmed: %+v", pod.Spec)
	}
	if app.Image != "nginx" {
		t.Error("image must stay")
	}
	if pod.Spec.InitContainers[0].Env != nil {
		t.Error("init container env not removed")
	}
}

func TestBuildSnapshotReadsFromCache(t *testing.T) {
	client := fake.NewClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "media"}},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:     "media",
				Name:          "jellyfin",
				ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}},
			},
		},
		&corev1.Event{
			ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "e1"},
			Type:       corev1.EventTypeWarning,
		},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "d"}},
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "s"}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "ds"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "pi-1"}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "j"}},
		&batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "cj"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "pvc"}},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "local-path"}},
	)
	cluster, err := New(client, nil, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if cluster.Synced() {
		t.Error("Synced() = true before Start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cluster.Start(ctx)
	if err := cluster.WaitForSync(ctx); err != nil {
		t.Fatalf("WaitForSync: %v", err)
	}
	if err := cluster.Ready(); err != nil {
		t.Errorf("Ready: %v", err)
	}

	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	snap := cluster.BuildSnapshot(ctx, now)

	if !snap.Now.Equal(now) {
		t.Errorf("Now = %v, want %v", snap.Now, now)
	}
	counts := map[string]int{
		"namespaces":     len(snap.Namespaces),
		"pods":           len(snap.Pods),
		"events":         len(snap.Events),
		"deployments":    len(snap.Deployments),
		"statefulsets":   len(snap.StatefulSets),
		"daemonsets":     len(snap.DaemonSets),
		"nodes":          len(snap.Nodes),
		"jobs":           len(snap.Jobs),
		"cronjobs":       len(snap.CronJobs),
		"pvcs":           len(snap.PVCs),
		"storageclasses": len(snap.StorageClasses),
	}
	for kind, n := range counts {
		if n != 1 {
			t.Errorf("snapshot has %d %s, want 1", n, kind)
		}
	}
	if len(snap.Pods[0].ManagedFields) != 0 {
		t.Error("transform was not applied to cached pod")
	}
}
