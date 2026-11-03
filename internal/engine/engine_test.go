package engine

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
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
	t        *testing.T
	path     string
	engine   *Engine
	notifier *fakeNotifier
	findings []checks.Finding
	now      time.Time
	logs     *bytes.Buffer
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
		Snapshot: func(now time.Time) *checks.Snapshot { return &checks.Snapshot{Now: now} },
		Checks: []checks.Check{{
			Name: "fake",
			Run:  func(*checks.Snapshot, checks.Options) []checks.Finding { return h.findings },
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
	h.engine.Snapshot = func(now time.Time) *checks.Snapshot { runs++; return &checks.Snapshot{Now: now} }
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
