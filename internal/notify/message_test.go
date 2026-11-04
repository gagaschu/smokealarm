package notify

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

var (
	t0 = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	t1 = time.Date(2026, 11, 3, 12, 30, 0, 0, time.UTC)
)

func crashLoop() Alert {
	return Alert{
		Finding: checks.Finding{
			Key: "pod-crashloop/Pod/media/jellyfin/CrashLoopBackOff", Check: "pod-crashloop", Severity: checks.Critical,
			Object:  checks.ObjectRef{Kind: "Pod", Namespace: "media", Name: "jellyfin"},
			Reason:  "CrashLoopBackOff",
			Message: "container jellyfin keeps crashing (7 restarts), last exit code 137 (Error)",
		},
		Since: t0,
	}
}

func replicasUnavailable() Alert {
	return Alert{
		Finding: checks.Finding{
			Key: "workload-replicas/Deployment/media/sonarr/ReplicasUnavailable", Check: "workload-replicas", Severity: checks.Warning,
			Object:  checks.ObjectRef{Kind: "Deployment", Namespace: "media", Name: "sonarr"},
			Reason:  "ReplicasUnavailable",
			Message: "1/2 replicas available",
		},
		Since: t0.Add(30 * time.Minute),
	}
}

func nodeNotReady() Alert {
	return Alert{
		Finding: checks.Finding{
			Key: "node-not-ready/Node//pi-2/NotReady", Check: "node-not-ready", Severity: checks.Critical,
			Object:  checks.ObjectRef{Kind: "Node", Name: "pi-2"},
			Reason:  "NotReady",
			Message: "node is not ready for 10m0s (status Unknown): Kubelet stopped posting node status.",
		},
		Since: t0,
	}
}

// nodeDownWithAffected is the node pi-3, newly down, holding back two problems.
func nodeDownWithAffected() Alert {
	a := nodeNotReady()
	a.Key = "node-not-ready/Node//pi-3/NotReady"
	a.Object.Name = "pi-3"
	a.Affected = []AffectedProblem{
		{Object: checks.ObjectRef{Kind: "Deployment", Namespace: "media", Name: "sonarr"}, Reason: "ReplicasUnavailable"},
		{Object: checks.ObjectRef{Kind: "Pod", Namespace: "media", Name: "sonarr-7d9f-x2k4"}, Reason: "CrashLoopBackOff"},
	}
	return a
}

// withAffected gives an alert one held-back problem.
func withAffected(a Alert) Alert {
	a.Affected = []AffectedProblem{{Object: checks.ObjectRef{Kind: "DaemonSet", Namespace: "kube-system", Name: "svclb"}, Reason: "ReplicasUnavailable"}}
	return a
}

func resolved(a Alert) Alert {
	a.ResolvedAt = t1
	return a
}

func TestSubjectAndBodyGolden(t *testing.T) {
	tests := []struct {
		name        string
		n           Notification
		wantSubject string
	}{
		{
			name:        "single_critical",
			n:           Notification{New: []Alert{crashLoop()}, BaseURL: "https://smokealarm.example.com"},
			wantSubject: "[smokealarm] CRITICAL: CrashLoopBackOff media/jellyfin",
		},
		{
			name:        "single_warning",
			n:           Notification{New: []Alert{replicasUnavailable()}},
			wantSubject: "[smokealarm] WARNING: ReplicasUnavailable media/sonarr",
		},
		{
			name:        "single_resolved",
			n:           Notification{Resolved: []Alert{resolved(crashLoop())}, BaseURL: "https://smokealarm.example.com"},
			wantSubject: "[smokealarm] RESOLVED: CrashLoopBackOff media/jellyfin",
		},
		{
			name:        "cluster_scoped_node",
			n:           Notification{New: []Alert{nodeNotReady()}},
			wantSubject: "[smokealarm] NODE DOWN: pi-2",
		},
		{
			name:        "several_new_without_url",
			n:           Notification{New: []Alert{crashLoop(), nodeNotReady(), replicasUnavailable()}},
			wantSubject: "[smokealarm] NODE DOWN: pi-2 (+ 2 new problems)",
		},
		{
			name: "mixed_with_url",
			n: Notification{
				New:      []Alert{crashLoop(), nodeNotReady(), replicasUnavailable()},
				Resolved: []Alert{resolved(replicasUnavailable())},
				BaseURL:  "https://smokealarm.example.com",
			},
			wantSubject: "[smokealarm] NODE DOWN: pi-2 (+ 2 new problems, 1 resolved)",
		},
		{
			name:        "one_new_and_one_resolved",
			n:           Notification{New: []Alert{crashLoop()}, Resolved: []Alert{resolved(replicasUnavailable())}},
			wantSubject: "[smokealarm] 1 new problem, 1 resolved",
		},
		{
			name:        "single_reminder",
			n:           Notification{Reminders: []Alert{crashLoop()}, BaseURL: "https://smokealarm.example.com"},
			wantSubject: "[smokealarm] STILL CRITICAL: CrashLoopBackOff media/jellyfin",
		},
		{
			name:        "one_new_and_one_reminder",
			n:           Notification{New: []Alert{replicasUnavailable()}, Reminders: []Alert{crashLoop()}},
			wantSubject: "[smokealarm] 1 new problem, 1 still critical",
		},
		{
			name: "new_reminders_and_resolved",
			n: Notification{
				New:       []Alert{replicasUnavailable()},
				Reminders: []Alert{crashLoop(), nodeNotReady()},
				Resolved:  []Alert{resolved(replicasUnavailable())},
			},
			wantSubject: "[smokealarm] 1 new problem, 2 still critical, 1 resolved",
		},
		{
			name: "node_down_with_affected",
			n: Notification{
				New:       []Alert{nodeDownWithAffected(), nodeNotReady(), crashLoop()},
				Reminders: []Alert{withAffected(nodeNotReady())},
				BaseURL:   "https://smokealarm.example.com",
			},
			wantSubject: "[smokealarm] NODE DOWN: pi-3, pi-2 (+ 1 new problem, 1 still critical)",
		},
		{
			name:        "node_reminder_keeps_the_old_subject",
			n:           Notification{Reminders: []Alert{withAffected(nodeNotReady())}},
			wantSubject: "[smokealarm] STILL CRITICAL: NotReady pi-2",
		},
		{
			name:        "several_resolved",
			n:           Notification{Resolved: []Alert{resolved(crashLoop()), resolved(nodeNotReady())}},
			wantSubject: "[smokealarm] 2 resolved",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Subject(tt.n); got != tt.wantSubject {
				t.Errorf("Subject = %q, want %q", got, tt.wantSubject)
			}
			body, err := Body(tt.n)
			if err != nil {
				t.Fatalf("Body: %v", err)
			}
			checkGolden(t, tt.name+".golden", body)
		})
	}
}

// checkGolden compares got with the file in testdata. Run the tests with
// UPDATE_GOLDEN=1 to rewrite the files after an intended change, and review
// the diff.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file (create it with UPDATE_GOLDEN=1): %v", err)
	}
	if got != string(want) {
		t.Errorf("body differs from %s:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

func TestSubjectNamesAtMostThreeNodes(t *testing.T) {
	var n Notification
	for _, name := range []string{"pi-1", "pi-2", "pi-3", "pi-4", "pi-5"} {
		a := nodeNotReady()
		a.Object.Name = name
		n.New = append(n.New, a)
	}
	want := "[smokealarm] NODE DOWN: pi-1, pi-2, pi-3, and 2 more"
	if got := Subject(n); got != want {
		t.Errorf("Subject = %q, want %q", got, want)
	}
	n.New = n.New[:3]
	if got := Subject(n); got != "[smokealarm] NODE DOWN: pi-1, pi-2, pi-3" {
		t.Errorf("Subject = %q, want all three names", got)
	}
}

func TestSubjectWithoutChanges(t *testing.T) {
	if got := Subject(Notification{}); got != "[smokealarm] no changes" {
		t.Errorf("Subject = %q", got)
	}
}
