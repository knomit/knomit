// Repo registry: the machine-local record of which repositories exist, what
// state each is in, and which knowledge base each one holds. A tenant of
// <home>/control.db alongside the lens registry — same file, own handle, same
// WAL + busy-timeout + single-connection discipline.
//
// The registry is authoritative. Manager.Start reads it rather than globbing
// the repos directory, so a repo's existence no longer depends on a filename
// and a repo that fails to open stays visible instead of vanishing.
//
// The registry never calls time.Now(): callers stamp CreatedAt/ArchivedAt.
package repos

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

// RepoState is the STORED lifecycle state of a registered repo. Deliberately
// only two values: `missing`, `unopenable` and `conflict` are OBSERVED at open
// time and never written, so they cannot drift out of sync with reality.
type RepoState string

const (
	StateActive   RepoState = "active"
	StateArchived RepoState = "archived"
)

var (
	// ErrRepoAlreadyRegistered rejects a second local copy of one knowledge
	// base. Two such repos would both write agent/<host> and clobber each
	// other on push to the shared origin, so one local copy per root commit is
	// enforced structurally rather than by convention.
	ErrRepoAlreadyRegistered = errors.New("this knowledge base is already registered")
	// ErrRegistryNotFound is returned when a uid has no row.
	ErrRegistryNotFound = errors.New("repo not in registry")
)

// Valid profile values. Absent rows read as ProfileCode.
const (
	ProfileCode    = "code"
	ProfileChat    = "chat"
	ProfileGeneric = "generic"
)

// ErrInvalidProfile is returned by SetProfile for unknown profile values.
var ErrInvalidProfile = errors.New("invalid profile (want code, chat, or generic)")

const registrySchema = `
CREATE TABLE IF NOT EXISTS repos (
    uid         TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    state       TEXT NOT NULL,
    profile     TEXT NOT NULL DEFAULT 'code',
    repo_id     TEXT,
    created_at  INTEGER NOT NULL,
    archived_at INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS repos_active_name
    ON repos(name) WHERE state = 'active';
CREATE UNIQUE INDEX IF NOT EXISTS repos_active_repo_id
    ON repos(repo_id) WHERE state = 'active' AND repo_id IS NOT NULL;
`

// RegistrySchemaSQL exposes the repos-table DDL to the one-shot
// `knomit migrate-registry` tool, which builds the whole registry inside a
// SINGLE control.db transaction (so an abort leaves no half-built registry)
// and therefore cannot go through OpenRegistry. Exported rather than copied so
// the migration tool can never drift from the schema it is supposed to produce.
const RegistrySchemaSQL = registrySchema

// RepoRecord is one registered repository.
type RepoRecord struct {
	UID     string
	Name    string
	State   RepoState
	Profile string
	// RepoID is the root-commit hash — the knowledge base this repo holds.
	// EMPTY until the repo is first opened successfully. MUTABLE: a
	// disjoint-history origin connect swaps the store and with it this value.
	RepoID     string
	CreatedAt  int64
	ArchivedAt int64
}

// Registry persists repo registration in control.db.
type Registry struct {
	db *sql.DB
	// schemaExisted records whether the repos table was already present when
	// this handle was opened. Manager.Start's boot guard needs exactly this
	// distinction: "never had a registry" (legacy home, unmigrated) versus
	// "has a registry that is currently empty" (a migrated home that has
	// purged every repo, or never registered one) look identical to IsEmpty
	// alone but must be treated differently — see SchemaExisted.
	schemaExisted bool
}

// OpenRegistry opens the repos tenant at path — the same control.db file the
// lens registry uses — and creates its schema if absent.
//
// Manager.Start deliberately does NOT use this: it opens with
// OpenRegistryNoSchema, runs the unmigrated-home guard, and only then calls
// EnsureSchema. See refuseUnmigratedHome for why the order is load-bearing.
func OpenRegistry(path string) (*Registry, error) {
	r, err := OpenRegistryNoSchema(path)
	if err != nil {
		return nil, err
	}
	if err := r.EnsureSchema(); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

// OpenRegistryNoSchema opens the repos tenant at path WITHOUT creating its
// schema. Callers that intend to read or write rows must follow with
// EnsureSchema; the split exists so a caller can first observe whether the
// repos table was ever there — evidence that creating it would destroy.
func OpenRegistryNoSchema(path string) (*Registry, error) {
	db, err := sql.Open("sqlite3", path+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open repo registry: %w", err)
	}
	db.SetMaxOpenConns(1)
	existed, err := tableExists(db, "repos")
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("repo registry schema check: %w", err)
	}
	return &Registry{db: db, schemaExisted: existed}, nil
}

// EnsureSchema creates the repos table and its indexes if they are absent.
// Idempotent, and it does NOT disturb schemaExisted: that field answers "was
// the table there when this handle opened", which stays true of the past
// however many times this runs.
func (r *Registry) EnsureSchema() error {
	if _, err := r.db.Exec(registrySchema); err != nil {
		return fmt.Errorf("repo registry schema: %w", err)
	}
	return nil
}

// HasLegacyLensSchema reports whether control.db still carries the PRE-registry
// lens tables: a `lenses` table with the name-keyed `write_repo` column, which
// the uid-keyed schema replaced with `write_uid`.
//
// This is DURABLE evidence of an unmigrated home, and that is why the boot
// guard leads with it: it is independent of SchemaExisted, whose durability
// rests on Manager.Start deferring EnsureSchema until the guard has passed.
// Anything that creates the `repos` table on the way past makes SchemaExisted
// report true on the second boot, and a guard resting on that arm alone would
// fire only on the first — under a restart policy (systemd Restart=on-failure,
// Docker, or an operator who simply tries again) nobody would ever see it.
//
// Nothing in a failed boot removes this column: OpenLensRegistry's CREATE TABLE
// IF NOT EXISTS is a no-op against the legacy table (see LensSchemaSQL).
// `knomit migrate-registry` is the only thing that drops and rebuilds those
// tables, so the signal clears exactly when the home is actually converted.
func HasLegacyLensSchema(db *sql.DB) (bool, error) {
	ok, err := tableExists(db, "lenses")
	if err != nil || !ok {
		return false, err
	}
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('lenses') WHERE name = 'write_repo'`).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// tableExists reports whether name is a table in db's sqlite_master.
func tableExists(db *sql.DB, name string) (bool, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// SchemaExisted reports whether the repos table was already present when this
// handle was opened (true — a migrated home, whether or not it currently has
// any rows) or absent (false — this home has never had a control.db registry).
// The boot guard in Manager.Start fires only on false: a table that already
// existed but is currently empty is a normal, valid state (e.g. every repo
// purged), not an unmigrated home.
//
// This is DURABLE only because Manager.Start opens via OpenRegistryNoSchema and
// defers EnsureSchema until after the guard has passed. Creating the table on
// the way past — which OpenRegistry does — makes the second boot against an
// unconverted home report true, and the guard that should fire on every attempt
// fires only on the first.
func (r *Registry) SchemaExisted() bool {
	return r.schemaExisted
}

// Close releases the underlying database handle.
func (r *Registry) Close() error { return r.db.Close() }

// DB exposes the handle for tenants that must share this connection —
// currently Origins, whose foreign key into repos(uid) requires it. Not for
// general use.
func (r *Registry) DB() *sql.DB { return r.db }

// Insert registers a repo. A name already held by an ACTIVE repo returns
// ErrRepoExists; archived rows keep their name for display without reserving
// it.
func (r *Registry) Insert(rec RepoRecord) error {
	if rec.UID == "" {
		return fmt.Errorf("registry insert: uid required")
	}
	var repoID any
	if rec.RepoID != "" {
		repoID = rec.RepoID
	}
	var archivedAt any
	if rec.ArchivedAt != 0 {
		archivedAt = rec.ArchivedAt
	}
	_, err := r.db.Exec(
		`INSERT INTO repos (uid, name, state, profile, repo_id, created_at, archived_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		rec.UID, rec.Name, string(rec.State), rec.Profile, repoID, rec.CreatedAt, archivedAt,
	)
	if err != nil {
		return classifyRegistryErr(err, rec.Name)
	}
	return nil
}

// classifyRegistryErr maps a SQLite constraint failure onto the sentinel that
// says WHICH uniqueness rule was hit. String matching is the accepted detection
// method for the stock driver (see isUniqueViolation).
func classifyRegistryErr(err error, name string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "repos_active_repo_id"),
		strings.Contains(msg, "UNIQUE constraint failed: repos.repo_id"):
		return ErrRepoAlreadyRegistered
	case strings.Contains(msg, "repos_active_name"),
		strings.Contains(msg, "UNIQUE constraint failed: repos.name"):
		return fmt.Errorf("%w: %q", ErrRepoExists, name)
	case strings.Contains(msg, "UNIQUE constraint failed: repos.uid"):
		return fmt.Errorf("registry: uid already present")
	}
	return fmt.Errorf("registry: %w", err)
}

func scanRepo(s interface{ Scan(...any) error }) (RepoRecord, error) {
	var rec RepoRecord
	var state string
	var repoID sql.NullString
	var archivedAt sql.NullInt64
	if err := s.Scan(&rec.UID, &rec.Name, &state, &rec.Profile, &repoID, &rec.CreatedAt, &archivedAt); err != nil {
		return RepoRecord{}, err
	}
	rec.State = RepoState(state)
	rec.RepoID = repoID.String
	rec.ArchivedAt = archivedAt.Int64
	return rec, nil
}

const repoCols = `uid, name, state, profile, repo_id, created_at, archived_at`

// Get returns the record for uid; ok is false when there is no such row.
func (r *Registry) Get(uid string) (RepoRecord, bool, error) {
	rec, err := scanRepo(r.db.QueryRow(`SELECT `+repoCols+` FROM repos WHERE uid = ?`, uid))
	if errors.Is(err, sql.ErrNoRows) {
		return RepoRecord{}, false, nil
	}
	if err != nil {
		return RepoRecord{}, false, fmt.Errorf("registry get: %w", err)
	}
	return rec, true, nil
}

// ByName returns the ACTIVE repo holding name. Archived rows are invisible
// here: their name is display metadata, not a reservation.
func (r *Registry) ByName(name string) (RepoRecord, bool, error) {
	rec, err := scanRepo(r.db.QueryRow(
		`SELECT `+repoCols+` FROM repos WHERE name = ? AND state = 'active'`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return RepoRecord{}, false, nil
	}
	if err != nil {
		return RepoRecord{}, false, fmt.Errorf("registry by name: %w", err)
	}
	return rec, true, nil
}

// List returns every repo in state, sorted by name.
func (r *Registry) List(state RepoState) ([]RepoRecord, error) {
	rows, err := r.db.Query(
		`SELECT `+repoCols+` FROM repos WHERE state = ? ORDER BY name`, string(state))
	if err != nil {
		return nil, fmt.Errorf("registry list: %w", err)
	}
	defer rows.Close()
	out := []RepoRecord{}
	for rows.Next() {
		rec, err := scanRepo(rows)
		if err != nil {
			return nil, fmt.Errorf("registry list: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// SetState flips a repo between active and archived. at stamps archived_at
// when archiving and is ignored (cleared) when re-activating.
func (r *Registry) SetState(uid string, state RepoState, at int64) error {
	var archivedAt any
	if state == StateArchived {
		archivedAt = at
	}
	res, err := r.db.Exec(
		`UPDATE repos SET state = ?, archived_at = ? WHERE uid = ?`,
		string(state), archivedAt, uid)
	if err != nil {
		// The only constraint a state flip can hit is repos_active_name, and the
		// name that collides is this row's own — so read it back rather than
		// reporting `repo already exists: ""`. Reachable on Restore when the
		// name's active holder is registered but its .db is missing, which is
		// exactly when the operator has no other way to see what is in the way.
		return classifyRegistryErr(err, r.nameOf(uid))
	}
	return requireOneRow(res, uid)
}

// nameOf is a best-effort display name for uid, used only to fill in an error
// message. An empty result means the message says less, never that it lies.
func (r *Registry) nameOf(uid string) string {
	var name string
	if err := r.db.QueryRow(`SELECT name FROM repos WHERE uid = ?`, uid).Scan(&name); err != nil {
		return ""
	}
	return name
}

// Rename changes a repo's display name. Collides only with ACTIVE holders.
func (r *Registry) Rename(uid, name string) error {
	res, err := r.db.Exec(`UPDATE repos SET name = ? WHERE uid = ?`, name, uid)
	if err != nil {
		return classifyRegistryErr(err, name)
	}
	return requireOneRow(res, uid)
}

// RenameIfNamed sets uid's name to `to` ONLY IF it currently holds `from`.
// Reports whether the row changed: (false, nil) means the predicate did not
// hold — a legitimate outcome, NOT an error. Contrast Rename, which is
// unconditional and reports zero rows as ErrRegistryNotFound; the two have
// different zero-row semantics and mixing them up is the trap here.
//
// This is a compare-and-swap, and Manager.RenameRepo uses it for BOTH halves of
// a rename — the forward write and its compensating revert. Neither may be
// unconditional, for two different reasons:
//
//   - FORWARD (the primitive's main job today). Two RenameRepo calls racing
//     from the same old name would both durably succeed under a plain UPDATE,
//     with the last writer winning independently of which one goes on to win
//     the in-memory map. As a CAS, only one can see the old name still there;
//     the loser matches zero rows, learns it lost before writing anything, and
//     never needs compensating at all.
//   - COMPENSATING (RenameIfNamed(uid, new, old), undoing this call's own
//     forward write after a lost revalidate). An unconditional revert clobbers
//     a concurrent winner: it would restore the old name durably while the
//     winner's in-memory state keeps the new one, and the next boot resolves
//     the disagreement in favour of the stale value.
//
// A caller that only wants "set the name, whatever it is now" is describing
// Rename, not this — but think twice: unconditional is what made the forward
// write racy in the first place.
func (r *Registry) RenameIfNamed(uid, from, to string) (bool, error) {
	res, err := r.db.Exec(`UPDATE repos SET name = ? WHERE uid = ? AND name = ?`, to, uid, from)
	if err != nil {
		return false, classifyRegistryErr(err, to)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("registry rename if named: %w", err)
	}
	return n > 0, nil
}

// SetProfile upserts the serving profile (code | chat | generic).
func (r *Registry) SetProfile(uid, profile string) error {
	switch profile {
	case ProfileCode, ProfileChat, ProfileGeneric:
	default:
		return fmt.Errorf("%w: %q", ErrInvalidProfile, profile)
	}
	res, err := r.db.Exec(`UPDATE repos SET profile = ? WHERE uid = ?`, profile, uid)
	if err != nil {
		return fmt.Errorf("registry set profile: %w", err)
	}
	return requireOneRow(res, uid)
}

// RecordRepoID records which knowledge base this repo holds, enforcing one
// local copy per root commit among ACTIVE repos.
//
// repo_id is MUTABLE. A disjoint-history origin connect replaces the repo's
// store wholesale (Manager.SwapStore), after which its root commit is the
// remote's — so this is called on every open and after every swap, not once at
// create. Recording an unchanged value is a no-op.
func (r *Registry) RecordRepoID(uid, repoID string) error {
	if repoID == "" {
		return fmt.Errorf("registry record repo id: empty id")
	}
	res, err := r.db.Exec(`UPDATE repos SET repo_id = ? WHERE uid = ?`, repoID, uid)
	if err != nil {
		return classifyRegistryErr(err, "")
	}
	return requireOneRow(res, uid)
}

// Delete removes a repo row permanently. Cascades to repo_origins.
func (r *Registry) Delete(uid string) error {
	if _, err := r.db.Exec(`DELETE FROM repos WHERE uid = ?`, uid); err != nil {
		return fmt.Errorf("registry delete: %w", err)
	}
	return nil
}

// IsEmpty reports whether any repo is registered. NOT what the boot guard
// uses to detect an unmigrated home — a migrated home that has purged every
// repo is also empty, and must boot. See SchemaExisted for the signal
// the guard actually needs.
func (r *Registry) IsEmpty() (bool, error) {
	var n int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM repos`).Scan(&n); err != nil {
		return false, fmt.Errorf("registry is empty: %w", err)
	}
	return n == 0, nil
}

func requireOneRow(res sql.Result, uid string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("registry: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %q", ErrRegistryNotFound, uid)
	}
	return nil
}
