package materialize

import (
	"container/list"
	"context"
	"sync"
	"sync/atomic"
	"time"

	"knomit/internal/engine/store/sessions"
	"knomit/internal/foundation/config"
)

// The FactSet cache: per process, keyed by FactSet id. It holds what a
// FactSet derives from its frozen row (the results and reserved tables, the
// planned PREFIX: what units 1..planned wrote and the unit that first wrote
// each fact, and the plans of a few recent units) under ONE budget across all
// FactSets (session.factset_memory_mib), least-recently-used idle entries
// evicted first. It is never stored: on a miss (eviction, restart, another
// node) the next call plans units 1..u again without bodies, and the bytes
// come out the same. A store's entries are dropped when it closes.
//
// The SAME budget bounds the working memory of plans in flight: a call that
// must plan reserves fsPlanReserve bytes first (fsReserve) and, over the
// budget, WAITS for memory (its context and FactSetStallLimit bound the
// wait) instead of being refused.

// DefaultFactSetCacheMiB is the cache budget when session.factset_memory_mib
// is 0 or unset.
const DefaultFactSetCacheMiB = 64

// keptPlans is how many unit plans an entry keeps (most recently used): a
// page spans at most HumanMaxLimit units, so two readers at two places of one
// FactSet each keep theirs. A unit not kept is re-planned alone from the
// prefix.
const keptPlans = 8

// fsEntry is one FactSet's cached state. Calls on one FactSet run
// concurrently: a unit within the planned prefix is read (or re-planned
// alone) by any number of calls at once; EXTENDING the prefix is done by the
// one call holding ext, the extension token, and a call waiting for an
// extension waits on its own context.
type fsEntry struct {
	id  string
	ti  *sessions.ToolIndex
	fr  *factsetFrozen
	st  fsState
	ext chan struct{} // the extension token: capacity 1

	pmu       sync.Mutex
	plans     map[int]*plannedUnit
	planOrder []int // least recently used first

	// size is the accounted bytes, elem the LRU element, users the calls
	// holding the entry (an entry in use is never evicted); cache lock.
	size  int64
	elem  *list.Element
	users int
}

func newFSEntry(id string, ti *sessions.ToolIndex, fr *factsetFrozen) *fsEntry {
	return &fsEntry{id: id, ti: ti, fr: fr, ext: make(chan struct{}, 1), plans: map[int]*plannedUnit{}}
}

// plan is unit u's kept plan, or nil.
func (e *fsEntry) plan(u int) *plannedUnit {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	pu, ok := e.plans[u]
	if ok {
		e.touchLocked(u)
	}
	return pu
}

// keepPlan keeps unit u's plan (plans are immutable), dropping the least
// recently used past keptPlans.
func (e *fsEntry) keepPlan(u int, pu *plannedUnit) {
	e.pmu.Lock()
	defer e.pmu.Unlock()
	if _, ok := e.plans[u]; ok {
		e.touchLocked(u)
		return
	}
	e.plans[u] = pu
	e.planOrder = append(e.planOrder, u)
	for len(e.planOrder) > keptPlans {
		delete(e.plans, e.planOrder[0])
		e.planOrder = e.planOrder[1:]
	}
}

func (e *fsEntry) touchLocked(u int) {
	for i, k := range e.planOrder {
		if k == u {
			copy(e.planOrder[i:], e.planOrder[i+1:])
			e.planOrder[len(e.planOrder)-1] = u
			return
		}
	}
}

// Accounted sizes: what one entry of each structure costs (key, value,
// slice and pointer overhead), measured once and kept as estimates; the
// measurement matrix reports the heap actually held.
const (
	bytesPerRoot    = 220
	bytesPerSection = 360
)

func (e *fsEntry) accountedSize() int64 {
	n := e.st.bytes()
	if e.fr != nil {
		n += int64(len(e.fr.Roots)) * bytesPerRoot
	}
	e.pmu.Lock()
	for _, pu := range e.plans {
		n += int64(len(pu.secs)) * bytesPerSection
	}
	e.pmu.Unlock()
	return n
}

// fsPlanReserve is the working memory a call reserves before it plans on fr:
// the two read-ahead windows it may hold (the one being planned and the next,
// read in the background) and the plan, each at most max_nodes sections per
// unit; and, while the FactSet's tables are not built, the reservation's
// candidates (24 bytes per depth-1 entry of every root).
func fsPlanReserve(roots, maxNodes int, built bool) int64 {
	n := int64(2*prefetchWindow+1) * int64(max(maxNodes, 1)) * bytesPerSection
	if !built {
		n += int64(roots) * int64(max(maxNodes, 1)) * 24
	}
	return n
}

type fsCacheT struct {
	mu     sync.Mutex
	budget int64
	// total is every cached entry's accounted size plus every reservation in
	// flight; inflight counts the reservations.
	total    int64
	inflight int
	freed    chan struct{} // closed (then replaced) when memory may have come free
	lru      *list.List    // front: most recently used
	byID     map[string]*fsEntry
	hooked   map[*sessions.ToolIndex]bool
}

var fsCache = &fsCacheT{budget: DefaultFactSetCacheMiB << 20, lru: list.New(), byID: map[string]*fsEntry{},
	hooked: map[*sessions.ToolIndex]bool{}}

// factsetBudgetWait is how long a call waits for planning memory before it
// gives up with ErrFactSetBusy (its context may end it sooner).
var factsetBudgetWait atomic.Int64

func init() { factsetBudgetWait.Store(int64(config.FactSetStallLimit)) }

// SetFactSetCacheBudget sets the budget in bytes (0 or less: the default).
// Idle entries past it are evicted at once, least recently used first.
func SetFactSetCacheBudget(bytes int64) {
	if bytes <= 0 {
		bytes = DefaultFactSetCacheMiB << 20
	}
	fsCache.mu.Lock()
	fsCache.budget = bytes
	fsCache.evictLocked(fsCache.budget)
	fsCache.wakeLocked()
	fsCache.mu.Unlock()
}

// FactSetCacheBytes is the accounted size of the cache, reservations in
// flight included (diagnostics, the measurement matrix).
func FactSetCacheBytes() int64 {
	fsCache.mu.Lock()
	defer fsCache.mu.Unlock()
	return fsCache.total
}

// fsCacheGet returns the cached entry of id for the store ti, held for the
// caller (fsCacheRelease gives it back), or nil (an idle entry of another
// store instance, a reopened store, is dropped).
func fsCacheGet(id string, ti *sessions.ToolIndex) *fsEntry {
	c := fsCache
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byID[id]
	if !ok {
		return nil
	}
	if e.ti != ti {
		c.removeLocked(e)
		return nil
	}
	c.lru.MoveToFront(e.elem)
	e.users++
	return e
}

// fsCachePut adds e, held for the caller (replacing any entry under its id).
func fsCachePut(e *fsEntry) {
	c := fsCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.byID[e.id]; ok {
		c.removeLocked(old)
	}
	c.insertLocked(e)
	e.users++
}

// fsCachePutIfAbsent adds e unless another call cached the id first, and
// returns the entry that is cached, held for the caller.
func fsCachePutIfAbsent(e *fsEntry) *fsEntry {
	c := fsCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.byID[e.id]; ok && old.ti == e.ti {
		c.lru.MoveToFront(old.elem)
		old.users++
		return old
	}
	if old, ok := c.byID[e.id]; ok {
		c.removeLocked(old)
	}
	c.insertLocked(e)
	e.users++
	return e
}

func (c *fsCacheT) insertLocked(e *fsEntry) {
	e.elem = c.lru.PushFront(e)
	c.byID[e.id] = e
	if e.ti != nil && !c.hooked[e.ti] {
		c.hooked[e.ti] = true
		ti := e.ti
		ti.OnClose("materialize.factset-cache", func() { fsCacheDropStore(ti) })
	}
}

func (c *fsCacheT) removeLocked(e *fsEntry) {
	if cur, ok := c.byID[e.id]; ok && cur == e {
		delete(c.byID, e.id)
	}
	if e.elem != nil {
		c.lru.Remove(e.elem)
		e.elem = nil
	}
	c.total -= e.size
	e.size = 0
	c.wakeLocked()
}

// fsCacheDrop forgets id.
func fsCacheDrop(id string) {
	c := fsCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.byID[id]; ok {
		c.removeLocked(e)
	}
}

// fsCacheDropStore forgets every entry of the store ti (it is closing).
func fsCacheDropStore(ti *sessions.ToolIndex) {
	c := fsCache
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.byID {
		if e.ti == ti {
			c.removeLocked(e)
		}
	}
	delete(c.hooked, ti)
}

// fsCacheAccount re-measures e and evicts least recently used idle entries
// past the budget.
func fsCacheAccount(e *fsEntry) {
	size := e.accountedSize()
	c := fsCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.elem == nil {
		return // evicted or dropped meanwhile: nothing to account
	}
	c.total += size - e.size
	e.size = size
	c.evictLocked(c.budget)
}

// fsCacheRelease re-measures e and gives the caller's hold back.
func fsCacheRelease(e *fsEntry) {
	fsCacheAccount(e)
	c := fsCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.users > 0 {
		e.users--
	}
	if e.users == 0 {
		c.evictLocked(c.budget)
		c.wakeLocked()
	}
}

// evictLocked evicts least recently used IDLE entries until total <= limit
// (or none is left idle).
func (c *fsCacheT) evictLocked(limit int64) {
	for el := c.lru.Back(); el != nil && c.total > limit; {
		prev := el.Prev()
		if e := el.Value.(*fsEntry); e.users == 0 {
			c.removeLocked(e)
		}
		el = prev
	}
}

// wakeLocked wakes every call waiting for memory.
func (c *fsCacheT) wakeLocked() {
	if c.freed != nil {
		close(c.freed)
		c.freed = nil
	}
}

// fsReservation is planning memory held under the budget.
type fsReservation struct{ size int64 }

// fsReserve reserves n bytes of planning memory: idle entries are evicted to
// make room; it is granted when it fits, or when no other reservation is in
// flight (one plan always proceeds, however large); otherwise the call waits
// until memory comes free, its context ends (ctx.Err()) or the budget wait
// passes (ErrFactSetBusy).
func fsReserve(ctx context.Context, n int64) (*fsReservation, error) {
	c := fsCache
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		c.mu.Lock()
		if c.total+n > c.budget {
			c.evictLocked(c.budget - n)
		}
		if c.total+n <= c.budget || c.inflight == 0 {
			c.total += n
			c.inflight++
			c.mu.Unlock()
			return &fsReservation{size: n}, nil
		}
		if c.freed == nil {
			c.freed = make(chan struct{})
		}
		freed := c.freed
		c.mu.Unlock()
		if timer == nil {
			timer = time.NewTimer(time.Duration(factsetBudgetWait.Load()))
		}
		select {
		case <-freed:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, ErrFactSetBusy
		}
	}
}

// release gives r's bytes back and wakes the waiters.
func (r *fsReservation) release() {
	c := fsCache
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total -= r.size
	c.inflight--
	c.wakeLocked()
}
