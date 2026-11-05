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
	corev1 "k8s.io/api/core/v1"
)

var testStart = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// fakeNotifier records what it was asked to send. When fail is set it
// refuses, like a mail server that is down. An empty name means "fake".
type fakeNotifier struct {
	name     string
	attempts int
	sent     []notify.Notification
	fail     bool
	onSend   func() // runs while sending, if set
}

func (f *fakeNotifier) Name() string {
	if f.name == "" {
		return "fake"
	}
	return f.name
}

func (f *fakeNotifier) Description() string { return f.Name() + " (test)" }

func (f *fakeNotifier) Send(_ context.Context, n notify.Notification) error {
	f.attempts++
	if f.onSend != nil {
		f.onSend()
	}
	if f.fail {
		return errors.New("smtp server unreachable")
	}
	f.sent = append(f.sent, n)
	return nil
}

// harness drives an engine with a controllable clock and controllable
// findings, against a real SQLite file.
type harness struct {
	t          *testing.T
	path       string
	engine     *Engine
	notifier   *fakeNotifier   // the first target
	others     []*fakeNotifier // more targets, see addTarget
	findings   []checks.Finding
	skipped    map[string]string   // what the snapshot reports as unable to run
	namespaces []*corev1.Namespace // in the snapshot, for the annotations
	nodes      []*corev1.Node      // in the snapshot, for the node causes
	pods       []*corev1.Pod
	checkRuns  int // how often the fake check was executed
	now        time.Time
	logs       *bytes.Buffer
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
	h.syncTargets(db)

	h.engine = &Engine{
		Snapshot: func(_ context.Context, now time.Time) *checks.Snapshot {
			return &checks.Snapshot{Now: now, Skipped: h.skipped, Namespaces: h.namespaces, Nodes: h.nodes, Pods: h.pods}
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
		Notifiers:  h.notifiers(),
		Now:        func() time.Time { return h.now },
		Logger:     slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

func (h *harness) notifiers() []notify.Notifier {
	list := []notify.Notifier{h.notifier}
	for _, n := range h.others {
		list = append(list, n)
	}
	return list
}

// syncTargets registers the targets, as main does at startup.
func (h *harness) syncTargets(db *store.Store) {
	h.t.Helper()
	var names []string
	for _, n := range h.notifiers() {
		names = append(names, n.Name())
	}
	if _, _, err := db.SyncTargets(context.Background(), names); err != nil {
		h.t.Fatalf("SyncTargets: %v", err)
	}
}

// addTarget adds another notification target, as if the process had just
// been started with it in the configuration.
func (h *harness) addTarget(name string) *fakeNotifier {
	h.t.Helper()
	n := &fakeNotifier{name: name}
	h.others = append(h.others, n)
	h.engine.Notifiers = h.notifiers()
	h.syncTargets(h.engine.Store)
	return n
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
	initial := h.engine.Status().Targets
	if len(initial) != 1 || initial[0].Name != "fake" || initial[0].Description != "fake (test)" || initial[0].LastError != "" {
		t.Fatalf("initial targets = %+v", initial)
	}

	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.notifier.fail = true
	_ = h.run(2 * time.Minute)

	got := h.engine.Status().Targets[0]
	if !strings.Contains(got.LastError, "smtp server unreachable") || !got.LastErrorAt.Equal(h.now) || !got.LastSentAt.IsZero() {
		t.Errorf("status after a failed send = %+v, want the error and its time", got)
	}

	h.notifier.fail = false
	h.mustRun(30 * time.Second)
	got = h.engine.Status().Targets[0]
	if got.LastError != "" || !got.LastErrorAt.IsZero() || !got.LastSentAt.Equal(h.now) {
		t.Errorf("status after recovery = %+v, want the error cleared and the time of the send", got)
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

	got, _ := runChecks(&checks.Snapshot{Now: testStart}, checks.Options{}, list, nil)

	var keys []string
	for _, f := range got {
		keys = append(keys, f.Key)
	}
	if want := "a/1,b/1,b/2"; strings.Join(keys, ",") != want {
		t.Errorf("keys = %v, want %s", keys, want)
	}
}

// firingCritical returns a harness with reminders every hour and problem "a"
// reported once, so the reminder clock starts at the current time.
func firingCritical(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	if len(h.notifier.sent) != 1 || alertKeys(h.notifier.sent[0].New) != "a" {
		t.Fatalf("sent %+v, want the first report of a", h.notifier.sent)
	}
	return h
}

func TestRemindersAreOffByDefault(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.mustRun(24 * time.Hour)
	if len(h.notifier.sent) != 1 {
		t.Fatalf("got %d notifications, want only the first report", len(h.notifier.sent))
	}
}

func TestReminderRepeatsAtTheInterval(t *testing.T) {
	h := firingCritical(t)

	for round := 1; round <= 2; round++ {
		h.mustRun(59 * time.Minute)
		if len(h.notifier.sent) != round {
			t.Fatalf("round %d: got %d notifications before the interval is over", round, len(h.notifier.sent))
		}
		h.mustRun(time.Minute)
		if len(h.notifier.sent) != round+1 {
			t.Fatalf("round %d: got %d notifications, want a reminder at the interval", round, len(h.notifier.sent))
		}
		got := h.notifier.sent[round]
		if alertKeys(got.Reminders) != "a" || len(got.New) != 0 || len(got.Resolved) != 0 {
			t.Fatalf("round %d: sent %+v, want only a reminder for a", round, got)
		}
		if since := got.Reminders[0].Since; !since.Equal(testStart) {
			t.Errorf("Since = %v, want first seen %v", since, testStart)
		}
	}
}

func TestWarningsGetNoReminder(t *testing.T) {
	h := newHarness(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.findings = []checks.Finding{finding("a", checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.mustRun(24 * time.Hour)
	if len(h.notifier.sent) != 1 {
		t.Fatalf("got %d notifications, want the warning reported once", len(h.notifier.sent))
	}
}

func TestFailedReminderIsRetriedAlsoAfterRestart(t *testing.T) {
	h := firingCritical(t)

	h.notifier.fail = true
	if err := h.run(time.Hour); err == nil {
		t.Fatal("RunOnce returned no error for a failed send")
	}
	attempts := h.notifier.attempts
	if err := h.run(30 * time.Second); err == nil {
		t.Fatal("RunOnce returned no error for the failed retry")
	}
	if h.notifier.attempts != attempts+1 {
		t.Fatalf("attempts = %d, want a retry in the next run", h.notifier.attempts)
	}

	h.restart()
	h.engine.CriticalRepeatInterval = time.Hour
	h.notifier.fail = false
	h.mustRun(30 * time.Second)
	if len(h.notifier.sent) != 2 || alertKeys(h.notifier.sent[1].Reminders) != "a" {
		t.Fatalf("sent %+v, want the reminder delivered after the restart", h.notifier.sent)
	}

	// The clock starts again at the successful send, not at the first attempt.
	h.mustRun(59 * time.Minute)
	if len(h.notifier.sent) != 2 {
		t.Fatalf("got %d notifications, want no reminder before the interval is over", len(h.notifier.sent))
	}
}

func TestReminderAndNewProblemShareOneMail(t *testing.T) {
	h := firingCritical(t)

	h.mustRun(58 * time.Minute)
	h.findings = []checks.Finding{finding("a", checks.Critical), finding("b", checks.Warning)}
	h.mustRun(0) // b is pending now
	h.mustRun(2 * time.Minute)

	if len(h.notifier.sent) != 2 {
		t.Fatalf("got %d notifications, want one combined mail after the first", len(h.notifier.sent))
	}
	got := h.notifier.sent[1]
	if alertKeys(got.New) != "b" || alertKeys(got.Reminders) != "a" {
		t.Fatalf("sent %+v, want b as new and a as reminder", got)
	}
}

func TestResolvedProblemGetsNoReminder(t *testing.T) {
	h := firingCritical(t)

	h.findings = nil
	h.mustRun(time.Hour)
	if len(h.notifier.sent) != 2 {
		t.Fatalf("got %d notifications, want the resolution", len(h.notifier.sent))
	}
	got := h.notifier.sent[1]
	if alertKeys(got.Resolved) != "a" || len(got.Reminders) != 0 {
		t.Fatalf("sent %+v, want only a resolved", got)
	}

	h.mustRun(2 * time.Hour)
	if len(h.notifier.sent) != 2 {
		t.Fatalf("got %d notifications, want nothing after the resolution", len(h.notifier.sent))
	}
}

func TestEscalationRestartsTheReminderClock(t *testing.T) {
	h := newHarness(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute) // reported as a warning

	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(2 * time.Hour) // long past the interval, but this is a new report
	if len(h.notifier.sent) != 2 {
		t.Fatalf("got %d notifications, want the warning and the escalation", len(h.notifier.sent))
	}
	if got := h.notifier.sent[1]; alertKeys(got.New) != "vol" || len(got.Reminders) != 0 {
		t.Fatalf("sent %+v, want the escalation as new and no reminder", got)
	}

	h.mustRun(59 * time.Minute)
	if len(h.notifier.sent) != 2 {
		t.Fatalf("got %d notifications, want no reminder before the interval is over", len(h.notifier.sent))
	}
	h.mustRun(time.Minute)
	if len(h.notifier.sent) != 3 || alertKeys(h.notifier.sent[2].Reminders) != "vol" {
		t.Fatalf("sent %+v, want a reminder one interval after the escalation", h.notifier.sent)
	}
}

func TestDeEscalationStopsReminders(t *testing.T) {
	h := newHarness(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)

	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(2 * time.Hour)
	h.mustRun(2 * time.Hour)
	if len(h.notifier.sent) != 1 {
		t.Fatalf("got %d notifications, want no reminder for a warning", len(h.notifier.sent))
	}
}

func TestSkippedCheckGetsNoReminderUntilItRunsAgain(t *testing.T) {
	h := firingCritical(t)

	h.skipped = map[string]string{"fake": "kubelet stats unavailable"}
	h.mustRun(2 * time.Hour)
	if len(h.notifier.sent) != 1 {
		t.Fatalf("got %d notifications, want no reminder while the state is unknown", len(h.notifier.sent))
	}

	h.skipped = nil
	h.mustRun(30 * time.Second)
	if len(h.notifier.sent) != 2 || alertKeys(h.notifier.sent[1].Reminders) != "a" {
		t.Fatalf("sent %+v, want the reminder once the check runs again", h.notifier.sent)
	}
}

// acknowledge does what the button in the web UI does, for the open problem
// with this key.
func (h *harness) acknowledge(key string) {
	h.t.Helper()
	ctx := context.Background()
	records, err := h.engine.Store.OpenRecords(ctx)
	if err != nil {
		h.t.Fatalf("OpenRecords: %v", err)
	}
	for _, r := range records {
		if r.Key != key {
			continue
		}
		_, changed, err := h.engine.Store.Acknowledge(ctx, r.ID, h.now)
		if err != nil || !changed {
			h.t.Fatalf("Acknowledge(%s) = %v, %v, want true, nil", key, changed, err)
		}
		return
	}
	h.t.Fatalf("no open problem with key %s", key)
}

func TestAcknowledgedProblemGetsNoRemindersButItsResolution(t *testing.T) {
	h := firingCritical(t)
	h.acknowledge("a")

	h.mustRun(3 * time.Hour) // several intervals, also after a restart
	h.restart()
	h.engine.CriticalRepeatInterval = time.Hour
	h.mustRun(3 * time.Hour)
	if len(h.notifier.sent) != 1 {
		t.Fatalf("sent %+v, want only the first mail", h.notifier.sent)
	}

	h.findings = nil
	h.mustRun(time.Minute)
	if len(h.notifier.sent) != 2 || alertKeys(h.notifier.sent[1].Resolved) != "a" {
		t.Fatalf("sent %+v, want a resolved mail for a", h.notifier.sent)
	}
}

func TestReminderStillDueForOtherProblems(t *testing.T) {
	h := firingCritical(t)
	h.findings = append(h.findings, finding("b", checks.Critical))
	h.mustRun(time.Minute)     // b is pending
	h.mustRun(2 * time.Minute) // b fires and is reported
	h.acknowledge("a")
	sent := len(h.notifier.sent)

	h.mustRun(time.Hour)
	if len(h.notifier.sent) != sent+1 || alertKeys(h.notifier.sent[sent].Reminders) != "b" {
		t.Fatalf("sent %+v, want a reminder for b only", h.notifier.sent[sent:])
	}
}

func TestAcknowledgedProblemStaysAcknowledgedAfterWarningAndCriticalAgain(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.acknowledge("vol")
	sent := len(h.notifier.sent)

	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(time.Minute)
	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(time.Minute)
	if len(h.notifier.sent) != sent {
		t.Fatalf("sent %+v, want no mail", h.notifier.sent[sent:])
	}

	records, err := h.engine.Store.OpenRecords(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatalf("OpenRecords = %v, %v", records, err)
	}
	if records[0].AcknowledgedAt == nil || records[0].Severity != checks.Critical {
		t.Errorf("record = %+v, want critical and still acknowledged", records[0])
	}
	if records[0].FiringNotifiedAt == nil {
		t.Error("the escalation cleared the marker of the last report")
	}

	h.findings = nil
	h.mustRun(time.Minute)
	if len(h.notifier.sent) != sent+1 || alertKeys(h.notifier.sent[sent].Resolved) != "vol" {
		t.Fatalf("sent %+v, want a resolved mail for vol", h.notifier.sent[sent:])
	}
}

func TestProblemAfterResolvedIsNewAndNotAcknowledged(t *testing.T) {
	h := firingCritical(t)
	h.acknowledge("a")
	h.findings = nil
	h.mustRun(time.Minute)
	sent := len(h.notifier.sent)

	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(time.Minute)
	h.mustRun(2 * time.Minute)
	if len(h.notifier.sent) != sent+1 || alertKeys(h.notifier.sent[sent].New) != "a" {
		t.Fatalf("sent %+v, want a new mail for a", h.notifier.sent[sent:])
	}

	h.mustRun(time.Hour)
	if len(h.notifier.sent) != sent+2 || alertKeys(h.notifier.sent[sent+1].Reminders) != "a" {
		t.Fatalf("sent %+v, want reminders again for the new alarm", h.notifier.sent[sent:])
	}
}

func TestAcknowledgedBeforeFirstMailDropsItAndReportsTheResolution(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)

	h.notifier.fail = true
	_ = h.run(2 * time.Minute) // fires, but the notifier is down
	h.notifier.fail = false
	h.acknowledge("a")

	h.mustRun(time.Minute) // the notifier works again
	if len(h.notifier.sent) != 0 {
		t.Fatalf("sent %+v, want no late first mail", h.notifier.sent)
	}

	h.findings = nil
	h.mustRun(time.Minute)
	if len(h.notifier.sent) != 1 || alertKeys(h.notifier.sent[0].Resolved) != "a" || len(h.notifier.sent[0].New) != 0 {
		t.Fatalf("sent %+v, want only a resolved mail for a", h.notifier.sent)
	}

	// It is reported once, not in every run.
	h.mustRun(time.Minute)
	if len(h.notifier.sent) != 1 {
		t.Fatalf("sent %d notifications, want the resolved mail only once", len(h.notifier.sent))
	}
}

func TestFailedResolvedMailOfAcknowledgedProblemIsRetried(t *testing.T) {
	h := firingCritical(t)
	h.acknowledge("a")
	sent := len(h.notifier.sent)

	h.findings = nil
	h.notifier.fail = true
	if err := h.run(time.Minute); err == nil {
		t.Fatal("RunOnce: want the send error")
	}
	h.notifier.fail = false
	h.restart()

	h.mustRun(time.Minute)
	if len(h.notifier.sent) != sent+1 || alertKeys(h.notifier.sent[sent].Resolved) != "a" {
		t.Fatalf("sent %+v, want the resolved mail after the restart", h.notifier.sent[sent:])
	}
}

// unacknowledge does what the "Remove acknowledgement" button does, for the
// open problem with this key.
func (h *harness) unacknowledge(key string) {
	h.t.Helper()
	ctx := context.Background()
	records, err := h.engine.Store.OpenRecords(ctx)
	if err != nil {
		h.t.Fatalf("OpenRecords: %v", err)
	}
	for _, r := range records {
		if r.Key != key {
			continue
		}
		_, changed, err := h.engine.Store.Unacknowledge(ctx, r.ID)
		if err != nil || !changed {
			h.t.Fatalf("Unacknowledge(%s) = %v, %v, want true, nil", key, changed, err)
		}
		return
	}
	h.t.Fatalf("no open problem with key %s", key)
}

func TestOverdueReminderGoesOutAfterRemovingTheAcknowledgement(t *testing.T) {
	h := firingCritical(t)
	h.acknowledge("a")
	h.mustRun(3 * time.Hour)
	h.wantSent(1)

	h.unacknowledge("a")
	h.mustRun(30 * time.Second)
	h.wantSent(2)
	if alertKeys(h.notifier.sent[1].Reminders) != "a" {
		t.Fatalf("sent %+v, want a reminder for a", h.notifier.sent[1])
	}

	// From there it repeats as usual.
	h.mustRun(59 * time.Minute)
	h.wantSent(2)
	h.mustRun(time.Minute)
	h.wantSent(3)
}

func TestReminderWaitsForTheIntervalAfterRemovingTheAcknowledgement(t *testing.T) {
	h := firingCritical(t)
	h.acknowledge("a")
	h.mustRun(10 * time.Minute)

	h.unacknowledge("a")
	h.mustRun(49 * time.Minute)
	h.wantSent(1)

	h.mustRun(time.Minute)
	h.wantSent(2)
	if alertKeys(h.notifier.sent[1].Reminders) != "a" {
		t.Fatalf("sent %+v, want a reminder for a", h.notifier.sent[1])
	}
}

func TestFirstMailGoesOutAfterRemovingTheAcknowledgement(t *testing.T) {
	h := firingUnreported(t)
	h.acknowledge("a")
	h.mustRun(time.Minute)
	h.wantSent(0)

	h.unacknowledge("a")
	h.mustRun(time.Minute)
	h.wantSent(1)
	h.wantLastNew("a")
}

func TestRemovingTheAcknowledgementOfAWarningSendsNothingUntilItIsCritical(t *testing.T) {
	h := newHarness(t)
	h.engine.CriticalRepeatInterval = time.Hour
	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	h.acknowledge("vol")
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(time.Minute)

	h.unacknowledge("vol")
	h.mustRun(2 * time.Hour)
	h.wantSent(1)

	h.findings = []checks.Finding{escalating(checks.Critical)}
	h.mustRun(time.Minute)
	h.wantSent(2)
	h.wantLastNew("vol")
}

func TestRemovingTheAcknowledgementDuringADowntimeSendsNothingUntilItEnds(t *testing.T) {
	h := firingCritical(t)
	h.acknowledge("a")
	h.setDowntime("a", time.Hour)

	h.unacknowledge("a")
	h.mustRun(59 * time.Minute)
	h.wantSent(1)

	h.mustRun(time.Minute)
	h.wantSent(2)
	h.wantLastNew("a")
}

func TestRemovingTheAcknowledgementAfterADowntimeBringsRemindersBack(t *testing.T) {
	h := firingCritical(t)
	h.acknowledge("a")
	h.setDowntime("a", time.Hour)
	h.mustRun(time.Hour)
	h.wantSent(1) // acknowledged, so the end of the downtime sends nothing

	h.unacknowledge("a")
	h.mustRun(30 * time.Second)
	h.wantSent(2)
	if alertKeys(h.notifier.sent[1].Reminders) != "a" {
		t.Fatalf("sent %+v, want a reminder for a", h.notifier.sent[1])
	}
}

func TestAcknowledgingAgainAfterRemovingIsSilentUntilResolved(t *testing.T) {
	h := firingCritical(t)
	h.acknowledge("a")
	h.unacknowledge("a")
	h.acknowledge("a")

	h.mustRun(3 * time.Hour)
	h.wantSent(1)

	h.findings = nil
	h.mustRun(time.Minute)
	h.wantSent(2)
	h.wantLastResolved("a")
}

// A target that was told "warning" hears the end, even if the mail about the
// escalation never went out.
func TestResolvedIsReportedAfterAFailedEscalation(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(0)
	h.mustRun(2 * time.Minute)
	if len(h.notifier.sent) != 1 {
		t.Fatalf("sent %d, want the warning", len(h.notifier.sent))
	}

	h.notifier.fail = true
	h.findings = []checks.Finding{escalating(checks.Critical)}
	if err := h.run(30 * time.Second); err == nil {
		t.Fatal("expected the failed send to be returned")
	}

	h.notifier.fail = false
	h.findings = nil
	h.mustRun(30 * time.Second)
	h.mustRun(30 * time.Second)

	if len(h.notifier.sent) != 2 {
		t.Fatalf("sent %d, want the warning and its end", len(h.notifier.sent))
	}
	h.wantLastResolved(escalating(checks.Warning).Key)
}

// A problem nobody was told about stays silent when it ends, also if the first
// mail failed: nobody heard the start.
func TestNoResolvedWhenTheFirstMailFailed(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{escalating(checks.Warning)}
	h.mustRun(0)
	h.notifier.fail = true
	if err := h.run(2 * time.Minute); err == nil {
		t.Fatal("expected the failed send to be returned")
	}

	h.notifier.fail = false
	h.findings = nil
	h.mustRun(30 * time.Second)
	h.wantSent(0)
}

// A message that went out while the process was told to stop counts as sent:
// after the restart it must not go out a second time.
func TestMessageSentDuringShutdownIsNotSentAgain(t *testing.T) {
	h := newHarness(t)
	h.findings = []checks.Finding{finding("a", checks.Critical)}
	h.mustRun(0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.notifier.onSend = cancel // SIGTERM arrives while the mail is on its way
	h.now = h.now.Add(2 * time.Minute)
	_ = h.engine.RunOnce(ctx) // cleanup may fail on the cancelled context
	h.notifier.onSend = nil
	h.wantSent(1)

	h.start()
	h.mustRun(30 * time.Second)
	h.wantSent(1)
}
