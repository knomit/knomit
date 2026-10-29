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

// SweepBookFiles garbage-collects the book directory (the periodic
// sweeper; at Open the directory is recreated empty). Under booksMu, the
// lock OpenBookFile registers its holders under, so a file a call is reading
// or writing right now is never deleted (it is Skipped and swept later):
//   - a book file whose cursor row is gone (reaped, replaced, crashed) is
//     an orphan;
//   - a book whose cursor was last used before Now - IdleTTL - Grace is
//     expired: its row and file go;
//   - a SQLite sidecar (-journal, -wal, -shm) of no live book, and any other
//     stray file older than Grace, goes;
//   - then, while the directory is over MaxBytes, the book idle longest goes
//     first, row and file.
//
// A book removed here reads as an expired cursor (the 404), never an error:
// its row is gone with it, and a row whose file is missing reads expired too.
func (ti *ToolIndex) SweepBookFiles(ctx context.Context, p BookSweepPolicy) (BookSweepStats, error) {
	var st BookSweepStats
	if ti.booksDir == "" {
		return st, nil
	}
	ti.booksMu.Lock()
	defer ti.booksMu.Unlock()
	entries, err := os.ReadDir(ti.booksDir)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	type book struct {
		id       string
		lastUsed time.Time
		size     int64
	}
	var live []book
	cutoff := p.Now.Add(-p.IdleTTL - p.Grace)
	for _, e := range entries {
		name := e.Name()
		info, err := e.Info()
		if err != nil {
			continue
		}
		id, isBook := strings.CutSuffix(name, bookFileSuffix)
		if !isBook {
			base := name
			for _, s := range []string{"-journal", "-wal", "-shm"} {
				base = strings.TrimSuffix(base, s)
			}
			owner := strings.TrimSuffix(base, bookFileSuffix)
			if base != name && ti.openBooks[owner] > 0 {
				st.Skipped++
				continue
			}
			if _, err := os.Stat(filepath.Join(ti.booksDir, base)); base != name && err == nil {
				continue // the sidecar of a book file that still exists
			}
			if base == name && p.Now.Sub(info.ModTime()) < p.Grace {
				continue // a stray file being created right now
			}
			_ = os.Remove(filepath.Join(ti.booksDir, name))
			st.Sidecars++
			continue
		}
		if ti.openBooks[id] > 0 {
			st.Skipped++
			st.Bytes += info.Size()
			continue
		}
		var lastUsed string
		err = ti.db.QueryRowContext(ctx,
			`SELECT ts.last_used_at FROM book_sessions b JOIN tool_sessions ts ON ts.id = b.session_id WHERE b.session_id = ?`, id,
		).Scan(&lastUsed)
		if errors.Is(err, sql.ErrNoRows) {
			removeBookFile(ti.bookPath(id))
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
		live = append(live, book{id: id, lastUsed: used, size: info.Size()})
		st.Bytes += info.Size()
	}
	if p.MaxBytes > 0 && st.Bytes > p.MaxBytes {
		sort.Slice(live, func(i, j int) bool { return live[i].lastUsed.Before(live[j].lastUsed) })
		for _, b := range live {
			if st.Bytes <= p.MaxBytes {
				break
			}
			if err := ti.dropBookLocked(ctx, b.id); err != nil {
				return st, err
			}
			st.Bytes -= b.size
			st.Evicted++
		}
	}
	return st, nil
}

// dropBookLocked deletes a book's cursor row and its file; booksMu is held.
func (ti *ToolIndex) dropBookLocked(ctx context.Context, id string) error {
	if err := ti.bookWrite(ctx, "SweepBookFiles", func(c *sql.Conn) error {
		_, err := c.ExecContext(ctx, `DELETE FROM tool_sessions WHERE id = ?`, id)
		return err
	}); err != nil {
		return err
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
	// MaxBookDirBytes caps one session store's book directory: about 15
	// books at the row cap (MaxBookRows, ~65 MiB each), far more than one
	// owner per endpoint needs.
	MaxBookDirBytes int64 = 1 << 30
)
