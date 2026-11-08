package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gagaschu/smokealarm/internal/notify"
	"github.com/gagaschu/smokealarm/internal/store"
)

// A notification target can be switched off ("muted") in the web UI. While it
// is off, notify skips it. The web UI only asks to switch it on again; the
// engine finishes that here, because only the engine knows which problems the
// target missed and must hear about again.

// finishMutes switches on the targets the web UI asked for. Like the end of a
// downtime, but for one target: what fires now and is meant for the target is
// reported again as new, and what ended while it was off stays silent. If the
// state of a check is unknown in this run it waits, so that no problem is
// reported that may be gone. One target that fails does not hold back the
// others; it stays off and the next run tries again.
func (e *Engine) finishMutes(ctx context.Context, unknown []string, now time.Time) error {
	if len(unknown) > 0 {
		return nil
	}
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
	var errs []error
	for _, n := range requested {
		if err := e.finishMute(ctx, n, open, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// finishMute switches one target on. An acknowledged problem keeps its marker,
// because it stays silent until its end, which the target then hears.
func (e *Engine) finishMute(ctx context.Context, n notify.Notifier, open []store.Record, now time.Time) error {
	name := n.Name()
	var ids []int64
	for _, rec := range open {
		if rec.State == store.StateFiring && rec.AcknowledgedAt == nil && e.routedTo(n, rec) {
			ids = append(ids, rec.ID)
		}
	}
	finished, err := e.Store.FinishUnmute(ctx, name, ids, now)
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
