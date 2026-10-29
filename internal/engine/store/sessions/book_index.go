package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	sqlite3 "github.com/mattn/go-sqlite3"

	"knomit/internal/engine/store/model"
)

// Book cursors: the session-DB half of "materialize a search as a book". A
// book is a tool_sessions row (so ownership, binding pin, read set and the
// idle reaper apply unchanged) plus a book_sessions row (its envelope and
// reading position). Its structure lives in a FILE of its own (book_file.go),
// written only by the book's one cursor holder, so a book never contends with
// another book or another cursor on the shared session DB.
//
// WRITES to the shared session DB are small (one session row, one position
// update per page) and each is one BEGIN IMMEDIATE transaction on a
// dedicated connection (bookWrite), the createPipelineSession / DequeuePaths
// pattern: a read-then-write never upgrades a deferred lock (invariant
// a8e8e8e4). bookWrite is the ONE seam every book write to the shared DB goes
// through, so an in-process writer gate can be added there without touching a
// caller; it records each write's lock wait (BookLockWaits). A lock wait that
// times out is ErrBusy, which an edge answers 503 with Retry-After.

// BookToolPrefix prefixes the tool_sessions.tool value of a book cursor:
// "materialize-book:<limit>".
const BookToolPrefix = "materialize-book:"

// ErrBusy is a session-DB write that could not take the write lock within
// the busy timeout. Nothing was written; the caller may retry.
var ErrBusy = model.ErrSessionBusy

// BookInfo is a book's envelope and cursor position.
type BookInfo struct {
	Env     string
	Units   int
	NextSeq int
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

// BookLockWaits reports how many book writes to the shared session DB ran
// on this index, their total and their longest wait for its write lock.
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

// CreateBook starts a book cursor for owner. It deletes owner's other book
// cursors and frees their books (one active book per endpoint + principal: a
// new first call replaces the old one), creates the session and its
// envelope, and registers the new book IN MEMORY with its results (the
// frozen search results, unit 1..len, with the anchors reserved for their
// roots). No file is created until the book must spill. On any failure
// nothing is left behind.
func (ti *ToolIndex) CreateBook(ctx context.Context, tool, branch, binding, readSet, owner, env string, results []BookResult) (string, error) {
	if !strings.HasPrefix(tool, BookToolPrefix) {
		return "", fmt.Errorf("CreateBook: tool %q is not a book tool", tool)
	}
	if ti.booksDir == "" {
		return "", errors.New("CreateBook: this session store has no book directory")
	}
	id := uuid.New().String()
	var replaced []string
	now := time.Now().UTC().Format(time.RFC3339)
	err := ti.bookWrite(ctx, "CreateBook", func(c *sql.Conn) error {
		rows, err := c.QueryContext(ctx, `SELECT id FROM tool_sessions WHERE owner = ? AND tool LIKE ?`, owner, BookToolPrefix+"%")
		if err != nil {
			return fmt.Errorf("CreateBook replace: %w", err)
		}
		for rows.Next() {
			var old string
			if err := rows.Scan(&old); err != nil {
				rows.Close()
				return err
			}
			replaced = append(replaced, old)
		}
		rows.Close()
		if _, err := c.ExecContext(ctx,
			`DELETE FROM tool_sessions WHERE owner = ? AND tool LIKE ?`, owner, BookToolPrefix+"%"); err != nil {
			return fmt.Errorf("CreateBook replace: %w", err)
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO tool_sessions(id, tool, branch, path_prefix, binding, read_set, owner, last_commit, status, created_at, updated_at, last_used_at)
			 VALUES (?, ?, ?, '', ?, ?, ?, '', 'active', ?, ?, ?)`,
			id, tool, branch, binding, readSet, owner, now, now, now); err != nil {
			return fmt.Errorf("CreateBook session: %w", err)
		}
		if _, err := c.ExecContext(ctx,
			`INSERT INTO book_sessions(session_id, env, units) VALUES (?, ?, ?)`, id, env, len(results)); err != nil {
			return fmt.Errorf("CreateBook book: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	bs := newMemBook(id, ti.bookPath(id), results)
	ti.booksMu.Lock()
	ti.books[id] = bs
	var old []*bookStore
	for _, o := range replaced {
		if b := ti.books[o]; b != nil {
			old = append(old, b)
			delete(ti.books, o)
		}
	}
	ti.booksMu.Unlock()
	for _, b := range old {
		b.drop()
	}
	for _, o := range replaced {
		removeBookFile(ti.bookPath(o))
	}
	return id, enforceBookBudget(ctx)
}

// DeleteBook removes a book cursor and frees its book (memory or file).
func (ti *ToolIndex) DeleteBook(ctx context.Context, id string) error {
	err := ti.bookWrite(ctx, "DeleteBook", func(c *sql.Conn) error {
		_, err := c.ExecContext(ctx, `DELETE FROM tool_sessions WHERE id = ?`, id)
		return err
	})
	ti.forgetBook(id)
	return err
}

// forgetBook drops book id from the registry and frees it.
func (ti *ToolIndex) forgetBook(id string) {
	ti.booksMu.Lock()
	bs := ti.books[id]
	delete(ti.books, id)
	ti.booksMu.Unlock()
	if bs != nil {
		bs.drop()
	}
	removeBookFile(ti.bookPath(id))
}

// GetBook returns a book's envelope and position, nil when id is not a book.
func (ti *ToolIndex) GetBook(ctx context.Context, id string) (*BookInfo, error) {
	var b BookInfo
	err := ti.db.QueryRowContext(ctx,
		`SELECT env, units, next_seq FROM book_sessions WHERE session_id = ?`, id,
	).Scan(&b.Env, &b.Units, &b.NextSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetBook: %w", err)
	}
	return &b, nil
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
			`UPDATE book_sessions SET next_seq = ? WHERE session_id = ? AND next_seq = ?`, to, id, from)
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

// OpenBook holds book id for this call. ok is false when the book is gone
// (replaced, expired, evicted, or lost with a restart). While held, the
// sweeper never drops it: the hold is registered under booksMu, the lock
// the sweeper drops books under, and Close releases it.
func (ti *ToolIndex) OpenBook(id string) (*Book, bool) {
	ti.booksMu.Lock()
	defer ti.booksMu.Unlock()
	bs := ti.books[id]
	if bs == nil {
		return nil, false
	}
	bs.holders++
	return &Book{bs: bs, release: func() {
		ti.booksMu.Lock()
		bs.holders--
		ti.booksMu.Unlock()
	}}, true
}

// BookFiles reports the book files on disk and their total size in bytes.
func (ti *ToolIndex) BookFiles() (files int, bytes int64, err error) {
	if ti.booksDir == "" {
		return 0, 0, nil
	}
	entries, err := os.ReadDir(ti.booksDir)
	if err != nil {
		return 0, 0, err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), bookFileSuffix) {
			continue
		}
		files++
		if fi, err := e.Info(); err == nil {
			bytes += fi.Size()
		}
	}
	return files, bytes, nil
}

// BookCount reports how many book cursors the session DB holds.
func (ti *ToolIndex) BookCount(ctx context.Context) (int, error) {
	var n int
	if err := ti.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM book_sessions`).Scan(&n); err != nil {
		return 0, fmt.Errorf("BookCount: %w", err)
	}
	return n, nil
}

// LiveBooks reports the books this index holds: in memory and spilled.
func (ti *ToolIndex) LiveBooks() (inMemory, spilled int) {
	ti.booksMu.Lock()
	defer ti.booksMu.Unlock()
	for _, bs := range ti.books {
		if bs.spilled() {
			spilled++
		} else {
			inMemory++
		}
	}
	return inMemory, spilled
}

func (ti *ToolIndex) bookPath(id string) string {
	return filepath.Join(ti.booksDir, filepath.Base(id)+bookFileSuffix)
}

// OpenBookFiles is how many SQLite handles spilled books hold right now
// (a spilled book keeps its file open until it is dropped).
func (ti *ToolIndex) OpenBookFiles() int {
	_, spilled := ti.LiveBooks()
	return spilled
}

// BookFilePath is the file of book id (for tests and diagnostics).
func (ti *ToolIndex) BookFilePath(id string) string { return ti.bookPath(id) }

// CloseBooks frees every live book of this index (the store is closing):
// their memory leaves the process budget and their files are closed.
func (ti *ToolIndex) CloseBooks() {
	ti.booksMu.Lock()
	books := ti.books
	ti.books = map[string]*bookStore{}
	ti.booksMu.Unlock()
	for _, bs := range books {
		bs.drop()
	}
}
