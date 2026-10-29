package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// A BOOK FILE holds one book's structure: the frozen search results, every
// appearance (row) of the units walked so far, and the visited set the next
// unit's walk is seeded with. It is a SQLite file of its own under
// <session DB>.books/, so books never share a lock. It is written only by the
// book's one cursor holder, a unit at a time, as pages need units. It is a
// rebuildable cache, never a record: journal_mode MEMORY and synchronous OFF,
// and the directory is recreated empty at every Open.
//
// Lifecycle: created by CreateBook; deleted when the owner's next first call
// replaces the book (CreateBook), by DeleteBook, by SweepBookFiles once the
// idle reaper has deleted the cursor, and wholesale at Open and Close.

const bookFileSuffix = ".db"

const bookFileSchema = `
CREATE TABLE results (
    unit   INTEGER PRIMARY KEY,   -- 1..units, the search order
    path   TEXT NOT NULL UNIQUE,  -- wire path
    anchor TEXT NOT NULL          -- the anchor reserved for the unit root
);
CREATE TABLE rows (
    seq     INTEGER PRIMARY KEY,  -- book order, 1..
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
CREATE TABLE anchors (anchor TEXT PRIMARY KEY);
CREATE TABLE tips (               -- cross-KB repos' tips, frozen on first read
    repo        TEXT PRIMARY KEY,
    commit_hash TEXT NOT NULL
);
`

// Tips is the frozen tip of every repo a unit entered cross-KB, by id12.
func (f *BookFile) Tips(ctx context.Context) (map[string]string, error) {
	rows, err := f.db.QueryContext(ctx, `SELECT repo, commit_hash FROM tips`)
	if err != nil {
		return nil, fmt.Errorf("book tips: %w", err)
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

// BookResult is one frozen search result: the root of unit Unit.
type BookResult struct {
	Unit   int
	Path   string
	Anchor string
}

// BookRow is one appearance of a fact in book order.
type BookRow struct {
	Seq     int
	Unit    int
	N       int
	Path    string
	Written bool
	State   string
}

// BookVisit is the first (written) appearance of a fact.
type BookVisit struct {
	Path   string
	Unit   int
	N      int
	Seq    int
	Number string
	Anchor string
	Commit string
	Blob   string
}

// ErrBookUnitTaken is an AppendUnit for a unit another call appended first
// (two calls racing one cursor). Nothing was written.
var ErrBookUnitTaken = errors.New("book unit already walked")

// BookFile is an open book file, for one call.
type BookFile struct {
	db      *sql.DB
	release func()
}

func bookDSN(path string) string {
	return "file:" + path + "?_journal_mode=MEMORY&_synchronous=OFF&_busy_timeout=5000"
}

func createBookFile(ctx context.Context, path string, results []BookResult) error {
	db, err := sql.Open("sqlite3", bookDSN(path))
	if err != nil {
		return fmt.Errorf("create book file: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	fail := func(err error) error {
		db.Close()
		removeBookFile(path)
		return fmt.Errorf("create book file: %w", err)
	}
	if _, err := db.ExecContext(ctx, bookFileSchema); err != nil {
		return fail(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	for _, r := range results {
		if _, err := tx.ExecContext(ctx, `INSERT INTO results(unit, path, anchor) VALUES (?, ?, ?)`, r.Unit, r.Path, r.Anchor); err != nil {
			return fail(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO anchors(anchor) VALUES (?)`, r.Anchor); err != nil {
			return fail(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	return nil
}

func openBookFile(path string) (*BookFile, bool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	db, err := sql.Open("sqlite3", bookDSN(path))
	if err != nil {
		return nil, false, fmt.Errorf("open book file: %w", err)
	}
	db.SetMaxOpenConns(1)
	return &BookFile{db: db}, true, nil
}

func removeBookFile(path string) {
	for _, s := range []string{"", "-journal", "-wal", "-shm"} {
		_ = os.Remove(path + s)
	}
}

// Close releases the file.
func (f *BookFile) Close() error {
	err := f.db.Close()
	if f.release != nil {
		f.release()
		f.release = nil
	}
	return err
}

// Results is every frozen search result, by unit.
func (f *BookFile) Results(ctx context.Context) ([]BookResult, error) {
	rows, err := f.db.QueryContext(ctx, `SELECT unit, path, anchor FROM results ORDER BY unit`)
	if err != nil {
		return nil, fmt.Errorf("book results: %w", err)
	}
	defer rows.Close()
	var out []BookResult
	for rows.Next() {
		var r BookResult
		if err := rows.Scan(&r.Unit, &r.Path, &r.Anchor); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Walked is how many units are walked and how many rows they hold.
func (f *BookFile) Walked(ctx context.Context) (units, rows int, err error) {
	var u, r sql.NullInt64
	if err := f.db.QueryRowContext(ctx, `SELECT MAX(unit), MAX(seq) FROM rows`).Scan(&u, &r); err != nil {
		return 0, 0, fmt.Errorf("book walked: %w", err)
	}
	return int(u.Int64), int(r.Int64), nil
}

// Visited is the first appearance of path, if it was written.
func (f *BookFile) Visited(ctx context.Context, path string) (BookVisit, bool, error) {
	v := BookVisit{Path: path}
	err := f.db.QueryRowContext(ctx,
		`SELECT unit, n, seq, number, anchor, commit_hash, blob FROM visited WHERE path = ?`, path,
	).Scan(&v.Unit, &v.N, &v.Seq, &v.Number, &v.Anchor, &v.Commit, &v.Blob)
	if errors.Is(err, sql.ErrNoRows) {
		return BookVisit{}, false, nil
	}
	if err != nil {
		return BookVisit{}, false, fmt.Errorf("book visited: %w", err)
	}
	return v, true, nil
}

// AnchorTaken reports whether an anchor is used (or reserved) in the book.
func (f *BookFile) AnchorTaken(ctx context.Context, anchor string) (bool, error) {
	var one int
	err := f.db.QueryRowContext(ctx, `SELECT 1 FROM anchors WHERE anchor = ?`, anchor).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// AppendUnit appends one walked unit: its rows, the visits of the facts it
// wrote and the anchors it used, in one transaction. It is refused with
// ErrBookUnitTaken unless unit is the next unit to walk.
func (f *BookFile) AppendUnit(ctx context.Context, unit int, rows []BookRow, visits []BookVisit, anchors []string, tips map[string]string) error {
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("book append: %w", err)
	}
	defer tx.Rollback()
	var last sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(unit) FROM rows`).Scan(&last); err != nil {
		return fmt.Errorf("book append: %w", err)
	}
	if int(last.Int64) != unit-1 {
		return ErrBookUnitTaken
	}
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO rows(seq, unit, n, path, written, state) VALUES (?, ?, ?, ?, ?, ?)`,
			r.Seq, r.Unit, r.N, r.Path, boolInt(r.Written), r.State); err != nil {
			return fmt.Errorf("book append row: %w", err)
		}
	}
	for _, v := range visits {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO visited(path, unit, n, seq, number, anchor, commit_hash, blob) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			v.Path, v.Unit, v.N, v.Seq, v.Number, v.Anchor, v.Commit, v.Blob); err != nil {
			return fmt.Errorf("book append visit: %w", err)
		}
	}
	for _, a := range anchors {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO anchors(anchor) VALUES (?)`, a); err != nil {
			return fmt.Errorf("book append anchor: %w", err)
		}
	}
	for repo, tip := range tips {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tips(repo, commit_hash) VALUES (?, ?)`, repo, tip); err != nil {
			return fmt.Errorf("book append tip: %w", err)
		}
	}
	return tx.Commit()
}

// Rows returns up to limit rows from seq from, in book order.
func (f *BookFile) Rows(ctx context.Context, from, limit int) ([]BookRow, error) {
	rows, err := f.db.QueryContext(ctx,
		`SELECT seq, unit, n, path, written, state FROM rows WHERE seq >= ? ORDER BY seq LIMIT ?`, from, limit)
	if err != nil {
		return nil, fmt.Errorf("book rows: %w", err)
	}
	defer rows.Close()
	var out []BookRow
	for rows.Next() {
		var r BookRow
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
func (f *BookFile) UnitStart(ctx context.Context, unit int) (int, error) {
	var seq sql.NullInt64
	if err := f.db.QueryRowContext(ctx, `SELECT MIN(seq) FROM rows WHERE unit = ?`, unit).Scan(&seq); err != nil {
		return 0, fmt.Errorf("book unit start: %w", err)
	}
	return int(seq.Int64), nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// load writes a spilled book's whole structure in one transaction (the
// results are already in the file).
func (f *BookFile) load(ctx context.Context, rows []BookRow, visits []BookVisit, anchors []string, tips map[string]string) error {
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
