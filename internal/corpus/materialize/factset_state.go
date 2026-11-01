package materialize

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
)

// A FactSet's cross-unit state in compact form. Go maps cost about twice
// their payload (buckets, tophash, load factor, growth by doubling): on the
// 500x499 synthetic store the map-based written set and reserved map held
// 36 MiB for 22 MiB of payload. Here:
//
//   - the results and the reserved map are FROZEN once the reservation is
//     built: sorted slices of compact entries (FactHash, check hash, blob64,
//     unit, k), searched by binary search, shared read-only by every call
//     and the export (24 bytes each);
//   - "written" records, per written fact, the unit that FIRST wrote it: a
//     uint16 per entry of each frozen table (a reserved fact or a result
//     written by its own unit is marked, not copied: its version is the one
//     reserved), plus an extra set for every other written fact: a sorted
//     slice of entries with a small unsorted tail merged in every extraTail
//     inserts.
//
// A fact is written at most once in a FactSet (every later appearance is a
// link), so "written before unit u" is "first written in a unit < u": ONE
// growing prefix (units 1..planned) answers every unit u <= planned, and a
// unit is re-planned alone, never by replaying the units before it.
//
// The check hash is kept beside every FactHash (64 bits in the extra set, 32
// in the frozen tables), so a collision is still detected wherever a key is
// looked up or inserted.

// fentry is one frozen entry: a result (k 0) or a reserved direct reference.
// 24 bytes: the check hash is kept at 32 bits here (a FactHash collision,
// about 3e-10 at 100k facts, then goes undetected only if these 32 bits also
// collide); blob64 stays 64 bits, since it decides different_version.
type fentry struct {
	h, blob uint64
	check   uint32
	unit, k uint16
}

// ftab is a frozen table, sorted by h, at most one entry per h.
type ftab []fentry

func (t ftab) find(h uint64) int {
	i := sort.Search(len(t), func(i int) bool { return t[i].h >= h })
	if i < len(t) && t[i].h == h {
		return i
	}
	return -1
}

// units is, per entry of a frozen table, the unit that first wrote it (0:
// not written in the planned prefix).
type units []uint16

func (w units) bytes() int64 { return int64(len(w)) * 2 }

// writtenBefore reports whether entry i was first written by a unit < u.
func (w units) writtenBefore(i, u int) bool { return w[i] != 0 && int(w[i]) < u }

// xentry is one written fact that is neither a result nor reserved (or was
// written at another version than the reserved one), with the unit that
// first wrote it.
type xentry struct {
	h, check, blob uint64
	unit           uint16
}

// extraTail is how many inserts the extra set takes unsorted before it
// merges them into its sorted main slice: lookups stay a binary search plus
// one small map probe, and a merge copies the main slice once per extraTail
// inserts (about 60 merges for a 250,000-fact FactSet).
const extraTail = 4096

// hset is the extra written set: a sorted main slice and a small tail.
type hset struct {
	main []xentry
	tail map[uint64]xentry
}

func (s *hset) get(h uint64) (xentry, bool) {
	i := sort.Search(len(s.main), func(i int) bool { return s.main[i].h >= h })
	if i < len(s.main) && s.main[i].h == h {
		return s.main[i], true
	}
	e, ok := s.tail[h]
	return e, ok
}

// put adds e, which the caller found absent.
func (s *hset) put(e xentry) {
	if s.tail == nil {
		s.tail = make(map[uint64]xentry, 64)
	}
	s.tail[e.h] = e
	if len(s.tail) >= extraTail {
		s.merge()
	}
}

func (s *hset) merge() {
	add := make([]xentry, 0, len(s.tail))
	for _, e := range s.tail {
		add = append(add, e)
	}
	slices.SortFunc(add, func(a, b xentry) int {
		switch {
		case a.h < b.h:
			return -1
		case a.h > b.h:
			return 1
		}
		return 0
	})
	out := make([]xentry, 0, len(s.main)+len(add))
	i, j := 0, 0
	for i < len(s.main) && j < len(add) {
		if s.main[i].h < add[j].h {
			out = append(out, s.main[i])
			i++
		} else {
			out = append(out, add[j])
			j++
		}
	}
	out = append(out, s.main[i:]...)
	out = append(out, add[j:]...)
	s.main = out
	s.tail = nil
}

func (s *hset) len() int     { return len(s.main) + len(s.tail) }
func (s *hset) bytes() int64 { return int64(len(s.main))*32 + int64(len(s.tail))*56 }

// fsState is a FactSet's cross-unit state: the frozen results and reserved
// tables (written once, before built is set, never changed after), and the
// planned PREFIX: what units 1..planned wrote, with the first unit of each,
// and each planned unit's section count. The prefix only grows; mu guards it
// (lookups take the read lock, one per lookup; applying a unit takes the
// write lock once).
type fsState struct {
	built    atomic.Bool
	results  ftab
	reserved ftab

	mu      sync.RWMutex
	wRoot   units
	wRes    units
	extra   hset
	planned int
	counts  []int         // counts[u-1]: unit u's sections
	grew    chan struct{} // closed (and replaced) whenever planned grows
}

// setTables installs the frozen tables and an empty prefix, then marks the
// state built (the caller holds the extension token).
func (s *fsState) setTables(results, reserved ftab) {
	s.mu.Lock()
	s.results, s.reserved = results, reserved
	s.wRoot, s.wRes, s.extra = make(units, len(results)), make(units, len(reserved)), hset{}
	s.planned, s.counts = 0, nil
	s.mu.Unlock()
	s.built.Store(true)
}

// extent is the planned prefix: units 1..planned, and a channel closed when
// it grows.
func (s *fsState) extent() (int, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.grew == nil {
		s.grew = make(chan struct{})
	}
	return s.planned, s.grew
}

// sectionsThrough is the number of sections of units 1..u (u <= planned).
func (s *fsState) sectionsThrough(u int) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, c := range s.counts[:min(u, len(s.counts))] {
		n += c
	}
	return n
}

func (s *fsState) bytes() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return int64(len(s.results)+len(s.reserved))*24 + s.wRoot.bytes() + s.wRes.bytes() + s.extra.bytes() + int64(len(s.counts))*8
}

// reservedOf is the reservation of h, if any.
func (s *fsState) reservedOf(h uint64) (unit, k int, ok bool) {
	if i := s.reserved.find(h); i >= 0 {
		return int(s.reserved[i].unit), int(s.reserved[i].k), true
	}
	return 0, 0, false
}

// plink says where a link node's fact is written.
type plink struct {
	unit   int // 0: a back link (the renderer resolves the anchor)
	number string
	back   bool
	blob   uint64 // the written version (blob64); 0 when unknown
}

// lookup classifies a fact newly reached in unit: nil when it is written
// here, else the link. Only what units BEFORE unit wrote counts, so any unit
// up to the planned extent is classified exactly as when it was first
// planned. A check mismatch is a collision.
func (s *fsState) lookup(h, check uint64, repo, rel string, unit int) (*plink, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.extra.get(h); ok {
		if e.check != check {
			return nil, errHashCollision{repo, rel}
		}
		if int(e.unit) < unit {
			return &plink{back: true, blob: e.blob}, nil
		}
	}
	ri := s.results.find(h)
	if ri >= 0 && s.results[ri].check != uint32(check) {
		return nil, errHashCollision{repo, rel}
	}
	vi := s.reserved.find(h)
	if vi >= 0 && s.reserved[vi].check != uint32(check) {
		return nil, errHashCollision{repo, rel}
	}
	switch {
	case ri >= 0 && s.wRoot.writtenBefore(ri, unit):
		return &plink{back: true, blob: s.results[ri].blob}, nil
	case vi >= 0 && s.wRes.writtenBefore(vi, unit):
		return &plink{back: true, blob: s.reserved[vi].blob}, nil
	case ri >= 0:
		r := s.results[ri]
		if int(r.unit) == unit {
			return nil, nil
		}
		return &plink{unit: int(r.unit), number: strconv.Itoa(int(r.unit)), back: int(r.unit) < unit, blob: r.blob}, nil
	case vi >= 0:
		r := s.reserved[vi]
		if int(r.unit) == unit {
			return nil, nil
		}
		return &plink{unit: int(r.unit), number: fmt.Sprintf("%d.%d", r.unit, r.k), back: int(r.unit) < unit, blob: r.blob}, nil
	}
	return nil, nil
}

// markWritten records one fact first written by unit (the caller holds mu):
// a result or a reserved fact written at its reserved version is marked in
// its table; anything else goes to the extra set. A fact already recorded
// (unit applied again after a failed extension) keeps its record.
func (s *fsState) markWritten(h, check, blob uint64, repo, rel string, unit int) error {
	if e, ok := s.extra.get(h); ok {
		if e.check != check {
			return errHashCollision{repo, rel}
		}
		return nil
	}
	if i := s.results.find(h); i >= 0 {
		if s.results[i].check != uint32(check) {
			return errHashCollision{repo, rel}
		}
		if s.results[i].blob == blob {
			if s.wRoot[i] == 0 {
				s.wRoot[i] = uint16(unit)
			}
			return nil
		}
	}
	if i := s.reserved.find(h); i >= 0 {
		if s.reserved[i].check != uint32(check) {
			return errHashCollision{repo, rel}
		}
		if s.reserved[i].blob == blob {
			if s.wRes[i] == 0 {
				s.wRes[i] = uint16(unit)
			}
			return nil
		}
	}
	s.extra.put(xentry{h: h, check: check, blob: blob, unit: uint16(unit)})
	return nil
}

// apply extends the prefix by unit planned+1: it records the unit's written
// facts and its section count, and wakes every call waiting for the extent.
func (s *fsState) apply(unit int, pu *plannedUnit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if unit != s.planned+1 {
		return fmt.Errorf("factset: unit %d applied after unit %d", unit, s.planned)
	}
	for _, sec := range pu.secs {
		if sec.link != nil {
			continue
		}
		if err := s.markWritten(sec.h, factCheck(sec.repo, sec.rel), blob64(sec.blob), sec.repo, sec.rel, unit); err != nil {
			logCollision(err)
			return fmt.Errorf("%w: %w", ErrFactIdentity, err)
		}
	}
	s.planned = unit
	s.counts = append(s.counts, len(pu.secs))
	if s.grew != nil {
		close(s.grew)
	}
	s.grew = make(chan struct{})
	return nil
}

// freeze sorts candidate entries by h IN PLACE and keeps the first of each h
// (the candidates come in unit order and the sort is stable, so the earliest
// unit wins); two candidates with one h and different checks are a
// collision. The table reuses the candidates' array: a reservation of 250,000
// direct references never holds a second copy.
func freeze(cands []fentry, describe func(e fentry) (string, string)) (ftab, error) {
	slices.SortStableFunc(cands, func(a, b fentry) int {
		switch {
		case a.h < b.h:
			return -1
		case a.h > b.h:
			return 1
		}
		return 0
	})
	n := 0
	for i, e := range cands {
		if i > 0 && cands[n-1].h == e.h {
			if cands[n-1].check != e.check {
				repo, rel := describe(e)
				return nil, errHashCollision{repo, rel}
			}
			continue
		}
		cands[n] = e
		n++
	}
	if n < cap(cands)/2 {
		// Mostly duplicates (results sharing their references): keep only
		// what is left.
		return slices.Clone(cands[:n]), nil
	}
	return ftab(cands[:n:n]), nil
}
