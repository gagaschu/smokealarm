package store

import (
	"context"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

func newDowntime(key string, expiresAt time.Time) Downtime {
	return Downtime{
		Key:       key,
		Check:     "pod-crashloop",
		Object:    checks.ObjectRef{Kind: "Pod", Namespace: "media", Name: "jellyfin"},
		Reason:    "CrashLoopBackOff",
		CreatedAt: testNow,
		ExpiresAt: expiresAt,
	}
}

func mustSetDowntime(t *testing.T, s *Store, d Downtime) {
	t.Helper()
	if err := s.SetDowntime(context.Background(), d); err != nil {
		t.Fatalf("SetDowntime: %v", err)
	}
}

func TestSetDowntimeStoresAllFields(t *testing.T) {
	s, _ := openTemp(t)
	want := newDowntime("a", testNow.Add(time.Hour))
	mustSetDowntime(t, s, want)

	got, err := s.ActiveDowntimes(context.Background(), testNow)
	if err != nil || len(got) != 1 {
		t.Fatalf("ActiveDowntimes = %v, %v", got, err)
	}
	want.ID = got[0].ID
	if got[0] != want {
		t.Errorf("got %+v, want %+v", got[0], want)
	}
}

func TestSetDowntimeReplacesTheOldOneForTheSameKey(t *testing.T) {
	s, _ := openTemp(t)
	mustSetDowntime(t, s, newDowntime("a", testNow.Add(time.Hour)))
	mustSetDowntime(t, s, newDowntime("a", testNow.Add(5*time.Hour)))

	got, err := s.ActiveDowntimes(context.Background(), testNow)
	if err != nil || len(got) != 1 {
		t.Fatalf("ActiveDowntimes = %v, %v, want one downtime", got, err)
	}
	if !got[0].ExpiresAt.Equal(testNow.Add(5 * time.Hour)) {
		t.Errorf("ExpiresAt = %v, want the new time", got[0].ExpiresAt)
	}
}

func TestActiveAndExpiredDowntimesAtTheBoundary(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	mustSetDowntime(t, s, newDowntime("before", testNow.Add(-time.Second)))
	mustSetDowntime(t, s, newDowntime("exactly", testNow))
	mustSetDowntime(t, s, newDowntime("after", testNow.Add(time.Second)))

	active, err := s.ActiveDowntimes(ctx, testNow)
	if err != nil {
		t.Fatalf("ActiveDowntimes: %v", err)
	}
	if len(active) != 1 || active[0].Key != "after" {
		t.Errorf("active = %+v, want only after", active)
	}

	expired, err := s.ExpiredDowntimes(ctx, testNow)
	if err != nil {
		t.Fatalf("ExpiredDowntimes: %v", err)
	}
	if len(expired) != 2 || expired[0].Key != "before" || expired[1].Key != "exactly" {
		t.Errorf("expired = %+v, want before and exactly", expired)
	}

	keys, err := s.DowntimeKeys(ctx)
	if err != nil {
		t.Fatalf("DowntimeKeys: %v", err)
	}
	if len(keys) != 3 {
		t.Errorf("keys = %v, want all three until they are finished", keys)
	}
}

func TestEndDowntimeOnlyOnce(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	mustSetDowntime(t, s, newDowntime("a", testNow.Add(time.Hour)))
	active, err := s.ActiveDowntimes(ctx, testNow)
	if err != nil || len(active) != 1 {
		t.Fatalf("ActiveDowntimes = %v, %v", active, err)
	}

	key, changed, err := s.EndDowntime(ctx, active[0].ID, testNow)
	if err != nil || !changed || key != "a" {
		t.Fatalf("EndDowntime = %q, %v, %v, want a, true, nil", key, changed, err)
	}
	if _, changed, err := s.EndDowntime(ctx, active[0].ID, testNow); err != nil || changed {
		t.Errorf("second EndDowntime: changed = %v, %v, want false, nil", changed, err)
	}
	if _, changed, err := s.EndDowntime(ctx, 42, testNow); err != nil || changed {
		t.Errorf("unknown id: changed = %v, %v, want false, nil", changed, err)
	}

	expired, err := s.ExpiredDowntimes(ctx, testNow)
	if err != nil || len(expired) != 1 {
		t.Errorf("ExpiredDowntimes = %v, %v, want the ended downtime", expired, err)
	}
}

func TestFinishDowntimeClearsTheMarkerOfFiringProblems(t *testing.T) {
	tests := []struct {
		name        string
		state       State
		acknowledge bool
		wantCleared bool
	}{
		{"firing", StateFiring, false, true},
		{"firing and acknowledged", StateFiring, true, false},
		{"resolved", StateResolved, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := openTemp(t)
			ctx := context.Background()
			id := insertedID(t, s, newRecord("a", tt.state, testNow))
			if err := s.MarkFiringNotified(ctx, []int64{id}, testNow); err != nil {
				t.Fatalf("MarkFiringNotified: %v", err)
			}
			if tt.acknowledge {
				if _, _, err := s.Acknowledge(ctx, id, testNow); err != nil {
					t.Fatalf("Acknowledge: %v", err)
				}
			}
			mustSetDowntime(t, s, newDowntime("a", testNow))

			if err := s.FinishDowntime(ctx, "a", testNow); err != nil {
				t.Fatalf("FinishDowntime: %v", err)
			}

			record, found, err := s.Record(ctx, id)
			if err != nil || !found {
				t.Fatalf("Record = %v, %v", found, err)
			}
			if cleared := record.FiringNotifiedAt == nil; cleared != tt.wantCleared {
				t.Errorf("marker cleared = %v, want %v", cleared, tt.wantCleared)
			}
			keys, err := s.DowntimeKeys(ctx)
			if err != nil || len(keys) != 0 {
				t.Errorf("DowntimeKeys = %v, %v, want no downtime left", keys, err)
			}
		})
	}
}

// Between reading the expired downtimes and finishing them, somebody can set a
// new end in the web UI.
func TestFinishDowntimeLeavesAnExtendedDowntimeAlone(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	id := insertedID(t, s, newRecord("a", StateFiring, testNow))
	if err := s.MarkFiringNotified(ctx, []int64{id}, testNow); err != nil {
		t.Fatalf("MarkFiringNotified: %v", err)
	}
	mustSetDowntime(t, s, newDowntime("a", testNow.Add(time.Hour)))

	if err := s.FinishDowntime(ctx, "a", testNow); err != nil {
		t.Fatalf("FinishDowntime: %v", err)
	}

	keys, err := s.DowntimeKeys(ctx)
	if err != nil || !keys["a"] {
		t.Errorf("DowntimeKeys = %v, %v, want the downtime to stay", keys, err)
	}
	record, _, err := s.Record(ctx, id)
	if err != nil || record.FiringNotifiedAt == nil {
		t.Errorf("record = %+v, %v, want the marker to stay", record, err)
	}
}

func TestRecordByID(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	id := insertedID(t, s, newRecord("a", StateResolved, testNow))

	record, found, err := s.Record(ctx, id)
	if err != nil || !found || record.Key != "a" {
		t.Errorf("Record = %+v, %v, %v, want a", record, found, err)
	}
	if _, found, err := s.Record(ctx, id+1); err != nil || found {
		t.Errorf("unknown id: found = %v, %v, want false, nil", found, err)
	}
}

func TestDowntimesSurviveReopening(t *testing.T) {
	s, path := openTemp(t)
	mustSetDowntime(t, s, newDowntime("a", testNow.Add(time.Hour)))
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(path, func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = again.Close() }()

	active, err := again.ActiveDowntimes(context.Background(), testNow)
	if err != nil || len(active) != 1 {
		t.Errorf("ActiveDowntimes = %v, %v, want the downtime", active, err)
	}
}
