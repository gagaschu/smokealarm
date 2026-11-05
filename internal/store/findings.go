package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// State is where a finding is in its life cycle.
type State string

// The states of a finding. Pending problems have not lasted long enough to
// report yet; firing ones have; resolved ones are gone again.
const (
	StatePending  State = "pending"
	StateFiring   State = "firing"
	StateResolved State = "resolved"
)

// Record is a finding as stored in the database, with its life cycle times.
// The *NotifiedAt fields remember what has already been reported, so nothing
// is sent twice by accident and failed notifications are retried.
// FiringNotifiedAt holds the latest report, which may be a reminder. Records
// read for one notification target (see Unnotified) carry that target's
// markers; all other records carry the summary over all targets: the latest
// report to any of them, empty if no target has been told.
// StartHeardAt is only read per target: when that target first heard about the
// problem. Unlike FiringNotifiedAt it stays when the problem is to be reported
// again, so a target that heard the start always hears the end.
// AcknowledgedAt is set once somebody has acknowledged the problem in the web
// UI; from then on only its end is reported.
type Record struct {
	ID int64
	checks.Finding
	State              State
	FirstSeen          time.Time
	LastSeen           time.Time
	FiringAt           *time.Time
	ResolvedAt         *time.Time
	FiringNotifiedAt   *time.Time
	StartHeardAt       *time.Time
	ResolvedNotifiedAt *time.Time
	AcknowledgedAt     *time.Time
	// PendingSince is when the ALERT_AFTER clock started: FirstSeen, or the
	// run in which the down node that held the problem back came back.
	// FirstSeen stays the real start for the reader. The cause itself is
	// Finding.CausedBy.
	PendingSince time.Time
}

// Changes is a set of modifications that is written in one transaction.
type Changes struct {
	Insert []Record
	Update []Record // matched by ID; see Apply for the fields that are written
	Delete []int64
	// Escalated lists firing problems that got worse: the listed targets
	// forget that they were told, and hear about them again.
	Escalated []Escalation
}

// Escalation is a firing problem that got worse. Targets are the notification
// targets (Notifier.Name) it is meant for now; only they are told again. A
// target that heard the start but is no longer meant for it keeps its marker,
// so it still hears the end.
type Escalation struct {
	ID      int64
	Targets []string
}

// selectColumns lists the columns in the order scanRecord reads them. The start
// marker exists per target only, see targetColumns.
const selectColumns = `id, key, check_name, severity, kind, namespace, name, reason, message,
	state, first_seen, last_seen, firing_at, resolved_at, firing_notified_at, resolved_notified_at, acknowledged_at, targets,
	caused_by, pending_since, NULL`

// OpenRecords returns all problems that are pending or firing.
func (s *Store) OpenRecords(ctx context.Context) ([]Record, error) {
	return s.query(ctx, `SELECT `+selectColumns+` FROM findings
		WHERE state IN ('pending', 'firing') ORDER BY id`)
}

// Record returns one problem by its id, in any state. The second result says
// whether it exists.
func (s *Store) Record(ctx context.Context, id int64) (Record, bool, error) {
	records, err := s.query(ctx, `SELECT `+selectColumns+` FROM findings WHERE id = ?`, id)
	if err != nil {
		return Record{}, false, err
	}
	if len(records) == 0 {
		return Record{}, false, nil
	}
	return records[0], true, nil
}

// Resolved returns resolved problems, newest first, for the history page. An
// empty namespace means all namespaces. At most limit records are returned.
func (s *Store) Resolved(ctx context.Context, namespace string, limit int) ([]Record, error) {
	return s.query(ctx, `SELECT `+selectColumns+` FROM findings
		WHERE state = 'resolved' AND (? = '' OR namespace = ?)
		ORDER BY resolved_at DESC, id DESC LIMIT ?`, namespace, namespace, limit)
}

// Namespaces returns the namespaces that appear in the history, for the
// filter on the history page. Cluster-scoped objects have no namespace and
// are not listed.
func (s *Store) Namespaces(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT namespace FROM findings
		WHERE state = 'resolved' AND namespace <> '' ORDER BY namespace`)
	if err != nil {
		return nil, fmt.Errorf("query namespaces: %w", err)
	}
	defer func() { _ = rows.Close() }() // a read error is reported by rows.Err below

	var namespaces []string
	for rows.Next() {
		var namespace string
		if err := rows.Scan(&namespace); err != nil {
			return nil, fmt.Errorf("scan namespace: %w", err)
		}
		namespaces = append(namespaces, namespace)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read namespaces: %w", err)
	}
	return namespaces, nil
}

// Apply writes all changes or none. Update only touches the columns that
// change during a life cycle (severity, reason, message, targets, state,
// last_seen, firing_at, resolved_at). firing_notified_at is written too, so that a
// problem that got worse can be reported again; the markers are otherwise set
// by MarkFiringNotified and MarkResolvedNotified. acknowledged_at is not
// written: the engine works on a copy it read earlier, and writing that copy
// back could undo an acknowledgement made in the web UI in the meantime.
func (s *Store) Apply(ctx context.Context, changes Changes) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful commit

	for _, r := range changes.Insert {
		_, err := tx.ExecContext(ctx, `INSERT INTO findings
			(key, check_name, severity, kind, namespace, name, reason, message, targets,
			 state, first_seen, last_seen, firing_at, resolved_at, caused_by, pending_since)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.Key, r.Check, r.Severity, r.Object.Kind, r.Object.Namespace, r.Object.Name, r.Reason, r.Message,
			joinTargets(r.Targets), r.State, unix(r.FirstSeen), unix(r.LastSeen), nullableUnix(r.FiringAt), nullableUnix(r.ResolvedAt),
			r.CausedBy, pendingSince(r))
		if err != nil {
			return fmt.Errorf("insert finding %s: %w", r.Key, err)
		}
	}
	for _, r := range changes.Update {
		_, err := tx.ExecContext(ctx, `UPDATE findings
			SET severity = ?, reason = ?, message = ?, targets = ?, state = ?, last_seen = ?, firing_at = ?,
			    resolved_at = ?, firing_notified_at = ?, caused_by = ?, pending_since = ?
			WHERE id = ?`,
			r.Severity, r.Reason, r.Message, joinTargets(r.Targets), r.State, unix(r.LastSeen), nullableUnix(r.FiringAt), nullableUnix(r.ResolvedAt),
			nullableUnix(r.FiringNotifiedAt), r.CausedBy, pendingSince(r), r.ID)
		if err != nil {
			return fmt.Errorf("update finding %s: %w", r.Key, err)
		}
	}
	for _, esc := range changes.Escalated {
		if err := clearFiringDeliveries(ctx, tx, esc.ID, esc.Targets); err != nil {
			return err
		}
	}
	for _, id := range changes.Delete {
		if _, err := tx.ExecContext(ctx, `DELETE FROM findings WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete finding %d: %w", id, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM deliveries WHERE finding_id = ?`, id); err != nil {
			return fmt.Errorf("delete deliveries of finding %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit changes: %w", err)
	}
	return nil
}

// Acknowledge records that somebody knows about this problem. Only a firing
// critical problem that is not acknowledged yet can be acknowledged. The rule
// is part of the statement, so a page that is out of date cannot acknowledge a
// problem that has changed since. It returns the key of the problem and whether
// anything changed.
func (s *Store) Acknowledge(ctx context.Context, id int64, at time.Time) (string, bool, error) {
	var key string
	err := s.db.QueryRowContext(ctx, `UPDATE findings SET acknowledged_at = ?
		WHERE id = ? AND state = 'firing' AND severity = 'critical' AND acknowledged_at IS NULL
		RETURNING key`, unix(at), id).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("acknowledge finding %d: %w", id, err)
	}
	return key, true, nil
}

// Unacknowledge takes an acknowledgement back while the problem is firing. The
// problem then behaves like any other: a reminder that is overdue goes out in
// the next run, and so does a first mail that was held back. It returns the key
// of the problem and whether anything changed.
func (s *Store) Unacknowledge(ctx context.Context, id int64) (string, bool, error) {
	var key string
	err := s.db.QueryRowContext(ctx, `UPDATE findings SET acknowledged_at = NULL
		WHERE id = ? AND state = 'firing' AND acknowledged_at IS NOT NULL
		RETURNING key`, id).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("remove acknowledgement of finding %d: %w", id, err)
	}
	return key, true, nil
}

// DeleteResolvedBefore removes resolved findings that ended before the given
// time (retention), together with what the targets were told about them, and
// returns how many findings were removed.
func (s *Store) DeleteResolvedBefore(ctx context.Context, before time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful commit

	_, err = tx.ExecContext(ctx, `DELETE FROM deliveries WHERE finding_id IN
		(SELECT id FROM findings WHERE state = 'resolved' AND resolved_at < ?)`, unix(before))
	if err != nil {
		return 0, fmt.Errorf("delete old deliveries: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM findings WHERE state = 'resolved' AND resolved_at < ?`, unix(before))
	if err != nil {
		return 0, fmt.Errorf("delete old findings: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted findings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit deleted findings: %w", err)
	}
	return n, nil
}

func (s *Store) query(ctx context.Context, statement string, args ...any) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query findings: %w", err)
	}
	defer func() { _ = rows.Close() }() // a read error is reported by rows.Err below

	var records []Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read findings: %w", err)
	}
	return records, nil
}

// scanRecord reads one row in the order of selectColumns.
func scanRecord(rows *sql.Rows) (Record, error) {
	var r Record
	var firstSeen, lastSeen int64
	var firingAt, resolvedAt, firingNotifiedAt, resolvedNotifiedAt, acknowledgedAt, pendingSinceAt, startHeardAt sql.NullInt64
	var targets string

	err := rows.Scan(&r.ID, &r.Key, &r.Check, &r.Severity, &r.Object.Kind, &r.Object.Namespace, &r.Object.Name,
		&r.Reason, &r.Message, &r.State, &firstSeen, &lastSeen,
		&firingAt, &resolvedAt, &firingNotifiedAt, &resolvedNotifiedAt, &acknowledgedAt, &targets,
		&r.CausedBy, &pendingSinceAt, &startHeardAt)
	if err != nil {
		return Record{}, fmt.Errorf("scan finding: %w", err)
	}
	r.FirstSeen = time.Unix(firstSeen, 0).UTC()
	r.LastSeen = time.Unix(lastSeen, 0).UTC()
	r.FiringAt = fromNullable(firingAt)
	r.ResolvedAt = fromNullable(resolvedAt)
	r.FiringNotifiedAt = fromNullable(firingNotifiedAt)
	r.StartHeardAt = fromNullable(startHeardAt)
	r.ResolvedNotifiedAt = fromNullable(resolvedNotifiedAt)
	r.AcknowledgedAt = fromNullable(acknowledgedAt)
	r.Targets = checks.ParseTargets(targets)
	r.PendingSince = r.FirstSeen
	if pendingSinceAt.Valid {
		r.PendingSince = time.Unix(pendingSinceAt.Int64, 0).UTC()
	}
	return r, nil
}

// pendingSince stores the start of the ALERT_AFTER clock only when it differs
// from first_seen, so most rows keep NULL.
func pendingSince(r Record) sql.NullInt64 {
	if r.PendingSince.IsZero() || r.PendingSince.Equal(r.FirstSeen) {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: r.PendingSince.Unix(), Valid: true}
}

// joinTargets stores the target list as "mail,slack"; empty means every target.
func joinTargets(targets []string) string {
	return strings.Join(targets, ",")
}

func unix(t time.Time) int64 { return t.Unix() }

func nullableUnix(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

func fromNullable(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(n.Int64, 0).UTC()
	return &t
}
