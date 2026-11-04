package web

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/engine"
	"github.com/gagaschu/smokealarm/internal/store"
)

func TestOverviewAndHistoryShowTheTargetsAnnotation(t *testing.T) {
	e := newTestEnv(t)
	e.status.LastRun = testNow
	routed := problem("routed", "media", checks.Critical, store.StateFiring, testNow.Add(-time.Hour), "broken")
	routed.Targets = []string{"mail", "slack"}
	everywhere := problem("everywhere", "media", checks.Critical, store.StateFiring, testNow.Add(-time.Hour), "broken")
	gone := resolvedProblem("gone", "media", testNow.Add(-2*time.Hour), testNow.Add(-time.Hour))
	gone.Targets = []string{"ntfy"}
	e.insert(routed, everywhere, gone)

	overview := e.do(http.MethodGet, "/", nil, e.login()).Body.String()
	if got := strings.Count(overview, "Notifies only:"); got != 1 {
		t.Errorf("overview shows %d target lines, want 1:\n%s", got, overview)
	}
	if !strings.Contains(overview, "Notifies only: mail, slack") {
		t.Errorf("overview does not name the targets:\n%s", overview)
	}

	history := e.do(http.MethodGet, "/history", nil, e.login()).Body.String()
	if !strings.Contains(history, "only ntfy") {
		t.Errorf("history does not name the targets:\n%s", history)
	}
}

func TestStatusPageShowsWhatEachTargetHears(t *testing.T) {
	e := newTestEnv(t)
	e.status = engine.Status{Targets: []engine.TargetStatus{
		{Name: "ntfy", Description: "ntfy", CriticalOnly: true},
		{Name: "slack", Description: "slack"},
	}}

	body := e.do(http.MethodGet, "/status", nil, e.login()).Body.String()
	ntfy, slack := strings.Index(body, "critical problems only"), strings.Index(body, "all problems")
	if ntfy < 0 || slack < 0 || ntfy > slack {
		t.Errorf("want ntfy with critical problems only, then slack with all problems:\n%s", body)
	}
}
