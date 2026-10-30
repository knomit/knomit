package sessions

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog/log"
)

// HYBRID FACTSET STORAGE (user direction, 2026-10-29: "use the file only if the
// rendering requires it ... serve it from memory directly"). A factset's
// structure (results, rows, visited set, anchors, cross-KB tips) starts in
// MEMORY, keyed to its cursor in its ToolIndex's registry. It moves to its
// own SQLite file (FactSetFile, under <session DB>.factsets/) only when it must:
//   - its own structure would pass FactSetSpillBytes, or
//   - all in-memory factsets of the process together would pass the memory
//     budget (SetFactSetMemoryBudget; session.factset_memory_mib): then the
//     LARGEST in-memory factset is spilled, never refused.
//
// A spill is one write of everything that exists, after which the factset
// appends to its file. The lifecycle is the same either way: replacement,
// DeleteFactSet and the sweeper's expiry drop the factset from the registry (its
// memory is freed, its file closed and deleted); a restart loses every factset,
// and their cursors read as expired. A factset a call holds is never freed
// under it: replacement, DeleteFactSet and CloseFactSets retire it and its last
// release frees it, and the sweeper skips it (ToolIndex.retireLocked).
//
// WHAT THE BUDGET BOUNDS. The accounted size (rowBytes, visitBytes, the
// anchors) is a factset's structure AT REST; the heap of resting in-memory
// factsets measured about 1.16x the accounted bytes, so the budget bounds
// resting factsets. It does NOT bound a walk's working memory (the reader and
// its version caches of up to 20,000 entries each, the unit's document),
// measured at 2-3x the accounted size of what the walk appends: that is
// bounded by materialize.MaxConcurrentFactSetBuilds, the cap on calls walking
// at once.
//
// One caller per cursor still holds; each factset's own mutex guards its data,
// and the registry is guarded by factsetsMu. No code path holds two factsets'
// mutexes at once (the budget is enforced after an append releases its factset).

// FactSetSpillBytes is the per-factset threshold: a factset whose in-memory structure
// would pass it is written to its file. 8 MiB is about 7,000 sections
// (~1.1 KiB each as accounted, measured on a real knowledge base): the
// largest real factset measured (every result of a 1,000-fact KB, 500 units,
// 1,346 sections) holds 1.5 MiB and stays in memory five times over, while
// synthetic factsets at the row cap (100,000 sections) go to disk.
const FactSetSpillBytes int64 = 8 << 20

// DefaultFactSetMemoryBudget is the process-wide budget for in-memory factset
// structure when session.factset_memory_mib is unset: 64 MiB, eight factsets at
// the spill threshold, a small share of the heap a server already runs.
const DefaultFactSetMemoryBudget int64 = 64 << 20

var (
	factsetMemBudget atomic.Int64
	factsetMemUsed   atomic.Int64
	// allFactSets is every in-memory factset of the process, for the budget.
	allFactSetsMu sync.Mutex
	allFactSets   = map[*factsetStore]struct{}{}
)

func init() { factsetMemBudget.Store(DefaultFactSetMemoryBudget) }

// SetFactSetMemoryBudget sets the process-wide in-memory factset budget in bytes
// (non-positive restores the default).
func SetFactSetMemoryBudget(bytes int64) {
	if bytes <= 0 {
		bytes = DefaultFactSetMemoryBudget
	}
	factsetMemBudget.Store(bytes)
}

// FactSetMemoryUsed is the bytes all in-memory factsets of the process hold.
func FactSetMemoryUsed() int64 { return factsetMemUsed.Load() }

// memData is a factset's structure in memory.
type memData struct {
	results   []FactSetResult
	rows      []FactSetRow // rows[i].Seq == i+1
	unitStart map[int]int
	visited   map[string]FactSetVisit
	anchors   map[string]bool
	tips      map[string]string
	reserved  map[string]FactSetReserve
}

// factsetStore is one live factset: in memory until spilled, then its file.
type factsetStore struct {
	id   string
	path string
	mu   sync.Mutex
	mem  *memData     // nil once spilled
	file *FactSetFile // set once spilled
	size atomic.Int64 // accounted in-memory bytes (0 once spilled)
	// holders is how many calls hold the factset open; retired says the factset
	// is out of service (replaced, deleted, store closing) while still held,
	// so the last release frees it. Both under factsetsMu.
	holders int
	retired bool
}

func rowBytes(r FactSetRow) int64 { return int64(len(r.State)+len(r.Path)) + 64 }
func visitBytes(v FactSetVisit) int64 {
	return int64(len(v.Path)+len(v.Number)+len(v.Anchor)+len(v.Commit)+len(v.Blob)) + 96
}

func reserveBytes(r FactSetReserve) int64 {
	return int64(len(r.Path)+len(r.Number)+2*len(r.Anchor)+len(r.Commit)+len(r.Blob)) + 128
}

func newMemFactSet(id, path string, results []FactSetResult, reserved []FactSetReserve) *factsetStore {
	m := &memData{results: results, unitStart: map[int]int{}, visited: map[string]FactSetVisit{},
		anchors: map[string]bool{}, tips: map[string]string{}, reserved: make(map[string]FactSetReserve, len(reserved))}
	bs := &factsetStore{id: id, path: path, mem: m}
	var n int64
	for _, r := range results {
		m.anchors[r.Anchor] = true
		n += int64(len(r.Path)+2*len(r.Anchor)+len(r.Title)) + 64
	}
	// The reserved set is part of the structure: it is counted like the
	// rest (the per-factset spill threshold and the process budget).
	for _, r := range reserved {
		m.reserved[r.Path] = r
		m.anchors[r.Anchor] = true
		n += reserveBytes(r)
	}
	bs.grow(n)
	allFactSetsMu.Lock()
	allFactSets[bs] = struct{}{}
	allFactSetsMu.Unlock()
	return bs
}

func (bs *factsetStore) grow(n int64) {
	bs.size.Add(n)
	factsetMemUsed.Add(n)
}

// spillLocked writes the whole in-memory structure to the factset's file and
// continues there; bs.mu is held.
func (bs *factsetStore) spillLocked(ctx context.Context) error {
	if bs.mem == nil {
		return nil
	}
	if err := createFactSetFile(ctx, bs.path, bs.mem.results, bs.mem.reserved); err != nil {
		return err
	}
	f, ok, err := openFactSetFile(bs.path)
	if err != nil || !ok {
		removeFactSetFile(bs.path)
		return fmt.Errorf("spill factset: open: %v", err)
	}
	m := bs.mem
	visits := make([]FactSetVisit, 0, len(m.visited))
	for _, v := range m.visited {
		visits = append(visits, v)
	}
	anchors := make([]string, 0, len(m.anchors))
	for a := range m.anchors {
		anchors = append(anchors, a)
	}
	if err := f.load(ctx, m.rows, visits, anchors, m.tips); err != nil {
		_ = f.Close()
		removeFactSetFile(bs.path)
		return fmt.Errorf("spill factset: %w", err)
	}
	bs.file, bs.mem = f, nil
	factsetMemUsed.Add(-bs.size.Swap(0))
	allFactSetsMu.Lock()
	delete(allFactSets, bs)
	allFactSetsMu.Unlock()
	return nil
}

// drop frees the factset: its memory, or its file (closed and deleted).
func (bs *factsetStore) drop() {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.mem != nil {
		factsetMemUsed.Add(-bs.size.Swap(0))
		bs.mem = nil
		allFactSetsMu.Lock()
		delete(allFactSets, bs)
		allFactSetsMu.Unlock()
	}
	if bs.file != nil {
		_ = bs.file.Close()
		bs.file = nil
	}
	removeFactSetFile(bs.path)
}

// spilled reports whether the factset lives in its file.
func (bs *factsetStore) spilled() bool {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	return bs.file != nil
}

// enforceFactSetBudget spills the largest in-memory factsets until the process
// is within its budget. It holds at most one factset's mutex at a time.
func enforceFactSetBudget(ctx context.Context) error {
	for factsetMemUsed.Load() > factsetMemBudget.Load() {
		allFactSetsMu.Lock()
		var largest *factsetStore
		var size int64
		for bs := range allFactSets {
			if s := bs.size.Load(); s > size {
				largest, size = bs, s
			}
		}
		allFactSetsMu.Unlock()
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

// enforceFactSetBudgetLogged enforces the process budget after this call grew
// it. The factset it spills may be ANOTHER caller's, so a failed spill is
// logged, never returned: this call's own write already succeeded, and the
// budget is enforced again on the next growth anywhere.
func enforceFactSetBudgetLogged(ctx context.Context) {
	if err := enforceFactSetBudget(ctx); err != nil {
		log.Warn().Err(err).Msg("factset memory budget: spilling the largest in-memory factset failed")
	}
}

// FactSet is one call's handle on a live factset (in memory or in its file).
// Close releases it; while held, no path frees the factset (retireLocked).
type FactSet struct {
	bs      *factsetStore
	release func()
}

// Close releases the handle.
func (b *FactSet) Close() error {
	if b.release != nil {
		b.release()
		b.release = nil
	}
	return nil
}

// Spilled reports whether the factset lives in its file.
func (b *FactSet) Spilled() bool { return b.bs.spilled() }

// Results is every frozen search result, by unit.
func (b *FactSet) Results(ctx context.Context) ([]FactSetResult, error) {
	b.bs.mu.Lock()
	defer b.bs.mu.Unlock()
	if f := b.bs.file; f != nil {
		return f.Results(ctx)
	}
	return append([]FactSetResult(nil), b.bs.mem.results...), nil
}

// Walked is how many units are walked and how many rows they hold.
func (b *FactSet) Walked(ctx context.Context) (units, rows int, err error) {
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
func (b *FactSet) Tips(ctx context.Context) (map[string]string, error) {
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
func (b *FactSet) Visited(ctx context.Context, path string) (FactSetVisit, bool, error) {
	b.bs.mu.Lock()
	defer b.bs.mu.Unlock()
	if f := b.bs.file; f != nil {
		return f.Visited(ctx, path)
	}
	v, ok := b.bs.mem.visited[path]
	return v, ok, nil
}

// Reserved is the reservation of path (a unit root's direct reference
// reserved for that unit before any unit was walked), if any.
func (b *FactSet) Reserved(ctx context.Context, path string) (FactSetReserve, bool, error) {
	b.bs.mu.Lock()
	defer b.bs.mu.Unlock()
	if f := b.bs.file; f != nil {
		return f.Reserved(ctx, path)
	}
	r, ok := b.bs.mem.reserved[path]
	return r, ok, nil
}

// AnchorTaken reports whether an anchor is used (or reserved) in the factset.
func (b *FactSet) AnchorTaken(ctx context.Context, anchor string) (bool, error) {
	b.bs.mu.Lock()
	defer b.bs.mu.Unlock()
	if f := b.bs.file; f != nil {
		return f.AnchorTaken(ctx, anchor)
	}
	return b.bs.mem.anchors[anchor], nil
}

// AnchorsTaken is AnchorTaken for many anchors at once (one query per chunk
// once the factset lives in its file): the taken ones are in the map.
func (b *FactSet) AnchorsTaken(ctx context.Context, anchors []string) (map[string]bool, error) {
	b.bs.mu.Lock()
	defer b.bs.mu.Unlock()
	out := make(map[string]bool, len(anchors))
	if f := b.bs.file; f != nil {
		return out, f.inSet(ctx, `SELECT anchor FROM anchors WHERE anchor IN (%s)`, anchors, func(rows *sql.Rows) error {
			var a string
			if err := rows.Scan(&a); err != nil {
				return err
			}
			out[a] = true
			return nil
		})
	}
	for _, a := range anchors {
		if b.bs.mem.anchors[a] {
			out[a] = true
		}
	}
	return out, nil
}

// ReservedOf is Reserved for many paths at once: the reserved ones are in
// the map.
func (b *FactSet) ReservedOf(ctx context.Context, paths []string) (map[string]FactSetReserve, error) {
	b.bs.mu.Lock()
	defer b.bs.mu.Unlock()
	out := make(map[string]FactSetReserve, len(paths))
	if f := b.bs.file; f != nil {
		return out, f.inSet(ctx, `SELECT path, unit, number, anchor, commit_hash, blob FROM reserved WHERE path IN (%s)`, paths, func(rows *sql.Rows) error {
			var r FactSetReserve
			if err := rows.Scan(&r.Path, &r.Unit, &r.Number, &r.Anchor, &r.Commit, &r.Blob); err != nil {
				return err
			}
			out[r.Path] = r
			return nil
		})
	}
	for _, p := range paths {
		if r, ok := b.bs.mem.reserved[p]; ok {
			out[p] = r
		}
	}
	return out, nil
}

// AppendUnit appends one walked unit (refused with ErrFactSetUnitTaken unless
// it is the next one). A factset whose structure would pass FactSetSpillBytes is
// spilled first; after the append, the process budget is enforced.
func (b *FactSet) AppendUnit(ctx context.Context, unit int, rows []FactSetRow, visits []FactSetVisit, anchors []string, tips map[string]string) error {
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
				return ErrFactSetUnitTaken
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
			if bs.size.Load()+delta <= factsetSpillBytes.Load() {
				for _, r := range rows {
					if r.Seq != len(m.rows)+1 {
						return fmt.Errorf("factset append: row seq %d out of order", r.Seq)
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
	enforceFactSetBudgetLogged(ctx)
	return nil
}

// Rows returns up to limit rows from seq from, in factset order.
func (b *FactSet) Rows(ctx context.Context, from, limit int) ([]FactSetRow, error) {
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
	return append([]FactSetRow(nil), rows[from-1:end]...), nil
}

// UnitStart is the seq of unit's first row, 0 when the unit is not walked.
func (b *FactSet) UnitStart(ctx context.Context, unit int) (int, error) {
	b.bs.mu.Lock()
	defer b.bs.mu.Unlock()
	if f := b.bs.file; f != nil {
		return f.UnitStart(ctx, unit)
	}
	return b.bs.mem.unitStart[unit], nil
}

// factsetSpillBytes is FactSetSpillBytes; a test moves it with
// SetFactSetSpillBytesForTest.
var factsetSpillBytes atomic.Int64

func init() { factsetSpillBytes.Store(FactSetSpillBytes) }

// SetFactSetSpillBytesForTest sets the per-factset spill threshold and returns its
// restore. Never called in production.
func SetFactSetSpillBytesForTest(n int64) (restore func()) {
	old := factsetSpillBytes.Swap(n)
	return func() { factsetSpillBytes.Store(old) }
}

// SetFactSetMemoryBudgetForTest sets the process budget and returns its
// restore. Never called in production.
func SetFactSetMemoryBudgetForTest(n int64) (restore func()) {
	old := factsetMemBudget.Swap(n)
	return func() { factsetMemBudget.Store(old) }
}
