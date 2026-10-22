// Package sessions owns the ephemeral session database: the separate SQLite
// file holding in-flight tool-session paging state and the review/hypothesize
// work-stealing pipeline, plus the pipeline watermarks that live in the MAIN
// database.
//
// It is a child of the store package and, unlike vcs, importable: it writes
// only SQLite (never a git object or ref), so nothing outside the store needs
// to be kept from holding its types. Callers get a *PipelineIndex from
// Service.Pipeline and a *ToolIndex from Service.ToolSession and import this
// package to name them; the row shapes live in store/model. Open, NewPipelineIndex,
// NewToolIndex and ReapIdle take raw database handles and are exported only
// because package store wires them (TestStoreSessionsPlumbing_OnlyInsideStore
// keeps every other package from calling them). No method here assumes a
// branch lock: each is a self-contained SQL statement or transaction.
package sessions

import (
	"context"
	"database/sql"
	_ "embed"
	"fmt"
	"os"
	"time"

	"knomit/internal/engine/store/model"
)

// schema is the full DDL for the ephemeral session database, applied wholesale
// to the fresh DB at Open (see initSchema). It is NOT run through the
// versioned migration runner — the DB is disposable and recreated empty each
// start, so there is nothing to version.
//
//go:embed session_schema.sql
var schema string

// Open removes any leftover session files (so the DB starts empty) and opens a
// fresh one with the stock sqlite3 driver, then applies the schema. Returns the
// opened DB and the resolved path.
func Open(mainPath string) (*sql.DB, string, error) {
	sessionPath := model.SessionDBPathFor(mainPath)
	// Start empty: a session cursor from a previous run is dead anyway.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(sessionPath + suffix)
	}

	// Stock "sqlite3" driver — NO vec extension, so no need for
	// _txlock=immediate or the warm/pinned pool the main DB requires.
	dsn := sessionPath + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1&_synchronous=OFF"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, "", fmt.Errorf("open: %w", err)
	}
	if err := initSchema(db); err != nil {
		db.Close()
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(sessionPath + suffix)
		}
		return nil, "", err
	}
	return db, sessionPath, nil
}

// initSchema execs the embedded session schema wholesale against the fresh DB.
// No versioning — the DB is recreated empty each start.
func initSchema(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("init session schema: %w", err)
	}
	return nil
}

// NewPipelineIndex returns the pipeline index over the MAIN db (watermarks)
// and the session db (sessions and work items).
func NewPipelineIndex(mainDB, sessionDB *sql.DB) *PipelineIndex {
	return &PipelineIndex{db: mainDB, sessionDB: sessionDB}
}

// NewToolIndex returns the tool-session index over the session db.
func NewToolIndex(sessionDB *sql.DB) *ToolIndex {
	return &ToolIndex{db: sessionDB}
}

// ReapIdle deletes session rows whose last_used_at is older than the
// per-cluster TTL, cascading to their queue/work-item children. Active sessions
// bump last_used_at on every page/work-item access and are never reaped, so this
// is safe under concurrent paging. A non-positive TTL skips that cluster.
// Returns the total number of session rows deleted across both clusters.
func ReapIdle(ctx context.Context, db *sql.DB, toolTTL, pipelineTTL time.Duration) (int, error) {
	var total int
	now := time.Now().UTC()
	reap := func(table string, ttl time.Duration) error {
		if ttl <= 0 {
			return nil
		}
		cutoff := now.Add(-ttl).Format(time.RFC3339)
		res, err := db.ExecContext(ctx,
			"DELETE FROM "+table+" WHERE last_used_at < ?", cutoff)
		if err != nil {
			return fmt.Errorf("reap %s: %w", table, err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
		return nil
	}
	if err := reap("tool_sessions", toolTTL); err != nil {
		return total, err
	}
	if err := reap("pipeline_sessions", pipelineTTL); err != nil {
		return total, err
	}
	return total, nil
}
