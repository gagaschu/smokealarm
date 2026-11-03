package engine

import (
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/store"
)

func reconcileEngine(alertAfter time.Duration) *Engine {
	return &Engine{
		AlertAfter: alertAfter,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func openRecord(id int64, key, check string, state store.State, firstSeen time.Time) store.Record {
	return store.Record{
		ID:        id,
		Finding:   checks.Finding{Key: key, Check: check, Severity: checks.Warning, Message: "old message"},
		State:     state,
		FirstSeen: firstSeen,
		LastSeen:  firstSeen,
	}
}

func TestReconcile(t *testing.T) {
	now := testStart
	f := checks.Finding{Key: "k", Check: "fake", Severity: checks.Critical, Message: "new message"}

	tests := []struct {
		name       string
		alertAfter time.Duration
		open       []store.Record
		findings   []checks.Finding
		skipped    []string

		wantInsert []store.State // states of inserted records
		wantUpdate []store.State // states of updated records
		wantDelete int
	}{
		{
			name:       "new finding starts pending",
			alertAfter: 2 * time.Minute,
			findings:   []checks.Finding{f},
			wantInsert: []store.State{store.StatePending},
		},
		{
			name:       "new finding fires at once when AlertAfter is zero",
			alertAfter: 0,
			findings:   []checks.Finding{f},
			wantInsert: []store.State{store.StateFiring},
		},
		{
			name:       "pending just below AlertAfter stays pending",
			alertAfter: 2 * time.Minute,
			open:       []store.Record{openRecord(1, "k", "fake", store.StatePending, now.Add(-time.Minute-59*time.Second))},
			findings:   []checks.Finding{f},
			wantUpdate: []store.State{store.StatePending},
		},
		{
			name:       "pending at AlertAfter fires",
			alertAfter: 2 * time.Minute,
			open:       []store.Record{openRecord(1, "k", "fake", store.StatePending, now.Add(-2*time.Minute))},
			findings:   []checks.Finding{f},
			wantUpdate: []store.State{store.StateFiring},
		},
		{
			name:       "firing stays firing",
			alertAfter: 2 * time.Minute,
			open:       []store.Record{openRecord(1, "k", "fake", store.StateFiring, now.Add(-time.Hour))},
			findings:   []checks.Finding{f},
			wantUpdate: []store.State{store.StateFiring},
		},
		{
			name:       "pending that disappears is dropped",
			alertAfter: 2 * time.Minute,
			open:       []store.Record{openRecord(1, "k", "fake", store.StatePending, now.Add(-time.Minute))},
			wantDelete: 1,
		},
		{
			name:       "firing that disappears is resolved",
			alertAfter: 2 * time.Minute,
			open:       []store.Record{openRecord(1, "k", "fake", store.StateFiring, now.Add(-time.Hour))},
			wantUpdate: []store.State{store.StateResolved},
		},
		{
			name:       "problem of a skipped check is left alone",
			alertAfter: 2 * time.Minute,
			open:       []store.Record{openRecord(1, "k", "fake", store.StateFiring, now.Add(-time.Hour))},
			skipped:    []string{"fake"},
		},
		{
			name:       "problem of a disabled check is resolved",
			alertAfter: 2 * time.Minute,
			open:       []store.Record{openRecord(1, "k", "removed-check", store.StateFiring, now.Add(-time.Hour))},
			skipped:    []string{"fake"},
			wantUpdate: []store.State{store.StateResolved},
		},
		{
			name:       "same key twice in one run is handled once",
			alertAfter: 2 * time.Minute,
			findings:   []checks.Finding{f, f},
			wantInsert: []store.State{store.StatePending},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			changes := reconcileEngine(tt.alertAfter).reconcile(tt.open, tt.findings, tt.skipped, now)

			if got := states(changes.Insert); !slices.Equal(got, tt.wantInsert) {
				t.Errorf("inserted states = %v, want %v", got, tt.wantInsert)
			}
			if got := states(changes.Update); !slices.Equal(got, tt.wantUpdate) {
				t.Errorf("updated states = %v, want %v", got, tt.wantUpdate)
			}
			if len(changes.Delete) != tt.wantDelete {
				t.Errorf("deleted %d, want %d", len(changes.Delete), tt.wantDelete)
			}
		})
	}
}

func TestReconcileRefreshesDetails(t *testing.T) {
	now := testStart
	open := openRecord(7, "k", "fake", store.StateFiring, now.Add(-time.Hour))
	f := checks.Finding{Key: "k", Check: "fake", Severity: checks.Critical, Message: "new message"}

	changes := reconcileEngine(2*time.Minute).reconcile([]store.Record{open}, []checks.Finding{f}, nil, now)

	got := changes.Update[0]
	if got.ID != 7 || got.Message != "new message" || got.Severity != checks.Critical || !got.LastSeen.Equal(now) {
		t.Errorf("record not refreshed: %+v", got)
	}
	if !got.FirstSeen.Equal(open.FirstSeen) {
		t.Errorf("FirstSeen changed to %v", got.FirstSeen)
	}
}

func TestReconcileResolvedHasTimestamp(t *testing.T) {
	now := testStart
	open := openRecord(1, "k", "fake", store.StateFiring, now.Add(-time.Hour))

	changes := reconcileEngine(2*time.Minute).reconcile([]store.Record{open}, nil, nil, now)

	if got := changes.Update[0].ResolvedAt; got == nil || !got.Equal(now) {
		t.Errorf("ResolvedAt = %v, want %v", got, now)
	}
}

func states(records []store.Record) []store.State {
	var result []store.State
	for _, r := range records {
		result = append(result, r.State)
	}
	return result
}
