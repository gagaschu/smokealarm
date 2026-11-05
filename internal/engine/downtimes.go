package engine

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// finishDowntimes removes the downtimes that have ended and brings back the
// alarms they covered, for the targets they are meant for now; a target that
// heard the start but is no longer meant for it keeps its marker and hears the
// end. If that fails, the downtime stays in the table and keeps
// the problem silent; the next run tries again. The downtime of a check whose
// state is unknown waits until the check runs again: a mail now would claim a
// problem that may be gone.
func (e *Engine) finishDowntimes(ctx context.Context, snap *checks.Snapshot, findings []checks.Finding, unknown []string, now time.Time) error {
	expired, err := e.Store.ExpiredDowntimes(ctx, now)
	if err != nil {
		return fmt.Errorf("load expired downtimes: %w", err)
	}
	if len(expired) == 0 {
		return nil
	}
	open, err := e.Store.OpenRecords(ctx)
	if err != nil {
		return fmt.Errorf("load open findings: %w", err)
	}
	for _, d := range expired {
		if slices.Contains(unknown, d.Check) || checks.VolumeUsageUnknown(snap, d.Check, d.Object) {
			continue
		}
		targets := e.routedNamesNow(open, findings, d.Key)
		if err := e.Store.FinishDowntime(ctx, d.Key, now, targets, !hasFinding(findings, d.Key)); err != nil {
			return fmt.Errorf("finish downtime: %w", err)
		}
		e.Logger.Info("downtime finished", "key", d.Key)
	}
	return nil
}

// hasFinding reports whether the problem with this key was found in this run.
func hasFinding(findings []checks.Finding, key string) bool {
	return slices.ContainsFunc(findings, func(f checks.Finding) bool { return f.Key == key })
}
