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
	"slices"
	"sort"
	"strings"
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

// SkippedCheck is a check that does not run, with the reason to show.
type SkippedCheck struct {
	Name   string
	Reason string
	// KeepProblems leaves the open problems of the check as they are, because
	// not running it says nothing about them (a missing permission, for
	// example). Without it they count as gone.
	KeepProblems bool
}

// CheckStatus is one line of the check list on the status page.
type CheckStatus struct {
	Name   string
	Active bool
	Reason string // why the check is not active; empty if it is
}

// TargetStatus is one line of the target list on the status page.
type TargetStatus struct {
	Name        string // Notifier.Name
	Description string // Notifier.Description, without secrets
	// CriticalOnly says that the target hears only about critical problems
	// (see Engine.MinSeverity).
	CriticalOnly bool
	LastSentAt   time.Time
	LastError    string    // empty if the last send worked
	LastErrorAt  time.Time // zero if there is no error
}

// Status is what the engine reports about itself, for the status page.
type Status struct {
	LastRun      time.Time // zero before the first run has finished
	LastRunError string    // empty if the last run had no errors
	Checks       []CheckStatus
	Targets      []TargetStatus // in the order of Engine.Notifiers
}

// Status returns a copy of the current status.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.status
	s.Checks = e.checkStatuses()
	for _, n := range e.Notifiers {
		target := e.targets[n.Name()]
		target.Name = n.Name()
		target.Description = n.Description()
		target.CriticalOnly = e.MinSeverity[n.Name()] == checks.Critical
		s.Targets = append(s.Targets, target)
	}
	return s
}

// checkStatuses lists the active and the skipped checks, sorted by name.
func (e *Engine) checkStatuses() []CheckStatus {
	var statuses []CheckStatus
	for _, c := range e.Checks {
		if reason, skipped := e.runSkipped[c.Name]; skipped {
			statuses = append(statuses, CheckStatus{Name: c.Name, Reason: reason})
			continue
		}
		statuses = append(statuses, CheckStatus{Name: c.Name, Active: true})
	}
	for _, c := range e.SkippedChecks {
		statuses = append(statuses, CheckStatus{Name: c.Name, Reason: c.Reason})
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].Name < statuses[j].Name })
	return statuses
}

// splitSkipped separates the checks that can run from those the snapshot
// reports as unable to run this time (check name -> reason). It also logs
// when that set changes, so a permanent problem is not repeated every run.
func (e *Engine) splitSkipped(snap *checks.Snapshot) ([]checks.Check, map[string]string) {
	var runnable []checks.Check
	skipped := map[string]string{}
	for _, c := range e.Checks {
		if reason, found := snap.Skipped[c.Name]; found {
			skipped[c.Name] = reason
			continue
		}
		runnable = append(runnable, c)
	}

	for name, reason := range skipped {
		if e.previousSkipped[name] != reason {
			e.Logger.Warn("check skipped", "check", name, "reason", reason)
		}
	}
	for name := range e.previousSkipped {
		if _, stillSkipped := skipped[name]; !stillSkipped {
			e.Logger.Info("check runs again", "check", name)
		}
	}
	e.previousSkipped = skipped
	return runnable, skipped
}

// skippedNames returns the names of the skipped checks, sorted.
func skippedNames(skipped map[string]string) []string {
	names := make([]string, 0, len(skipped))
	for name := range skipped {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// checksWithUnknownState names the checks that say nothing about their problems
// in this run: those the snapshot skipped, and those skipped for the whole
// process with KeepProblems.
func (e *Engine) checksWithUnknownState(runSkipped map[string]string) []string {
	names := skippedNames(runSkipped)
	for _, c := range e.SkippedChecks {
		if c.KeepProblems {
			names = append(names, c.Name)
		}
	}
	return names
}

// setRunResult remembers when the last run finished, which checks were
// skipped, and how it went.
func (e *Engine) setRunResult(finished time.Time, skipped map[string]string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status.LastRun = finished
	e.runSkipped = skipped
	e.status.LastRunError = ""
	if err != nil {
		e.status.LastRunError = err.Error()
	}
}

// setNotifyResult remembers whether the last send to a target worked. The
// error is cleared by the next success or when nothing is left to send (see
// clearNotifyError), so the status shows the current situation and not an old
// incident.
func (e *Engine) setNotifyResult(target string, err error, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.targets == nil {
		e.targets = map[string]TargetStatus{}
	}
	status := e.targets[target]
	if err == nil {
		status.LastSentAt = now
		status.LastError = ""
		status.LastErrorAt = time.Time{}
	} else {
		status.LastError = err.Error()
		status.LastErrorAt = now
	}
	e.targets[target] = status
}

// clearNotifyError removes the error of a target that has nothing left to send:
// what failed is over or acknowledged, so no message is waiting anymore.
func (e *Engine) clearNotifyError(target string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if status, found := e.targets[target]; found {
		status.LastError = ""
		status.LastErrorAt = time.Time{}
		e.targets[target] = status
	}
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
	e.setRunResult(finished, skipped, err)
	e.Logger.Info("check run finished",
		"findings", len(findings),
		"checks", len(runnable),
		"duration", finished.Sub(start).String(),
	)
	return err
}

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
	// go on: a problem that was firing when its node went down is not touched.
	open = slices.DeleteFunc(open, func(r store.Record) bool {
		return checks.VolumeUsageUnknown(snap, r.Check, r.Object)
	})
	changes := e.reconcile(open, findings, skipped, now)
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
		if seen[record.Key] || slices.Contains(skipped, record.Check) {
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
	held := HeldBack(open)
	var errs []error
	for _, target := range e.Notifiers {
		if err := e.notifyTarget(ctx, target, unknown, silenced, held, now); err != nil {
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
	silenced map[string]bool, held map[string][]store.Record, now time.Time) error {
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
	e.addAffected(r.notification, held, target)
	n := r.notification
	if err := e.Store.MarkResolvedNotified(ctx, name, r.silentIDs, now); err != nil {
		return fmt.Errorf("mark silently resolved findings for %s: %w", name, err)
	}
	if len(n.New) == 0 && len(n.Reminders) == 0 && len(n.Resolved) == 0 {
		e.clearNotifyError(name)
		return nil
	}

	err = target.Send(ctx, n)
	e.setNotifyResult(name, err, now)
	if err != nil {
		return fmt.Errorf("send notification via %s: %w", target.Description(), err)
	}
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

// HeldBack groups the problems that wait for a down node by the node's name.
// The web server uses it too.
func HeldBack(open []store.Record) map[string][]store.Record {
	held := map[string][]store.Record{}
	for _, r := range open {
		if r.State == store.StatePending && r.CausedBy != "" {
			held[r.CausedBy] = append(held[r.CausedBy], r)
		}
	}
	return held
}

// addAffected lists under every node that is down the problems it holds back,
// as far as the target would hear about them; the rest is not its business.
// New problems and reminders get the list; the end of a node needs none,
// because the problems it held back are gone or fire on their own then.
func (e *Engine) addAffected(n notify.Notification, held map[string][]store.Record, target notify.Notifier) {
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
// the output order is stable between runs. The second result lists the
// annotation values that could not be used.
func runChecks(snap *checks.Snapshot, opts checks.Options, list []checks.Check, down map[string]bool) ([]checks.Finding, []string) {
	var findings []checks.Finding
	for _, c := range list {
		findings = append(findings, c.Run(snap, opts)...)
	}
	findings, problems := checks.ApplyAnnotations(snap, findings)
	findings = checks.ApplyNodeCauses(snap, findings, down)
	sort.Slice(findings, func(i, j int) bool { return findings[i].Key < findings[j].Key })
	return findings, problems
}

// heldBackBy returns the down nodes whose consequences are held back in this
// run. That is only the case while node-not-ready runs: switched off, or unable
// to run, it would leave a failed node without any alarm, and its consequences
// are reported as usual instead.
func heldBackBy(snap *checks.Snapshot, runnable []checks.Check) map[string]bool {
	for _, c := range runnable {
		if c.Name == checks.CheckNodeNotReady {
			return checks.DownNodes(snap)
		}
	}
	return nil
}

// routedTo says whether the target is meant to hear about a problem: its
// severity is high enough for the target, and the targets annotation, if any,
// names the kind of the target.
func (e *Engine) routedTo(target notify.Notifier, rec store.Record) bool {
	if e.MinSeverity[target.Name()] == checks.Critical && rec.Severity != checks.Critical {
		return false
	}
	return e.listedTarget(rec.Targets, target)
}

// Audience says who hears about a problem, for the web UI: the descriptions of
// the targets it is meant for, and whether that is every target. It uses the
// same rule as the sending, so a typo in an annotation (which goes to every
// target) or a warning for critical-only targets shows what really happens.
// The web server calls it; it reads only fields that do not change after the
// start.
func (e *Engine) Audience(rec store.Record) (targets []string, all bool) {
	for _, n := range e.Notifiers {
		if e.routedTo(n, rec) {
			targets = append(targets, n.Description())
		}
	}
	return targets, len(targets) == len(e.Notifiers)
}

// routedNamesNow returns the targets the open problem with this key is meant for
// in this run. It runs before the findings are saved, so the severity and the
// targets come from the current finding, if there is one.
func (e *Engine) routedNamesNow(open []store.Record, findings []checks.Finding, key string) []string {
	for _, r := range open {
		if r.Key != key {
			continue
		}
		for _, f := range findings {
			if f.Key == key {
				r.Severity, r.Targets = f.Severity, f.Targets
			}
		}
		return e.routedNames(r)
	}
	return nil
}

// hasFinding reports whether the problem with this key was found in this run.
func hasFinding(findings []checks.Finding, key string) bool {
	return slices.ContainsFunc(findings, func(f checks.Finding) bool { return f.Key == key })
}

// routedNames returns the names of the targets the problem is meant for.
func (e *Engine) routedNames(rec store.Record) []string {
	var names []string
	for _, n := range e.Notifiers {
		if e.routedTo(n, rec) {
			names = append(names, n.Name())
		}
	}
	return names
}

// listedTarget says whether the target is one of the listed kinds. A list that
// names no configured target at all (a typo, a target that was removed) counts
// as no list, so a mistake in an annotation never makes an alarm silent.
func (e *Engine) listedTarget(listed []string, target notify.Notifier) bool {
	if len(listed) == 0 || slices.Contains(listed, notify.Kind(target.Name())) {
		return true
	}
	return !slices.ContainsFunc(listed, e.configuredKind)
}

// warnAboutAnnotations logs annotation values that cannot be used (an invalid
// severity, target names that are not configured) and targets lists that send
// a problem to no target at all (only critical-only targets, for a warning).
// Each sentence names the object or namespace that carries the annotation and
// is logged once while the process runs, not in every run.
func (e *Engine) warnAboutAnnotations(findings []checks.Finding, severityProblems []string) {
	for _, problem := range severityProblems {
		e.warnOnce("annotation ignored", problem)
	}
	for _, f := range findings {
		if len(f.Targets) == 0 {
			continue
		}
		for _, name := range f.Targets {
			if !e.configuredKind(name) {
				e.warnOnce("annotation ignored", fmt.Sprintf("%s: %s lists %q, which is not a configured notification target",
					f.TargetsFrom, checks.TargetsAnnotation, name))
			}
		}
		if len(e.routedNames(store.Record{Finding: f})) == 0 {
			e.warnOnce("problems reach no notification target", fmt.Sprintf(
				"%s: its %s problems reach no notification target: %s %q lists only targets that get critical problems only",
				f.TargetsFrom, f.Severity, checks.TargetsAnnotation, strings.Join(f.Targets, ",")))
		}
	}
}

// warnOnce logs a warning the first time this process sees it.
func (e *Engine) warnOnce(msg, problem string) {
	if e.loggedAnnotationWarnings[problem] {
		return
	}
	if e.loggedAnnotationWarnings == nil {
		e.loggedAnnotationWarnings = map[string]bool{}
	}
	e.loggedAnnotationWarnings[problem] = true
	e.Logger.Warn(msg, "problem", problem)
}

// configuredKind says whether a target of this kind is configured.
func (e *Engine) configuredKind(kind string) bool {
	for _, n := range e.Notifiers {
		if notify.Kind(n.Name()) == kind {
			return true
		}
	}
	return false
}
