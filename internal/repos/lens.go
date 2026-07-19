// Lens registry: the first tenant of the machine-local control-plane database
// (<home>/control.db). A lens binds one write repo and N read repos into a
// single named knowledge base (lenses RFC, discussion #8). The registry is
// authoritative operational config — deliberately NOT git-backed — and stores
// per-machine repo NAMES, never repo IDs (renames update it via Manager).
//
// The registry never calls time.Now(): callers stamp CreatedAt/UpdatedAt.
// It has no dependency on Manager; Manager owns its lifecycle.
package repos

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	// Registers the stock "sqlite3" driver. The registry deliberately does
	// not use the custom "sqlite3_knomit" driver — no vec/GraphQLite needed.
	_ "github.com/mattn/go-sqlite3"
)

var (
	// ErrLensExists is returned by Create when the lens name is taken.
	ErrLensExists = errors.New("lens already exists")
	// ErrLensNameEmpty is returned by Create when the lens name is empty.
	ErrLensNameEmpty = errors.New("lens name required")
	// ErrLensWriteEmpty is returned by Create when the write repo is empty.
	ErrLensWriteEmpty = errors.New("lens write repo required")
	// ErrLensDescriptionTooLong is returned when a lens description exceeds
	// MaxLensDescriptionBytes. Description is display-only metadata, so the cap
	// is a pure input check independent of the live repo set.
	ErrLensDescriptionTooLong = errors.New("lens description too long")
	// ErrRepoInUseByLens blocks Archive/Purge of a lens-referenced repo.
	ErrRepoInUseByLens = errors.New("repo is referenced by a lens; delete the lens first")
)

// MaxLensDescriptionBytes caps a lens description (free markdown text, rendered
// client-side). Byte length, not rune count: it bounds stored + wire size.
const MaxLensDescriptionBytes = 4096

// LensRead is one read mount of a lens.
type LensRead struct {
	Repo   string // repo name (per-machine alias)
	Branch string // "" = that repo's agent branch, resolved at bind time
	Source string // optional src:// slug for verify metadata
}

// Lens is a named binding of one write repo + N read repos. Writes always
// target the write repo's own agent branch (RFC decision 19), so there is no
// write-branch field. Reads always include the write repo after normalize.
type Lens struct {
	Name        string
	Write       string
	Description string // free markdown text, display-only; ignored by normalize
	Reads       []LensRead
	CreatedAt   int64
	UpdatedAt   int64
}

// normalize returns a copy whose Reads are deduped by Repo (first occurrence
// wins, so an explicit read entry for the write repo keeps its configured
// branch), always include the write repo, and are sorted by Repo.
func (l Lens) normalize() Lens {
	seen := make(map[string]struct{}, len(l.Reads)+1)
	reads := make([]LensRead, 0, len(l.Reads)+1)
	for _, r := range l.Reads {
		if _, dup := seen[r.Repo]; dup || r.Repo == "" {
			continue
		}
		seen[r.Repo] = struct{}{}
		reads = append(reads, r)
	}
	if _, ok := seen[l.Write]; !ok {
		reads = append(reads, LensRead{Repo: l.Write})
	}
	sort.Slice(reads, func(i, j int) bool { return reads[i].Repo < reads[j].Repo })
	l.Reads = reads
	return l
}

const lensSchema = `
CREATE TABLE IF NOT EXISTS lenses (
    name        TEXT PRIMARY KEY,
    write_repo  TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS lens_reads (
    lens_name TEXT NOT NULL REFERENCES lenses(name) ON DELETE CASCADE,
    repo      TEXT NOT NULL,
    branch    TEXT NOT NULL DEFAULT '',
    source    TEXT,
    PRIMARY KEY (lens_name, repo)
);
`

// LensRegistry persists lens definitions in the control-plane database.
type LensRegistry struct {
	db *sql.DB
}

// OpenLensRegistry opens (creating if needed) the lens tables at path.
// Foreign keys are enabled so deleting a lens cascades to its read rows; WAL
// mode plus a busy timeout and a single connection fully serialize concurrent
// access to this control-plane config DB, avoiding "database is locked" errors.
func OpenLensRegistry(path string) (*LensRegistry, error) {
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open lens registry: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(lensSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("lens registry schema: %w", err)
	}
	// Upgrade control.dbs created before the description column existed. The
	// column is already in lensSchema, so on a fresh DB this ALTER fails with
	// "duplicate column name" — the ONE error we ignore. Any other ALTER failure
	// (locked DB, corruption) fails the open rather than silently continuing.
	if _, err := db.Exec(`ALTER TABLE lenses ADD COLUMN description TEXT NOT NULL DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("lens registry migrate description: %w", err)
		}
	}
	return &LensRegistry{db: db}, nil
}

// Close releases the underlying database handle.
func (r *LensRegistry) Close() error {
	return r.db.Close()
}

// List returns all lenses sorted by name.
func (r *LensRegistry) List() ([]Lens, error) {
	rows, err := r.db.Query(`SELECT name, write_repo, description, created_at, updated_at FROM lenses ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list lenses: %w", err)
	}
	defer rows.Close()
	var out []Lens
	for rows.Next() {
		var l Lens
		if err := rows.Scan(&l.Name, &l.Write, &l.Description, &l.CreatedAt, &l.UpdatedAt); err != nil {
			return nil, fmt.Errorf("list lenses: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list lenses: %w", err)
	}
	for i := range out {
		reads, err := r.readsOf(out[i].Name)
		if err != nil {
			return nil, err
		}
		out[i].Reads = reads
	}
	return out, nil
}

// readsOf loads the read mounts for one lens, sorted by repo.
func (r *LensRegistry) readsOf(name string) ([]LensRead, error) {
	rows, err := r.db.Query(`SELECT repo, branch, COALESCE(source, '') FROM lens_reads WHERE lens_name = ? ORDER BY repo`, name)
	if err != nil {
		return nil, fmt.Errorf("lens reads: %w", err)
	}
	defer rows.Close()
	var reads []LensRead
	for rows.Next() {
		var lr LensRead
		if err := rows.Scan(&lr.Repo, &lr.Branch, &lr.Source); err != nil {
			return nil, fmt.Errorf("lens reads: %w", err)
		}
		reads = append(reads, lr)
	}
	return reads, rows.Err()
}

// Create stores a lens and returns the normalized form that was persisted.
// The caller stamps CreatedAt/UpdatedAt (the registry never reads the clock).
func (r *LensRegistry) Create(l Lens) (Lens, error) {
	if l.Name == "" {
		return Lens{}, ErrLensNameEmpty
	}
	if l.Write == "" {
		return Lens{}, ErrLensWriteEmpty
	}
	l = l.normalize()

	tx, err := r.db.Begin()
	if err != nil {
		return Lens{}, fmt.Errorf("create lens: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO lenses (name, write_repo, description, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		l.Name, l.Write, l.Description, l.CreatedAt, l.UpdatedAt,
	); err != nil {
		if isUniqueViolation(err) {
			return Lens{}, fmt.Errorf("%w: %q", ErrLensExists, l.Name)
		}
		return Lens{}, fmt.Errorf("create lens: %w", err)
	}
	for _, lr := range l.Reads {
		var source any
		if lr.Source != "" {
			source = lr.Source
		}
		if _, err := tx.Exec(
			`INSERT INTO lens_reads (lens_name, repo, branch, source) VALUES (?, ?, ?, ?)`,
			l.Name, lr.Repo, lr.Branch, source,
		); err != nil {
			return Lens{}, fmt.Errorf("create lens reads: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Lens{}, fmt.Errorf("create lens: %w", err)
	}
	return l, nil
}

// Get returns the lens by name; ok is false when it does not exist.
func (r *LensRegistry) Get(name string) (Lens, bool, error) {
	var l Lens
	err := r.db.QueryRow(
		`SELECT name, write_repo, description, created_at, updated_at FROM lenses WHERE name = ?`, name,
	).Scan(&l.Name, &l.Write, &l.Description, &l.CreatedAt, &l.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Lens{}, false, nil
	}
	if err != nil {
		return Lens{}, false, fmt.Errorf("get lens: %w", err)
	}
	reads, err := r.readsOf(name)
	if err != nil {
		return Lens{}, false, fmt.Errorf("get lens: %w", err)
	}
	l.Reads = reads
	return l, true, nil
}

// Delete removes a lens; deleting an absent lens is not an error. The
// lens_reads rows cascade via the foreign key.
func (r *LensRegistry) Delete(name string) error {
	if _, err := r.db.Exec(`DELETE FROM lenses WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete lens: %w", err)
	}
	return nil
}

// RefsRepo returns the names of all lenses referencing repo as their write
// repo or as a read mount, deduped and sorted. The UNION over both tables is
// belt-and-suspenders: normalization already mirrors the write repo into
// lens_reads, but this stays correct if a future path ever stores a write
// without mirroring.
func (r *LensRegistry) RefsRepo(repo string) ([]string, error) {
	rows, err := r.db.Query(
		`SELECT name FROM lenses WHERE write_repo = ?
		 UNION
		 SELECT lens_name FROM lens_reads WHERE repo = ?
		 ORDER BY 1`, repo, repo)
	if err != nil {
		return nil, fmt.Errorf("refs repo: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("refs repo: %w", err)
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// isUniqueViolation reports whether err is a SQLite UNIQUE constraint failure.
// String matching is the accepted detection method for the stock driver.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
