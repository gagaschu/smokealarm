package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// firingRecord inserts a firing critical problem and returns its id.
func firingRecord(t *testing.T, s *Store, key string) int64 {
	t.Helper()
	r := newRecord(key, StateFiring, testNow)
	firingAt := testNow
	r.FiringAt = &firingAt
	return insertedID(t, s, r)
}

func mustMarkFiring(t *testing.T, s *Store, target string, id int64, at time.Time) {
	t.Helper()
	if err := s.MarkFiringNotified(context.Background(), target, []int64{id}, at); err != nil {
		t.Fatalf("MarkFiringNotified(%s): %v", target, err)
	}
}

func unnotifiedKeys(t *testing.T, s *Store, target string) string {
	t.Helper()
	records, err := s.Unnotified(context.Background(), target)
	if err != nil {
		t.Fatalf("Unnotified(%s): %v", target, err)
	}
	return recordKeys(records)
}

func TestEveryTargetHasItsOwnMarkers(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	id := firingRecord(t, s, "a")

	mustMarkFiring(t, s, "slack", id, testNow)

	if keys := unnotifiedKeys(t, s, "slack"); keys != "" {
		t.Errorf("slack: Unnotified = %q, want nothing", keys)
	}
	if keys := unnotifiedKeys(t, s, "ntfy"); keys != "a" {
		t.Errorf("ntfy: Unnotified = %q, want a (it was not told yet)", keys)
	}
	// The summary says "reported somewhere", which the web UI and the
	// escalation rule rely on.
	record, _, err := s.Record(ctx, id)
	if err != nil || record.FiringNotifiedAt == nil || !record.FiringNotifiedAt.Equal(testNow) {
		t.Errorf("summary marker = %v, %v, want %v", record.FiringNotifiedAt, err, testNow)
	}
	records, _ := s.Unnotified(ctx, "ntfy")
	if len(records) == 1 && records[0].FiringNotifiedAt != nil {
		t.Error("a record read for ntfy must carry the marker of ntfy, not the summary")
	}
}

func TestRemindersHaveOneClockPerTarget(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	id := firingRecord(t, s, "a")
	mustMarkFiring(t, s, "slack", id, testNow)
	mustMarkFiring(t, s, "ntfy", id, testNow.Add(30*time.Minute))

	before := testNow.Add(10 * time.Minute)
	for target, want := range map[string]string{"slack": "a", "ntfy": "", "teams": ""} {
		due, err := s.CriticalDueForReminder(ctx, target, before)
		if err != nil || recordKeys(due) != want {
			t.Errorf("%s: CriticalDueForReminder = %q, %v, want %q", target, recordKeys(due), err, want)
		}
	}
}

func TestClearingTheMarkerReachesEveryTarget(t *testing.T) {
	t.Run("escalation", func(t *testing.T) {
		s, _ := openTemp(t)
		ctx := context.Background()
		id := firingRecord(t, s, "a")
		mustMarkFiring(t, s, "slack", id, testNow)
		mustMarkFiring(t, s, "ntfy", id, testNow)

		record, _, _ := s.Record(ctx, id)
		record.FiringNotifiedAt = nil
		if err := s.Apply(ctx, Changes{Update: []Record{record}}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		for _, target := range []string{"slack", "ntfy"} {
			if keys := unnotifiedKeys(t, s, target); keys != "a" {
				t.Errorf("%s: Unnotified = %q, want a", target, keys)
			}
		}
	})
	t.Run("an update that keeps the marker", func(t *testing.T) {
		s, _ := openTemp(t)
		ctx := context.Background()
		id := firingRecord(t, s, "a")
		mustMarkFiring(t, s, "slack", id, testNow)

		record, _, _ := s.Record(ctx, id)
		record.Message = "still crashing"
		if err := s.Apply(ctx, Changes{Update: []Record{record}}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if keys := unnotifiedKeys(t, s, "slack"); keys != "" {
			t.Errorf("slack: Unnotified = %q, want nothing", keys)
		}
	})
	t.Run("end of a downtime", func(t *testing.T) {
		s, _ := openTemp(t)
		ctx := context.Background()
		id := firingRecord(t, s, "a")
		mustMarkFiring(t, s, "slack", id, testNow)
		mustMarkFiring(t, s, "ntfy", id, testNow)
		mustSetDowntime(t, s, newDowntime("a", testNow))

		if err := s.FinishDowntime(ctx, "a", testNow); err != nil {
			t.Fatalf("FinishDowntime: %v", err)
		}
		for _, target := range []string{"slack", "ntfy"} {
			if keys := unnotifiedKeys(t, s, target); keys != "a" {
				t.Errorf("%s: Unnotified = %q, want a", target, keys)
			}
		}
	})
}

func TestSyncTargets(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	reported := firingRecord(t, s, "reported")
	firingRecord(t, s, "waiting") // its first message has not gone out anywhere
	mustMarkFiring(t, s, "mail", reported, testNow)

	added, removed, err := s.SyncTargets(ctx, []string{"mail", "slack"}, testNow)
	if err != nil || len(added) != 2 || len(removed) != 0 {
		t.Fatalf("first SyncTargets = %v, %v, %v, want two added", added, removed, err)
	}
	// A new target starts from the current state: no history, but what is
	// still waiting for its first message.
	if keys := unnotifiedKeys(t, s, "slack"); keys != "waiting" {
		t.Errorf("slack: Unnotified = %q, want waiting", keys)
	}

	// The marker of a known target is kept, even if it has delivered nothing.
	if _, _, err := s.SyncTargets(ctx, []string{"mail", "slack"}, testNow); err != nil {
		t.Fatalf("second SyncTargets: %v", err)
	}
	mustMarkFiring(t, s, "mail", reported, testNow.Add(time.Hour))
	if keys := unnotifiedKeys(t, s, "slack"); keys != "waiting" {
		t.Errorf("after a restart: slack: Unnotified = %q, want waiting", keys)
	}

	// A removed target is forgotten; when it comes back, it starts afresh.
	added, removed, err = s.SyncTargets(ctx, []string{"mail"}, testNow)
	if err != nil || len(added) != 0 || len(removed) != 1 || removed[0] != "slack" {
		t.Fatalf("SyncTargets without slack = %v, %v, %v, want slack removed", added, removed, err)
	}
	if n := countDeliveries(t, s, "slack"); n != 0 {
		t.Errorf("%d deliveries of slack left, want none", n)
	}
}

func TestRetentionRemovesDeliveries(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	old := newRecord("old", StateResolved, testNow.Add(-48*time.Hour))
	resolvedAt := testNow.Add(-time.Hour)
	old.ResolvedAt = &resolvedAt
	id := insertedID(t, s, old)
	if err := s.MarkResolvedNotified(ctx, "slack", []int64{id}, resolvedAt); err != nil {
		t.Fatalf("MarkResolvedNotified: %v", err)
	}

	if n, err := s.DeleteResolvedBefore(ctx, testNow); err != nil || n != 1 {
		t.Fatalf("DeleteResolvedBefore = %d, %v, want 1", n, err)
	}
	if n := countDeliveries(t, s, "slack"); n != 0 {
		t.Errorf("%d deliveries left, want none", n)
	}
}

// A database from before phase 13 has markers only in findings. After the
// upgrade, the mail target must not report anything again.
func TestUpgradeSendsNothingTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	s, err := Open(path, func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	id := firingRecord(t, s, "a")
	_, err = s.db.ExecContext(ctx, `DROP TABLE deliveries; DROP TABLE targets;
		DELETE FROM schema_migrations WHERE version = 5;
		UPDATE findings SET firing_notified_at = ? WHERE id = ?`, unix(testNow), id)
	if err != nil {
		t.Fatalf("turn the database back into the old schema: %v", err)
	}
	_ = s.Close()

	s, err = Open(path, func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("Open after the upgrade: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, _, err := s.SyncTargets(ctx, []string{"mail"}, testNow); err != nil {
		t.Fatalf("SyncTargets: %v", err)
	}
	if keys := unnotifiedKeys(t, s, "mail"); keys != "" {
		t.Errorf("mail: Unnotified = %q, want nothing", keys)
	}
}

func countDeliveries(t *testing.T, s *Store, target string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM deliveries WHERE target = ?`, target).Scan(&n); err != nil {
		t.Fatalf("count deliveries: %v", err)
	}
	return n
}
