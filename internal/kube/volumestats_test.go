package kube

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// summaryJSON looks like the answer of a kubelet: volumes of pods, some of
// them claims, some not.
const summaryJSON = `{
  "node": {"nodeName": "pi-1"},
  "pods": [
    {"podRef": {"name": "db-0", "namespace": "data"},
     "volume": [
       {"name": "data", "usedBytes": 9500, "capacityBytes": 10000, "availableBytes": 500,
        "pvcRef": {"name": "db-data", "namespace": "data"}},
       {"name": "scratch", "usedBytes": 10, "capacityBytes": 100},
       {"name": "kube-api-access", "usedBytes": 4096, "capacityBytes": 8192}
     ]},
    {"podRef": {"name": "db-1", "namespace": "data"},
     "volume": [
       {"name": "data", "usedBytes": 9700, "capacityBytes": 10000,
        "pvcRef": {"name": "db-data", "namespace": "data"}},
       {"name": "other", "usedBytes": 1, "capacityBytes": 2,
        "pvcRef": {"name": "other-data", "namespace": "media"}},
       {"name": "no-numbers", "pvcRef": {"name": "broken", "namespace": "media"}}
     ]}
  ]
}`

func TestParseVolumeUsage(t *testing.T) {
	got, err := parseVolumeUsage([]byte(summaryJSON))
	if err != nil {
		t.Fatalf("parseVolumeUsage: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d volumes %v, want the 2 claims with numbers", len(got), got)
	}
	// The claim mounted by two pods keeps its highest usage.
	if db := got["data/db-data"]; db.UsedBytes != 9700 || db.CapacityBytes != 10000 {
		t.Errorf("data/db-data = %+v, want used 9700 of 10000", db)
	}
	if other := got["media/other-data"]; other.UsedBytes != 1 || other.CapacityBytes != 2 {
		t.Errorf("media/other-data = %+v", other)
	}
}

func TestParseVolumeUsageErrors(t *testing.T) {
	if _, err := parseVolumeUsage([]byte("not json")); err == nil {
		t.Error("expected an error for invalid JSON")
	}
	got, err := parseVolumeUsage([]byte(`{"pods": []}`))
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v; want an empty result for a node without pods", got, err)
	}
}

func readyNode(name string, ready corev1.ConditionStatus) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: ready},
		}},
	}
}

func testCluster(t *testing.T, fetch statsFetcher) *Cluster {
	t.Helper()
	cluster, err := New(fake.NewClientset(), nil, Options{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cluster.fetchStats = fetch
	return cluster
}

func forbidden() error {
	return &kubeletStatusError{Code: http.StatusForbidden, Excerpt: "Forbidden"}
}

func TestCollectVolumeUsageMergesNodes(t *testing.T) {
	cluster := testCluster(t, func(_ context.Context, node *corev1.Node) ([]byte, error) {
		if node.Name == "pi-1" {
			return []byte(`{"pods":[{"volume":[{"usedBytes":50,"capacityBytes":100,"pvcRef":{"name":"shared","namespace":"a"}}]}]}`), nil
		}
		return []byte(`{"pods":[{"volume":[
			{"usedBytes":80,"capacityBytes":100,"pvcRef":{"name":"shared","namespace":"a"}},
			{"usedBytes":10,"capacityBytes":100,"pvcRef":{"name":"mine","namespace":"b"}}]}]}`), nil
	})

	usage, reason := cluster.collectVolumeUsage(context.Background(),
		[]*corev1.Node{readyNode("pi-1", corev1.ConditionTrue), readyNode("pi-2", corev1.ConditionTrue)})

	if reason != "" || len(usage) != 2 {
		t.Fatalf("got %v, reason %q; want 2 volumes and no reason", usage, reason)
	}
	if usage["a/shared"].UsedBytes != 80 {
		t.Errorf("a/shared = %+v, want the higher usage of the two nodes", usage["a/shared"])
	}
}

func TestCollectVolumeUsageForbidden(t *testing.T) {
	cluster := testCluster(t, func(context.Context, *corev1.Node) ([]byte, error) { return nil, forbidden() })

	usage, reason := cluster.collectVolumeUsage(context.Background(), []*corev1.Node{readyNode("pi-1", corev1.ConditionTrue)})
	if usage != nil {
		t.Errorf("usage = %v, want none when the stats cannot be read", usage)
	}
	for _, want := range []string{"HTTP 403", "nodes/stats", "webhook", "SMOKEALARM_DISABLED_CHECKS=pvc-usage"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not contain %q", reason, want)
		}
	}
}

func TestCollectVolumeUsageOneNodeFailsSkipsEverything(t *testing.T) {
	cluster := testCluster(t, func(_ context.Context, node *corev1.Node) ([]byte, error) {
		if node.Name == "pi-2" {
			return nil, errors.New("timeout")
		}
		return []byte(`{"pods":[]}`), nil
	})

	usage, reason := cluster.collectVolumeUsage(context.Background(),
		[]*corev1.Node{readyNode("pi-1", corev1.ConditionTrue), readyNode("pi-2", corev1.ConditionTrue)})
	if usage != nil || !strings.Contains(reason, "pi-2") || !strings.Contains(reason, "timeout") {
		t.Errorf("usage %v, reason %q; want the check skipped and the failing node named", usage, reason)
	}
}

func TestCollectVolumeUsageInvalidAnswerSkips(t *testing.T) {
	cluster := testCluster(t, func(context.Context, *corev1.Node) ([]byte, error) { return []byte("<html>"), nil })
	usage, reason := cluster.collectVolumeUsage(context.Background(), []*corev1.Node{readyNode("pi-1", corev1.ConditionTrue)})
	if usage != nil || reason == "" {
		t.Errorf("usage %v, reason %q; want a skip for unreadable stats", usage, reason)
	}
}

func TestCollectVolumeUsageIgnoresNotReadyNodes(t *testing.T) {
	var asked []string
	cluster := testCluster(t, func(_ context.Context, node *corev1.Node) ([]byte, error) {
		asked = append(asked, node.Name)
		return []byte(`{"pods":[]}`), nil
	})

	_, reason := cluster.collectVolumeUsage(context.Background(), []*corev1.Node{
		readyNode("up", corev1.ConditionTrue),
		readyNode("down", corev1.ConditionFalse),
		readyNode("lost", corev1.ConditionUnknown),
		{ObjectMeta: metav1.ObjectMeta{Name: "new"}}, // no Ready condition yet
	})
	if reason != "" || len(asked) != 1 || asked[0] != "up" {
		t.Errorf("asked %v, reason %q; want only the Ready node to be asked, without a skip", asked, reason)
	}
}

func TestBuildSnapshotCollectsVolumeUsage(t *testing.T) {
	client := fake.NewClientset(readyNode("pi-1", corev1.ConditionTrue))
	cluster, err := New(client, nil, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	cluster.fetchStats = func(context.Context, *corev1.Node) ([]byte, error) { return []byte(summaryJSON), nil }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cluster.Start(ctx)
	if err := cluster.WaitForSync(ctx); err != nil {
		t.Fatal(err)
	}

	snap := cluster.BuildSnapshot(ctx, time.Now())
	if len(snap.VolumeUsage) != 2 || snap.Skipped != nil {
		t.Errorf("VolumeUsage %v, Skipped %v; want 2 volumes and nothing skipped", snap.VolumeUsage, snap.Skipped)
	}
}

func TestBuildSnapshotMarksCheckSkipped(t *testing.T) {
	client := fake.NewClientset(readyNode("pi-1", corev1.ConditionTrue))
	cluster, err := New(client, nil, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	cluster.fetchStats = func(context.Context, *corev1.Node) ([]byte, error) { return nil, forbidden() }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cluster.Start(ctx)
	if err := cluster.WaitForSync(ctx); err != nil {
		t.Fatal(err)
	}

	snap := cluster.BuildSnapshot(ctx, time.Now())
	if reason := snap.Skipped[checks.PVCUsageCheck]; !strings.Contains(reason, "nodes/stats") || snap.VolumeUsage != nil {
		t.Errorf("Skipped %v, VolumeUsage %v; want pvc-usage skipped with the RBAC hint", snap.Skipped, snap.VolumeUsage)
	}
}

// Without a kubelet client (pvc-usage is off) no kubelet is asked: there is
// nothing to ask with.
func TestVolumeStatsOffMakesNoRequests(t *testing.T) {
	client := fake.NewClientset(readyNode("pi-1", corev1.ConditionTrue))
	cluster, err := New(client, nil, Options{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if cluster.fetchStats != nil {
		t.Fatal("a stats fetcher exists although no kubelet client was given")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cluster.Start(ctx)
	if err := cluster.WaitForSync(ctx); err != nil {
		t.Fatal(err)
	}
	if snap := cluster.BuildSnapshot(ctx, time.Now()); snap.VolumeUsage != nil || snap.Skipped != nil {
		t.Errorf("VolumeUsage %v, Skipped %v; want neither with stats off", snap.VolumeUsage, snap.Skipped)
	}
}
