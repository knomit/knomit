package sessions

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BookSweepPolicy is what one book-file sweep enforces.
type BookSweepPolicy struct {
	// Now is the sweep's clock.
	Now time.Time
	// IdleTTL is the tool-cursor idle expiry (session.tool_idle_ttl): a
	// book whose cursor was last used longer ago than IdleTTL + Grace is
	// deleted, row and file, even if the reaper has not run yet.
	IdleTTL time.Duration
	// Grace covers a page in flight and clock skew between the row's
	// heartbeat and the file's mtime; a stray file younger than it is left.
	Grace time.Duration
	// MaxBytes caps the directory: past it, the books idle longest are
	// evicted first (row and file) until it fits. 0 is no cap.
	MaxBytes int64
}

// BookSweepStats counts what one sweep did.
type BookSweepStats struct {
	Orphans  int // a file whose cursor row is gone
	Expired  int // a file (and row) idle past IdleTTL + Grace
	Sidecars int // a -journal/-wal/-shm/stray file with no live book
	Evicted  int // removed to bring the directory under MaxBytes
	Skipped  int // held open by a call right now
	Bytes    int64
}

// SweepBookFiles garbage-collects the live books (in memory and in files)
// and the book directory (the periodic sweeper; at Open the directory is
// recreated empty). Under booksMu, the lock OpenBook registers holders
// under, so a book a call holds right now is never dropped (Skipped, swept
// on a later tick):
//   - a live book whose cursor row is gone (reaped, replaced) is an orphan:
//     its memory is freed, or its file deleted;
//   - a book whose cursor was last used before Now - IdleTTL - Grace is
//     expired: its row goes, and its memory or file with it;
//   - in the directory, a book file of no live book, a SQLite sidecar
//     (-journal, -wal, -shm) of no existing book file, and any other stray
//     file older than Grace, goes;
//   - then, while the spilled books' files are over MaxBytes, the book idle
//     longest goes first, row and file.
//
// A book removed here reads as an expired cursor (the 404), never an error.
func (ti *ToolIndex) SweepBookFiles(ctx context.Context, p BookSweepPolicy) (BookSweepStats, error) {
	var st BookSweepStats
	if ti.booksDir == "" {
		return st, nil
	}
	ti.booksMu.Lock()
	defer ti.booksMu.Unlock()
	type file struct {
		id       string
		lastUsed time.Time
		size     int64
	}
	var spilled []file
	cutoff := p.Now.Add(-p.IdleTTL - p.Grace)
	for id, bs := range ti.books {
		if bs.holders > 0 {
			st.Skipped++
			continue
		}
		var lastUsed string
		err := ti.db.QueryRowContext(ctx,
			`SELECT ts.last_used_at FROM book_sessions b JOIN tool_sessions ts ON ts.id = b.session_id WHERE b.session_id = ?`, id,
		).Scan(&lastUsed)
		if errors.Is(err, sql.ErrNoRows) {
			delete(ti.books, id)
			bs.drop()
			st.Orphans++
			continue
		}
		if err != nil {
			return st, fmt.Errorf("sweep books: %w", err)
		}
		used, perr := time.Parse(time.RFC3339, lastUsed)
		if perr != nil || used.Before(cutoff) {
			if err := ti.dropBookLocked(ctx, id); err != nil {
				return st, err
			}
			st.Expired++
			continue
		}
		if bs.spilled() {
			if fi, err := os.Stat(bs.path); err == nil {
				spilled = append(spilled, file{id: id, lastUsed: used, size: fi.Size()})
				st.Bytes += fi.Size()
			}
		}
	}
	entries, err := os.ReadDir(ti.booksDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return st, err
	}
	for _, e := range entries {
		name := e.Name()
		info, err := e.Info()
		if err != nil {
			continue
		}
		if id, isBook := strings.CutSuffix(name, bookFileSuffix); isBook {
			if ti.books[id] == nil {
				removeBookFile(filepath.Join(ti.booksDir, name))
				st.Orphans++
			}
			continue
		}
		base := name
		for _, s := range []string{"-journal", "-wal", "-shm"} {
			base = strings.TrimSuffix(base, s)
		}
		if base != name {
			if _, err := os.Stat(filepath.Join(ti.booksDir, base)); err == nil {
				continue // the sidecar of a book file that still exists
			}
		} else if p.Now.Sub(info.ModTime()) < p.Grace {
			continue // a stray file being created right now
		}
		_ = os.Remove(filepath.Join(ti.booksDir, name))
		st.Sidecars++
	}
	if p.MaxBytes > 0 && st.Bytes > p.MaxBytes {
		sort.Slice(spilled, func(i, j int) bool { return spilled[i].lastUsed.Before(spilled[j].lastUsed) })
		for _, f := range spilled {
			if st.Bytes <= p.MaxBytes {
				break
			}
			if err := ti.dropBookLocked(ctx, f.id); err != nil {
				return st, err
			}
			st.Bytes -= f.size
			st.Evicted++
		}
	}
	return st, nil
}

// dropBookLocked deletes a book's cursor row and frees the book; booksMu
// is held.
func (ti *ToolIndex) dropBookLocked(ctx context.Context, id string) error {
	if err := ti.bookWrite(ctx, "SweepBookFiles", func(c *sql.Conn) error {
		_, err := c.ExecContext(ctx, `DELETE FROM tool_sessions WHERE id = ?`, id)
		return err
	}); err != nil {
		return err
	}
	if bs := ti.books[id]; bs != nil {
		delete(ti.books, id)
		bs.drop()
	}
	removeBookFile(ti.bookPath(id))
	return nil
}

// Sweep defaults, applied by store.Service.ReapIdleSessions on every reaper
// tick.
const (
	// BookSweepGrace is added to the idle expiry before a book is swept,
	// and is how old a stray file must be: longer than any page takes, so
	// a page in flight or a file being created is never caught.
	BookSweepGrace = 5 * time.Minute
	// MaxBookDirBytes caps one session store's book directory: about 19
	// books at the row cap (MaxBookRows, ~53 MiB each as measured), far
	// more than one owner per endpoint needs. It is enforced on the
	// sweeper's tick, not on every write.
	MaxBookDirBytes int64 = 1 << 30
)
