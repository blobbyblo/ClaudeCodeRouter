// Package db provides SQLite-backed persistence for ClaudeCodeRouter.
// It uses modernc.org/sqlite, a pure-Go SQLite driver that requires no CGO.
package db

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // register "sqlite" driver
)

const schema = `
CREATE TABLE IF NOT EXISTS client_keys (
    id         TEXT PRIMARY KEY,   -- sk-ccr-<24 hex chars>
    name       TEXT NOT NULL,
    created_at INTEGER NOT NULL,   -- unix seconds
    last_used  INTEGER,
    revoked    INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS usage (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    ts              INTEGER NOT NULL,
    client_key      TEXT NOT NULL,
    requested_model TEXT NOT NULL,
    routed_model    TEXT NOT NULL,
    provider        TEXT NOT NULL,
    input_tokens    INTEGER DEFAULT 0,
    output_tokens   INTEGER DEFAULT 0,
    latency_ms      INTEGER DEFAULT 0,
    status          INTEGER NOT NULL,
    fallback_count  INTEGER DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_usage_ts  ON usage(ts DESC);
CREATE INDEX IF NOT EXISTS idx_usage_key ON usage(client_key);

CREATE TABLE IF NOT EXISTS provider_keys (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    provider_id TEXT NOT NULL,
    key_value   TEXT NOT NULL,
    created_at  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_provider_keys_pid ON provider_keys(provider_id);
`

// Open opens (or creates) the SQLite database at the given path and runs
// all schema migrations.  Pass ":memory:" for an ephemeral in-process DB.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("db.Open: open %q: %w", path, err)
	}

	// Enforce WAL mode for better concurrent read performance.
	if _, err = db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("db.Open: set WAL mode: %w", err)
	}

	// Enable foreign-key enforcement.
	if _, err = db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("db.Open: enable foreign keys: %w", err)
	}

	if err = migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

// migrate applies the embedded schema DDL.  Because every statement uses
// CREATE TABLE/INDEX IF NOT EXISTS this is idempotent and safe to run on an
// already-initialised database.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("db.migrate: %w", err)
	}
	return nil
}
