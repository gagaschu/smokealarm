// Package engine runs the main loop: take a snapshot, run all checks and
// handle the findings. In this phase the findings are only logged; later
// phases add state tracking in SQLite and notifications.
package engine

import (
	"context"
	"log/slog"
	"sort"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// Engine holds everything the loop needs. main() creates the dependencies
// and passes them in, so the loop can be tested with fake functions.
type Engine struct {
	// Snapshot returns the current cluster objects (the informer caches).
	Snapshot func(now time.Time) *checks.Snapshot
	Checks   []checks.Check
	Options  checks.Options
	Interval time.Duration
	// Now is injectable so tests control the time.
	Now    func() time.Time
	Logger *slog.Logger
}

// Run executes one pass immediately and then one per interval until the
// context is cancelled.
func (e *Engine) Run(ctx context.Context) {
	e.RunOnce()

	ticker := time.NewTicker(e.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.RunOnce()
		}
	}
}

// RunOnce takes a snapshot, runs all checks and logs what they found.
func (e *Engine) RunOnce() {
	start := e.Now()
	snap := e.Snapshot(start)
	findings := runChecks(snap, e.Options, e.Checks)

	for _, f := range findings {
		e.Logger.Warn("finding",
			"key", f.Key,
			"severity", f.Severity,
			"object", f.Object.Kind+" "+f.Object.Namespace+"/"+f.Object.Name,
			"reason", f.Reason,
			"message", f.Message,
		)
	}
	e.Logger.Info("check run finished",
		"findings", len(findings),
		"checks", len(e.Checks),
		"duration", e.Now().Sub(start).String(),
	)
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
