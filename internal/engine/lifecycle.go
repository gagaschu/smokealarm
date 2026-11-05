package engine

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/store"
)

// saveState compares the findings with the stored problems and writes the
// result back in one transaction.
func (e *Engine) saveState(ctx context.Context, snap *checks.Snapshot, findings []checks.Finding, skipped []string, now time.Time) error {
	open, err := e.Store.OpenRecords(ctx)
	if err != nil {
		return fmt.Errorf("load open findings: %w", err)
	}
	// Problems of checks whose state is unknown stay as they are. Problems of
	// checks that are disabled (or gone) are not in the skipped list and are
	// resolved, not left "firing" forever.
	// The same goes for a single problem this run says nothing about. Reminders
	// go on: a problem that was firing when its node went down is not touched,
	// except that a wait for RESOLVE_AFTER starts again.
	var unmeasured []store.Record
	open = slices.DeleteFunc(open, func(r store.Record) bool {
		if !checks.VolumeUsageUnknown(snap, r.Check, r.Object) {
			return false
		}
		if r.GoneSince != nil {
			unmeasured = append(unmeasured, restartResolveWait(r, now))
		}
		return true
	})
	changes := e.reconcile(open, findings, skipped, now)
	changes.Update = append(changes.Update, unmeasured...)
	if err := e.Store.Apply(ctx, changes); err != nil {
		return fmt.Errorf("save findings: %w", err)
	}
	return nil
}

// reconcile decides what changes: it compares the stored open problems with
// the current findings. It does no I/O, which keeps the rules easy to test.
// Problems of the skipped checks (checks whose state is unknown) are left
// alone, because "no findings" would just mean "unknown". Findings caused by a
// down node are held back (see holdBack).
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
		updated := e.refresh(record, f, now)
		if record.FiringNotifiedAt != nil && updated.FiringNotifiedAt == nil {
			changes.Escalated = append(changes.Escalated,
				store.Escalation{ID: record.ID, Targets: e.routedNames(updated)})
		}
		changes.Update = append(changes.Update, updated)
	}

	for _, record := range open {
		if seen[record.Key] {
			continue
		}
		if slices.Contains(skipped, record.Check) {
			if record.GoneSince != nil {
				changes.Update = append(changes.Update, restartResolveWait(record, now))
			}
			continue
		}
		if record.State == store.StatePending {
			e.Logger.Info("finding dropped before it fired", "key", record.Key)
			changes.Delete = append(changes.Delete, record.ID)
			continue
		}
		changes.Update = append(changes.Update, e.resolveOrWait(record, now))
	}
	return changes
}

// resolveOrWait handles a firing problem that was not found in this run. It
// counts as resolved only once it has been gone for ResolveAfter, from the run
// in which it was first gone: a crash loop is gone between two back-offs and
// comes back, and that must not send a pair of messages every time.
func (e *Engine) resolveOrWait(record store.Record, now time.Time) store.Record {
	if record.GoneSince == nil {
		record.GoneSince = &now
		e.Logger.Info("finding no longer seen", "key", record.Key)
	}
	if now.Sub(*record.GoneSince) < e.ResolveAfter {
		return record
	}
	record.State = store.StateResolved
	record.ResolvedAt = record.GoneSince
	record.GoneSince = nil
	record.EscalatingSince = nil
	e.Logger.Info("finding resolved", "key", record.Key)
	return record
}

// restartResolveWait is for a problem that waits for ResolveAfter in a run that
// says nothing about it. The wait counts time in which the check looked and did
// not find it, so it starts again from this run. Right after an outage the data
// can still be old for a moment, and that must not resolve anything.
func restartResolveWait(record store.Record, now time.Time) store.Record {
	record.GoneSince = &now
	return record
}

// newRecord creates the record for a problem seen for the first time. With
// AlertAfter 0 it fires right away, unless a down node holds it back.
func (e *Engine) newRecord(f checks.Finding, now time.Time) store.Record {
	record := store.Record{Finding: f, State: store.StatePending, FirstSeen: now, LastSeen: now, PendingSince: now}
	e.Logger.Info("finding pending", "key", f.Key, "severity", f.Severity)
	if f.CausedBy != "" {
		e.Logger.Info("finding held back, its node is down", "key", f.Key, "node", f.CausedBy)
		return record
	}
	if e.AlertAfter <= 0 {
		record.State = store.StateFiring
		record.FiringAt = &now
		e.Logger.Info("finding firing", "key", f.Key, "severity", f.Severity)
	}
	return record
}

// refresh updates a known problem with the latest details and lets it fire
// once it has lasted long enough and no down node holds it back.
func (e *Engine) refresh(record store.Record, f checks.Finding, now time.Time) store.Record {
	worse := record.Severity == checks.Warning && f.Severity == checks.Critical
	record.Severity = f.Severity
	record.Reason = f.Reason
	record.Message = f.Message
	record.Targets = f.Targets // a changed annotation counts from now on
	record.LastSeen = now
	if record.GoneSince != nil {
		record.GoneSince = nil
		e.Logger.Info("finding seen again", "key", f.Key)
		// If it was about to be reported as worse, that state has only just
		// begun again. It is kept, not cleared: the severity is critical
		// already, so a new escalation would not be noticed anymore.
		if record.EscalatingSince != nil {
			record.EscalatingSince = &now
		}
	}

	if record.State == store.StateFiring {
		record = e.trackEscalation(record, worse, now)
	}
	if record.State == store.StatePending {
		record = e.holdBack(record, f, now)
	}
	if record.State == store.StatePending && record.CausedBy == "" && now.Sub(clockStart(record)) >= e.AlertAfter {
		record.State = store.StateFiring
		record.FiringAt = &now
		e.Logger.Info("finding firing", "key", f.Key, "severity", f.Severity)
	}
	return record
}

// trackEscalation decides when a firing problem that got worse is reported. It
// is still the same problem, so it is not resolved and restarted. But somebody
// who read "warning" must hear that it is critical now, once that has lasted
// AlertAfter: a problem that flips between the two reports nothing per flip.
// Clearing the marker then lists the problem as new again. Getting better is
// not reported; the resolution will be. An acknowledged problem stays
// acknowledged: somebody already knows, so the escalation is not reported. (A
// problem that is still pending is simply reported with the higher severity
// when it fires.)
func (e *Engine) trackEscalation(record store.Record, worse bool, now time.Time) store.Record {
	if record.Severity != checks.Critical {
		record.EscalatingSince = nil
		return record
	}
	if worse {
		record.EscalatingSince = &now
	}
	if record.EscalatingSince == nil || now.Sub(*record.EscalatingSince) < e.AlertAfter {
		return record
	}
	record.EscalatingSince = nil
	if record.AcknowledgedAt == nil {
		record.FiringNotifiedAt = nil
		e.Logger.Info("finding escalated", "key", record.Key, "severity", record.Severity, "reason", record.Reason)
	}
	return record
}

// holdBack decides whether a pending problem waits for a down node: as long as
// the finding says it is caused by one (see checks.ApplyNodeCauses). When that
// ends, because the node is back or because the problem can no longer be
// explained by it, the ALERT_AFTER clock starts again: the pods need a moment
// to become ready, and a problem that lasts beyond that is a problem of its
// own. Firing problems are never held back: what was reported stays reported.
func (e *Engine) holdBack(record store.Record, f checks.Finding, now time.Time) store.Record {
	switch {
	case f.CausedBy != "":
		if record.CausedBy == "" {
			e.Logger.Info("finding held back, its node is down", "key", f.Key, "node", f.CausedBy)
		}
		record.CausedBy = f.CausedBy
	case record.CausedBy != "":
		e.Logger.Info("finding no longer held back by its node", "key", f.Key, "node", record.CausedBy)
		record.CausedBy = ""
		record.PendingSince = now
	}
	return record
}

// clockStart is when the ALERT_AFTER clock of a pending problem started.
func clockStart(record store.Record) time.Time {
	if record.PendingSince.IsZero() {
		return record.FirstSeen
	}
	return record.PendingSince
}
