package materialize

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
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
//   - "written" is a bitset over each frozen table (a reserved fact or a
//     result written by its own unit is marked, not copied: its version is
//     the one reserved), plus an extra set for every other written fact: a
//     sorted slice of 24-byte entries with a small unsorted tail merged in
//     every extraTail inserts.
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

// bitset is a set of indexes into a frozen table.
type bitset []uint64

func newBitset(n int) bitset    { return make(bitset, (n+63)/64) }
func (b bitset) has(i int) bool { return b[i/64]&(1<<(i%64)) != 0 }
func (b bitset) set(i int)      { b[i/64] |= 1 << (i % 64) }
func (b bitset) bytes() int64   { return int64(len(b)) * 8 }

// xentry is one written fact that is neither a result nor reserved.
type xentry struct{ h, check, blob uint64 }

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
func (s *hset) bytes() int64 { return int64(len(s.main))*24 + int64(len(s.tail))*48 }

// fsState is a FactSet's cross-unit state: the frozen results and reserved
// tables, and what has been written.
type fsState struct {
	built    bool
	results  ftab
	reserved ftab
	wRoot    bitset
	wRes     bitset
	extra    hset
}

// freshWritten clears what has been written (a replay starts over); the
// frozen tables stay.
func (s *fsState) freshWritten() {
	s.wRoot, s.wRes, s.extra = newBitset(len(s.results)), newBitset(len(s.reserved)), hset{}
}

// withFreshWritten is s's frozen tables with its own, empty, written state
// (the export's).
func (s *fsState) withFreshWritten() *fsState {
	c := &fsState{built: true, results: s.results, reserved: s.reserved}
	c.freshWritten()
	return c
}

func (s *fsState) bytes() int64 {
	return int64(len(s.results)+len(s.reserved))*24 + s.wRoot.bytes() + s.wRes.bytes() + s.extra.bytes()
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
// here, else the link. A check mismatch is a collision.
func (s *fsState) lookup(h, check uint64, repo, rel string, unit int) (*plink, error) {
	if e, ok := s.extra.get(h); ok {
		if e.check != check {
			return nil, errHashCollision{repo, rel}
		}
		return &plink{back: true, blob: e.blob}, nil
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
	case ri >= 0 && s.wRoot.has(ri):
		return &plink{back: true, blob: s.results[ri].blob}, nil
	case vi >= 0 && s.wRes.has(vi):
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

// markWritten records one written fact: a result or a reserved fact written
// at its reserved version is a bit; anything else goes to the extra set.
func (s *fsState) markWritten(h, check, blob uint64, repo, rel string) error {
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
			s.wRoot.set(i)
			return nil
		}
	}
	if i := s.reserved.find(h); i >= 0 {
		if s.reserved[i].check != uint32(check) {
			return errHashCollision{repo, rel}
		}
		if s.reserved[i].blob == blob {
			s.wRes.set(i)
			return nil
		}
	}
	s.extra.put(xentry{h: h, check: check, blob: blob})
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
