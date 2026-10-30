package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// A FACTSET FILE holds one factset's structure: the frozen search results, every
// appearance (row) of the units walked so far, and the visited set the next
// unit's walk is seeded with. It is a SQLite file of its own under
// <session DB>.factsets/, so factsets never share a lock. It is written only by the
// factset's one cursor holder, a unit at a time, as pages need units. It is a
// rebuildable cache, never a record: journal_mode MEMORY and synchronous OFF,
// and the directory is recreated empty at every Open.
//
// Lifecycle: created when an in-memory factset spills (factset_store.go); deleted
// when the owner's next first call replaces the factset (CreateFactSet), by
// DeleteFactSet, by SweepFactSetFiles once the idle reaper has deleted the cursor,
// and wholesale at Open and Close; never while a call holds the factset (the
// last holder's release deletes it then).

const factsetFileSuffix = ".db"

const factsetFileSchema = `
CREATE TABLE results (
    unit   INTEGER PRIMARY KEY,   -- 1..units, the search order
    path   TEXT NOT NULL UNIQUE,  -- wire path
    anchor TEXT NOT NULL,         -- the anchor reserved for the unit root
    title  TEXT NOT NULL          -- the title the search listed
);
CREATE TABLE rows (
    seq     INTEGER PRIMARY KEY,  -- factset order, 1..
    unit    INTEGER NOT NULL,
    n       INTEGER NOT NULL,     -- ordinal inside the unit's walk
    path    TEXT NOT NULL,
    written INTEGER NOT NULL,     -- 1: carries the body; 0: a link
    state   TEXT NOT NULL         -- JSON node without body
);
CREATE INDEX rows_unit ON rows(unit, seq);
CREATE TABLE visited (            -- first appearance of every written fact
    path        TEXT PRIMARY KEY,
    unit        INTEGER NOT NULL,
    n           INTEGER NOT NULL,
    seq         INTEGER NOT NULL,
    number      TEXT NOT NULL,
    anchor      TEXT NOT NULL,
    commit_hash TEXT NOT NULL,
    blob        TEXT NOT NULL
);
CREATE TABLE reserved (           -- direct references reserved for a unit
    path        TEXT PRIMARY KEY,
    unit        INTEGER NOT NULL,
    number      TEXT NOT NULL,
    anchor      TEXT NOT NULL,
    commit_hash TEXT NOT NULL,
    blob        TEXT NOT NULL
);
CREATE TABLE anchors (anchor TEXT PRIMARY KEY);
CREATE TABLE tips (               -- cross-KB repos' tips, frozen on first read
    repo        TEXT PRIMARY KEY,
    commit_hash TEXT NOT NULL
);
`

// Tips is the frozen tip of every repo a unit entered cross-KB, by id12.
func (f *FactSetFile) Tips(ctx context.Context) (map[string]string, error) {
	rows, err := f.db.QueryContext(ctx, `SELECT repo, commit_hash FROM tips`)
	if err != nil {
		return nil, fmt.Errorf("factset tips: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// FactSetResult is one frozen search result: the root of unit Unit.
type FactSetResult struct {
	Unit   int
	Path   string
	Anchor string
	// Title is the result's title as the search listed it: a unit whose
	// fact is gone by the time it is walked is shown under it.
	Title string
}

// FactSetReserve is a direct reference of a unit's root reserved for that unit
// before any unit is walked (two-level reservation): it is written there,
// as the root's child number Number, under Anchor, at the version
// Commit/Blob the root cites; every earlier unit that reaches it links
// forward to it.
type FactSetReserve struct {
	Path   string
	Unit   int
	Number string
	Anchor string
	Commit string
	Blob   string
}

// FactSetRow is one appearance of a fact in factset order.
type FactSetRow struct {
	Seq     int
	Unit    int
	N       int
	Path    string
	Written bool
	State   string
}

// FactSetVisit is the first (written) appearance of a fact.
type FactSetVisit struct {
	Path   string
	Unit   int
	N      int
	Seq    int
	Number string
	Anchor string
	Commit string
	Blob   string
}

// ErrFactSetUnitTaken is an AppendUnit for a unit another call appended first
// (two calls racing one cursor). Nothing was written.
var ErrFactSetUnitTaken = errors.New("factset unit already walked")

// FactSetFile is an open factset file, for one call.
type FactSetFile struct {
	db      *sql.DB
	release func()
}

func factsetDSN(path string) string {
	return "file:" + path + "?_journal_mode=MEMORY&_synchronous=OFF&_busy_timeout=5000"
}

func createFactSetFile(ctx context.Context, path string, results []FactSetResult, reserved map[string]FactSetReserve) error {
	db, err := sql.Open("sqlite3", factsetDSN(path))
	if err != nil {
		return fmt.Errorf("create factset file: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	fail := func(err error) error {
		db.Close()
		removeFactSetFile(path)
		return fmt.Errorf("create factset file: %w", err)
	}
	if _, err := db.ExecContext(ctx, factsetFileSchema); err != nil {
		return fail(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	for _, r := range results {
		if _, err := tx.ExecContext(ctx, `INSERT INTO results(unit, path, anchor, title) VALUES (?, ?, ?, ?)`, r.Unit, r.Path, r.Anchor, r.Title); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO anchors(anchor) VALUES (?)`, r.Anchor); err != nil {
			return fail(err)
		}
	}
	for _, r := range reserved {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO reserved(path, unit, number, anchor, commit_hash, blob) VALUES (?, ?, ?, ?, ?, ?)`,
			r.Path, r.Unit, r.Number, r.Anchor, r.Commit, r.Blob); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO anchors(anchor) VALUES (?)`, r.Anchor); err != nil {
			return fail(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	return nil
}

func openFactSetFile(path string) (*FactSetFile, bool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	db, err := sql.Open("sqlite3", factsetDSN(path))
	if err != nil {
		return nil, false, fmt.Errorf("open factset file: %w", err)
	}
	db.SetMaxOpenConns(1)
	return &FactSetFile{db: db}, true, nil
}

func removeFactSetFile(path string) {
	for _, s := range []string{"", "-journal", "-wal", "-shm"} {
		_ = os.Remove(path + s)
	}
}

// Close releases the file.
func (f *FactSetFile) Close() error {
	err := f.db.Close()
	if f.release != nil {
		f.release()
		f.release = nil
	}
	return err
}

// Results is every frozen search result, by unit.
func (f *FactSetFile) Results(ctx context.Context) ([]FactSetResult, error) {
	rows, err := f.db.QueryContext(ctx, `SELECT unit, path, anchor, title FROM results ORDER BY unit`)
	if err != nil {
		return nil, fmt.Errorf("factset results: %w", err)
	}
	defer rows.Close()
	var out []FactSetResult
	for rows.Next() {
		var r FactSetResult
		if err := rows.Scan(&r.Unit, &r.Path, &r.Anchor, &r.Title); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Walked is how many units are walked and how many rows they hold.
func (f *FactSetFile) Walked(ctx context.Context) (units, rows int, err error) {
	var u, r sql.NullInt64
	if err := f.db.QueryRowContext(ctx, `SELECT MAX(unit), MAX(seq) FROM rows`).Scan(&u, &r); err != nil {
		return 0, 0, fmt.Errorf("factset walked: %w", err)
	}
	return int(u.Int64), int(r.Int64), nil
}

// Visited is the first appearance of path, if it was written.
func (f *FactSetFile) Visited(ctx context.Context, path string) (FactSetVisit, bool, error) {
	v := FactSetVisit{Path: path}
	err := f.db.QueryRowContext(ctx,
		`SELECT unit, n, seq, number, anchor, commit_hash, blob FROM visited WHERE path = ?`, path,
	).Scan(&v.Unit, &v.N, &v.Seq, &v.Number, &v.Anchor, &v.Commit, &v.Blob)
	if errors.Is(err, sql.ErrNoRows) {
		return FactSetVisit{}, false, nil
	}
	if err != nil {
		return FactSetVisit{}, false, fmt.Errorf("factset visited: %w", err)
	}
	return v, true, nil
}

// Reserved is the reservation of path, if a unit reserved it.
func (f *FactSetFile) Reserved(ctx context.Context, path string) (FactSetReserve, bool, error) {
	r := FactSetReserve{Path: path}
	err := f.db.QueryRowContext(ctx,
		`SELECT unit, number, anchor, commit_hash, blob FROM reserved WHERE path = ?`, path,
	).Scan(&r.Unit, &r.Number, &r.Anchor, &r.Commit, &r.Blob)
	if errors.Is(err, sql.ErrNoRows) {
		return FactSetReserve{}, false, nil
	}
	if err != nil {
		return FactSetReserve{}, false, fmt.Errorf("factset reserved: %w", err)
	}
	return r, true, nil
}

// AnchorTaken reports whether an anchor is used (or reserved) in the factset.
func (f *FactSetFile) AnchorTaken(ctx context.Context, anchor string) (bool, error) {
	var one int
	err := f.db.QueryRowContext(ctx, `SELECT 1 FROM anchors WHERE anchor = ?`, anchor).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// AppendUnit appends one walked unit: its rows, the visits of the facts it
// wrote and the anchors it used, in one transaction. It is refused with
// ErrFactSetUnitTaken unless unit is the next unit to walk.
func (f *FactSetFile) AppendUnit(ctx context.Context, unit int, rows []FactSetRow, visits []FactSetVisit, anchors []string, tips map[string]string) error {
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("factset append: %w", err)
	}
	defer tx.Rollback()
	var last sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(unit) FROM rows`).Scan(&last); err != nil {
		return fmt.Errorf("factset append: %w", err)
	}
	if int(last.Int64) != unit-1 {
		return ErrFactSetUnitTaken
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rows(seq, unit, n, path, written, state) VALUES (?, ?, ?, ?, ?, ?)`,
			r.Seq, r.Unit, r.N, r.Path, boolInt(r.Written), r.State); err != nil {
			return fmt.Errorf("factset append row: %w", err)
		}
	}
	for _, v := range visits {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO visited(path, unit, n, seq, number, anchor, commit_hash, blob) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			v.Path, v.Unit, v.N, v.Seq, v.Number, v.Anchor, v.Commit, v.Blob); err != nil {
			return fmt.Errorf("factset append visit: %w", err)
		}
	}
	for _, a := range anchors {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO anchors(anchor) VALUES (?)`, a); err != nil {
			return fmt.Errorf("factset append anchor: %w", err)
		}
	}
	for repo, tip := range tips {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tips(repo, commit_hash) VALUES (?, ?)`, repo, tip); err != nil {
			return fmt.Errorf("factset append tip: %w", err)
		}
	}
	return tx.Commit()
}

// Rows returns up to limit rows from seq from, in factset order.
func (f *FactSetFile) Rows(ctx context.Context, from, limit int) ([]FactSetRow, error) {
	rows, err := f.db.QueryContext(ctx,
		`SELECT seq, unit, n, path, written, state FROM rows WHERE seq >= ? ORDER BY seq LIMIT ?`, from, limit)
	if err != nil {
		return nil, fmt.Errorf("factset rows: %w", err)
	}
	defer rows.Close()
	var out []FactSetRow
	for rows.Next() {
		var r FactSetRow
		var w int
		if err := rows.Scan(&r.Seq, &r.Unit, &r.N, &r.Path, &w, &r.State); err != nil {
			return nil, err
		}
		r.Written = w == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// UnitStart is the seq of unit's first row, 0 when the unit is not walked.
func (f *FactSetFile) UnitStart(ctx context.Context, unit int) (int, error) {
	var seq sql.NullInt64
	if err := f.db.QueryRowContext(ctx, `SELECT MIN(seq) FROM rows WHERE unit = ?`, unit).Scan(&seq); err != nil {
		return 0, fmt.Errorf("factset unit start: %w", err)
	}
	return int(seq.Int64), nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// load writes a spilled factset's whole structure in one transaction (the
// results are already in the file).
func (f *FactSetFile) load(ctx context.Context, rows []FactSetRow, visits []FactSetVisit, anchors []string, tips map[string]string) error {
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rows(seq, unit, n, path, written, state) VALUES (?, ?, ?, ?, ?, ?)`,
			r.Seq, r.Unit, r.N, r.Path, boolInt(r.Written), r.State); err != nil {
			return err
		}
	}
	for _, v := range visits {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO visited(path, unit, n, seq, number, anchor, commit_hash, blob) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			v.Path, v.Unit, v.N, v.Seq, v.Number, v.Anchor, v.Commit, v.Blob); err != nil {
			return err
		}
	}
	for _, a := range anchors {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO anchors(anchor) VALUES (?)`, a); err != nil {
			return err
		}
	}
	for repo, tip := range tips {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tips(repo, commit_hash) VALUES (?, ?)`, repo, tip); err != nil {
			return err
		}
	}
	return tx.Commit()
}
