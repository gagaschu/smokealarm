// Package store keeps findings in a SQLite database so that state survives
// restarts. It only persists what the engine decides; it contains no
// pending/firing/resolved logic itself.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite driver, registers itself as "sqlite"
)

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Open opens (and creates, if needed) the database file and brings the schema
// up to date. The clock is injected so migration timestamps are testable.
func Open(path string, now func() time.Time) (*Store, error) {
	// busy_timeout: wait instead of failing if the file is locked briefly.
	// WAL: readers (the web UI) do not block the writer (the engine).
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	// One connection avoids "database is locked" between our own goroutines.
	db.SetMaxOpenConns(1)

	if err := migrate(db, now()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate database %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Ping checks that the database answers. It backs /readyz.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping database: %w", err)
	}
	return nil
}

// Close releases the database.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("close database: %w", err)
	}
	return nil
}
