package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
	"github.com/gagaschu/smokealarm/internal/notify"
	"github.com/gagaschu/smokealarm/internal/store"
)

// notify tells every target what it has not been told yet. The targets are
// independent: one that fails does not stop the others, and only it is tried
// again at the next run.
func (e *Engine) notify(ctx context.Context, unknown []string, now time.Time) error {
	silenced, err := e.Store.DowntimeKeys(ctx)
	if err != nil {
		return fmt.Errorf("load downtimes: %w", err)
	}
	open, err := e.Store.OpenRecords(ctx)
	if err != nil {
		return fmt.Errorf("load open findings: %w", err)
	}
	var errs []error
	for _, target := range e.Notifiers {
		if err := e.notifyTarget(ctx, target, unknown, silenced, open, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// notifyTarget sends one combined notification to one target, with everything
// it has not been told yet. Records are marked as reported to this target only
// after it succeeded, so a failed send is simply tried again at the next run,
// also after a restart. Reminders for critical problems that are due go into
// the same notification, except for checks whose state is unknown. Of an
// acknowledged problem only the end is reported; the store leaves out the rest.
// A problem in a downtime sends nothing at all.
func (e *Engine) notifyTarget(ctx context.Context, target notify.Notifier, unknown []string,
	silenced map[string]bool, open []store.Record, now time.Time) error {
	name := target.Name()
	records, err := e.Store.Unnotified(ctx, name)
	if err != nil {
		return fmt.Errorf("load findings not reported via %s: %w", name, err)
	}
	due, err := e.dueReminders(ctx, target, unknown, silenced, now)
	if err != nil {
		return fmt.Errorf("load findings due for a reminder via %s: %w", name, err)
	}

	r := e.buildReport(records, due, silenced, target)
	e.addAffected(&r.notification, HeldBack(open), target)
	n := r.notification
	if err := e.Store.MarkResolvedNotified(ctx, name, r.silentIDs, now); err != nil {
		return fmt.Errorf("mark silently resolved findings for %s: %w", name, err)
	}
	if len(n.New) == 0 && len(n.Reminders) == 0 && len(n.Resolved) == 0 {
		e.settleFailed(target, open, records, r.silentIDs)
		return nil
	}

	err = target.Send(ctx, n)
	var fallback *notify.FallbackError
	switch {
	case errors.As(err, &fallback):
		// The message went out, only not with the user's template. Sending
		// it again would not help and would repeat it everywhere.
		e.Logger.Warn("message sent with the built-in template", "target", target.Description(), "error", err)
		e.setTemplateResult(name, err, now)
		err = nil
	case err == nil:
		e.setTemplateResult(name, nil, now)
	}
	e.setNotifyResult(name, err, now)
	if err != nil {
		e.rememberFailed(name, r.firingIDs, r.resolvedIDs)
		return fmt.Errorf("send notification via %s: %w", target.Description(), err)
	}
	delete(e.failedIDs, name)
	e.Logger.Info("notification sent", "target", target.Description(),
		"new", len(n.New), "reminders", len(n.Reminders), "resolved", len(n.Resolved))

	// The message is out, so this must be written even if the process is being
	// stopped right now. Otherwise it goes out again after the restart.
	markCtx := context.WithoutCancel(ctx)
	if err := e.Store.MarkFiringNotified(markCtx, name, r.firingIDs, now); err != nil {
		return fmt.Errorf("mark new findings as reported via %s: %w", name, err)
	}
	if err := e.Store.MarkResolvedNotified(markCtx, name, r.resolvedIDs, now); err != nil {
		return fmt.Errorf("mark resolved findings as reported via %s: %w", name, err)
	}
	return nil
}

// report is what one run tells the reader, and which markers to set for it.
type report struct {
	notification notify.Notification
	firingIDs    []int64 // reported as new or as a reminder, once sending worked
	resolvedIDs  []int64 // reported as resolved, once sending worked
	silentIDs    []int64 // over without a mail
}

// buildReport sorts the unreported records and the due reminders for one
// target into a report. A firing problem the target is not meant for gets no
// marker, so the target hears about it as new once that changes (it gets
// worse, or the annotation changes).
func (e *Engine) buildReport(records, due []store.Record, silenced map[string]bool,
	target notify.Notifier) report {
	r := report{notification: notify.Notification{BaseURL: e.BaseURL}}
	n := &r.notification
	for _, rec := range records {
		firing := rec.State == store.StateFiring
		switch {
		case silenced[rec.Key] && firing:
			// Not marked as reported: the mail goes out when the downtime ends.
		case firing && rec.EscalatingSince != nil:
			// It just got worse. Whoever has not heard about it yet, like a
			// target for critical problems only, hears when that has lasted.
		case firing && rec.GoneSince != nil:
			// It is gone at the moment, so "new" would claim something that is
			// not true. It is reported if it comes back.
		case silenced[rec.Key]:
			// It went away during the downtime, so nothing is sent, not even
			// after it ends. The downtime also wins over an acknowledgement.
			r.silentIDs = append(r.silentIDs, rec.ID)
		case firing && !e.routedTo(target, rec):
			// Not for this target (yet).
		case firing:
			n.New = append(n.New, alertFor(rec))
			r.firingIDs = append(r.firingIDs, rec.ID)
		case rec.StartHeardAt == nil && (rec.AcknowledgedAt == nil || !e.routedTo(target, rec)):
			// It was fixed before this target heard about it. Telling the
			// reader it is resolved would be confusing. After an
			// acknowledgement somebody did hear about it, so that case
			// gets its mail, even if the first mail never went out, but
			// only at a target the problem is meant for. A target that heard
			// the start always hears the end, even if the problem is no longer
			// meant for it.
			r.silentIDs = append(r.silentIDs, rec.ID)
		default:
			n.Resolved = append(n.Resolved, alertFor(rec))
			r.resolvedIDs = append(r.resolvedIDs, rec.ID)
		}
	}
	for _, rec := range due {
		n.Reminders = append(n.Reminders, alertFor(rec))
		r.firingIDs = append(r.firingIDs, rec.ID)
	}
	sortAlerts(n.New)
	sortAlerts(n.Reminders)
	sortAlerts(n.Resolved)
	return r
}

// addAffected lists under every node that is down the problems it holds back,
// as far as the target would hear about them; the rest is not its business.
// New problems and reminders get the list; the end of a node needs none,
// because the problems it held back are gone or fire on their own then.
func (e *Engine) addAffected(n *notify.Notification, held map[string][]store.Record, target notify.Notifier) {
	for _, alerts := range [][]notify.Alert{n.New, n.Reminders} {
		for i := range alerts {
			if !alerts[i].IsNodeDown() {
				continue
			}
			for _, rec := range held[alerts[i].Object.Name] {
				if e.routedTo(target, rec) {
					alerts[i].Affected = append(alerts[i].Affected,
						notify.AffectedProblem{Object: rec.Object, Reason: rec.Reason})
				}
			}
		}
	}
}

// dueReminders returns the critical problems whose last report to the target
// is at least one repeat interval old. Problems of checks whose state is unknown are left
// out, because a reminder would claim they still exist; they are reminded of
// as soon as the check runs again. Problems in a downtime are left out too, and
// so are problems no longer meant for the target (the annotation changed),
// problems that just became critical (nobody has heard about that yet), and
// problems that are gone at the moment.
func (e *Engine) dueReminders(ctx context.Context, target notify.Notifier, unknown []string,
	silenced map[string]bool, now time.Time) ([]store.Record, error) {
	if e.CriticalRepeatInterval <= 0 {
		return nil, nil
	}
	records, err := e.Store.CriticalDueForReminder(ctx, target.Name(), now.Add(-e.CriticalRepeatInterval))
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(records, func(r store.Record) bool {
		return slices.Contains(unknown, r.Check) || silenced[r.Key] || r.EscalatingSince != nil || r.GoneSince != nil || !e.routedTo(target, r)
	}), nil
}

func alertFor(r store.Record) notify.Alert {
	alert := notify.Alert{Finding: r.Finding, Since: r.FirstSeen}
	if r.ResolvedAt != nil {
		alert.ResolvedAt = *r.ResolvedAt
	}
	return alert
}

// sortAlerts puts nodes that are down first, because the problems around them
// are their consequences, then critical problems, then orders by key for a
// stable result.
func sortAlerts(alerts []notify.Alert) {
	sort.Slice(alerts, func(i, j int) bool {
		a, b := alerts[i], alerts[j]
		if a.IsNodeDown() != b.IsNodeDown() {
			return a.IsNodeDown()
		}
		if a.Severity != b.Severity {
			return a.Severity == checks.Critical
		}
		return a.Key < b.Key
	})
}

// rememberFailed notes the problems whose message to the target just failed,
// so its error can be cleared once none of them is owed anymore.
func (e *Engine) rememberFailed(target string, idLists ...[]int64) {
	if e.failedIDs == nil {
		e.failedIDs = map[string]map[int64]bool{}
	}
	if e.failedIDs[target] == nil {
		e.failedIDs[target] = map[int64]bool{}
	}
	for _, ids := range idLists {
		for _, id := range ids {
			e.failedIDs[target][id] = true
		}
	}
}

// settleFailed runs when the target has nothing to send in this run. It
// forgets the failed messages that are no longer owed, and the error of the
// target once none is left. A firing problem stays owed while its message is
// only held back (a downtime, RESOLVE_AFTER, an escalation that waits, a check
// whose state is unknown), as long as it is meant for the target or the target
// heard its start (see stillOwed). A resolved problem stays owed until its end
// is reported, or it is over without a message (silent). The decision rests on
// what failed, not on the shape of this run's report, so a new rule that holds
// messages back needs no change here.
func (e *Engine) settleFailed(target notify.Notifier, open, unnotified []store.Record, silentIDs []int64) {
	name := target.Name()
	for id := range e.failedIDs[name] {
		if !e.stillOwed(target, id, open, unnotified, silentIDs) {
			delete(e.failedIDs[name], id)
		}
	}
	if len(e.failedIDs[name]) > 0 {
		return
	}
	delete(e.failedIDs, name)
	e.mu.Lock()
	defer e.mu.Unlock()
	if status, found := e.targets[name]; found {
		status.LastError = ""
		status.LastErrorAt = time.Time{}
		e.targets[name] = status
	}
}

// stillOwed says whether the target still has to hear about this problem. A
// firing problem is owed while it is not acknowledged and either is meant for
// the target or its start was heard there, because then the end follows. The
// per-target marker comes from the unreported records: a firing problem that
// is not among them was reported to the target, so it heard the start.
func (e *Engine) stillOwed(target notify.Notifier, id int64, open, unnotified []store.Record, silentIDs []int64) bool {
	for _, r := range open {
		if r.ID != id {
			continue
		}
		if r.State != store.StateFiring || r.AcknowledgedAt != nil {
			return false
		}
		for _, u := range unnotified {
			if u.ID == id {
				return u.StartHeardAt != nil || e.routedTo(target, r)
			}
		}
		return true
	}
	for _, r := range unnotified {
		if r.ID == id {
			return r.State == store.StateResolved && !slices.Contains(silentIDs, id)
		}
	}
	return false
}
