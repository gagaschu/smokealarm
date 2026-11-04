package store

import (
	"context"
	"testing"
	"time"
)

func TestCausedByAndPendingSinceAreStoredAndUpdated(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	r := newRecord("a", StatePending, testNow)
	r.CausedBy = "pi-2"
	r.ID = insertedID(t, s, r)

	records, err := s.OpenRecords(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("OpenRecords = %v, %v", records, err)
	}
	if records[0].CausedBy != "pi-2" {
		t.Errorf("CausedBy = %q, want pi-2", records[0].CausedBy)
	}
	if !records[0].PendingSince.Equal(testNow) {
		t.Errorf("PendingSince = %v, want first_seen %v", records[0].PendingSince, testNow)
	}

	// The node is back: the cause is gone and the clock starts again.
	back := testNow.Add(10 * time.Minute)
	r.CausedBy, r.PendingSince = "", back
	if err := s.Apply(ctx, Changes{Update: []Record{r}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	unnotified, err := s.Unnotified(ctx, testTarget)
	if err != nil {
		t.Fatalf("Unnotified: %v", err)
	}
	records, err = s.OpenRecords(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("OpenRecords = %v, %v", records, err)
	}
	got := records[0]
	if got.CausedBy != "" || !got.PendingSince.Equal(back) || !got.FirstSeen.Equal(testNow) {
		t.Errorf("got CausedBy %q, PendingSince %v, FirstSeen %v; want none, %v, %v",
			got.CausedBy, got.PendingSince, got.FirstSeen, back, testNow)
	}
	if len(unnotified) != 0 {
		t.Errorf("Unnotified = %v, want nothing for a pending problem", unnotified)
	}
}

func TestNodeCauseMigrationKeepsExistingRows(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	insertedID(t, s, newRecord("a", StateFiring, testNow))

	// Back to the schema before the migration.
	for _, statement := range []string{
		`ALTER TABLE findings DROP COLUMN caused_by`,
		`ALTER TABLE findings DROP COLUMN pending_since`,
		`DELETE FROM schema_migrations WHERE version = 7`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	again, err := Open(path, func() time.Time { return testNow })
	if err != nil {
		t.Fatalf("Open after upgrade: %v", err)
	}
	defer func() { _ = again.Close() }()

	records, err := again.OpenRecords(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("OpenRecords = %v, %v, want the existing row", records, err)
	}
	if records[0].CausedBy != "" || !records[0].PendingSince.Equal(records[0].FirstSeen) {
		t.Errorf("got CausedBy %q, PendingSince %v; want none and first_seen", records[0].CausedBy, records[0].PendingSince)
	}
}
