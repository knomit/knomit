// Per-repo settings: the second tenant of the machine-local control-plane
// database (<home>/control.db). Holds serving configuration keyed by repo ID
// (root commit hash — rename-proof; lenses RFC decision 12). Each tenant of
// control.db opens its own handle to the shared file; WAL + busy timeout +
// a single connection per handle keep concurrent tenants safe.
package repos

import (
	"database/sql"
	"errors"
	"fmt"
)

// Valid profile values. Absent rows read as ProfileCode.
const (
	ProfileCode    = "code"
	ProfileChat    = "chat"
	ProfileGeneric = "generic"
)

// ErrInvalidProfile is returned by SetProfile for unknown profile values.
var ErrInvalidProfile = errors.New("invalid profile (want code, chat, or generic)")

// ErrEmptyRepoID is returned by SetProfile when the repo id is empty.
var ErrEmptyRepoID = errors.New("repo id required")

const repoSettingsSchema = `
CREATE TABLE IF NOT EXISTS repo_settings (
    repo_id TEXT PRIMARY KEY,
    profile TEXT NOT NULL
);
`

// RepoSettings persists per-repo serving configuration in control.db.
type RepoSettings struct {
	db *sql.DB
}

// OpenRepoSettings opens (creating if needed) the repo_settings tenant at
// path — the same control.db file the lens registry uses.
func OpenRepoSettings(path string) (*RepoSettings, error) {
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open repo settings: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(repoSettingsSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("repo settings schema: %w", err)
	}
	return &RepoSettings{db: db}, nil
}

// Close releases the underlying database handle.
func (s *RepoSettings) Close() error {
	return s.db.Close()
}

// Profile returns the stored profile for repoID, defaulting to ProfileCode
// for absent rows and empty IDs (identity unknown reads as the default —
// never an error).
func (s *RepoSettings) Profile(repoID string) (string, error) {
	if repoID == "" {
		return ProfileCode, nil
	}
	var p string
	err := s.db.QueryRow(`SELECT profile FROM repo_settings WHERE repo_id = ?`, repoID).Scan(&p)
	if errors.Is(err, sql.ErrNoRows) {
		return ProfileCode, nil
	}
	if err != nil {
		return "", fmt.Errorf("profile: %w", err)
	}
	return p, nil
}

// SetProfile upserts the profile for repoID.
func (s *RepoSettings) SetProfile(repoID, profile string) error {
	if repoID == "" {
		return ErrEmptyRepoID
	}
	switch profile {
	case ProfileCode, ProfileChat, ProfileGeneric:
	default:
		return fmt.Errorf("%w: %q", ErrInvalidProfile, profile)
	}
	if _, err := s.db.Exec(
		`INSERT INTO repo_settings (repo_id, profile) VALUES (?, ?)
		 ON CONFLICT(repo_id) DO UPDATE SET profile = excluded.profile`,
		repoID, profile,
	); err != nil {
		return fmt.Errorf("set profile: %w", err)
	}
	return nil
}
