package checks

import (
	"strings"
	"testing"
	"time"
)

func TestAPIConnection(t *testing.T) {
	since := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		problem      *APIProblem
		wantFindings int
		wantMessage  string
	}{
		{name: "connection is fine"},
		{
			name:         "nothing can be read",
			problem:      &APIProblem{Since: since, Blind: true, Error: "connection refused"},
			wantFindings: 1,
			wantMessage:  "the other checks are paused",
		},
		{
			name:         "some resources cannot be read",
			problem:      &APIProblem{Since: since, Resources: []string{"certificates", "pods"}, Error: "forbidden"},
			wantFindings: 1,
			wantMessage:  "Cannot read certificates, pods",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			findings := APIConnection(&Snapshot{Now: since, APIProblem: tt.problem}, Options{})
			if len(findings) != tt.wantFindings {
				t.Fatalf("got %d findings, want %d", len(findings), tt.wantFindings)
			}
			if tt.wantFindings == 0 {
				return
			}
			f := findings[0]
			if f.Severity != Critical || f.Check != CheckAPIConnection {
				t.Errorf("finding = %+v, want a critical %s", f, CheckAPIConnection)
			}
			if !strings.Contains(f.Message, tt.wantMessage) || !strings.Contains(f.Message, tt.problem.Error) {
				t.Errorf("message = %q, want %q and the error", f.Message, tt.wantMessage)
			}
		})
	}
}

// A problem that gets better or worse must stay the same problem, so going from
// some resources to all of them is no "resolved".
func TestAPIConnectionKeepsItsKeyWhenItGetsWorse(t *testing.T) {
	some := APIConnection(&Snapshot{APIProblem: &APIProblem{Resources: []string{"pods"}}}, Options{})
	all := APIConnection(&Snapshot{APIProblem: &APIProblem{Blind: true}}, Options{})
	if some[0].Key != all[0].Key {
		t.Errorf("keys differ: %q, %q", some[0].Key, all[0].Key)
	}
}
