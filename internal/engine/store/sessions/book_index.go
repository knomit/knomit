package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	sqlite3 "github.com/mattn/go-sqlite3"
)

// Book cursors: the session-DB half of "materialize a search as a book". A
// book is a tool_sessions row (so ownership, binding pin, read set and the
// idle reaper apply unchanged) plus book_sessions (the envelope and the
// cursor position) and book_rows (the frozen structure, one row per
// appearance of a fact). See session_schema.sql.
//
// WRITES. Every write here is one BEGIN IMMEDIATE transaction on a
// dedicated connection (bookWrite), the createPipelineSession / DequeuePaths
// pattern: a read-then-write never upgrades a deferred lock (invariant
// a8e8e8e4), and a waiter is bounded by the busy timeout. Inserts are
// chunked by the caller (one unit, at most a unit's MaxNodes rows, per
// transaction), so no write holds the lock for a whole book. bookWrite is
// the ONE seam every book write goes through, so an in-process writer gate
// can be added there without touching a caller; it records how long each
// write waited for the lock (BookLockWaits). A lock wait that times out is
// ErrBusy, which an edge answers 503 with Retry-After.

// BookToolPrefix prefixes the tool_sessions.tool value of a book cursor:
// "materialize-book:<limit>".
const BookToolPrefix = "materialize-book:"

// ErrBusy is a session-DB write that could not take the write lock within
// the busy timeout. Nothing was written; the caller may retry.
var ErrBusy = errors.New("session store busy")

// BookRow is one appearance of a fact in a book's structure.
type BookRow struct {
	Seq       int
	Unit      int
	N         int
	Path      string
	Room      int
	Result    bool
	Written   bool
	Number    string
	TreeDepth int
	Title     string
	ShortID   string
	Anchor    string
	Commit    string
	Blob      string
	State     string
}

// BookInfo is a book's envelope and cursor position.
type BookInfo struct {
	Env     string
	Units   int
	Rows    int
	NextSeq int
	Ready   bool
}

// bookLockWaits is the lock-wait record of every book write on this index.
type bookLockWaits struct {
	count, totalNs, maxNs atomic.Int64
}

func (w *bookLockWaits) record(d time.Duration) {
	w.count.Add(1)
	w.totalNs.Add(int64(d))
	for {
		cur := w.maxNs.Load()
		if int64(d) <= cur || w.maxNs.CompareAndSwap(cur, int64(d)) {
			return
		}
	}
}

// BookLockWaits reports how many book writes ran on this index, their total
// and their longest wait for the session DB's write lock.
func (ti *ToolIndex) BookLockWaits() (count int64, total, max time.Duration) {
	return ti.waits.count.Load(), time.Duration(ti.waits.totalNs.Load()), time.Duration(ti.waits.maxNs.Load())
}

// isBusy reports whether err is SQLite's "database is locked".
func isBusy(err error) bool {
	var se sqlite3.Error
	if errors.As(err, &se) {
		return se.Code == sqlite3.ErrBusy || se.Code == sqlite3.ErrLocked
	}
	return false
}

// bookWrite runs fn in BEGIN IMMEDIATE on a connection of its own, and
// commits; any failure rolls back (under a context that cannot be cancelled,
// so a cancelled request never leaves the lock held). A lock that could not
// be taken is ErrBusy.
func (ti *ToolIndex) bookWrite(ctx context.Context, op string, fn func(c *sql.Conn) error) error {
	c, err := ti.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("%s conn: %w", op, err)
	}
	defer c.Close()
	start := time.Now()
	if _, err := c.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		if isBusy(err) {
			return fmt.Errorf("%s: %w", op, ErrBusy)
		}
		return fmt.Errorf("%s begin: %w", op, err)
	}
	ti.waits.record(time.Since(start))
	committed := false
	defer func() {
		if !committed {
			_, _ = c.ExecContext(context.WithoutCancel(ctx), "ROLLBACK")
		}
	}()
	if err := fn(c); err != nil {
		if isBusy(err) {
			return fmt.Errorf("%s: %w", op, ErrBusy)
		}
		return err
	}
	if _, err := c.ExecContext(ctx, "COMMIT"); err != nil {
		if isBusy(err) {
			return fmt.Errorf("%s commit: %w", op, ErrBusy)
		}
		return fmt.Errorf("%s commit: %w", op, err)
	}
	committed = true
	return nil
}

// CreateBook starts a book cursor for owner: it deletes owner's other book
// cursors (one active book per endpoint + principal: a new first call
// replaces the old one), then creates the session in status "building" with
// its envelope. The structure is appended with AppendBookRows and placed by
// FinishBook; until then the cursor resumes as expired.
func (ti *ToolIndex) CreateBook(ctx context.Context, tool, branch, binding, readSet, owner, env string) (string, error) {
	if !strings.HasPrefix(tool, BookToolPrefix) {
		return "", fmt.Errorf("CreateBook: tool %q is not a book tool", tool)
	}
	id := uuid.New().String()
	now := time.Now().UTC().Format(time.RFC3339)
	err := ti.bookWrite(ctx, "CreateBook", func(c *sql.Conn) error {
		if _, err := c.ExecContext(ctx,
			`DELETE FROM tool_sessions WHERE owner = ? AND tool LIKE ?`, owner, BookToolPrefix+"%"); err != nil {
			return fmt.Errorf("CreateBook replace: %w", err)
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO tool_sessions(id, tool, branch, path_prefix, binding, read_set, owner, last_commit, status, created_at, updated_at, last_used_at)
			 VALUES (?, ?, ?, '', ?, ?, ?, '', 'building', ?, ?, ?)`,
			id, tool, branch, binding, readSet, owner, now, now, now); err != nil {
			return fmt.Errorf("CreateBook session: %w", err)
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO book_sessions(session_id, env) VALUES (?, ?)`, id, env); err != nil {
			return fmt.Errorf("CreateBook book: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// ErrBookGone is a build whose cursor no longer exists: replaced by the
// owner's newer first call, or reaped.
var ErrBookGone = errors.New("book cursor replaced or expired")

// AppendBookRows appends one chunk of a book's structure. It fails with
// ErrBookGone when the book was replaced meanwhile, so an abandoned build
// stops writing.
func (ti *ToolIndex) AppendBookRows(ctx context.Context, id string, rows []BookRow) error {
	if len(rows) == 0 {
		return nil
	}
	return ti.bookWrite(ctx, "AppendBookRows", func(c *sql.Conn) error {
		var one int
		if err := c.QueryRowContext(ctx, `SELECT 1 FROM book_sessions WHERE session_id = ?`, id).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrBookGone
			}
			return fmt.Errorf("AppendBookRows check: %w", err)
		}
		st, err := c.PrepareContext(ctx,
			`INSERT INTO book_rows(session_id, seq, unit, n, path, room, result, number, tree_depth, title, short_id, anchor, commit_hash, blob, state)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("AppendBookRows prepare: %w", err)
		}
		defer st.Close()
		for _, r := range rows {
			if _, err := st.ExecContext(ctx, id, r.Seq, r.Unit, r.N, r.Path, r.Room, boolInt(r.Result),
				r.Number, r.TreeDepth, r.Title, r.ShortID, r.Anchor, r.Commit, r.Blob, r.State); err != nil {
				return fmt.Errorf("AppendBookRows insert: %w", err)
			}
		}
		return nil
	})
}

// FinishBook places the structure and opens the cursor. Placement (the
// write-once rule): for every path, exactly one appearance is written — the
// unit root when the path is a search result, otherwise the appearance with
// the most room, ties to the earliest in book order. Every written
// appearance gets a book-unique anchor "f-<id8>", suffixed "-2", "-3", ...
// in book order when two facts share an id8. env replaces the envelope (it
// now knows the counts).
func (ti *ToolIndex) FinishBook(ctx context.Context, id, env string, units, rows int) error {
	return ti.bookWrite(ctx, "FinishBook", func(c *sql.Conn) error {
		res, err := c.ExecContext(ctx,
			`UPDATE book_sessions SET env = ?, units = ?, rows = ?, next_seq = 1, ready = 1 WHERE session_id = ?`,
			env, units, rows, id)
		if err != nil {
			return fmt.Errorf("FinishBook envelope: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrBookGone
		}
		if _, err := c.ExecContext(ctx,
			`UPDATE book_rows SET written = 1 WHERE session_id = ?1 AND seq IN (
			   SELECT seq FROM (
			     SELECT seq, ROW_NUMBER() OVER (PARTITION BY path ORDER BY result DESC, room DESC, seq ASC) AS rk
			     FROM book_rows WHERE session_id = ?1)
			   WHERE rk = 1)`, id); err != nil {
			return fmt.Errorf("FinishBook placement: %w", err)
		}
		if _, err := c.ExecContext(ctx,
			`WITH w AS (
			   SELECT seq, 'f-' || short_id AS base,
			          ROW_NUMBER() OVER (PARTITION BY short_id ORDER BY seq) AS k
			   FROM book_rows WHERE session_id = ?1 AND written = 1)
			 UPDATE book_rows SET anchor = CASE WHEN w.k > 1 THEN w.base || '-' || w.k ELSE w.base END
			 FROM w WHERE book_rows.session_id = ?1 AND book_rows.seq = w.seq`, id); err != nil {
			return fmt.Errorf("FinishBook anchors: %w", err)
		}
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := c.ExecContext(ctx,
			`UPDATE tool_sessions SET status = 'active', updated_at = ?, last_used_at = ? WHERE id = ?`, now, now, id); err != nil {
			return fmt.Errorf("FinishBook activate: %w", err)
		}
		return nil
	})
}

// DeleteBook removes a book cursor and its structure (a failed build).
func (ti *ToolIndex) DeleteBook(ctx context.Context, id string) error {
	return ti.bookWrite(ctx, "DeleteBook", func(c *sql.Conn) error {
		_, err := c.ExecContext(ctx, `DELETE FROM tool_sessions WHERE id = ?`, id)
		return err
	})
}

// GetBook returns a book's envelope and position, nil when id is not a book.
func (ti *ToolIndex) GetBook(ctx context.Context, id string) (*BookInfo, error) {
	var b BookInfo
	var ready int
	err := ti.db.QueryRowContext(ctx,
		`SELECT env, units, rows, next_seq, ready FROM book_sessions WHERE session_id = ?`, id,
	).Scan(&b.Env, &b.Units, &b.Rows, &b.NextSeq, &ready)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetBook: %w", err)
	}
	b.Ready = ready == 1
	return &b, nil
}

const bookRowCols = `seq, unit, n, path, room, result, written, number, tree_depth, title, short_id, anchor, commit_hash, blob, state`

func scanBookRows(rows *sql.Rows) ([]BookRow, error) {
	defer rows.Close()
	var out []BookRow
	for rows.Next() {
		var r BookRow
		var result, written int
		if err := rows.Scan(&r.Seq, &r.Unit, &r.N, &r.Path, &r.Room, &result, &written, &r.Number,
			&r.TreeDepth, &r.Title, &r.ShortID, &r.Anchor, &r.Commit, &r.Blob, &r.State); err != nil {
			return nil, err
		}
		r.Result, r.Written = result == 1, written == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// BookRows returns up to limit rows from seq from, in book order.
func (ti *ToolIndex) BookRows(ctx context.Context, id string, from, limit int) ([]BookRow, error) {
	rows, err := ti.db.QueryContext(ctx,
		`SELECT `+bookRowCols+` FROM book_rows WHERE session_id = ? AND seq >= ? ORDER BY seq LIMIT ?`, id, from, limit)
	if err != nil {
		return nil, fmt.Errorf("BookRows: %w", err)
	}
	out, err := scanBookRows(rows)
	if err != nil {
		return nil, fmt.Errorf("BookRows scan: %w", err)
	}
	return out, nil
}

// BookUnitStart is the seq of unit's first row, 0 when the book has no such
// unit.
func (ti *ToolIndex) BookUnitStart(ctx context.Context, id string, unit int) (int, error) {
	var seq sql.NullInt64
	if err := ti.db.QueryRowContext(ctx,
		`SELECT MIN(seq) FROM book_rows WHERE session_id = ? AND unit = ?`, id, unit).Scan(&seq); err != nil {
		return 0, fmt.Errorf("BookUnitStart: %w", err)
	}
	return int(seq.Int64), nil
}

// BookWritten returns, for each of paths, its written appearance.
func (ti *ToolIndex) BookWritten(ctx context.Context, id string, paths []string) (map[string]BookRow, error) {
	out := make(map[string]BookRow, len(paths))
	for start := 0; start < len(paths); start += 400 {
		chunk := paths[start:min(start+400, len(paths))]
		args := []any{id}
		for _, p := range chunk {
			args = append(args, p)
		}
		rows, err := ti.db.QueryContext(ctx,
			`SELECT `+bookRowCols+` FROM book_rows WHERE session_id = ? AND written = 1 AND path IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("BookWritten: %w", err)
		}
		got, err := scanBookRows(rows)
		if err != nil {
			return nil, fmt.Errorf("BookWritten scan: %w", err)
		}
		for _, r := range got {
			out[r.Path] = r
		}
	}
	return out, nil
}

// BookUnitRows returns the rows of unit with the given walk ordinals, by n.
func (ti *ToolIndex) BookUnitRows(ctx context.Context, id string, unit int, ns []int) (map[int]BookRow, error) {
	out := make(map[int]BookRow, len(ns))
	for start := 0; start < len(ns); start += 400 {
		chunk := ns[start:min(start+400, len(ns))]
		args := []any{id, unit}
		for _, n := range chunk {
			args = append(args, n)
		}
		rows, err := ti.db.QueryContext(ctx,
			`SELECT `+bookRowCols+` FROM book_rows WHERE session_id = ? AND unit = ? AND n IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("BookUnitRows: %w", err)
		}
		got, err := scanBookRows(rows)
		if err != nil {
			return nil, fmt.Errorf("BookUnitRows scan: %w", err)
		}
		for _, r := range got {
			out[r.N] = r
		}
	}
	return out, nil
}

// AdvanceBook moves the cursor from seq from to seq to, only if it is still
// at from (a compare-and-swap: a cursor has one reader at a time, and a
// concurrent call on the same cursor loses instead of serving the same rows
// twice), and bumps the idle-reaper heartbeat. ok is false when the cursor
// moved meanwhile or is gone.
func (ti *ToolIndex) AdvanceBook(ctx context.Context, id string, from, to int) (bool, error) {
	ok := false
	err := ti.bookWrite(ctx, "AdvanceBook", func(c *sql.Conn) error {
		res, err := c.ExecContext(ctx,
			`UPDATE book_sessions SET next_seq = ? WHERE session_id = ? AND next_seq = ? AND ready = 1`, to, id, from)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		ok = n == 1
		if !ok {
			return nil
		}
		now := time.Now().UTC().Format(time.RFC3339)
		_, err = c.ExecContext(ctx, `UPDATE tool_sessions SET last_used_at = ? WHERE id = ?`, now, id)
		return err
	})
	return ok, err
}

// TouchBook bumps the heartbeat without moving the cursor (an export reads
// the whole book and leaves the position where the reader left it).
func (ti *ToolIndex) TouchBook(ctx context.Context, id string) error {
	return ti.bookWrite(ctx, "TouchBook", func(c *sql.Conn) error {
		now := time.Now().UTC().Format(time.RFC3339)
		_, err := c.ExecContext(ctx, `UPDATE tool_sessions SET last_used_at = ? WHERE id = ?`, now, id)
		return err
	})
}

// BookStorage reports the session DB's size on disk in bytes (page count x
// page size): the measurement for the structure's disk cost.
func (ti *ToolIndex) BookStorage(ctx context.Context) (int64, error) {
	var pages, size int64
	if err := ti.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pages); err != nil {
		return 0, err
	}
	if err := ti.db.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&size); err != nil {
		return 0, err
	}
	return pages * size, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// BookCount reports how many book cursors and structure rows the session DB
// holds (every owner's).
func (ti *ToolIndex) BookCount(ctx context.Context) (books, rows int, err error) {
	if err = ti.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM book_sessions`).Scan(&books); err != nil {
		return 0, 0, fmt.Errorf("BookCount: %w", err)
	}
	if err = ti.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM book_rows`).Scan(&rows); err != nil {
		return 0, 0, fmt.Errorf("BookCount: %w", err)
	}
	return books, rows, nil
}
