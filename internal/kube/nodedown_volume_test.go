package kube

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/engine"
	"github.com/gagaschu/smokealarm/internal/notify"
	"github.com/gagaschu/smokealarm/internal/store"
)

// subjectNotifier remembers one line per alert, to compare the whole story.
type subjectNotifier struct{ lines []string }

func (n *subjectNotifier) Name() string        { return "fake" }
func (n *subjectNotifier) Description() string { return "fake (test)" }
func (n *subjectNotifier) Send(_ context.Context, msg notify.Notification) error {
	for _, a := range msg.New {
		n.lines = append(n.lines, "NEW "+a.Reason+" "+a.Object.Name)
	}
	for _, a := range msg.Resolved {
		n.lines = append(n.lines, "RESOLVED "+a.Reason+" "+a.Object.Name)
	}
	return nil
}

// volumeStory runs the real snapshot, checks, engine and store on a cluster
// with nodes pi-1 and pi-2. The volume "db" lives on pi-2.
type volumeStory struct {
	t        *testing.T
	cluster  *Cluster
	engine   *engine.Engine
	notifier *subjectNotifier
	now      time.Time
	used     int64 // of 100 bytes
}

func newVolumeStory(t *testing.T) *volumeStory {
	t.Helper()
	story := &volumeStory{t: t, notifier: &subjectNotifier{}, used: 96,
		now: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	story.cluster = testCluster(t, func(_ context.Context, node string) ([]byte, error) {
		if node != "pi-2" {
			return []byte(`{"pods":[]}`), nil
		}
		return fmt.Appendf(nil, `{"pods":[{"volume":[{"usedBytes":%d,"capacityBytes":100,"pvcRef":{"name":"db","namespace":"data"}}]}]}`, story.used), nil
	})
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"), func() time.Time { return story.now })
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, _, err := db.SyncTargets(context.Background(), []string{"fake"}); err != nil {
		t.Fatalf("SyncTargets: %v", err)
	}
	var enabled []checks.Check
	for _, c := range checks.All() {
		if c.Name == checks.PVCUsageCheck || c.Name == checks.CheckNodeNotReady {
			enabled = append(enabled, c)
		}
	}
	story.engine = &engine.Engine{
		Snapshot:   story.cluster.BuildSnapshot,
		Checks:     enabled,
		AlertAfter: 2 * time.Minute,
		Retention:  time.Hour,
		Store:      db,
		Notifiers:  []notify.Notifier{story.notifier},
		Now:        func() time.Time { return story.now },
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return story
}

// run sets the state of pi-2, advances the clock and does one engine run.
func (s *volumeStory) run(pi2 corev1.ConditionStatus) {
	s.t.Helper()
	nodes := []any{readyNode("pi-1", corev1.ConditionTrue), readyNode("pi-2", pi2)}
	if err := s.cluster.nodes.GetStore().Replace(nodes, ""); err != nil {
		s.t.Fatalf("Replace: %v", err)
	}
	s.now = s.now.Add(3 * time.Minute)
	if err := s.engine.RunOnce(context.Background()); err != nil {
		s.t.Fatalf("RunOnce: %v", err)
	}
}

func (s *volumeStory) want(lines ...string) {
	s.t.Helper()
	got, want := strings.Join(s.notifier.lines, "; "), strings.Join(lines, "; ")
	if got != want {
		s.t.Errorf("notifications:\n got %s\nwant %s", got, want)
	}
}

// Usage of a volume on a node that cannot be asked is unknown, not "fine".
func TestFullVolumeIsNotResolvedWhenItsNodeGoesDown(t *testing.T) {
	story := newVolumeStory(t)
	story.run(corev1.ConditionTrue)
	story.run(corev1.ConditionTrue)
	story.want("NEW VolumeFull db")

	story.run(corev1.ConditionFalse)
	story.run(corev1.ConditionFalse)
	story.want("NEW VolumeFull db", "NEW NotReady pi-2")

	story.run(corev1.ConditionTrue)
	story.run(corev1.ConditionTrue)
	story.want("NEW VolumeFull db", "NEW NotReady pi-2", "RESOLVED NotReady pi-2")
}

// If the node comes back and the volume was cleaned up meanwhile, the problem
// ends as usual.
func TestVolumeIsResolvedWhenItsNodeComesBackWithRoom(t *testing.T) {
	story := newVolumeStory(t)
	story.run(corev1.ConditionTrue)
	story.run(corev1.ConditionTrue)
	story.run(corev1.ConditionFalse)
	story.used = 10
	story.run(corev1.ConditionTrue)
	story.want("NEW VolumeFull db", "RESOLVED VolumeFull db")
}

// While another node is down, a volume that can still be measured ends as usual.
func TestMeasuredVolumeIsResolvedWhileAnotherNodeIsDown(t *testing.T) {
	story := newVolumeStory(t)
	story.run(corev1.ConditionTrue)
	story.run(corev1.ConditionTrue)
	story.used = 10

	nodes := []any{readyNode("pi-1", corev1.ConditionFalse), readyNode("pi-2", corev1.ConditionTrue)}
	if err := story.cluster.nodes.GetStore().Replace(nodes, ""); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	story.now = story.now.Add(3 * time.Minute)
	if err := story.engine.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	story.want("NEW VolumeFull db", "RESOLVED VolumeFull db")
}

// A downtime that ends while the volume cannot be measured waits: a new mail
// now would claim a problem that may be gone.
func TestDowntimeOfAnUnmeasuredVolumeWaitsForItsNode(t *testing.T) {
	story := newVolumeStory(t)
	story.run(corev1.ConditionTrue)
	story.run(corev1.ConditionTrue)
	const key = "pvc-usage/PersistentVolumeClaim/data/db/usage"
	err := story.engine.Store.SetDowntime(context.Background(), store.Downtime{
		Key: key, Check: checks.PVCUsageCheck, Reason: "usage",
		Object:    checks.ObjectRef{Kind: checks.KindPVC, Namespace: "data", Name: "db"},
		CreatedAt: story.now, ExpiresAt: story.now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("SetDowntime: %v", err)
	}

	story.run(corev1.ConditionFalse) // NotReady is still pending after one run
	story.want("NEW VolumeFull db")
	active, err := story.engine.Store.DowntimeKeys(context.Background())
	if err != nil || !active[key] {
		t.Fatalf("DowntimeKeys = %v, %v; want the downtime to wait", active, err)
	}

	story.run(corev1.ConditionTrue)
	story.want("NEW VolumeFull db", "NEW VolumeFull db")
}

// A volume that cannot be measured says nothing, as a check that cannot run. The
// RESOLVE_AFTER wait starts again from the last run in which it could not be.
func TestUnmeasuredVolumeStartsTheResolveAfterWaitAgain(t *testing.T) {
	story := newVolumeStory(t)
	story.run(corev1.ConditionTrue)
	story.run(corev1.ConditionTrue)
	story.want("NEW VolumeFull db")

	story.engine.ResolveAfter = 5 * time.Minute
	story.used = 10
	story.run(corev1.ConditionTrue) // the wait starts
	story.run(corev1.ConditionFalse)
	story.run(corev1.ConditionFalse)
	story.want("NEW VolumeFull db", "NEW NotReady pi-2")

	story.run(corev1.ConditionTrue) // 3 minutes since it could last not be measured
	story.want("NEW VolumeFull db", "NEW NotReady pi-2")

	story.run(corev1.ConditionTrue)
	story.want("NEW VolumeFull db", "NEW NotReady pi-2", "RESOLVED VolumeFull db")
}
