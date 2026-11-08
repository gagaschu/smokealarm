package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/notify"
	"github.com/gagaschu/smokealarm/internal/store"
)

// A notification target can be switched off ("muted") in the web UI. While it
// is off, notify skips it. The web UI only asks to switch it on again; the
// engine finishes that here, because only the engine knows which problems the
// target missed and must hear about again.

// finishMutes switches on the targets the web UI asked for. Like the end of a
// downtime, but for one target: what fires now and is meant for the target is
// reported again as new, and what ended while it was off stays silent. A
// problem whose check state is unknown in this run, or whose volume was not
// measured (a node is down), may be gone, so it is left as it was. The
// switch-on does not wait for such a run to pass: a check that is skipped for
// good (no permission, kubelet stats that always fail) would keep the target
// off forever. One target that fails does not hold back the others; it stays
// off and the next run tries again.
func (e *Engine) finishMutes(ctx context.Context, snap *checks.Snapshot, unknown []string, now time.Time) error {
	mutes, err := e.Store.TargetMutes(ctx)
	if err != nil {
		return fmt.Errorf("load muted targets: %w", err)
	}
	var requested []notify.Notifier
	for _, n := range e.Notifiers {
		if m, found := mutes[n.Name()]; found && m.UnmuteRequestedAt != nil {
			requested = append(requested, n)
		}
	}
	if len(requested) == 0 {
		return nil
	}
	open, err := e.Store.OpenRecords(ctx)
	if err != nil {
		return fmt.Errorf("load open findings: %w", err)
	}
	// Left out of both lists, the problem keeps the target's markers.
	open = slices.DeleteFunc(open, func(rec store.Record) bool {
		return slices.Contains(unknown, rec.Check) || checks.VolumeUsageUnknown(snap, rec.Check, rec.Object)
	})
	var errs []error
	for _, n := range requested {
		if err := e.finishMute(ctx, n, open, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// finishMute switches one target on. An acknowledged problem keeps its marker,
// because it stays silent until its end, which the target then hears. Only
// problems meant for the target now are told again; one it heard but is no
// longer meant for keeps its marker and hears the end. A problem that is gone
// at the moment (waiting for RESOLVE_AFTER) ended while the target was off, as
// at the end of a downtime: its end stays silent, and if it comes back it is
// new.
func (e *Engine) finishMute(ctx context.Context, n notify.Notifier, open []store.Record, now time.Time) error {
	name := n.Name()
	var firingIDs, goneIDs []int64
	for _, rec := range open {
		switch {
		case rec.State != store.StateFiring || rec.AcknowledgedAt != nil || !e.routedTo(n, rec):
		case rec.GoneSince != nil:
			goneIDs = append(goneIDs, rec.ID)
		default:
			firingIDs = append(firingIDs, rec.ID)
		}
	}
	finished, err := e.Store.FinishUnmute(ctx, name, firingIDs, goneIDs, now)
	if err != nil {
		return fmt.Errorf("switch on notification target %s: %w", n.Description(), err)
	}
	if !finished {
		return nil // switched off again in the meantime
	}
	// Nothing from before is owed any more: what still fires is sent again as
	// new, and what ended is over without a message. So an old sending error
	// would only claim a problem that is gone.
	delete(e.failedIDs, name)
	e.mu.Lock()
	if status, found := e.targets[name]; found {
		status.LastError = ""
		status.LastErrorAt = time.Time{}
		e.targets[name] = status
	}
	e.mu.Unlock()
	e.Logger.Info("notification target switched on", "target", n.Description())
	return nil
}
