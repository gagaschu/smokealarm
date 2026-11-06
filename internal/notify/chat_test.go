package notify

import (
	"fmt"
	"strings"
	"testing"
)

const testBaseURL = "https://smokealarm.example.com"

// mixed is a notification with something in every list, and a node that is
// newly down with the problems it holds back.
func mixed() Notification {
	return Notification{
		New:       []Alert{nodeDownWithAffected(), crashLoop(), replicasUnavailable()},
		Reminders: []Alert{nodeNotReady()},
		Resolved:  []Alert{resolved(replicasUnavailable())},
		BaseURL:   testBaseURL,
	}
}

// manyPods is a notification with more new problems than any chat message
// can hold.
func manyPods(count int, baseURL string) Notification {
	n := Notification{BaseURL: baseURL}
	for i := range count {
		a := crashLoop()
		a.Object.Name = fmt.Sprintf("app-%03d", i)
		a.Key = "pod-crashloop/Pod/media/" + a.Object.Name + "/CrashLoopBackOff"
		n.New = append(n.New, a)
	}
	n.Resolved = []Alert{resolved(replicasUnavailable())}
	return n
}

func TestChatBodyGolden(t *testing.T) {
	tests := []struct {
		name  string
		n     Notification
		limit int
	}{
		{"chat_mixed_with_url", mixed(), 4000},
		{"chat_single_warning_without_url", Notification{New: []Alert{replicasUnavailable()}}, 4000},
		{"chat_resolved_only", Notification{Resolved: []Alert{resolved(crashLoop()), resolved(nodeNotReady())}}, 4000},
		{"chat_cut_with_url", manyPods(40, testBaseURL), 600},
		{"chat_cut_without_url", manyPods(40, ""), 600},
		// The cut falls right after a heading: the heading goes too.
		{"chat_cut_drops_lonely_heading", Notification{
			New:      []Alert{crashLoop()},
			Resolved: []Alert{resolved(replicasUnavailable()), resolved(nodeNotReady())},
		}, 140},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ChatBody(tt.n, tt.limit)
			if len(got) > tt.limit {
				t.Errorf("length %d is over the limit %d", len(got), tt.limit)
			}
			checkGolden(t, tt.name+".golden", got)
		})
	}
}

// Whatever the limit, the text fits and says how much is missing.
func TestChatBodyAlwaysFits(t *testing.T) {
	n := manyPods(200, testBaseURL)
	if got := ChatBody(n, 60); got != "… and 201 more, see "+testBaseURL {
		t.Errorf("nothing fits: got %q", got)
	}
	for limit := 200; limit <= 20000; limit += 97 {
		got := ChatBody(n, limit)
		if len(got) > limit {
			t.Fatalf("limit %d: length %d", limit, len(got))
		}
		shown := strings.Count(got, "\n- ")
		if shown == 201 {
			continue // everything fits
		}
		if !strings.HasPrefix(got, "New:\n- ") || !strings.Contains(got, fmt.Sprintf("… and %d more, see %s", 201-shown, testBaseURL)) {
			t.Fatalf("limit %d: %d problems shown, but the last line does not count the rest:\n%s", limit, shown, got)
		}
	}
}

func TestCutText(t *testing.T) {
	tests := []struct {
		in    string
		limit int
		want  string
	}{
		{"short", 10, "short"},
		{"exactly ten", 11, "exactly ten"},
		{"a bit too long", 10, "a bit t…"},
		{"müßig und lang", 7, "mü…"}, // never splits ß, which takes two bytes
		{"no room for the dots", 2, "no"},
		{"müßig", 2, "m"},
		{"nothing", 0, ""},
		{"less than nothing", -5, ""},
	}
	for _, tt := range tests {
		if got := cutText(tt.in, tt.limit); got != tt.want || len(got) > max(tt.limit, 0) {
			t.Errorf("cutText(%q, %d) = %q, want %q", tt.in, tt.limit, got, tt.want)
		}
	}
}

func TestNtfyPriority(t *testing.T) {
	warning := replicasUnavailable()
	tests := []struct {
		name         string
		n            Notification
		wantPriority string
		wantTags     string
	}{
		{"new critical", Notification{New: []Alert{warning, crashLoop()}, Resolved: []Alert{resolved(warning)}}, "urgent", "rotating_light"},
		{"reminder only", Notification{Reminders: []Alert{crashLoop()}}, "high", "rotating_light"},
		{"reminder and new warning", Notification{New: []Alert{warning}, Reminders: []Alert{crashLoop()}}, "high", "rotating_light"},
		{"new warning", Notification{New: []Alert{warning}, Resolved: []Alert{resolved(crashLoop())}}, "default", "warning"},
		{"resolved only", Notification{Resolved: []Alert{resolved(crashLoop())}}, "low", "white_check_mark"},
	}
	for _, tt := range tests {
		priority, tags := ntfyPriority(tt.n)
		if priority != tt.wantPriority || tags != tt.wantTags {
			t.Errorf("%s: got %s/%s, want %s/%s", tt.name, priority, tags, tt.wantPriority, tt.wantTags)
		}
	}
}
