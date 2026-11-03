package engine

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/notify"
	"github.com/gagaschu/smokealarm/internal/store"
)

var testStart = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// fakeNotifier records what it was asked to send. When fail is set it
// refuses, like a mail server that is down.
type fakeNotifier struct {
	attempts int
	sent     []notify.Notification
	fail     bool
}

func (f *fakeNotifier) Name() string { return "fake" }

func (f *fakeNotifier) Send(_ context.Context, n notify.Notification) error {
	f.attempts++
	if f.fail {
		return errors.New("smtp server unreachable")
	}
	f.sent = append(f.sent, n)
	return nil
}

// harness drives an engine with a controllable clock and controllable
// findings, against a real SQLite file.
type harness struct {
	t         *testing.T
	path      string
	engine    *Engine
	notifier  *fakeNotifier
	findings  []checks.Finding
	skipped   map[string]string // what the snapshot reports as unable to run
	checkRuns int               // how often the fake check was executed
	now       time.Time
	logs      *bytes.Buffer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:        t,
		path:     filepath.Join(t.TempDir(), "test.db"),
		notifier: &fakeNotifier{},
		now:      testStart,
		logs:     &bytes.Buffer{},
	}
	h.start()
	return h
}

// start creates the store and engine; calling it again simulates a restart
// of the process with the same database file.
func (h *harness) start() {
	h.t.Helper()
	db, err := store.Open(h.path, func() time.Time { return h.now })
	if err != nil {
		h.t.Fatalf("store.Open: %v", err)
	}
	h.t.Cleanup(func() { _ = db.Close() })

	h.engine = &Engine{
		Snapshot: func(_ context.Context, now time.Time) *checks.Snapshot {
			return &checks.Snapshot{Now: now, Skipped: h.skipped}
		},
		Checks: []checks.Check{{
			Name: "fake",
			Run: func(*checks.Snapshot, checks.Options) []checks.Finding {
				h.checkRuns++
				return h.findings
			},
		}},
		AlertAfter: 2 * time.Minute,
		Retention:  30 * 24 * time.Hour,
		Store:      db,
		Notifier:   h.notifier,
		Now:        func() time.Time { return h.now },
		Logger:     slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

func (h *harness) restart() {
	h.t.Helper()
	if err := h.engine.Store.Close(); err != nil {
		h.t.Fatalf("close store: %v", err)
	}
	h.start()
}

// run moves the clock forward and does one pass.
func (h *harness) run(advance time.Duration) error {
	h.now = h.now.Add(advance)
	return h.engine.RunOnce(context.Background())
}

func (h *harness) mustRun(advance time.Duration) {
	h.t.Helper()
	if err := h.run(advance); err != nil {
		h.t.Fatalf("RunOnce: %v", err)
	}
}

// countRows counts rows in the findings table, optionally for one state.
func (h *harness) countRows(state string) int {
	h.t.Helper()
	db, err := sql.Open("sqlite", h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	query, args := `SELECT COUNT(*) FROM findings`, []any{}
	if state != "" {
		query, args = query+` WHERE state = ?`, []any{state}
	}
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func finding(key string, severity checks.Severity) checks.Finding {
	return checks.Finding{
		Key: key, Check: "fake", Severity: severity,
		Object:  checks.ObjectRef{Kind: "Pod", Namespace: "media", Name: key},
		Reason:  "Broken",
		Message: "it is broken",
	}
}

// escalating builds the same problem (same key) in a given severity, with a
// reason that says how bad it is, like pvc-usage does.
func escalating(severity checks.Severity) checks.Finding {
	f := finding("vol", severity)
	f.Reason = "Volume-" + string(severity)
	return f
}

func alertKeys(alerts []notify.Alert) string {
	var keys []string
	for _, a := range alerts {
		keys = append(keys, a.Key)
	}
	return strings.Join(keys, ",")
}

func TestLifecyclePendingFiringResolved(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}

	h.mustRun(0) // first seen: pending, nothing to report yet
	if h.countRows("pending") != 1 || len(h.notifier.sent) != 0 {
		t.Fatalf("after first run: %d pending, %d notifications; want 1 and 0", h.countRows("pending"), len(h.notifier.sent))
	}

	h.mustRun(time.Minute) // 1m: still below AlertAfter
	if len(h.notifier.sent) != 0 {
		t.Fatalf("notified after 1m, want to wait for AlertAfter")
	}

	h.mustRun(time.Minute) // 2m: fires
	if len(h.notifier.sent) != 1 || alertKeys(h.notifier.sent[0].New) != "a" || len(h.notifier.sent[0].Resolved) != 0 {
		t.Fatalf("after 2m: sent %+v, want one notification with new problem a", h.notifier.sent)
	}
	if got := h.notifier.sent[0].New[0].Since; !got.Equal(testStart) {
		t.Errorf("Since = %v, want first seen %v", got, testStart)
	}

	h.mustRun(30 * time.Second) // still firing: must not be reported again
	if len(h.notifier.sent) != 1 {
		t.Fatalf("got %d notifications, want the firing problem reported once", len(h.notifier.sent))
	}

	h.findings = nil
	h.mustRun(30 * time.Second) // gone: resolved
	if len(h.notifier.sent) != 2 || alertKeys(h.notifier.sent[1].Resolved) != "a" || len(h.notifier.sent[1].New) != 0 {
		t.Fatalf("after fix: sent %+v, want a second notification with a resolved", h.notifier.sent)
	}
	if h.notifier.sent[1].Resolved[0].ResolvedAt.IsZero() {
		t.Error("ResolvedAt is not set")
	}

	h.mustRun(30 * time.Second)
	if len(h.notifier.sent) != 2 {
		t.Errorf("got %d notifications, want the resolution reported once", len(h.notifier.sent))
	}
}

func TestFlapIsNotReported(t *testing.T) {
	h := newHarness(t)

	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(time.Minute)

	h.findings = nil // gone again after 1m, below AlertAfter
	h.mustRun(30 * time.Second)
	h.mustRun(5 * time.Minute)

	if h.notifier.attempts != 0 {
		t.Errorf("notifier was called %d times, want never", h.notifier.attempts)
	}
	if rows := h.countRows(""); rows != 0 {
		t.Errorf("%d rows left, want the flap forgotten", rows)
	}
}

func TestFailedSendIsRetried(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)

	h.notifier.fail = true
	if err := h.run(2 * time.Minute); err == nil {
		t.Fatal("expected RunOnce to report the failed send")
	}
	if h.notifier.attempts != 1 || len(h.notifier.sent) != 0 {
		t.Fatalf("attempts %d, sent %d; want 1 failed attempt", h.notifier.attempts, len(h.notifier.sent))
	}

	_ = h.run(30 * time.Second) // still failing: tried again
	if h.notifier.attempts != 2 {
		t.Fatalf("attempts = %d, want a retry at the next run", h.notifier.attempts)
	}

	h.notifier.fail = false
	h.mustRun(30 * time.Second)
	if len(h.notifier.sent) != 1 || alertKeys(h.notifier.sent[0].New) != "a" {
		t.Fatalf("sent %+v, want the problem delivered after the notifier recovered", h.notifier.sent)
	}

	h.mustRun(30 * time.Second)
	if len(h.notifier.sent) != 1 {
		t.Errorf("got %d notifications, want no repeat after success", len(h.notifier.sent))
	}
}

func TestRestartKeepsState(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical), finding("b", checks.Warning)}
	h.mustRun(0)

	// "a" and "b" fire, but the notifier is down: nothing is reported.
	h.notifier.fail = true
	_ = h.run(2 * time.Minute)
	h.restart()

	// The new process knows the problems from the database and delivers them.
	h.notifier.fail = false
	h.mustRun(30 * time.Second)
	if len(h.notifier.sent) != 1 || alertKeys(h.notifier.sent[0].New) != "a,b" {
		t.Fatalf("sent %+v, want both problems delivered after the restart", h.notifier.sent)
	}

	// After another restart, already reported problems stay quiet.
	h.restart()
	h.mustRun(30 * time.Second)
	if len(h.notifier.sent) != 1 {
		t.Errorf("got %d notifications, want no repeat after a restart", len(h.notifier.sent))
	}
}

func TestOneNotificationForSeveralChanges(t *testing.T) {
	h := newHarness(t)

	h.findings = []checks.Finding{finding("a", checks.Warning)}
	h.mustRun(0)
	h.findings = []checks.Finding{finding("a", checks.Warning), finding("d", checks.Warning), finding("e", checks.Critical)}
	h.mustRun(time.Minute)
	h.mustRun(time.Minute) // a fires (2m old); d and e are only 1m old
	if len(h.notifier.sent) != 1 || alertKeys(h.notifier.sent[0].New) != "a" {
		t.Fatalf("sent %+v, want only a so far", h.notifier.sent)
	}

	// Next run: a is fixed, d and e fire. One notification carries all three.
	h.findings = []checks.Finding{finding("d", checks.Warning), finding("e", checks.Critical)}
	h.mustRun(time.Minute)

	if len(h.notifier.sent) != 2 {
		t.Fatalf("got %d notifications, want exactly one more", len(h.notifier.sent))
	}
	last := h.notifier.sent[1]
	if got := alertKeys(last.New); got != "e,d" {
		t.Errorf("new = %s, want e,d (critical first)", got)
	}
	if got := alertKeys(last.Resolved); got != "a" {
		t.Errorf("resolved = %s, want a", got)
	}
}

func TestStatusShowsSendErrorUntilNextSuccess(t *testing.T) {
	h := newHarness(t)
	if got := h.engine.Status(); got.NotifierName != "fake" || got.LastNotifyError != "" {
		t.Fatalf("initial status = %+v", got)
	}

	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.notifier.fail = true
	_ = h.run(2 * time.Minute)

	got := h.engine.Status()
	if !strings.Contains(got.LastNotifyError, "smtp server unreachable") || !got.LastNotifyErrorAt.Equal(h.now) {
		t.Errorf("status after a failed send = %+v, want the error and its time", got)
	}

	h.notifier.fail = false
	h.mustRun(30 * time.Second)
	if got := h.engine.Status(); got.LastNotifyError != "" || !got.LastNotifyErrorAt.IsZero() {
		t.Errorf("status after recovery = %+v, want the error cleared", got)
	}
}

func TestEscalationIsReportedAgainWithoutResolving(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute) // fires as a warning
	if len(h.notifier.sent) != 1 || h.notifier.sent[0].New[0].Severity != checks.Warning {
		t.Fatalf("sent %+v, want the warning reported", h.notifier.sent)
	}

	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(30 * time.Second)

	// Same problem, still firing: nothing resolved, nothing pending.
	if h.countRows("resolved") != 0 || h.countRows("pending") != 0 || h.countRows("firing") != 1 {
		t.Errorf("%d resolved, %d pending, %d firing; want just the one firing problem",
			h.countRows("resolved"), h.countRows("pending"), h.countRows("firing"))
	}
	// ... and the reader hears about it at once, as a new critical problem
	// with the original start time, without a "resolved" for the warning.
	if len(h.notifier.sent) != 2 {
		t.Fatalf("got %d notifications, want one more for the escalation", len(h.notifier.sent))
	}
	last := h.notifier.sent[1]
	if len(last.Resolved) != 0 || len(last.New) != 1 || last.New[0].Severity != checks.Critical ||
		last.New[0].Reason != "Volume-critical" || !last.New[0].Since.Equal(testStart) {
		t.Errorf("escalation notification = %+v", last)
	}
	if !strings.Contains(h.logs.String(), "finding escalated") {
		t.Error("the escalation is not logged")
	}

	// It is reported once, not at every run.
	h.mustRun(30 * time.Second)
	if len(h.notifier.sent) != 2 {
		t.Errorf("got %d notifications, want the escalation reported once", len(h.notifier.sent))
	}
}

func TestDeEscalationIsSilentAndResolutionIsReported(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute) // fires as critical
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(30 * time.Second)

	if len(h.notifier.sent) != 1 || h.countRows("firing") != 1 {
		t.Fatalf("%d notifications, %d firing; want getting better to be silent", len(h.notifier.sent), h.countRows("firing"))
	}

	h.findings = nil
	h.mustRun(30 * time.Second)
	if len(h.notifier.sent) != 2 || len(h.notifier.sent[1].Resolved) != 1 || h.notifier.sent[1].Resolved[0].Severity != checks.Warning {
		t.Errorf("sent %+v, want the resolution reported with the last known severity", h.notifier.sent)
	}
}

func TestEscalationWhilePendingIsReportedOnceAsCritical(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(0) // pending
	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(time.Minute) // still pending, now critical
	if len(h.notifier.sent) != 0 {
		t.Fatalf("sent %+v, want nothing before ALERT_AFTER", h.notifier.sent)
	}

	h.mustRun(time.Minute) // 2 minutes after the first sighting: fires
	if len(h.notifier.sent) != 1 || h.notifier.sent[0].New[0].Severity != checks.Critical {
		t.Errorf("sent %+v, want one notification, critical", h.notifier.sent)
	}
	if !h.notifier.sent[0].New[0].Since.Equal(testStart) {
		t.Errorf("Since = %v, want the first sighting %v", h.notifier.sent[0].New[0].Since, testStart)
	}
}

func TestSkippedCheckKeepsItsProblems(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical), finding("pending", checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute) // "a" and "pending" are both 2 minutes old: both fire
	if len(h.notifier.sent) != 1 {
		t.Fatalf("got %d notifications, want the first report", len(h.notifier.sent))
	}

	// The data of the check becomes unavailable: it is not run, and its
	// problems must neither resolve nor be reported again.
	h.skipped = map[string]string{"fake": "kubelet stats unavailable"}
	h.findings = nil
	runsBefore := h.checkRuns
	h.mustRun(time.Minute)
	h.mustRun(time.Minute)

	if h.checkRuns != runsBefore {
		t.Error("a skipped check was executed")
	}
	if h.countRows("firing") != 2 || h.countRows("resolved") != 0 {
		t.Errorf("%d firing, %d resolved; want the problems untouched while the check is skipped",
			h.countRows("firing"), h.countRows("resolved"))
	}
	if len(h.notifier.sent) != 1 {
		t.Errorf("got %d notifications, want no resolved notification caused by missing data", len(h.notifier.sent))
	}

	// Data is back and the problems are really gone: now they resolve.
	h.skipped = nil
	h.mustRun(time.Minute)
	if h.countRows("resolved") != 2 || len(h.notifier.sent) != 2 || len(h.notifier.sent[1].Resolved) != 2 {
		t.Errorf("%d resolved, %d notifications; want both problems resolved and reported once data is back",
			h.countRows("resolved"), len(h.notifier.sent))
	}
}

func TestPendingProblemOfSkippedCheckIsNotDropped(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Warning)}
	h.mustRun(0) // pending

	h.skipped = map[string]string{"fake": "no data"}
	h.findings = nil
	h.mustRun(time.Minute)
	if h.countRows("pending") != 1 {
		t.Error("a pending problem was dropped only because its check could not run")
	}

	h.skipped = nil
	h.findings = []checks.Finding{finding("a", checks.Warning)}
	h.mustRun(time.Minute) // still the same problem, now 2 minutes old: fires
	if len(h.notifier.sent) != 1 || !h.notifier.sent[0].New[0].Since.Equal(testStart) {
		t.Errorf("sent %+v; want the problem to keep its original start time", h.notifier.sent)
	}
}

func TestSkipIsLoggedOnlyWhenItChanges(t *testing.T) {
	h := newHarness(t)
	h.skipped = map[string]string{"fake": "kubelet stats unavailable"}
	h.mustRun(0)
	h.mustRun(30 * time.Second)
	h.mustRun(30 * time.Second)
	if n := strings.Count(h.logs.String(), "check skipped"); n != 1 {
		t.Errorf("\"check skipped\" logged %d times, want once:\n%s", n, h.logs.String())
	}
	if !strings.Contains(h.logs.String(), "kubelet stats unavailable") {
		t.Error("the reason is missing from the log")
	}

	h.skipped = nil
	h.mustRun(30 * time.Second)
	h.mustRun(30 * time.Second)
	if n := strings.Count(h.logs.String(), "check runs again"); n != 1 {
		t.Errorf("\"check runs again\" logged %d times, want once", n)
	}
}

func TestStatusShowsTemporarilySkippedCheck(t *testing.T) {
	h := newHarness(t)
	h.skipped = map[string]string{"fake": "kubelet stats unavailable", "not-ours": "ignored"}
	h.mustRun(0)

	want := []CheckStatus{{Name: "fake", Active: false, Reason: "kubelet stats unavailable"}}
	if got := h.engine.Status().Checks; !slices.Equal(got, want) {
		t.Errorf("Checks = %+v, want %+v (names the engine does not know are ignored)", got, want)
	}

	h.skipped = nil
	h.mustRun(time.Minute)
	if got := h.engine.Status().Checks; len(got) != 1 || !got[0].Active || got[0].Reason != "" {
		t.Errorf("Checks = %+v, want the check active again", got)
	}
}

// A check that cannot run for the whole process, for example after a restart
// with a missing permission: its problems stay open with KeepProblems, and count
// as gone without it (disabled check, CRD not installed).
func TestProcessWideSkippedCheck(t *testing.T) {
	tests := []struct {
		name         string
		keepProblems bool
		wantFiring   int
		wantResolved int
		wantMails    int
	}{
		{"missing permission keeps the problem", true, 1, 0, 1},
		{"otherwise the problem is resolved", false, 0, 1, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.findings = []checks.Finding{finding("a", checks.Critical)}
			h.mustRun(0)
			h.mustRun(2 * time.Minute)
			if h.countRows("firing") != 1 || len(h.notifier.sent) != 1 {
				t.Fatalf("%d firing, %d mails; want the problem reported once", h.countRows("firing"), len(h.notifier.sent))
			}

			h.restart()
			h.engine.Checks = nil
			h.engine.SkippedChecks = []SkippedCheck{{Name: "fake", Reason: "cannot run", KeepProblems: tt.keepProblems}}
			h.mustRun(time.Minute)

			if h.countRows("firing") != tt.wantFiring || h.countRows("resolved") != tt.wantResolved {
				t.Errorf("%d firing, %d resolved; want %d, %d",
					h.countRows("firing"), h.countRows("resolved"), tt.wantFiring, tt.wantResolved)
			}
			if len(h.notifier.sent) != tt.wantMails {
				t.Errorf("%d mails, want %d", len(h.notifier.sent), tt.wantMails)
			}
		})
	}
}

func TestStatusAfterRun(t *testing.T) {
	h := newHarness(t)
	h.engine.SkippedChecks = []SkippedCheck{{Name: "argocd-health", Reason: "Argo CD CRDs not installed"}}

	if got := h.engine.Status(); !got.LastRun.IsZero() {
		t.Errorf("LastRun = %v before the first run, want zero", got.LastRun)
	}

	h.mustRun(time.Minute)
	got := h.engine.Status()
	if !got.LastRun.Equal(h.now) || got.LastRunError != "" {
		t.Errorf("LastRun = %v, error %q; want the run time and no error", got.LastRun, got.LastRunError)
	}

	want := []CheckStatus{
		{Name: "argocd-health", Active: false, Reason: "Argo CD CRDs not installed"},
		{Name: "fake", Active: true},
	}
	if !slices.Equal(got.Checks, want) {
		t.Errorf("Checks = %+v, want %+v", got.Checks, want)
	}
}

func TestStatusShowsRunError(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.notifier.fail = true
	_ = h.run(2 * time.Minute)

	if got := h.engine.Status(); !strings.Contains(got.LastRunError, "smtp server unreachable") {
		t.Errorf("LastRunError = %q, want the send error", got.LastRunError)
	}
	h.notifier.fail = false
	h.mustRun(time.Minute)
	if got := h.engine.Status(); got.LastRunError != "" {
		t.Errorf("LastRunError = %q after a clean run, want it cleared", got.LastRunError)
	}
}

func TestCleanupDeletesExpiredSessions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, session := range []store.Session{
		{IDHash: "expired", Username: "admin", CreatedAt: h.now.Add(-2 * time.Hour), ExpiresAt: h.now.Add(-time.Hour)},
		{IDHash: "valid", Username: "admin", CreatedAt: h.now, ExpiresAt: h.now.Add(time.Hour)},
	} {
		if err := h.engine.Store.CreateSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}

	h.mustRun(0)

	if _, found, _ := h.engine.Store.Session(ctx, "valid", h.now); !found {
		t.Error("the valid session was removed")
	}
	if n, _ := h.engine.Store.DeleteExpiredSessions(ctx, h.now); n != 0 {
		t.Errorf("%d expired sessions were left behind by the cleanup", n)
	}
}

func TestNotificationCarriesBaseURL(t *testing.T) {
	h := newHarness(t)
	h.engine.BaseURL = "https://smokealarm.example.com"
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)

	if len(h.notifier.sent) != 1 || h.notifier.sent[0].BaseURL != "https://smokealarm.example.com" {
		t.Errorf("sent %+v, want the base URL in the notification", h.notifier.sent)
	}
}

func TestResolvedBeforeReportedStaysSilent(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)

	h.notifier.fail = true
	_ = h.run(2 * time.Minute) // fires, but the notifier is down
	h.notifier.fail = false

	h.findings = nil
	h.mustRun(30 * time.Second) // fixed before anyone heard about it

	if len(h.notifier.sent) != 0 {
		t.Errorf("sent %+v, want nothing: the reader never heard of this problem", h.notifier.sent)
	}
	attempts := h.notifier.attempts
	h.mustRun(30 * time.Second)
	if h.notifier.attempts != attempts {
		t.Error("the silently resolved problem is still being picked up")
	}
}

func TestProblemThatComesBackIsNewAgain(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.findings = nil
	h.mustRun(time.Minute)

	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(time.Minute) // pending again with a fresh timer
	if len(h.notifier.sent) != 2 {
		t.Fatalf("got %d notifications, want only fired + resolved so far", len(h.notifier.sent))
	}
	h.mustRun(2 * time.Minute)
	if len(h.notifier.sent) != 3 || alertKeys(h.notifier.sent[2].New) != "a" {
		t.Errorf("sent %+v, want the returning problem reported as new", h.notifier.sent)
	}
}

func TestRetentionDeletesOldResolved(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.findings = nil
	h.mustRun(time.Minute) // resolved and reported
	if h.countRows("resolved") != 1 {
		t.Fatal("expected one resolved row")
	}

	h.mustRun(29 * 24 * time.Hour) // still within 30 days
	if h.countRows("resolved") != 1 {
		t.Error("resolved row deleted before the retention time")
	}
	h.mustRun(2 * 24 * time.Hour) // now older than 30 days
	if h.countRows("") != 0 {
		t.Error("resolved row still there after the retention time")
	}
}

func TestOpenProblemsAreNeverDeletedByRetention(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)

	h.mustRun(90 * 24 * time.Hour)
	if h.countRows("firing") != 1 {
		t.Error("a long-lasting firing problem was deleted")
	}
}

func TestRunOnceLogsTransitions(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.findings = nil
	h.mustRun(time.Minute)

	log := h.logs.String()
	for _, want := range []string{"finding pending", "finding firing", "finding resolved", "notification sent", "check run finished"} {
		if !strings.Contains(log, want) {
			t.Errorf("log does not contain %q:\n%s", want, log)
		}
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	h := newHarness(t)
	runs := 0
	h.engine.Snapshot = func(_ context.Context, now time.Time) *checks.Snapshot { runs++; return &checks.Snapshot{Now: now} }
	h.engine.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h.engine.Run(ctx) // must return instead of blocking

	if runs != 1 {
		t.Errorf("runs = %d, want 1 (the immediate first pass)", runs)
	}
}

func TestRunChecksSortsByKey(t *testing.T) {
	list := []checks.Check{
		{Name: "b", Run: func(*checks.Snapshot, checks.Options) []checks.Finding {
			return []checks.Finding{{Key: "b/2"}, {Key: "b/1"}}
		}},
		{Name: "a", Run: func(*checks.Snapshot, checks.Options) []checks.Finding {
			return []checks.Finding{{Key: "a/1"}}
		}},
		{Name: "none", Run: func(*checks.Snapshot, checks.Options) []checks.Finding { return nil }},
	}

	got := runChecks(&checks.Snapshot{Now: testStart}, checks.Options{}, list)

	var keys []string
	for _, f := range got {
		keys = append(keys, f.Key)
	}
	if want := "a/1,b/1,b/2"; strings.Join(keys, ",") != want {
		t.Errorf("keys = %v, want %s", keys, want)
	}
}
