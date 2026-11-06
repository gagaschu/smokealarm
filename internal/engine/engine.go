// Package engine runs the main loop: take a snapshot, run all checks, compare
// the findings with the stored state, send notifications for what changed and
// clean up old data.
//
// A problem has a life cycle: it starts as pending, becomes firing once it has
// lasted AlertAfter, and ends as resolved once it has been gone for
// ResolveAfter. Problems that vanish while still pending are dropped silently,
// so short flaps never reach anybody.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	Snapshot func(ctx context.Context, now time.Time) *checks.Snapshot
	Checks   []checks.Check
	Options  checks.Options
	Interval time.Duration

	// AlertAfter is how long a problem must last before it is reported.
	AlertAfter time.Duration
	// ResolveAfter is how long a firing problem must be gone before it counts as
	// resolved. Zero resolves it in the first run without it.
	ResolveAfter time.Duration
	// CriticalRepeatInterval is how often a critical problem that is still
	// open is reported again. Zero means never.
	CriticalRepeatInterval time.Duration
	// Retention is how long resolved problems are kept as history.
	Retention time.Duration

	// SkippedChecks lists checks that never run in this process and why
	// (disabled by configuration, optional CRDs not installed, no permission).
	// They appear in the status, and their open problems are resolved unless
	// KeepProblems is set. Checks that cannot run only temporarily are reported
	// by the snapshot (see checks.Snapshot.Skipped).
	SkippedChecks []SkippedCheck

	Store *store.Store
	// Notifiers are the notification targets. Each one remembers on its own
	// what it was told, so a target that fails is retried without sending
	// the message to the others again. There must be at least one (main uses
	// notify.Log when nothing is configured).
	Notifiers []notify.Notifier
	// MinSeverity is the lowest severity a target hears about, by
	// Notifier.Name. A target that is missing hears about everything; with
	// checks.Critical it hears only about critical problems. It applies to new
	// problems and reminders; the end of a problem goes to every target that
	// heard about its start.
	MinSeverity map[string]checks.Severity
	// BaseURL is the link to the web UI that notifications point to. It may
	// be empty.
	BaseURL string

	// Now is injectable so tests control the time.
	Now    func() time.Time
	Logger *slog.Logger

	mu         sync.Mutex // protects status, runSkipped and targets, which the web server reads
	status     Status
	runSkipped map[string]string       // checks that could not run in the last run
	targets    map[string]TargetStatus // by Notifier.Name

	// previousSkipped is what was skipped in the last run, to log only when
	// that changes. Only RunOnce touches it.
	previousSkipped map[string]string
	// loggedAnnotationWarnings are the annotation warnings logged so far, so
	// each is logged once while the process runs. They name the annotation
	// and its value, not every problem, so the set stays small. Only RunOnce
	// touches it.
	loggedAnnotationWarnings map[string]bool
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
	snap := e.Snapshot(ctx, start)
	runnable, skipped := e.splitSkipped(snap)
	findings, annotationProblems := runChecks(snap, e.Options, runnable, heldBackBy(snap, runnable))
	e.warnAboutAnnotations(findings, annotationProblems)
	for _, f := range findings {
		e.Logger.Debug("finding",
			"key", f.Key,
			"severity", f.Severity,
			"message", f.Message,
		)
	}

	unknown := e.checksWithUnknownState(skipped)
	var errs []error
	// Before saveState, which would otherwise write back the old "reported"
	// markers from the copy it reads.
	if err := e.finishDowntimes(ctx, snap, findings, unknown, start); err != nil {
		errs = append(errs, err)
	}
	if err := e.saveState(ctx, snap, findings, unknown, start); err != nil {
		errs = append(errs, err)
	}
	if err := e.notify(ctx, unknown, start); err != nil {
		errs = append(errs, err)
	}
	if err := e.cleanup(ctx, start); err != nil {
		errs = append(errs, err)
	}

	finished := e.Now()
	err := errors.Join(errs...)
	e.setRunResult(finished, skipped, snap.APIProblem, err)
	e.Logger.Info("check run finished",
		"findings", len(findings),
		"checks", len(runnable),
		"duration", finished.Sub(start).String(),
	)
	return err
}

// cleanup deletes resolved problems that are older than the retention time,
// and login sessions that have expired.
func (e *Engine) cleanup(ctx context.Context, now time.Time) error {
	deleted, err := e.Store.DeleteResolvedBefore(ctx, now.Add(-e.Retention))
	if err != nil {
		return fmt.Errorf("clean up old findings: %w", err)
	}
	if deleted > 0 {
		e.Logger.Info("old findings deleted", "count", deleted)
	}
	if _, err := e.Store.DeleteExpiredSessions(ctx, now); err != nil {
		return fmt.Errorf("clean up expired sessions: %w", err)
	}
	return nil
}

// runChecks runs every check, applies the smokealarm annotations, marks the
// consequences of the nodes in down and returns all findings sorted by key, so
// the output order is stable between runs. The sort keeps the order of findings
// with the same key, so "the first one" (see reconcile) is well defined. The second result lists the
// annotation values that could not be used.
func runChecks(snap *checks.Snapshot, opts checks.Options, list []checks.Check, down map[string]bool) ([]checks.Finding, []string) {
	var findings []checks.Finding
	for _, c := range list {
		findings = append(findings, c.Run(snap, opts)...)
	}
	findings, problems := checks.ApplyAnnotations(snap, findings)
	findings = checks.ApplyNodeCauses(snap, findings, down)
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Key < findings[j].Key })
	return findings, problems
}
