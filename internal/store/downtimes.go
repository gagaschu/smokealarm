package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gagaschu/smokealarm/internal/checks"
)

// Downtime silences the mails of one problem, identified by its key, until
// ExpiresAt. It is independent of the rows in findings: it stays when the
// problem resolves, and it covers the problem if it comes back.
type Downtime struct {
	ID        int64
	Key       string
	Check     string
	Object    checks.ObjectRef
	Reason    string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// downtimeColumns lists the columns in the order queryDowntimes reads them.
const downtimeColumns = `id, key, check_name, kind, namespace, name, reason, created_at, expires_at`

// SetDowntime creates the downtime for a key, or replaces the existing one.
func (s *Store) SetDowntime(ctx context.Context, d Downtime) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO downtimes
		(key, check_name, kind, namespace, name, reason, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO UPDATE SET
			check_name = excluded.check_name, kind = excluded.kind, namespace = excluded.namespace,
			name = excluded.name, reason = excluded.reason,
			created_at = excluded.created_at, expires_at = excluded.expires_at`,
		d.Key, d.Check, d.Object.Kind, d.Object.Namespace, d.Object.Name, d.Reason, unix(d.CreatedAt), unix(d.ExpiresAt))
	if err != nil {
		return fmt.Errorf("set downtime %s: %w", d.Key, err)
	}
	return nil
}

// ActiveDowntimes returns the downtimes that are still running, the one that
// ends first at the top. A downtime has ended at exactly its expiry time.
func (s *Store) ActiveDowntimes(ctx context.Context, now time.Time) ([]Downtime, error) {
	return s.queryDowntimes(ctx, `SELECT `+downtimeColumns+` FROM downtimes
		WHERE expires_at > ? ORDER BY expires_at, key`, unix(now))
}

// ExpiredDowntimes returns the downtimes that have ended but are not finished
// yet (see FinishDowntime).
func (s *Store) ExpiredDowntimes(ctx context.Context, now time.Time) ([]Downtime, error) {
	return s.queryDowntimes(ctx, `SELECT `+downtimeColumns+` FROM downtimes
		WHERE expires_at <= ? ORDER BY expires_at, key`, unix(now))
}

// EndDowntime ends a running downtime now. The engine then finishes it like one
// that ran out, so both take the same path. It returns the key and whether
// anything changed.
func (s *Store) EndDowntime(ctx context.Context, id int64, now time.Time) (string, bool, error) {
	var key string
	err := s.db.QueryRowContext(ctx, `UPDATE downtimes SET expires_at = ?
		WHERE id = ? AND expires_at > ?
		RETURNING key`, unix(now), id, unix(now)).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("end downtime %d: %w", id, err)
	}
	return key, true, nil
}

// FinishDowntime removes a downtime that has ended and clears the "reported"
// marker of the problem if it is still firing, for every notification target,
// so it is reported again as new. That is what brings back an alarm that was
// reported before the downtime; the reminder clock then runs from this new
// report. An acknowledged problem keeps its marker, because it stays silent
// until it is resolved. All changes happen in one transaction, so a restart
// cannot lose the message. A downtime that was
// extended in the meantime is left alone.
func (s *Store) FinishDowntime(ctx context.Context, key string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful commit

	result, err := tx.ExecContext(ctx, `DELETE FROM downtimes WHERE key = ? AND expires_at <= ?`, key, unix(now))
	if err != nil {
		return fmt.Errorf("delete downtime %s: %w", key, err)
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count deleted downtimes: %w", err)
	}
	if deleted == 0 {
		return nil
	}
	var id int64
	err = tx.QueryRowContext(ctx, `UPDATE findings SET firing_notified_at = NULL
		WHERE key = ? AND state = 'firing' AND acknowledged_at IS NULL
		RETURNING id`, key).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Nothing is firing, or it is acknowledged: no marker to clear.
	case err != nil:
		return fmt.Errorf("clear marker of finding %s: %w", key, err)
	default:
		if err := clearFiringDeliveries(ctx, tx, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit finished downtime: %w", err)
	}
	return nil
}

// DowntimeKeys returns the keys of all downtimes in the table. These are the
// problems that send no mail. An entry that has ended but is not finished yet
// counts too, because its mail is still to come.
func (s *Store) DowntimeKeys(ctx context.Context) (map[string]bool, error) {
	downtimes, err := s.queryDowntimes(ctx, `SELECT `+downtimeColumns+` FROM downtimes`)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, d := range downtimes {
		keys[d.Key] = true
	}
	return keys, nil
}

func (s *Store) queryDowntimes(ctx context.Context, statement string, args ...any) ([]Downtime, error) {
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("query downtimes: %w", err)
	}
	defer func() { _ = rows.Close() }() // a read error is reported by rows.Err below

	var downtimes []Downtime
	for rows.Next() {
		var d Downtime
		var createdAt, expiresAt int64
		err := rows.Scan(&d.ID, &d.Key, &d.Check, &d.Object.Kind, &d.Object.Namespace, &d.Object.Name,
			&d.Reason, &createdAt, &expiresAt)
		if err != nil {
			return nil, fmt.Errorf("scan downtime: %w", err)
		}
		d.CreatedAt = time.Unix(createdAt, 0).UTC()
		d.ExpiresAt = time.Unix(expiresAt, 0).UTC()
		downtimes = append(downtimes, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read downtimes: %w", err)
	}
	return downtimes, nil
}
