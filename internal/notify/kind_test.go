package notify

import "testing"

func TestKind(t *testing.T) {
	for name, want := range map[string]string{
		"mail":              "mail",
		"slack":             "slack",
		"telegram:-1001234": "telegram",
		"telegram:@ops":     "telegram",
		"log":               "log",
	} {
		if got := Kind(name); got != want {
			t.Errorf("Kind(%q) = %q, want %q", name, got, want)
		}
	}
}
