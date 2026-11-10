package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// Every notification target (mail, Slack, one Telegram chat, ...) keeps its own
// "reported" markers in the table deliveries. The queries in this file read and
// write the markers of one target, named by Notifier.Name. The columns of the
// same name in findings are the summary: the latest report to any target.

// targetColumns is selectColumns with the markers of one target instead of the
// summary. It is used together with targetJoin, whose parameter is the target.
const targetColumns = `f.id, f.key, f.check_name, f.severity, f.kind, f.namespace, f.name, f.reason, f.message,
	f.state, f.first_seen, f.last_seen, f.firing_at, f.resolved_at,
	d.firing_notified_at, d.resolved_notified_at, f.acknowledged_at, f.targets,
	f.caused_by, f.pending_since, d.start_heard_at, f.escalating_since, f.gone_since`

const targetJoin = ` FROM findings f LEFT JOIN deliveries d ON d.finding_id = f.id AND d.target = ?`

// Unnotified returns what still has to be reported to the target: firing
// problems whose start it was not told yet, and resolved problems whose end it
// was not told yet. An acknowledged problem is left out while it fires,
// because somebody already knows about it, even if its first message never
// went out.
func (s *Store) Unnotified(ctx context.Context, target string) ([]Record, error) {
	return s.query(ctx, `SELECT `+targetColumns+targetJoin+`
		WHERE (f.state = 'firing' AND d.firing_notified_at IS NULL AND f.acknowledged_at IS NULL)
		   OR (f.state = 'resolved' AND d.resolved_notified_at IS NULL)
		ORDER BY f.id`, target)
}

// CriticalDueForReminder returns critical problems that are still firing and
// were last reported to the target at or before the given time. Problems the
// target was not told about yet are left out; Unnotified returns those.
// Acknowledged problems get no reminders.
func (s *Store) CriticalDueForReminder(ctx context.Context, target string, before time.Time) ([]Record, error) {
	return s.query(ctx, `SELECT `+targetColumns+targetJoin+`
		WHERE f.state = 'firing' AND f.severity = 'critical'
		  AND d.firing_notified_at IS NOT NULL AND d.firing_notified_at <= ?
		  AND f.acknowledged_at IS NULL
		ORDER BY f.id`, target, unix(before))
}

// MarkFiringNotified remembers that the target was just told about these
// firing problems, for the first time or as a reminder.
func (s *Store) MarkFiringNotified(ctx context.Context, target string, ids []int64, at time.Time) error {
	return s.markNotified(ctx, ids, at,
		`INSERT INTO deliveries (finding_id, target, firing_notified_at, start_heard_at) VALUES (?1, ?2, ?3, ?3)
			ON CONFLICT (finding_id, target) DO UPDATE SET
				firing_notified_at = excluded.firing_notified_at,
				start_heard_at = COALESCE(start_heard_at, excluded.start_heard_at)`,
		`UPDATE findings SET firing_notified_at = ? WHERE id = ?`, target)
}

// MarkResolvedNotified remembers that the target was told about the end of
// these problems, or that it does not need to be told.
func (s *Store) MarkResolvedNotified(ctx context.Context, target string, ids []int64, at time.Time) error {
	return s.markNotified(ctx, ids, at,
		`INSERT INTO deliveries (finding_id, target, resolved_notified_at) VALUES (?, ?, ?)
			ON CONFLICT (finding_id, target) DO UPDATE SET resolved_notified_at = excluded.resolved_notified_at`,
		`UPDATE findings SET resolved_notified_at = ? WHERE id = ?`, target)
}

// markNotified sets the marker of the target and the summary in findings, for
// all ids in one transaction.
func (s *Store) markNotified(ctx context.Context, ids []int64, at time.Time, delivery, summary, target string) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful commit

	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, delivery, id, target, unix(at)); err != nil {
			return fmt.Errorf("mark finding %d as notified via %s: %w", id, target, err)
		}
		if _, err := tx.ExecContext(ctx, summary, unix(at), id); err != nil {
			return fmt.Errorf("mark finding %d as notified: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit notified markers: %w", err)
	}
	return nil
}

// clearFiringDeliveries forgets that these targets were told about this firing
// problem, so they are told again. The other targets keep their marker: they
// heard the start and must hear the end, even though the problem is no longer
// meant for them.
func clearFiringDeliveries(ctx context.Context, tx *sql.Tx, findingID int64, targets []string) error {
	for _, target := range targets {
		_, err := tx.ExecContext(ctx, `UPDATE deliveries SET firing_notified_at = NULL
			WHERE finding_id = ? AND target = ?`, findingID, target)
		if err != nil {
			return fmt.Errorf("clear delivery marker of finding %d for %s: %w", findingID, target, err)
		}
	}
	return nil
}

// forgetStartHeard makes these targets forget that they heard the start of this
// problem, so its end is not reported to them.
func forgetStartHeard(ctx context.Context, tx *sql.Tx, findingID int64, targets []string) error {
	for _, target := range targets {
		_, err := tx.ExecContext(ctx, `UPDATE deliveries SET start_heard_at = NULL
			WHERE finding_id = ? AND target = ?`, findingID, target)
		if err != nil {
			return fmt.Errorf("clear start marker of finding %d for %s: %w", findingID, target, err)
		}
	}
	return nil
}

// SyncTargets records which notification targets are configured now. It runs
// once at startup and returns the targets it added and removed.
//
// A new target is not sent the history, but it hears about what is firing now,
// so it does not stay silent during an alarm and then report only its end. The
// first start after the upgrade from a version without this table is the
// exception: no target is known yet, and the summary markers are what the old
// version has told, so they are copied and nothing is sent twice. A target that
// is no longer configured is forgotten; if it comes back, it starts afresh.
func (s *Store) SyncTargets(ctx context.Context, names []string) (added, removed []string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful commit

	known, err := knownTargets(ctx, tx)
	if err != nil {
		return nil, nil, err
	}
	for _, name := range known {
		if slices.Contains(names, name) {
			continue
		}
		if err := forgetTarget(ctx, tx, name); err != nil {
			return nil, nil, err
		}
		removed = append(removed, name)
	}
	upgrade := len(known) == 0
	for _, name := range names {
		if slices.Contains(known, name) {
			continue
		}
		if err := addTarget(ctx, tx, name, upgrade); err != nil {
			return nil, nil, err
		}
		added = append(added, name)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit notification targets: %w", err)
	}
	return added, removed, nil
}

func knownTargets(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM targets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("query notification targets: %w", err)
	}
	defer func() { _ = rows.Close() }() // a read error is reported by rows.Err below

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan notification target: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read notification targets: %w", err)
	}
	return names, nil
}

// addTarget registers a target and sets its markers. Without a firing marker,
// a firing problem counts as not reported yet, and one that has ended is closed
// without a message (see the engine), because the target never heard about it.
func addTarget(ctx context.Context, tx *sql.Tx, name string, copyFiring bool) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO targets (name) VALUES (?)`, name); err != nil {
		return fmt.Errorf("add notification target %s: %w", name, err)
	}
	seed := `INSERT INTO deliveries (finding_id, target, resolved_notified_at)
		SELECT id, ?, resolved_notified_at FROM findings
		WHERE resolved_notified_at IS NOT NULL
		ON CONFLICT (finding_id, target) DO UPDATE SET
			resolved_notified_at = excluded.resolved_notified_at`
	if copyFiring {
		seed = `INSERT INTO deliveries (finding_id, target, firing_notified_at, resolved_notified_at, start_heard_at)
		SELECT id, ?, firing_notified_at, resolved_notified_at, firing_notified_at FROM findings
		WHERE firing_notified_at IS NOT NULL OR resolved_notified_at IS NOT NULL
		ON CONFLICT (finding_id, target) DO UPDATE SET
			firing_notified_at = excluded.firing_notified_at,
			resolved_notified_at = excluded.resolved_notified_at,
			start_heard_at = excluded.start_heard_at`
	}
	if _, err := tx.ExecContext(ctx, seed, name); err != nil {
		return fmt.Errorf("copy markers to notification target %s: %w", name, err)
	}
	return nil
}

func forgetTarget(ctx context.Context, tx *sql.Tx, name string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM deliveries WHERE target = ?`, name); err != nil {
		return fmt.Errorf("delete deliveries of notification target %s: %w", name, err)
	}
	// A target that comes back starts afresh, switched on.
	if _, err := tx.ExecContext(ctx, `DELETE FROM target_mutes WHERE target = ?`, name); err != nil {
		return fmt.Errorf("delete mute of notification target %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM targets WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete notification target %s: %w", name, err)
	}
	return nil
}
