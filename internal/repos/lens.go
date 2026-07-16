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
	// ErrRepoInUseByLens blocks Archive/Purge of a lens-referenced repo.
	ErrRepoInUseByLens = errors.New("repo is referenced by a lens; delete the lens first")
)

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
	Name      string
	Write     string
	Reads     []LensRead
	CreatedAt int64
	UpdatedAt int64
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
    name       TEXT PRIMARY KEY,
    write_repo TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
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
// Foreign keys are enabled so deleting a lens cascades to its read rows.
func OpenLensRegistry(path string) (*LensRegistry, error) {
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("open lens registry: %w", err)
	}
	if _, err := db.Exec(lensSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("lens registry schema: %w", err)
	}
	return &LensRegistry{db: db}, nil
}

// Close releases the underlying database handle.
func (r *LensRegistry) Close() error {
	return r.db.Close()
}

// List returns all lenses sorted by name.
func (r *LensRegistry) List() ([]Lens, error) {
	rows, err := r.db.Query(`SELECT name, write_repo, created_at, updated_at FROM lenses ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list lenses: %w", err)
	}
	defer rows.Close()
	var out []Lens
	for rows.Next() {
		var l Lens
		if err := rows.Scan(&l.Name, &l.Write, &l.CreatedAt, &l.UpdatedAt); err != nil {
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
