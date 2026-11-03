// Package engine runs the main loop: take a snapshot, run all checks, compare
// the findings with the stored state, send notifications for what changed and
// clean up old data.
//
// A problem has a life cycle: it starts as pending, becomes firing once it has
// lasted AlertAfter, and ends as resolved. Problems that vanish while still
// pending are dropped silently, so short flaps never reach anybody.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/notify"
	"github.com/gagaschu/smokealarm/internal/store"
)

// Engine holds everything the loop needs. main() creates the dependencies
// and passes them in, so the loop can be tested with fake functions.
type Engine struct {
	// Snapshot returns the current cluster objects (the informer caches).
	Snapshot func(now time.Time) *checks.Snapshot
	Checks   []checks.Check
	Options  checks.Options
	Interval time.Duration

	// AlertAfter is how long a problem must last before it is reported.
	AlertAfter time.Duration
	// Retention is how long resolved problems are kept as history.
	Retention time.Duration

	Store    *store.Store
	Notifier notify.Notifier
	// BaseURL is the link to the web UI that notifications point to. It may
	// be empty.
	BaseURL string

	// Now is injectable so tests control the time.
	Now    func() time.Time
	Logger *slog.Logger

	mu     sync.Mutex // protects status, which the web server reads
	status Status
}

// Status is what the engine reports about itself, for the status page.
type Status struct {
	NotifierName      string
	LastNotifyError   string    // empty if the last send worked
	LastNotifyErrorAt time.Time // zero if there is no error
}

// Status returns a copy of the current status.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.status
	s.NotifierName = e.Notifier.Name()
	return s
}

// setNotifyResult remembers whether the last send worked. The error is
// cleared by the next success, so the status shows the current situation and
// not an old incident.
func (e *Engine) setNotifyResult(err error, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err == nil {
		e.status.LastNotifyError = ""
		e.status.LastNotifyErrorAt = time.Time{}
		return
	}
	e.status.LastNotifyError = err.Error()
	e.status.LastNotifyErrorAt = now
}

// Run executes one pass immediately and then one per interval until the
// context is cancelled. A failed pass is logged and retried at the next tick.
func (e *Engine) Run(ctx context.Context) {
	e.runAndLog(ctx)

	ticker := time.NewTicker(e.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.runAndLog(ctx)
		}
	}
}

func (e *Engine) runAndLog(ctx context.Context) {
	if err := e.RunOnce(ctx); err != nil {
		e.Logger.Error("check run failed", "error", err)
	}
}

// RunOnce does one full pass. The steps are independent: if one fails, the
// others still run, and all errors are returned together.
func (e *Engine) RunOnce(ctx context.Context) error {
	start := e.Now()
	snap := e.Snapshot(start)
	findings := runChecks(snap, e.Options, e.Checks)
	for _, f := range findings {
		e.Logger.Debug("finding",
			"key", f.Key,
			"severity", f.Severity,
			"message", f.Message,
		)
	}

	var errs []error
	if err := e.saveState(ctx, findings, start); err != nil {
		errs = append(errs, err)
	}
	if err := e.notify(ctx, start); err != nil {
		errs = append(errs, err)
	}
	if err := e.cleanup(ctx, start); err != nil {
		errs = append(errs, err)
	}

	e.Logger.Info("check run finished",
		"findings", len(findings),
		"checks", len(e.Checks),
		"duration", e.Now().Sub(start).String(),
	)
	return errors.Join(errs...)
}

// saveState compares the findings with the stored problems and writes the
// result back in one transaction.
func (e *Engine) saveState(ctx context.Context, findings []checks.Finding, now time.Time) error {
	open, err := e.Store.OpenRecords(ctx)
	if err != nil {
		return fmt.Errorf("load open findings: %w", err)
	}
	// Every enabled check ran, so nothing is skipped. Records of checks that
	// were disabled since the last run are therefore resolved, not left
	// "firing" forever.
	changes := e.reconcile(open, findings, nil, now)
	if err := e.Store.Apply(ctx, changes); err != nil {
		return fmt.Errorf("save findings: %w", err)
	}
	return nil
}

// reconcile decides what changes: it compares the stored open problems with
// the current findings. It does no I/O, which keeps the rules easy to test.
// Problems of the skipped checks (checks that could not run this time) are
// left alone, because "no findings" would just mean "unknown".
func (e *Engine) reconcile(open []store.Record, findings []checks.Finding, skipped []string, now time.Time) store.Changes {
	var changes store.Changes

	openByKey := map[string]store.Record{}
	for _, r := range open {
		openByKey[r.Key] = r
	}

	seen := map[string]bool{}
	for _, f := range findings {
		if seen[f.Key] {
			continue
		}
		seen[f.Key] = true

		record, exists := openByKey[f.Key]
		if !exists {
			changes.Insert = append(changes.Insert, e.newRecord(f, now))
			continue
		}
		changes.Update = append(changes.Update, e.refresh(record, f, now))
	}

	for _, record := range open {
		if seen[record.Key] || slices.Contains(skipped, record.Check) {
			continue
		}
		if record.State == store.StatePending {
			e.Logger.Info("finding dropped before it fired", "key", record.Key)
			changes.Delete = append(changes.Delete, record.ID)
			continue
		}
		record.State = store.StateResolved
		record.ResolvedAt = &now
		e.Logger.Info("finding resolved", "key", record.Key)
		changes.Update = append(changes.Update, record)
	}
	return changes
}

// newRecord creates the record for a problem seen for the first time. With
// AlertAfter 0 it fires right away.
func (e *Engine) newRecord(f checks.Finding, now time.Time) store.Record {
	record := store.Record{Finding: f, State: store.StatePending, FirstSeen: now, LastSeen: now}
	e.Logger.Info("finding pending", "key", f.Key, "severity", f.Severity)
	if e.AlertAfter <= 0 {
		record.State = store.StateFiring
		record.FiringAt = &now
		e.Logger.Info("finding firing", "key", f.Key, "severity", f.Severity)
	}
	return record
}

// refresh updates a known problem with the latest details and lets it fire
// once it has lasted long enough.
func (e *Engine) refresh(record store.Record, f checks.Finding, now time.Time) store.Record {
	record.Severity = f.Severity
	record.Message = f.Message
	record.LastSeen = now
	if record.State == store.StatePending && now.Sub(record.FirstSeen) >= e.AlertAfter {
		record.State = store.StateFiring
		record.FiringAt = &now
		e.Logger.Info("finding firing", "key", f.Key, "severity", f.Severity)
	}
	return record
}

// notify sends one combined notification for everything that has not been
// reported yet. Records are marked as reported only after the notifier
// succeeded, so a failed send is simply tried again at the next run, also
// after a restart.
func (e *Engine) notify(ctx context.Context, now time.Time) error {
	records, err := e.Store.Unnotified(ctx)
	if err != nil {
		return fmt.Errorf("load unreported findings: %w", err)
	}

	n := notify.Notification{BaseURL: e.BaseURL}
	var firingIDs, resolvedIDs, silentIDs []int64
	for _, r := range records {
		switch {
		case r.State == store.StateFiring:
			n.New = append(n.New, alertFor(r))
			firingIDs = append(firingIDs, r.ID)
		case r.FiringNotifiedAt == nil:
			// It was fixed before anybody heard about it. Telling the
			// reader it is resolved would be confusing.
			silentIDs = append(silentIDs, r.ID)
		default:
			n.Resolved = append(n.Resolved, alertFor(r))
			resolvedIDs = append(resolvedIDs, r.ID)
		}
	}

	if err := e.Store.MarkResolvedNotified(ctx, silentIDs, now); err != nil {
		return fmt.Errorf("mark silently resolved findings: %w", err)
	}
	if len(n.New) == 0 && len(n.Resolved) == 0 {
		return nil
	}

	sortAlerts(n.New)
	sortAlerts(n.Resolved)
	err = e.Notifier.Send(ctx, n)
	e.setNotifyResult(err, now)
	if err != nil {
		return fmt.Errorf("send notification via %s: %w", e.Notifier.Name(), err)
	}
	e.Logger.Info("notification sent", "notifier", e.Notifier.Name(), "new", len(n.New), "resolved", len(n.Resolved))

	if err := e.Store.MarkFiringNotified(ctx, firingIDs, now); err != nil {
		return fmt.Errorf("mark new findings as reported: %w", err)
	}
	if err := e.Store.MarkResolvedNotified(ctx, resolvedIDs, now); err != nil {
		return fmt.Errorf("mark resolved findings as reported: %w", err)
	}
	return nil
}

func alertFor(r store.Record) notify.Alert {
	alert := notify.Alert{Finding: r.Finding, Since: r.FirstSeen}
	if r.ResolvedAt != nil {
		alert.ResolvedAt = *r.ResolvedAt
	}
	return alert
}

// sortAlerts puts critical problems first, then orders by key for a stable
// result.
func sortAlerts(alerts []notify.Alert) {
	sort.Slice(alerts, func(i, j int) bool {
		a, b := alerts[i], alerts[j]
		if a.Severity != b.Severity {
			return a.Severity == checks.Critical
		}
		return a.Key < b.Key
	})
}

// cleanup deletes resolved problems that are older than the retention time.
func (e *Engine) cleanup(ctx context.Context, now time.Time) error {
	deleted, err := e.Store.DeleteResolvedBefore(ctx, now.Add(-e.Retention))
	if err != nil {
		return fmt.Errorf("clean up old findings: %w", err)
	}
	if deleted > 0 {
		e.Logger.Info("old findings deleted", "count", deleted)
	}
	return nil
}

// runChecks runs every check and returns all findings sorted by key, so the
// output order is stable between runs.
func runChecks(snap *checks.Snapshot, opts checks.Options, list []checks.Check) []checks.Finding {
	var findings []checks.Finding
	for _, c := range list {
		findings = append(findings, c.Run(snap, opts)...)
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Key < findings[j].Key })
	return findings
}
