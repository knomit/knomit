// Repo registry: the third tenant of the machine-local control-plane database
// (<home>/control.db). Before this table the registry WAS the filesystem —
// Manager.Start globbed repos/*.db, which returns nothing on an empty disk,
// so a server restored from backup could not know which repos should exist.
// This table is the answer: it stores per-machine repo NAMES plus enough
// provenance (origin URL/branch) to re-clone, and archive_id in place of the
// repos/archive/<id>.json file litestream (SQLite-only) can never carry.
package repos

import (
	"database/sql"
	"fmt"
	"time"

	storemigrate "knomit/internal/store/migrate"
)

// RepoState is the lifecycle state of a registered repo.
type RepoState string

const (
	RepoActive   RepoState = "active"
	RepoArchived RepoState = "archived"
)

// RepoRecord is one row of the authoritative repo registry.
//
// This is what answers "what repos should exist, and where did each come from?"
// on a machine whose disk is empty — the question the filesystem cannot answer.
type RepoRecord struct {
	Name         string
	OriginURL    string
	OriginBranch string
	State        RepoState
	ArchiveID    string
	CreatedAt    time.Time
	ArchivedAt   time.Time
}

// RepoRegistry persists the repo registry in control.db.
type RepoRegistry struct {
	db *sql.DB
}

// OpenRepoRegistry opens the repos tenant of control.db at path — the same file
// the lens registry and repo settings use.
func OpenRepoRegistry(path string) (*RepoRegistry, error) {
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open repo registry: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := storemigrate.Control(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("repo registry schema: %w", err)
	}
	return &RepoRegistry{db: db}, nil
}

// Close releases the underlying database handle.
func (r *RepoRegistry) Close() error { return r.db.Close() }

// List returns registered repos in the given state, ordered by name. An empty
// state returns every row.
func (r *RepoRegistry) List(state RepoState) ([]RepoRecord, error) {
	query := `SELECT name, origin_url, origin_branch, state, archive_id, created_at, archived_at
	          FROM repos`
	args := []any{}
	if state != "" {
		query += ` WHERE state = ?`
		args = append(args, string(state))
	}
	query += ` ORDER BY name`

	rows, err := r.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}
	defer rows.Close()

	out := []RepoRecord{}
	for rows.Next() {
		var rec RepoRecord
		var st string
		var created, archived int64
		if err := rows.Scan(&rec.Name, &rec.OriginURL, &rec.OriginBranch, &st, &rec.ArchiveID, &created, &archived); err != nil {
			return nil, fmt.Errorf("scan repo row: %w", err)
		}
		rec.State = RepoState(st)
		if created != 0 {
			rec.CreatedAt = time.Unix(created, 0).UTC()
		}
		if archived != 0 {
			rec.ArchivedAt = time.Unix(archived, 0).UTC()
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Upsert inserts or replaces a registry row, keyed by name.
func (r *RepoRegistry) Upsert(rec RepoRecord) error {
	if rec.Name == "" {
		return fmt.Errorf("upsert repo: name required")
	}
	if rec.State == "" {
		rec.State = RepoActive
	}
	var created, archived int64
	if !rec.CreatedAt.IsZero() {
		created = rec.CreatedAt.Unix()
	}
	if !rec.ArchivedAt.IsZero() {
		archived = rec.ArchivedAt.Unix()
	}
	_, err := r.db.Exec(`
		INSERT INTO repos (name, origin_url, origin_branch, state, archive_id, created_at, archived_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			origin_url    = excluded.origin_url,
			origin_branch = excluded.origin_branch,
			state         = excluded.state,
			archive_id    = excluded.archive_id,
			created_at    = excluded.created_at,
			archived_at   = excluded.archived_at
	`, rec.Name, rec.OriginURL, rec.OriginBranch, string(rec.State), rec.ArchiveID, created, archived)
	if err != nil {
		return fmt.Errorf("upsert repo %q: %w", rec.Name, err)
	}
	return nil
}

// SetState moves a repo between active and archived.
func (r *RepoRegistry) SetState(name string, s RepoState) error {
	res, err := r.db.Exec(`UPDATE repos SET state = ? WHERE name = ?`, string(s), name)
	if err != nil {
		return fmt.Errorf("set state for %q: %w", name, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("set state for %q: %w", name, ErrRepoNotFound)
	}
	return nil
}

// Delete removes a repo from the registry entirely (purge).
func (r *RepoRegistry) Delete(name string) error {
	if _, err := r.db.Exec(`DELETE FROM repos WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete repo %q: %w", name, err)
	}
	return nil
}
