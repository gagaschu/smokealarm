package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

var testNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// fakeCheck returns the given findings and records the snapshot it received.
func fakeCheck(name string, findings ...checks.Finding) checks.Check {
	return checks.Check{
		Name: name,
		Run:  func(*checks.Snapshot, checks.Options) []checks.Finding { return findings },
	}
}

func TestRunChecksSortsByKey(t *testing.T) {
	list := []checks.Check{
		fakeCheck("b", checks.Finding{Key: "b/2"}, checks.Finding{Key: "b/1"}),
		fakeCheck("a", checks.Finding{Key: "a/1"}),
		fakeCheck("none"),
	}

	got := runChecks(&checks.Snapshot{Now: testNow}, checks.Options{}, list)

	var keys []string
	for _, f := range got {
		keys = append(keys, f.Key)
	}
	if want := "a/1,b/1,b/2"; strings.Join(keys, ",") != want {
		t.Errorf("keys = %v, want %s", keys, want)
	}
}

func TestRunOnceLogsFindings(t *testing.T) {
	var out bytes.Buffer
	e := &Engine{
		Snapshot: func(now time.Time) *checks.Snapshot { return &checks.Snapshot{Now: now} },
		Checks: []checks.Check{fakeCheck("pod-crashloop", checks.Finding{
			Key:      "pod-crashloop/Pod/media/jellyfin/CrashLoopBackOff",
			Severity: checks.Critical,
			Object:   checks.ObjectRef{Kind: "Pod", Namespace: "media", Name: "jellyfin"},
			Reason:   "CrashLoopBackOff",
			Message:  "container app keeps crashing",
		})},
		Now:    func() time.Time { return testNow },
		Logger: slog.New(slog.NewTextHandler(&out, nil)),
	}

	e.RunOnce()

	log := out.String()
	for _, want := range []string{"finding", "Pod media/jellyfin", "CrashLoopBackOff", "check run finished", "findings=1"} {
		if !strings.Contains(log, want) {
			t.Errorf("log does not contain %q:\n%s", want, log)
		}
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	runs := 0
	e := &Engine{
		Snapshot: func(now time.Time) *checks.Snapshot { runs++; return &checks.Snapshot{Now: now} },
		Interval: time.Hour,
		Now:      func() time.Time { return testNow },
		Logger:   slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	e.Run(ctx) // must return instead of blocking

	if runs != 1 {
		t.Errorf("runs = %d, want 1 (the immediate first pass)", runs)
	}
}
