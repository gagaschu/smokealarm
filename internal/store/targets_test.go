package store

import (
	"context"
	"slices"
	"testing"
	"time"
)

func TestTargetsAreStoredAndUpdated(t *testing.T) {
	s, _ := openTemp(t)
	ctx := context.Background()
	r := newRecord("a", StateFiring, testNow)
	r.Targets = []string{"mail", "slack"}
	r.ID = insertedID(t, s, r)

	records, err := s.OpenRecords(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("OpenRecords = %v, %v", records, err)
	}
	if !slices.Equal(records[0].Targets, []string{"mail", "slack"}) {
		t.Errorf("Targets = %v, want [mail slack]", records[0].Targets)
	}

	// The annotation was removed: the problem goes to every target again.
	r.Targets = nil
	if err := s.Apply(ctx, Changes{Update: []Record{r}}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	unnotified, err := s.Unnotified(ctx, testTarget)
	if err != nil || len(unnotified) != 1 {
		t.Fatalf("Unnotified = %v, %v", unnotified, err)
	}
	if unnotified[0].Targets != nil {
		t.Errorf("Targets = %v, want none after the update", unnotified[0].Targets)
	}
}

func TestTargetsMigrationKeepsExistingRows(t *testing.T) {
	s, path := openTemp(t)
	ctx := context.Background()
	insertedID(t, s, newRecord("a", StateFiring, testNow))

	// Back to the schema before the migration.
	for _, statement := range []string{
		`ALTER TABLE findings DROP COLUMN targets`,
		`DELETE FROM schema_migrations WHERE version = 6`,
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
	if records[0].Targets != nil {
		t.Errorf("Targets = %v, want none (every target) for an existing row", records[0].Targets)
	}
}
