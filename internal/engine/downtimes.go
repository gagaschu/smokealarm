package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/store"
)

// finishDowntimes removes the downtimes that have ended and brings back the
// alarms they covered, for the targets they are meant for now; a target that
// heard the start but is no longer meant for it keeps its marker and hears the
// end. If that fails, the downtime stays in the table and keeps
// the problem silent; the next run tries again. One downtime that fails does
// not hold back the others, unless the process is stopping. The downtime of a check whose
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
	openByKey := map[string]store.Record{}
	for _, r := range open {
		openByKey[r.Key] = r
	}
	found := map[string]checks.Finding{} // runChecks keeps every key once
	for _, f := range findings {
		found[f.Key] = f
	}

	var errs []error
	for _, d := range expired {
		if slices.Contains(unknown, d.Check) || checks.VolumeUsageUnknown(snap, d.Check, d.Object) {
			continue
		}
		f, foundNow := found[d.Key]
		targets := e.routedNamesNow(openByKey, f, foundNow, d.Key)
		if err := e.Store.FinishDowntime(ctx, d.Key, now, targets, !foundNow); err != nil {
			errs = append(errs, fmt.Errorf("finish downtime %s: %w", d.Key, err))
			if ctx.Err() != nil {
				break // the process stops: the others would fail the same way
			}
			continue
		}
		e.Logger.Info("downtime finished", "key", d.Key)
	}
	return errors.Join(errs...)
}
