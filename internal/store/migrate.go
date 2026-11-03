package store

import (
	"database/sql"
	"embed"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// migrationFiles holds the .sql files. go:embed needs a package-level
// variable; it is never modified.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrate applies every migration that has not been applied yet, in order.
// Files are named NNN_description.sql; NNN is the version. Each migration
// runs in its own transaction, so a failure leaves no half-applied schema.
func migrate(db *sql.DB, now time.Time) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`)
	if err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := migrationFiles.ReadDir("migrations") // sorted by file name
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	for _, entry := range entries {
		version, err := migrationVersion(entry.Name())
		if err != nil {
			return err
		}
		if err := applyMigration(db, entry.Name(), version, now); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(db *sql.DB, file string, version int, now time.Time) error {
	var applied int
	err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&applied)
	if err != nil {
		return fmt.Errorf("check migration %s: %w", file, err)
	}
	if applied > 0 {
		return nil
	}

	script, err := migrationFiles.ReadFile("migrations/" + file)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", file, err)
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", file, err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful commit

	if _, err := tx.Exec(string(script)); err != nil {
		return fmt.Errorf("run migration %s: %w", file, err)
	}
	if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, now.Unix()); err != nil {
		return fmt.Errorf("record migration %s: %w", file, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", file, err)
	}
	return nil
}

// migrationVersion extracts NNN from "NNN_description.sql".
func migrationVersion(file string) (int, error) {
	prefix, _, found := strings.Cut(file, "_")
	if !found {
		return 0, fmt.Errorf("migration %q: file name must look like 001_description.sql", file)
	}
	version, err := strconv.Atoi(prefix)
	if err != nil {
		return 0, fmt.Errorf("migration %q: version prefix is not a number: %w", file, err)
	}
	return version, nil
}
