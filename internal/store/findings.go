package store

import (
	"context"
	"database/sql"
	"fmt"
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
// is sent twice and failed notifications are retried.
type Record struct {
	ID int64
	checks.Finding
	State              State
	FirstSeen          time.Time
	LastSeen           time.Time
	FiringAt           *time.Time
	ResolvedAt         *time.Time
	FiringNotifiedAt   *time.Time
	ResolvedNotifiedAt *time.Time
}

// Changes is a set of modifications that is written in one transaction.
type Changes struct {
	Insert []Record
	Update []Record // matched by ID; see Apply for the fields that are written
	Delete []int64
}

// selectColumns lists the columns in the order scanRecord reads them.
const selectColumns = `id, key, check_name, severity, kind, namespace, name, reason, message,
	state, first_seen, last_seen, firing_at, resolved_at, firing_notified_at, resolved_notified_at`

// OpenRecords returns all problems that are pending or firing.
func (s *Store) OpenRecords(ctx context.Context) ([]Record, error) {
	return s.query(ctx, `SELECT `+selectColumns+` FROM findings
		WHERE state IN ('pending', 'firing') ORDER BY id`)
}

// Unnotified returns what still has to be reported: firing problems whose
// start was not reported yet, and resolved problems whose end was not
// reported yet.
func (s *Store) Unnotified(ctx context.Context) ([]Record, error) {
	return s.query(ctx, `SELECT `+selectColumns+` FROM findings
		WHERE (state = 'firing' AND firing_notified_at IS NULL)
		   OR (state = 'resolved' AND resolved_notified_at IS NULL)
		ORDER BY id`)
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
// change during a life cycle (severity, message, state, last_seen, firing_at,
// resolved_at); the notified markers have their own methods.
func (s *Store) Apply(ctx context.Context, changes Changes) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful commit

	for _, r := range changes.Insert {
		_, err := tx.ExecContext(ctx, `INSERT INTO findings
			(key, check_name, severity, kind, namespace, name, reason, message,
			 state, first_seen, last_seen, firing_at, resolved_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.Key, r.Check, r.Severity, r.Object.Kind, r.Object.Namespace, r.Object.Name, r.Reason, r.Message,
			r.State, unix(r.FirstSeen), unix(r.LastSeen), nullableUnix(r.FiringAt), nullableUnix(r.ResolvedAt))
		if err != nil {
			return fmt.Errorf("insert finding %s: %w", r.Key, err)
		}
	}
	for _, r := range changes.Update {
		_, err := tx.ExecContext(ctx, `UPDATE findings
			SET severity = ?, message = ?, state = ?, last_seen = ?, firing_at = ?, resolved_at = ?
			WHERE id = ?`,
			r.Severity, r.Message, r.State, unix(r.LastSeen), nullableUnix(r.FiringAt), nullableUnix(r.ResolvedAt), r.ID)
		if err != nil {
			return fmt.Errorf("update finding %s: %w", r.Key, err)
		}
	}
	for _, id := range changes.Delete {
		if _, err := tx.ExecContext(ctx, `DELETE FROM findings WHERE id = ?`, id); err != nil {
			return fmt.Errorf("delete finding %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit changes: %w", err)
	}
	return nil
}

// MarkFiringNotified remembers that the start of these problems was reported.
func (s *Store) MarkFiringNotified(ctx context.Context, ids []int64, at time.Time) error {
	return s.markNotified(ctx, `UPDATE findings SET firing_notified_at = ? WHERE id = ?`, ids, at)
}

// MarkResolvedNotified remembers that the end of these problems was reported.
func (s *Store) MarkResolvedNotified(ctx context.Context, ids []int64, at time.Time) error {
	return s.markNotified(ctx, `UPDATE findings SET resolved_notified_at = ? WHERE id = ?`, ids, at)
}

func (s *Store) markNotified(ctx context.Context, statement string, ids []int64, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful commit

	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, statement, unix(at), id); err != nil {
			return fmt.Errorf("mark finding %d as notified: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit notified markers: %w", err)
	}
	return nil
}

// DeleteResolvedBefore removes resolved findings that ended before the given
// time (retention) and returns how many were removed.
func (s *Store) DeleteResolvedBefore(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM findings WHERE state = 'resolved' AND resolved_at < ?`, unix(before))
	if err != nil {
		return 0, fmt.Errorf("delete old findings: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted findings: %w", err)
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
	var firingAt, resolvedAt, firingNotifiedAt, resolvedNotifiedAt sql.NullInt64

	err := rows.Scan(&r.ID, &r.Key, &r.Check, &r.Severity, &r.Object.Kind, &r.Object.Namespace, &r.Object.Name,
		&r.Reason, &r.Message, &r.State, &firstSeen, &lastSeen,
		&firingAt, &resolvedAt, &firingNotifiedAt, &resolvedNotifiedAt)
	if err != nil {
		return Record{}, fmt.Errorf("scan finding: %w", err)
	}
	r.FirstSeen = time.Unix(firstSeen, 0).UTC()
	r.LastSeen = time.Unix(lastSeen, 0).UTC()
	r.FiringAt = fromNullable(firingAt)
	r.ResolvedAt = fromNullable(resolvedAt)
	r.FiringNotifiedAt = fromNullable(firingNotifiedAt)
	r.ResolvedNotifiedAt = fromNullable(resolvedNotifiedAt)
	return r, nil
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
