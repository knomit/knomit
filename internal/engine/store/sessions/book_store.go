package sessions

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog/log"
)

// HYBRID BOOK STORAGE (user direction, 2026-10-29: "use the file only if the
// rendering requires it ... serve it from memory directly"). A book's
// structure (results, rows, visited set, anchors, cross-KB tips) starts in
// MEMORY, keyed to its cursor in its ToolIndex's registry. It moves to its
// own SQLite file (BookFile, under <session DB>.books/) only when it must:
//   - its own structure would pass BookSpillBytes, or
//   - all in-memory books of the process together would pass the memory
//     budget (SetBookMemoryBudget; session.book_memory_mib): then the
//     LARGEST in-memory book is spilled, never refused.
//
// A spill is one write of everything that exists, after which the book
// appends to its file. The lifecycle is the same either way: replacement,
// DeleteBook and the sweeper's expiry drop the book from the registry (its
// memory is freed, its file closed and deleted); a restart loses every book,
// and their cursors read as expired. A book a call holds is never freed
// under it: replacement, DeleteBook and CloseBooks retire it and its last
// release frees it, and the sweeper skips it (ToolIndex.retireLocked).
//
// WHAT THE BUDGET BOUNDS. The accounted size (rowBytes, visitBytes, the
// anchors) is a book's structure AT REST; the heap of resting in-memory
// books measured about 1.16x the accounted bytes, so the budget bounds
// resting books. It does NOT bound a walk's working memory (the reader and
// its version caches of up to 20,000 entries each, the unit's document),
// measured at 2-3x the accounted size of what the walk appends: that is
// bounded by materialize.MaxConcurrentBookBuilds, the cap on calls walking
// at once.
//
// One caller per cursor still holds; each book's own mutex guards its data,
// and the registry is guarded by booksMu. No code path holds two books'
// mutexes at once (the budget is enforced after an append releases its book).

// BookSpillBytes is the per-book threshold: a book whose in-memory structure
// would pass it is written to its file. 8 MiB is about 7,000 sections
// (~1.1 KiB each as accounted, measured on a real knowledge base): the
// largest real book measured (every result of a 1,000-fact KB, 500 units,
// 1,346 sections) holds 1.5 MiB and stays in memory five times over, while
// synthetic books at the row cap (100,000 sections) go to disk.
const BookSpillBytes int64 = 8 << 20

// DefaultBookMemoryBudget is the process-wide budget for in-memory book
// structure when session.book_memory_mib is unset: 64 MiB, eight books at
// the spill threshold, a small share of the heap a server already runs.
const DefaultBookMemoryBudget int64 = 64 << 20

var (
	bookMemBudget atomic.Int64
	bookMemUsed   atomic.Int64
	// allBooks is every in-memory book of the process, for the budget.
	allBooksMu sync.Mutex
	allBooks   = map[*bookStore]struct{}{}
)

func init() { bookMemBudget.Store(DefaultBookMemoryBudget) }

// SetBookMemoryBudget sets the process-wide in-memory book budget in bytes
// (non-positive restores the default).
func SetBookMemoryBudget(bytes int64) {
	if bytes <= 0 {
		bytes = DefaultBookMemoryBudget
	}
	bookMemBudget.Store(bytes)
}

// BookMemoryUsed is the bytes all in-memory books of the process hold.
func BookMemoryUsed() int64 { return bookMemUsed.Load() }

// memData is a book's structure in memory.
type memData struct {
	results   []BookResult
	rows      []BookRow // rows[i].Seq == i+1
	unitStart map[int]int
	visited   map[string]BookVisit
	anchors   map[string]bool
	tips      map[string]string
}

// bookStore is one live book: in memory until spilled, then its file.
type bookStore struct {
	id   string
	path string
	mu   sync.Mutex
	mem  *memData     // nil once spilled
	file *BookFile    // set once spilled
	size atomic.Int64 // accounted in-memory bytes (0 once spilled)
	// holders is how many calls hold the book open; retired says the book
	// is out of service (replaced, deleted, store closing) while still held,
	// so the last release frees it. Both under booksMu.
	holders int
	retired bool
}

func rowBytes(r BookRow) int64 { return int64(len(r.State)+len(r.Path)) + 64 }
func visitBytes(v BookVisit) int64 {
	return int64(len(v.Path)+len(v.Number)+len(v.Anchor)+len(v.Commit)+len(v.Blob)) + 96
}

func newMemBook(id, path string, results []BookResult) *bookStore {
	m := &memData{results: results, unitStart: map[int]int{}, visited: map[string]BookVisit{},
		anchors: map[string]bool{}, tips: map[string]string{}}
	bs := &bookStore{id: id, path: path, mem: m}
	var n int64
	for _, r := range results {
		m.anchors[r.Anchor] = true
		n += int64(len(r.Path)+2*len(r.Anchor)+len(r.Title)) + 64
	}
	bs.grow(n)
	allBooksMu.Lock()
	allBooks[bs] = struct{}{}
	allBooksMu.Unlock()
	return bs
}

func (bs *bookStore) grow(n int64) {
	bs.size.Add(n)
	bookMemUsed.Add(n)
}

// spillLocked writes the whole in-memory structure to the book's file and
// continues there; bs.mu is held.
func (bs *bookStore) spillLocked(ctx context.Context) error {
	if bs.mem == nil {
		return nil
	}
	if err := createBookFile(ctx, bs.path, bs.mem.results); err != nil {
		return err
	}
	f, ok, err := openBookFile(bs.path)
	if err != nil || !ok {
		removeBookFile(bs.path)
		return fmt.Errorf("spill book: open: %v", err)
	}
	m := bs.mem
	visits := make([]BookVisit, 0, len(m.visited))
	for _, v := range m.visited {
		visits = append(visits, v)
	}
	anchors := make([]string, 0, len(m.anchors))
	for a := range m.anchors {
		anchors = append(anchors, a)
	}
	if err := f.load(ctx, m.rows, visits, anchors, m.tips); err != nil {
		_ = f.Close()
		removeBookFile(bs.path)
		return fmt.Errorf("spill book: %w", err)
	}
	bs.file, bs.mem = f, nil
	bookMemUsed.Add(-bs.size.Swap(0))
	allBooksMu.Lock()
	delete(allBooks, bs)
	allBooksMu.Unlock()
	return nil
}

// drop frees the book: its memory, or its file (closed and deleted).
func (bs *bookStore) drop() {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.mem != nil {
		bookMemUsed.Add(-bs.size.Swap(0))
		bs.mem = nil
		allBooksMu.Lock()
		delete(allBooks, bs)
		allBooksMu.Unlock()
	}
	if bs.file != nil {
		_ = bs.file.Close()
		bs.file = nil
	}
	removeBookFile(bs.path)
}

// spilled reports whether the book lives in its file.
func (bs *bookStore) spilled() bool {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return bs.file != nil
}

// enforceBookBudget spills the largest in-memory books until the process
// is within its budget. It holds at most one book's mutex at a time.
func enforceBookBudget(ctx context.Context) error {
	for bookMemUsed.Load() > bookMemBudget.Load() {
		allBooksMu.Lock()
		var largest *bookStore
		var size int64
		for bs := range allBooks {
			if s := bs.size.Load(); s > size {
				largest, size = bs, s
			}
		}
		allBooksMu.Unlock()
		if largest == nil {
			return nil
		}
		largest.mu.Lock()
		err := largest.spillLocked(ctx)
		largest.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// enforceBookBudgetLogged enforces the process budget after this call grew
// it. The book it spills may be ANOTHER caller's, so a failed spill is
// logged, never returned: this call's own write already succeeded, and the
// budget is enforced again on the next growth anywhere.
func enforceBookBudgetLogged(ctx context.Context) {
	if err := enforceBookBudget(ctx); err != nil {
		log.Warn().Err(err).Msg("book memory budget: spilling the largest in-memory book failed")
	}
}

// Book is one call's handle on a live book (in memory or in its file).
// Close releases it; while held, no path frees the book (retireLocked).
type Book struct {
	bs      *bookStore
	release func()
}

// Close releases the handle.
func (b *Book) Close() error {
	if b.release != nil {
		b.release()
		b.release = nil
	}
	return nil
}

// Spilled reports whether the book lives in its file.
func (b *Book) Spilled() bool { return b.bs.spilled() }

// Results is every frozen search result, by unit.
func (b *Book) Results(ctx context.Context) ([]BookResult, error) {
	b.bs.mu.Lock()
	defer b.bs.mu.Unlock()
	if f := b.bs.file; f != nil {
		return f.Results(ctx)
	}
	return append([]BookResult(nil), b.bs.mem.results...), nil
}

// Walked is how many units are walked and how many rows they hold.
func (b *Book) Walked(ctx context.Context) (units, rows int, err error) {
	b.bs.mu.Lock()
	defer b.bs.mu.Unlock()
	if f := b.bs.file; f != nil {
		return f.Walked(ctx)
	}
	m := b.bs.mem
	if len(m.rows) == 0 {
		return 0, 0, nil
	}
	return m.rows[len(m.rows)-1].Unit, len(m.rows), nil
}

// Tips is the frozen tip of every repo a unit entered cross-KB.
func (b *Book) Tips(ctx context.Context) (map[string]string, error) {
	b.bs.mu.Lock()
	defer b.bs.mu.Unlock()
	if f := b.bs.file; f != nil {
		return f.Tips(ctx)
	}
	out := make(map[string]string, len(b.bs.mem.tips))
	for k, v := range b.bs.mem.tips {
		out[k] = v
	}
	return out, nil
}

// Visited is the first appearance of path, if it was written.
func (b *Book) Visited(ctx context.Context, path string) (BookVisit, bool, error) {
	b.bs.mu.Lock()
	defer b.bs.mu.Unlock()
	if f := b.bs.file; f != nil {
		return f.Visited(ctx, path)
	}
	v, ok := b.bs.mem.visited[path]
	return v, ok, nil
}

// AnchorTaken reports whether an anchor is used (or reserved) in the book.
func (b *Book) AnchorTaken(ctx context.Context, anchor string) (bool, error) {
	b.bs.mu.Lock()
	defer b.bs.mu.Unlock()
	if f := b.bs.file; f != nil {
		return f.AnchorTaken(ctx, anchor)
	}
	return b.bs.mem.anchors[anchor], nil
}

// AppendUnit appends one walked unit (refused with ErrBookUnitTaken unless
// it is the next one). A book whose structure would pass BookSpillBytes is
// spilled first; after the append, the process budget is enforced.
func (b *Book) AppendUnit(ctx context.Context, unit int, rows []BookRow, visits []BookVisit, anchors []string, tips map[string]string) error {
	bs := b.bs
	bs.mu.Lock()
	err := func() error {
		if bs.mem != nil {
			m := bs.mem
			last := 0
			if len(m.rows) > 0 {
				last = m.rows[len(m.rows)-1].Unit
			}
			if last != unit-1 {
				return ErrBookUnitTaken
			}
			var delta int64
			for _, r := range rows {
				delta += rowBytes(r)
			}
			for _, v := range visits {
				delta += visitBytes(v)
			}
			for _, a := range anchors {
				delta += int64(len(a)) + 32
			}
			if bs.size.Load()+delta <= bookSpillBytes.Load() {
				for _, r := range rows {
					if r.Seq != len(m.rows)+1 {
						return fmt.Errorf("book append: row seq %d out of order", r.Seq)
					}
					if _, ok := m.unitStart[r.Unit]; !ok {
						m.unitStart[r.Unit] = r.Seq
					}
					m.rows = append(m.rows, r)
				}
				for _, v := range visits {
					m.visited[v.Path] = v
				}
				for _, a := range anchors {
					m.anchors[a] = true
				}
				for k, v := range tips {
					if _, ok := m.tips[k]; !ok {
						m.tips[k] = v
					}
				}
				bs.grow(delta)
				return nil
			}
			if err := bs.spillLocked(ctx); err != nil {
				return err
			}
		}
		return bs.file.AppendUnit(ctx, unit, rows, visits, anchors, tips)
	}()
	bs.mu.Unlock()
	if err != nil {
		return err
	}
	enforceBookBudgetLogged(ctx)
	return nil
}

// Rows returns up to limit rows from seq from, in book order.
func (b *Book) Rows(ctx context.Context, from, limit int) ([]BookRow, error) {
	b.bs.mu.Lock()
	defer b.bs.mu.Unlock()
	if f := b.bs.file; f != nil {
		return f.Rows(ctx, from, limit)
	}
	rows := b.bs.mem.rows
	if from < 1 {
		from = 1
	}
	if from > len(rows) {
		return nil, nil
	}
	end := min(len(rows), from-1+limit)
	return append([]BookRow(nil), rows[from-1:end]...), nil
}

// UnitStart is the seq of unit's first row, 0 when the unit is not walked.
func (b *Book) UnitStart(ctx context.Context, unit int) (int, error) {
	b.bs.mu.Lock()
	defer b.bs.mu.Unlock()
	if f := b.bs.file; f != nil {
		return f.UnitStart(ctx, unit)
	}
	return b.bs.mem.unitStart[unit], nil
}

// bookSpillBytes is BookSpillBytes; a test moves it with
// SetBookSpillBytesForTest.
var bookSpillBytes atomic.Int64

func init() { bookSpillBytes.Store(BookSpillBytes) }

// SetBookSpillBytesForTest sets the per-book spill threshold and returns its
// restore. Never called in production.
func SetBookSpillBytesForTest(n int64) (restore func()) {
	old := bookSpillBytes.Swap(n)
	return func() { bookSpillBytes.Store(old) }
}

// SetBookMemoryBudgetForTest sets the process budget and returns its
// restore. Never called in production.
func SetBookMemoryBudgetForTest(n int64) (restore func()) {
	old := bookMemBudget.Swap(n)
	return func() { bookMemBudget.Store(old) }
}
