package materialize

import (
	"container/list"
	"sync"

	"knomit/internal/engine/store/sessions"
)

// The FactSet cache: per process, keyed by FactSet id. It holds what a
// FactSet derives from its frozen row (the results and reserved maps, the
// written set up to the planned extent, the plans of the last few units)
// under ONE budget across all FactSets (session.factset_memory_mib),
// least-recently-used evicted first. It is never stored: on a miss (eviction,
// restart, another node) the entry is rebuilt by replaying units 1..u-1
// without bodies, and the bytes come out the same. A store's entries are
// dropped when it closes.

// DefaultFactSetCacheMiB is the cache budget when session.factset_memory_mib
// is 0 or unset.
const DefaultFactSetCacheMiB = 64

// fsEntry is one FactSet's cached state. mu serializes the calls on one
// FactSet (two calls on one cursor both succeed, one after the other, with
// the same bytes).
type fsEntry struct {
	mu      sync.Mutex
	id      string
	ti      *sessions.ToolIndex
	fr      *factsetFrozen
	st      fsState
	planned int
	counts  []int
	plans   map[int]*plannedUnit
	// size is the accounted bytes, elem the LRU element (cache lock).
	size int64
	elem *list.Element
}

// Accounted sizes: what one entry of each structure costs in a Go map or
// slice (key, value, bucket overhead), measured once and kept as estimates;
// the measurement matrix reports the heap actually held.
const (
	bytesPerRoot    = 220
	bytesPerSection = 360
)

func (e *fsEntry) accountedSize() int64 {
	n := e.st.bytes() + int64(len(e.counts))*8
	if e.fr != nil {
		n += int64(len(e.fr.Roots)) * bytesPerRoot
	}
	for _, pu := range e.plans {
		n += int64(len(pu.secs)) * bytesPerSection
	}
	return n
}

type fsCacheT struct {
	mu     sync.Mutex
	budget int64
	total  int64
	lru    *list.List // front: most recently used
	byID   map[string]*fsEntry
	hooked map[*sessions.ToolIndex]bool
}

var fsCache = &fsCacheT{budget: DefaultFactSetCacheMiB << 20, lru: list.New(), byID: map[string]*fsEntry{},
	hooked: map[*sessions.ToolIndex]bool{}}

// SetFactSetCacheBudget sets the cache budget in bytes (0 or less: the
// default). Entries past it are evicted at once, least recently used first.
func SetFactSetCacheBudget(bytes int64) {
	if bytes <= 0 {
		bytes = DefaultFactSetCacheMiB << 20
	}
	fsCache.mu.Lock()
	fsCache.budget = bytes
	fsCache.evictLocked(nil)
	fsCache.mu.Unlock()
}

// FactSetCacheBytes is the accounted size of the cache (diagnostics, the
// measurement matrix).
func FactSetCacheBytes() int64 {
	fsCache.mu.Lock()
	defer fsCache.mu.Unlock()
	return fsCache.total
}

// fsCacheGet returns the cached entry of id for the store ti, or nil (an
// entry of another store instance, a reopened store, is dropped).
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
	return e
}

// fsCachePut adds e (replacing any entry under its id).
func fsCachePut(e *fsEntry) {
	c := fsCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.byID[e.id]; ok {
		c.removeLocked(old)
	}
	c.insertLocked(e)
}

// fsCachePutIfAbsent adds e unless another call cached the id first, and
// returns the entry that is cached.
func fsCachePutIfAbsent(e *fsEntry) *fsEntry {
	c := fsCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.byID[e.id]; ok && old.ti == e.ti {
		c.lru.MoveToFront(old.elem)
		return old
	}
	if old, ok := c.byID[e.id]; ok {
		c.removeLocked(old)
	}
	c.insertLocked(e)
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

// fsCacheAccount re-measures e (its caller holds e.mu) and evicts least
// recently used entries past the budget; e itself, in use, is kept.
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
	c.evictLocked(e)
}

func (c *fsCacheT) evictLocked(keep *fsEntry) {
	for c.total > c.budget {
		back := c.lru.Back()
		if back == nil {
			return
		}
		e := back.Value.(*fsEntry)
		if e == keep {
			if c.lru.Len() == 1 {
				return
			}
			c.lru.MoveToFront(back)
			continue
		}
		c.removeLocked(e)
	}
}
