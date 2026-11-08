package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// A notification target can be switched off ("muted") in the web UI. The table
// target_mutes remembers that across restarts. Switching a target on again
// takes two steps: the web UI only asks (RequestUnmute), and the engine
// finishes it (FinishUnmute) in one transaction, because only the engine knows
// what the target missed while it was off.

// TargetMute is a notification target that is switched off.
type TargetMute struct {
	Target            string // Notifier.Name
	MutedAt           time.Time
	UnmuteRequestedAt *time.Time // nil while it stays off; set when the web UI asked to switch it on
}

// MuteTarget switches a target off. If the web UI asked to switch it on and the
// engine has not done so yet, this cancels the request. It reports whether
// anything changed.
func (s *Store) MuteTarget(ctx context.Context, target string, at time.Time) (changed bool, err error) {
	// The WHERE clause keeps a target that is already off without a request
	// untouched, so changed is false for it.
	result, err := s.db.ExecContext(ctx, `INSERT INTO target_mutes (target, muted_at) VALUES (?, ?)
		ON CONFLICT (target) DO UPDATE SET unmute_requested_at = NULL
			WHERE target_mutes.unmute_requested_at IS NOT NULL`, target, unix(at))
	if err != nil {
		return false, fmt.Errorf("mute target %s: %w", target, err)
	}
	return rowsChanged(result, "mute target "+target)
}

// RequestUnmute records that the web UI wants the target switched on. It
// reports whether anything changed: it does not for a target that is not off or
// that was asked for already.
func (s *Store) RequestUnmute(ctx context.Context, target string, at time.Time) (changed bool, err error) {
	result, err := s.db.ExecContext(ctx, `UPDATE target_mutes SET unmute_requested_at = ?
		WHERE target = ? AND unmute_requested_at IS NULL`, unix(at), target)
	if err != nil {
		return false, fmt.Errorf("request unmute of target %s: %w", target, err)
	}
	return rowsChanged(result, "request unmute of target "+target)
}

// TargetMutes returns the targets that are switched off, keyed by target.
func (s *Store) TargetMutes(ctx context.Context) (map[string]TargetMute, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT target, muted_at, unmute_requested_at FROM target_mutes`)
	if err != nil {
		return nil, fmt.Errorf("query target mutes: %w", err)
	}
	defer func() { _ = rows.Close() }() // a read error is reported by rows.Err below

	mutes := map[string]TargetMute{}
	for rows.Next() {
		var m TargetMute
		var mutedAt int64
		var requestedAt sql.NullInt64
		if err := rows.Scan(&m.Target, &mutedAt, &requestedAt); err != nil {
			return nil, fmt.Errorf("scan target mute: %w", err)
		}
		m.MutedAt = time.Unix(mutedAt, 0).UTC()
		m.UnmuteRequestedAt = fromNullable(requestedAt)
		mutes[m.Target] = m
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read target mutes: %w", err)
	}
	return mutes, nil
}

// FinishUnmute switches a target on after the web UI asked for it. The target
// is then told again about the problems that fire now (firingIDs, chosen by the
// engine), so it does not stay silent during an alarm it missed. It keeps
// "heard the start" (start_heard_at), so their end is reported. Problems that
// are gone at the moment but still wait for RESOLVE_AFTER (goneIDs) count as
// ended while it was off, as at the end of a downtime: the target forgets their
// start too, so their end stays silent, and if they come back they are new.
// Problems that ended while it was off are marked as told without a message,
// because it never heard about them. Everything happens in one transaction.
//
// It returns false and changes nothing if there is no request: the target was
// switched off again in the meantime.
func (s *Store) FinishUnmute(ctx context.Context, target string, firingIDs, goneIDs []int64, at time.Time) (finished bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful commit

	result, err := tx.ExecContext(ctx, `DELETE FROM target_mutes
		WHERE target = ? AND unmute_requested_at IS NOT NULL`, target)
	if err != nil {
		return false, fmt.Errorf("delete mute of target %s: %w", target, err)
	}
	deleted, err := rowsChanged(result, "delete mute of target "+target)
	if err != nil || !deleted {
		return false, err
	}
	targets := []string{target}
	for _, id := range append(slices.Clone(firingIDs), goneIDs...) {
		if err := clearFiringDeliveries(ctx, tx, id, targets); err != nil {
			return false, err
		}
	}
	for _, id := range goneIDs {
		if err := forgetStartHeard(ctx, tx, id, targets); err != nil {
			return false, err
		}
	}
	if err := silenceResolved(ctx, tx, target, at); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit unmute of target %s: %w", target, err)
	}
	return true, nil
}

// silenceResolved marks the resolved problems that were not reported to the
// target as reported, and in the summary of findings as well. The summary comes
// first, because the condition looks at the marker of the target.
func silenceResolved(ctx context.Context, tx *sql.Tx, target string, at time.Time) error {
	const notReported = `f.state = 'resolved' AND NOT EXISTS (SELECT 1 FROM deliveries d
		WHERE d.finding_id = f.id AND d.target = ?1 AND d.resolved_notified_at IS NOT NULL)`
	_, err := tx.ExecContext(ctx, `UPDATE findings AS f SET resolved_notified_at = COALESCE(f.resolved_notified_at, ?2)
		WHERE `+notReported, target, unix(at))
	if err != nil {
		return fmt.Errorf("mark resolved problems as reported: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO deliveries (finding_id, target, resolved_notified_at)
		SELECT f.id, ?1, ?2 FROM findings f WHERE `+notReported+`
		ON CONFLICT (finding_id, target) DO UPDATE SET resolved_notified_at = excluded.resolved_notified_at`,
		target, unix(at))
	if err != nil {
		return fmt.Errorf("silence resolved problems for %s: %w", target, err)
	}
	return nil
}

func rowsChanged(result sql.Result, what string) (bool, error) {
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count rows of %s: %w", what, err)
	}
	return n > 0, nil
}
