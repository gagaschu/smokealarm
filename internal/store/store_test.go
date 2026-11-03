package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

var testNow = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path, func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func newRecord(key string, state State, firstSeen time.Time) Record {
	return Record{
		Finding: checks.Finding{
			Key:      key,
			Check:    "pod-crashloop",
			Severity: checks.Critical,
			Object:   checks.ObjectRef{Kind: "Pod", Namespace: "media", Name: "jellyfin"},
			Reason:   "CrashLoopBackOff",
			Message:  "container app keeps crashing",
		},
		State:     state,
		FirstSeen: firstSeen,
		LastSeen:  firstSeen,
	}
}

func TestOpenRunsMigrationsOnce(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	if err := s.Apply(ctx, Changes{Insert: []Record{newRecord("a", StatePending, testNow)}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Opening again must keep the data and not rerun 001 (which would fail
	// with "table already exists").
	again, err := Open(path, func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer func() { _ = again.Close() }()

	records, err := again.OpenRecords(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("got %d records, err %v; want the 1 stored record", len(records), err)
	}

	var applied int
	if err := again.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatal(err)
	}
	if applied != len(mustMigrationFiles(t)) {
		t.Errorf("schema_migrations has %d rows, want one per migration file", applied)
	}
}

func mustMigrationFiles(t *testing.T) []string {
	t.Helper()
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestMigrationVersion(t *testing.T) {
	tests := []struct {
		file    string
		want    int
		wantErr bool
	}{
		{"001_findings.sql", 1, false},
		{"012_more.sql", 12, false},
		{"findings.sql", 0, true},
		{"abc_findings.sql", 0, true},
	}
	for _, tt := range tests {
		got, err := migrationVersion(tt.file)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("migrationVersion(%q) = %d, %v; want %d, error %v", tt.file, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestApplyInsertUpdateDelete(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	err := s.Apply(ctx, Changes{Insert: []Record{
		newRecord("a", StatePending, testNow),
		newRecord("b", StatePending, testNow),
	}})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	records, err := s.OpenRecords(ctx)
	if err != nil || len(records) != 2 {
		t.Fatalf("got %d records, err %v; want 2", len(records), err)
	}
	a, b := records[0], records[1]
	if a.Key != "a" || a.Object.Name != "jellyfin" || a.Severity != checks.Critical || !a.FirstSeen.Equal(testNow) {
		t.Errorf("record not stored faithfully: %+v", a)
	}
	if a.FiringAt != nil || a.ResolvedAt != nil || a.FiringNotifiedAt != nil {
		t.Errorf("times that were not set must be nil: %+v", a)
	}

	firingAt := testNow.Add(2 * time.Minute)
	a.State, a.FiringAt, a.LastSeen, a.Message = StateFiring, &firingAt, firingAt, "new message"
	if err := s.Apply(ctx, Changes{Update: []Record{a}, Delete: []int64{b.ID}}); err != nil {
		t.Fatalf("update/delete: %v", err)
	}

	records, err = s.OpenRecords(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("got %d records, err %v; want 1 after delete", len(records), err)
	}
	got := records[0]
	if got.State != StateFiring || got.FiringAt == nil || !got.FiringAt.Equal(firingAt) || got.Message != "new message" {
		t.Errorf("update not stored: %+v", got)
	}
}

func TestApplyIsAtomic(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	// The second insert violates the unique index on open keys, so the first
	// one must be rolled back too.
	err := s.Apply(ctx, Changes{Insert: []Record{
		newRecord("a", StatePending, testNow),
		newRecord("a", StatePending, testNow),
	}})
	if err == nil {
		t.Fatal("expected an error for a duplicate open key")
	}
	records, err := s.OpenRecords(ctx)
	if err != nil || len(records) != 0 {
		t.Errorf("got %d records, err %v; want none after a failed Apply", len(records), err)
	}
}

func TestUniqueIndexOnlyCoversOpenRecords(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	resolvedAt := testNow.Add(time.Hour)
	old := newRecord("a", StateResolved, testNow)
	old.ResolvedAt = &resolvedAt
	// A resolved record and a new open record with the same key may coexist:
	// that is how a problem that comes back after being fixed is stored.
	err := s.Apply(ctx, Changes{Insert: []Record{old, newRecord("a", StatePending, resolvedAt)}})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func TestUnnotifiedAndMarkers(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	firingAt := testNow
	resolvedAt := testNow.Add(time.Hour)
	pending := newRecord("pending", StatePending, testNow)
	firing := newRecord("firing", StateFiring, testNow)
	firing.FiringAt = &firingAt
	resolved := newRecord("resolved", StateResolved, testNow)
	resolved.FiringAt, resolved.ResolvedAt = &firingAt, &resolvedAt

	if err := s.Apply(ctx, Changes{Insert: []Record{pending, firing, resolved}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := s.Unnotified(ctx)
	if err != nil {
		t.Fatalf("Unnotified: %v", err)
	}
	if keys := recordKeys(got); keys != "firing,resolved" {
		t.Fatalf("Unnotified = %s, want firing,resolved (pending must not be reported)", keys)
	}

	notifiedAt := testNow.Add(2 * time.Hour)
	if err := s.MarkFiringNotified(ctx, []int64{got[0].ID}, notifiedAt); err != nil {
		t.Fatalf("MarkFiringNotified: %v", err)
	}
	got, _ = s.Unnotified(ctx)
	if keys := recordKeys(got); keys != "resolved" {
		t.Fatalf("after marking firing: Unnotified = %s, want resolved", keys)
	}
	if got[0].FiringNotifiedAt != nil {
		t.Error("the resolved record never had its firing reported")
	}

	if err := s.MarkResolvedNotified(ctx, []int64{got[0].ID}, notifiedAt); err != nil {
		t.Fatalf("MarkResolvedNotified: %v", err)
	}
	if got, _ = s.Unnotified(ctx); len(got) != 0 {
		t.Errorf("after marking everything: Unnotified = %s, want none", recordKeys(got))
	}
}

func TestDeleteResolvedBefore(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()

	cutoff := testNow
	resolvedAt := func(d time.Duration) *time.Time { t := cutoff.Add(d); return &t }

	oldResolved := newRecord("old", StateResolved, testNow.Add(-48*time.Hour))
	oldResolved.ResolvedAt = resolvedAt(-time.Hour)
	recentResolved := newRecord("recent", StateResolved, testNow.Add(-48*time.Hour))
	recentResolved.ResolvedAt = resolvedAt(time.Hour)
	// An old but still open problem must never be deleted.
	stillOpen := newRecord("open", StateFiring, testNow.Add(-90*24*time.Hour))

	if err := s.Apply(ctx, Changes{Insert: []Record{oldResolved, recentResolved, stillOpen}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	n, err := s.DeleteResolvedBefore(ctx, cutoff)
	if err != nil || n != 1 {
		t.Fatalf("DeleteResolvedBefore = %d, %v; want 1", n, err)
	}

	open, _ := s.OpenRecords(ctx)
	unnotified, _ := s.Unnotified(ctx)
	if recordKeys(open) != "open" || recordKeys(unnotified) != "recent,open" {
		t.Errorf("open = %s, unnotified = %s; want only the old resolved record gone",
			recordKeys(open), recordKeys(unnotified))
	}
}

func recordKeys(records []Record) string {
	keys := ""
	for i, r := range records {
		if i > 0 {
			keys += ","
		}
		keys += r.Key
	}
	return keys
}
