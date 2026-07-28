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

const repoColumns = `name, origin_url, origin_branch, state, archive_id, created_at, archived_at`

// List returns registered repos in the given state, ordered by name. An empty
// state returns every row.
func (r *RepoRegistry) List(state RepoState) ([]RepoRecord, error) {
	query := `SELECT ` + repoColumns + ` FROM repos`
	args := []any{}
	if state != "" {
		query += ` WHERE state = ?`
		args = append(args, string(state))
	}
	query += ` ORDER BY name`
	return r.query(query, args...)
}

// ActiveRecord returns the active row for name. A name has at most one, since
// active rows all carry an empty archive_id.
func (r *RepoRegistry) ActiveRecord(name string) (RepoRecord, bool, error) {
	return r.one(`WHERE name = ? AND archive_id = ''`, name)
}

// ArchiveRecord returns the archived row with this archive id.
func (r *RepoRegistry) ArchiveRecord(archiveID string) (RepoRecord, bool, error) {
	if archiveID == "" {
		return RepoRecord{}, false, nil
	}
	return r.one(`WHERE archive_id = ?`, archiveID)
}

// one runs a single-row lookup with the given WHERE clause.
func (r *RepoRegistry) one(where string, args ...any) (RepoRecord, bool, error) {
	recs, err := r.query(`SELECT `+repoColumns+` FROM repos `+where+` LIMIT 1`, args...)
	if err != nil || len(recs) == 0 {
		return RepoRecord{}, false, err
	}
	return recs[0], true, nil
}

// query runs a SELECT of repoColumns and decodes the rows.
func (r *RepoRegistry) query(query string, args ...any) ([]RepoRecord, error) {
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

// Upsert inserts or replaces a registry row, keyed by (name, archive_id).
//
// Active repos carry an empty ArchiveID, so a name has at most one active row
// and upserting an active repo by name updates it in place. Archived repos are
// keyed by their archive id as well, because a name is unique only among ACTIVE
// repos: archiving "work" and creating a new "work" is supported, and keying on
// name alone would make the archived repo unrecoverable the moment its name was
// reused.
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
		ON CONFLICT(name, archive_id) DO UPDATE SET
			origin_url    = excluded.origin_url,
			origin_branch = excluded.origin_branch,
			state         = excluded.state,
			created_at    = excluded.created_at,
			archived_at   = excluded.archived_at
	`, rec.Name, rec.OriginURL, rec.OriginBranch, string(rec.State), rec.ArchiveID, created, archived)
	if err != nil {
		return fmt.Errorf("upsert repo %q: %w", rec.Name, err)
	}
	return nil
}

// There is deliberately no SetState(name, state) and no Delete(name).
//
// Both read as if a name identified one repo, which stopped being true when the
// key widened to (name, archive_id): a name can carry one active row AND any
// number of archived rows. SetState("work", archived) would flip every one of
// them at once and leave the active row with an empty archive_id — an archived
// row no Restore or Purge could ever address. Delete("work") would destroy a
// live repo's registration along with the archives. Archiving is not a state
// flip on one row; it is "insert the archived row, retire the active one",
// which is what DeleteActive and DeleteArchive below express.

// DeleteActive removes the ACTIVE row for name, leaving any archived rows that
// share the name untouched. This is how Archive retires a repo's live
// registration: without it the stale active row outlives the archive, and the
// next Start reads it as a repo whose database has gone missing — re-cloning it
// if it has an origin, or refusing to boot under StrictMissing if it does not.
func (r *RepoRegistry) DeleteActive(name string) error {
	if _, err := r.db.Exec(`DELETE FROM repos WHERE name = ? AND archive_id = ''`, name); err != nil {
		return fmt.Errorf("delete active repo %q: %w", name, err)
	}
	return nil
}

// DeleteArchive removes the single archived row identified by archiveID. This
// is the delete Purge and Restore need: deleting by name would also take out an
// unrelated ACTIVE repo that has since claimed the archived repo's name.
func (r *RepoRegistry) DeleteArchive(archiveID string) error {
	if archiveID == "" {
		return fmt.Errorf("delete archive: id required")
	}
	if _, err := r.db.Exec(`DELETE FROM repos WHERE archive_id = ?`, archiveID); err != nil {
		return fmt.Errorf("delete archive %q: %w", archiveID, err)
	}
	return nil
}
