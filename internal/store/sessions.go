package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Session is a logged-in browser. IDHash is the SHA-256 hash of the secret
// session id; the id itself only exists in the cookie.
type Session struct {
	IDHash    string
	Username  string
	CreatedAt time.Time
	ExpiresAt time.Time
	// CredentialsHash says which credentials the session was started with.
	CredentialsHash string
}

// CreateSession stores a new session.
func (s *Store) CreateSession(ctx context.Context, session Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id_hash, username, created_at, expires_at, credentials_hash) VALUES (?, ?, ?, ?, ?)`,
		session.IDHash, session.Username, unix(session.CreatedAt), unix(session.ExpiresAt), session.CredentialsHash)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// Session looks up a session that has not expired at the given time. The
// bool is false if there is none; that is not an error.
func (s *Store) Session(ctx context.Context, idHash string, now time.Time) (Session, bool, error) {
	var session Session
	var createdAt, expiresAt int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id_hash, username, created_at, expires_at, credentials_hash FROM sessions WHERE id_hash = ? AND expires_at > ?`,
		idHash, unix(now)).Scan(&session.IDHash, &session.Username, &createdAt, &expiresAt, &session.CredentialsHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, false, nil
	}
	if err != nil {
		return Session{}, false, fmt.Errorf("load session: %w", err)
	}
	session.CreatedAt = time.Unix(createdAt, 0).UTC()
	session.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	return session, true, nil
}

// DeleteSession removes a session (logout). A missing session is fine.
func (s *Store) DeleteSession(ctx context.Context, idHash string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, idHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteExpiredSessions removes sessions that ended before the given time and
// returns how many were removed.
func (s *Store) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, unix(now))
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count expired sessions: %w", err)
	}
	return n, nil
}
