// Explore session CRUD for progressive knowledge-base exploration.
package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ExploreSession represents an explore session for a branch.
type ExploreSession struct {
	ID         string
	Branch     string
	PathPrefix string
	LastCommit string
	Status     string // "active", "completed", "abandoned"
	CreatedAt  string
	UpdatedAt  string
}

// CreateExploreSession creates a new explore session for the given branch and path prefix.
func (idx *Index) CreateExploreSession(branch, pathPrefix string) (*ExploreSession, error) {
	now := time.Now().UTC().Format(time.RFC3339)

	s := &ExploreSession{
		ID:         uuid.New().String(),
		Branch:     branch,
		PathPrefix: pathPrefix,
		LastCommit: "",
		Status:     "active",
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	_, err := idx.db.Exec(
		`INSERT INTO explore_sessions(id, branch, path_prefix, last_commit, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.Branch, s.PathPrefix, s.LastCommit, s.Status, s.CreatedAt, s.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("CreateExploreSession: %w", err)
	}
	return s, nil
}

// GetExploreSession returns the session with the given ID, or nil if not found.
func (idx *Index) GetExploreSession(id string) (*ExploreSession, error) {
	var s ExploreSession
	err := idx.db.QueryRow(
		`SELECT id, branch, path_prefix, last_commit, status, created_at, updated_at FROM explore_sessions WHERE id = ?`, id,
	).Scan(&s.ID, &s.Branch, &s.PathPrefix, &s.LastCommit, &s.Status, &s.CreatedAt, &s.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetExploreSession: %w", err)
	}
	return &s, nil
}

// UpdateExploreSession updates the last_commit, status, and updated_at for a session.
func (idx *Index) UpdateExploreSession(id, lastCommit, status string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := idx.db.Exec(
		`UPDATE explore_sessions SET last_commit = ?, status = ?, updated_at = ? WHERE id = ?`,
		lastCommit, status, now, id,
	)
	if err != nil {
		return fmt.Errorf("UpdateExploreSession: %w", err)
	}
	return nil
}

// GetExploreSeenPaths returns all seen paths for the given session as a set.
func (idx *Index) GetExploreSeenPaths(sessionID string) (map[string]bool, error) {
	rows, err := idx.db.Query(
		`SELECT path FROM explore_seen_paths WHERE session_id = ?`, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("GetExploreSeenPaths: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]bool)
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("GetExploreSeenPaths scan: %w", err)
		}
		seen[p] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetExploreSeenPaths rows: %w", err)
	}
	return seen, nil
}

// AddExploreSeenPaths batch-inserts seen paths for a session, ignoring duplicates.
func (idx *Index) AddExploreSeenPaths(sessionID string, paths []string) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("AddExploreSeenPaths begin: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO explore_seen_paths(session_id, path) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("AddExploreSeenPaths prepare: %w", err)
	}
	defer stmt.Close()

	for _, p := range paths {
		if _, err := stmt.Exec(sessionID, p); err != nil {
			return fmt.Errorf("AddExploreSeenPaths exec: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("AddExploreSeenPaths commit: %w", err)
	}
	return nil
}

// GCExploreSessions deletes all but the most recent `keep` sessions for a branch.
// Seen paths are cascaded via the foreign key constraint.
func (idx *Index) GCExploreSessions(branch string, keep int) error {
	// Enable foreign keys for cascade deletes.
	if _, err := idx.db.Exec(`PRAGMA foreign_keys = ON`); err != nil {
		return fmt.Errorf("GCExploreSessions pragma: %w", err)
	}
	_, err := idx.db.Exec(
		`DELETE FROM explore_sessions
		 WHERE branch = ? AND id NOT IN (
		     SELECT id FROM explore_sessions
		     WHERE branch = ?
		     ORDER BY rowid DESC
		     LIMIT ?
		 )`,
		branch, branch, keep,
	)
	if err != nil {
		return fmt.Errorf("GCExploreSessions: %w", err)
	}
	return nil
}
