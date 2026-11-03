package checks

import (
	"strings"
	"testing"
)

func TestSelect(t *testing.T) {
	all, err := Select(nil)
	if err != nil {
		t.Fatalf("Select(nil): %v", err)
	}
	if len(all) != len(All()) {
		t.Errorf("Select(nil) returned %d checks, want %d", len(all), len(All()))
	}

	some, err := Select([]string{"pod-restarts", "pod-pending"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(some) != len(All())-2 {
		t.Errorf("got %d checks, want %d", len(some), len(All())-2)
	}
	for _, c := range some {
		if c.Name == "pod-restarts" || c.Name == "pod-pending" {
			t.Errorf("check %s should be disabled", c.Name)
		}
	}
}

func TestSelectRejectsUnknownName(t *testing.T) {
	_, err := Select([]string{"pod-crashloo"})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{`"pod-crashloo"`, "pod-crashloop"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestCheckNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range All() {
		if seen[c.Name] {
			t.Errorf("duplicate check name %s", c.Name)
		}
		seen[c.Name] = true
	}
}
