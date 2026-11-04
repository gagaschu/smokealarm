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

// fakeAudience stands in for engine.Engine.Audience: problems with "only" in
// the key reach slack, "nobody" none, the rest every target.
func fakeAudience(rec store.Record) ([]string, bool) {
	switch {
	case strings.Contains(rec.Key, "only"):
		return []string{"slack", "mail (smtp.example.com)"}, false
	case strings.Contains(rec.Key, "nobody"):
		return nil, false
	}
	return []string{"slack", "mail (smtp.example.com)", "ntfy"}, true
}

func TestOverviewShowsWhoIsNotified(t *testing.T) {
	e := newTestEnv(t)
	e.server.Audience = fakeAudience
	e.status.LastRun = testNow
	e.insert(
		problem("only-some", "media", checks.Critical, store.StateFiring, testNow.Add(-time.Hour), "broken"),
		problem("nobody", "media", checks.Warning, store.StateFiring, testNow.Add(-time.Hour), "broken"),
		problem("everywhere", "media", checks.Critical, store.StateFiring, testNow.Add(-time.Hour), "broken"),
	)

	body := e.do(http.MethodGet, "/", nil, e.login()).Body.String()
	if got := strings.Count(body, "Notifies only: slack, mail (smtp.example.com)"); got != 1 {
		t.Errorf("overview shows the target line %d times, want once:\n%s", got, body)
	}
	if got := strings.Count(body, "Notifies nobody"); got != 1 {
		t.Errorf("overview shows \"Notifies nobody\" %d times, want once:\n%s", got, body)
	}
}

func TestHistoryShowsWhoIsNotified(t *testing.T) {
	e := newTestEnv(t)
	e.server.Audience = fakeAudience
	e.insert(
		resolvedProblem("only-some", "media", testNow.Add(-2*time.Hour), testNow.Add(-time.Hour)),
		resolvedProblem("nobody", "media", testNow.Add(-2*time.Hour), testNow.Add(-time.Hour)),
		resolvedProblem("everywhere", "media", testNow.Add(-2*time.Hour), testNow.Add(-time.Hour)),
	)

	body := e.do(http.MethodGet, "/history", nil, e.login()).Body.String()
	if got := strings.Count(body, "only slack, mail (smtp.example.com)"); got != 1 {
		t.Errorf("history shows the target line %d times, want once:\n%s", got, body)
	}
	if got := strings.Count(body, "nobody notified"); got != 1 {
		t.Errorf("history shows \"nobody notified\" %d times, want once:\n%s", got, body)
	}
}

func TestPagesSayNothingWithoutAudience(t *testing.T) {
	e := newTestEnv(t) // Audience is nil
	e.status.LastRun = testNow
	e.insert(problem("only-some", "media", checks.Critical, store.StateFiring, testNow.Add(-time.Hour), "broken"))

	body := e.do(http.MethodGet, "/", nil, e.login()).Body.String()
	if strings.Contains(body, "Notifies") {
		t.Errorf("overview talks about targets without knowing them:\n%s", body)
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
