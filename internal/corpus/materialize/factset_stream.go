package materialize

// PROTOTYPE (proto/factset-streaming), not wired to any route. It is the
// streaming FactSet of the redesign notes (.claude/plans/factset-redesign-notes.md):
//
//   - frozen inputs: the results as (path, commit, blob), the anchor and the
//     frozen tip of every repo entered cross-KB. They are the cursor's one
//     session-DB row, written once at creation and never again;
//   - first level: ONE set-based join per KB from the frozen list into the
//     graph (index.GraphStore.DerivedFromOfNodes) builds the RESERVED map,
//     direct ref -> (owning unit, k), with round 3's two-level rule and the
//     "owner can show it" limit rule;
//   - each unit: a BFS that reads one join per level (Depth+1 per unit at
//     most), then the same reading order as walker.order (pinDirect), each
//     node written, a back link (in the WRITTEN set), or a forward link (a
//     later result, or reserved for a later unit);
//   - identity and anchor = a 64-bit hash of (repo, path); links are anchors
//     the renderer resolves to numbers;
//   - state between calls is an in-memory cache (written set + reserved map +
//     the plan of the last unit walked); when it is gone it is rebuilt by
//     replaying units from the frozen inputs, without bodies;
//   - bodies are read by blob hash from the store's object table, only for
//     the sections a page emits.

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"sync"
	"time"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/index"
)

// FactHash is a fact's identity in a streamed FactSet: 64 bits of FNV-1a over
// (repo id12, repo-relative path). The same value is the written set's key,
// the reserved map's key and the output anchor.
func FactHash(repo, rel string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(repo))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(rel))
	return h.Sum64()
}

// AnchorOf is the output anchor of a fact hash.
func AnchorOf(h uint64) string { return "f-" + strconv.FormatUint(h, 16) }

// StreamRoot is one frozen result.
type StreamRoot struct {
	Path   string `json:"p"`
	Commit string `json:"c"`
	Blob   string `json:"b"`
	Title  string `json:"t"`
	Date   int64  `json:"d,omitempty"`
}

// StreamCursor is the frozen input of a streamed FactSet: the ONE row it
// writes to the session DB.
type StreamCursor struct {
	Search  FactSetSearch     `json:"search"`
	Options Options           `json:"options"`
	Anchor  string            `json:"anchor"`
	Tips    map[string]string `json:"tips"`
	Roots   []StreamRoot      `json:"roots"`
}

// StreamLink says where a heading-only appearance is written: its anchor,
// and the unit (and, for a reserved fact or a result, the number) when the
// server knows it. Back links carry the anchor only: the renderer has
// already rendered the target. CitedBlob is the version this appearance
// cites (the renderer compares it with the written one: different_version).
type StreamLink struct {
	Anchor    string `json:"anchor"`
	Direction string `json:"direction"`
	Unit      int    `json:"unit,omitempty"`
	Number    string `json:"number,omitempty"`
	CitedBlob string `json:"cited_blob,omitempty"`
}

// StreamRef is one builds_on entry: the anchor of a fact this one cites that
// is in this unit, and the version cited.
type StreamRef struct {
	Anchor    string `json:"anchor"`
	CitedBlob string `json:"cited_blob,omitempty"`
}

// StreamSection is one emitted section.
type StreamSection struct {
	Unit        int         `json:"unit"`
	Number      string      `json:"number"`
	TreeDepth   int         `json:"tree_depth"`
	Depth       int         `json:"depth"`
	Anchor      string      `json:"anchor"`
	Path        string      `json:"path"`
	Repo        string      `json:"repo"`
	Commit      string      `json:"commit"`
	Blob        string      `json:"blob,omitempty"`
	Title       string      `json:"title"`
	Type        string      `json:"type"`
	Kind        string      `json:"kind"`
	Confidence  float64     `json:"confidence"`
	Date        string      `json:"date,omitempty"`
	Written     bool        `json:"written"`
	Unavailable bool        `json:"unavailable,omitempty"`
	Marks       []string    `json:"marks,omitempty"`
	Link        *StreamLink `json:"link,omitempty"`
	BuildsOn    []StreamRef `json:"builds_on,omitempty"`
	NotIncluded []Omission  `json:"not_included,omitempty"`
	Sources     []Source    `json:"sources,omitempty"`
	Body        string      `json:"body,omitempty"`
	Truncated   *Truncation `json:"truncated,omitempty"`
}

// StreamStats counts what the prototype did, for the measurements.
type StreamStats struct {
	IndexQueries  int // SQL statements on an index (one per chunk of a set read)
	ObjectReads   int // blobs read from the object table (bodies, orphan versions)
	SessionWrites int // writes to the session DB
	PerFactOps    int // per-fact reads (the cross-KB fallback); 0 by design
	UnitsWalked   int
	Sections      int
}

type reservation struct{ unit, k int32 }

// StreamFactSet is a streamed FactSet's in-memory state: a CACHE over the
// frozen cursor. Losing it loses nothing: OpenStream rebuilds it.
type StreamFactSet struct {
	ID      string
	cur     StreamCursor
	b       *repos.Binding
	mgr     *repos.Manager
	may     func(string) bool
	writeID string
	// The cursor's scope: who may resume it (owner: endpoint + principal),
	// through which binding identity (PinID) and read set. Checked on EVERY
	// open, the cache hit included; a mismatch answers like an expired cursor.
	owner, pin, readSet string
	resultUnit          map[uint64]int32
	reserved            map[uint64]reservation
	written             map[uint64]struct{}
	walked              int             // units applied to written
	last                []StreamSection // the plan of unit `walked` (no bodies), for a page that ends inside it
	Stats               StreamStats
	mu                  sync.Mutex
}

// streamCache is the process's cache of streamed FactSets (prototype: no
// budget; DropStreamCacheForTest empties it to force a replay).
var streamCache sync.Map

// DropStreamCacheForTest forgets every cached streamed FactSet.
func DropStreamCacheForTest() {
	streamCache.Range(func(k, _ any) bool { streamCache.Delete(k); return true })
}

func (s *StreamFactSet) chunks(n int) int { return (n + 399) / 400 }

// CreateStream runs the search, freezes the results with their versions,
// builds the reserved map (one first-level join per KB) and writes the one
// session-DB row.
func CreateStream(ctx context.Context, sc CursorScope, req FactSetRequest) (*StreamFactSet, error) {
	if err := req.Options.Validate(); err != nil {
		return nil, err
	}
	if req.Options.Unpin {
		return nil, fmt.Errorf("%w: the streaming prototype reads pinned versions only", ErrInvalid)
	}
	r := newReader(req.Binding, req.Manager, req.MayRead)
	defer r.close()
	wm, err := r.open(req.Binding.Write(), req.Binding.WriteMountBranch())
	if err != nil {
		return nil, err
	}
	if sc.Pin == "" || sc.Owner == "" {
		return nil, fmt.Errorf("%w: a factset cursor needs an owner and a binding pin", ErrInvalid)
	}
	s := &StreamFactSet{b: req.Binding, mgr: req.Manager, may: req.MayRead, writeID: fact12(req.Binding),
		owner: sc.Owner, pin: sc.Pin, readSet: sc.ReadSet}
	hits, err := runFactSetSearch(ctx, wm, req)
	if err != nil {
		return nil, err
	}
	s.Stats.IndexQueries++ // the search (counted once; it is #692's, unchanged)
	anchor, err := wm.svc.HeadCommit(ctx, wm.branch)
	if err != nil {
		return nil, err
	}
	paths := make([]string, len(hits))
	for i, h := range hits {
		paths[i] = h.path
	}
	live, err := wm.svc.FactQuery().LiveVersions(ctx, wm.branch, paths)
	if err != nil {
		return nil, err
	}
	s.Stats.IndexQueries += s.chunks(len(paths))
	cur := StreamCursor{Search: req.Search, Options: req.Options, Anchor: anchor, Tips: map[string]string{}}
	for _, h := range hits {
		v := live[h.path]
		cur.Roots = append(cur.Roots, StreamRoot{Path: h.path, Commit: v.Commit, Blob: v.Meta.Blob, Title: h.title, Date: v.Date})
	}
	// Freeze the tip of every repo the caller can reach cross-KB: its tip
	// now is what every unit reads it at.
	if req.Options.CrossKB {
		for _, rt := range req.Binding.Reads() {
			id := fact.ID12(rt.RI.ID())
			if id == s.writeID {
				continue
			}
			m, err := r.foreign(ctx, id)
			if err != nil {
				return nil, err
			}
			if m != nil {
				cur.Tips[id] = m.anchor
			}
		}
	}
	s.cur = cur
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	state, err := json.Marshal(cur)
	if err != nil {
		return nil, err
	}
	id, err := sc.Sessions.CreateStreamFactSet(ctx, "materialize-factset-stream:"+strconv.Itoa(req.Limit), sc.Branch, sc.Pin, sc.ReadSet, sc.Owner, string(state))
	if err != nil {
		return nil, err
	}
	s.Stats.SessionWrites++
	s.ID = id
	streamCache.Store(id, s)
	return s, nil
}

// OpenStream returns the cached state of a streamed FactSet, or rebuilds it
// from its one session-DB row (the reserved map again; the written set by
// replay when a page asks for a position).
func OpenStream(ctx context.Context, sc CursorScope, b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool, id string) (*StreamFactSet, error) {
	if sc.Pin == "" || sc.Owner == "" {
		return nil, ErrCursorExpired
	}
	if v, ok := streamCache.Load(id); ok {
		s := v.(*StreamFactSet)
		if s.owner != sc.Owner || s.pin != sc.Pin || s.readSet != sc.ReadSet || s.pin != b.PinID() {
			return nil, ErrCursorExpired
		}
		return s, nil
	}
	if sc.Pin != b.PinID() {
		return nil, ErrCursorExpired
	}
	state, err := sc.Sessions.GetStreamFactSet(ctx, id, sc.Owner, sc.Pin, sc.ReadSet)
	if err != nil {
		return nil, err
	}
	if state == "" {
		return nil, ErrCursorExpired
	}
	s := &StreamFactSet{ID: id, b: b, mgr: mgr, may: mayRead, writeID: fact12(b), owner: sc.Owner, pin: sc.Pin, readSet: sc.ReadSet}
	if err := json.Unmarshal([]byte(state), &s.cur); err != nil {
		return nil, err
	}
	if err := s.init(ctx); err != nil {
		return nil, err
	}
	streamCache.Store(id, s)
	return s, nil
}

// init builds the result map and the reserved map from the frozen cursor.
func (s *StreamFactSet) init(ctx context.Context) error {
	s.resultUnit = make(map[uint64]int32, len(s.cur.Roots))
	for i, root := range s.cur.Roots {
		s.resultUnit[FactHash(s.writeID, root.Path)] = int32(i + 1)
	}
	s.written = map[uint64]struct{}{}
	s.walked, s.last = 0, nil
	return s.reserve(ctx)
}

// Units is the number of units (results).
func (s *StreamFactSet) Units() int { return len(s.cur.Roots) }

// Cursor is the frozen input (for tests).
func (s *StreamFactSet) Cursor() StreamCursor { return s.cur }

// ReservedNumber is the reserved number of a fact ("" when not reserved).
func (s *StreamFactSet) ReservedNumber(repo, rel string) string {
	rv, ok := s.reserved[FactHash(repo, rel)]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d.%d", rv.unit, rv.k)
}

// MemoryEntries is the size of the state: written set + reserved map.
func (s *StreamFactSet) MemoryEntries() (written, reserved int) {
	return len(s.written), len(s.reserved)
}

// ---- the walk -------------------------------------------------------------

// sw is one walk over the frozen inputs: the mounts, the frozen tips.
type sw struct {
	s      *StreamFactSet
	ctx    context.Context
	r      *reader
	meta   bool // read the targets' metadata (false: the reservation needs paths only)
	extras map[key]*sx
}

func (s *StreamFactSet) newWalk(ctx context.Context) *sw {
	r := newReader(s.b, s.mgr, s.may)
	r.tips = s.cur.Tips
	return &sw{s: s, ctx: ctx, r: r, meta: true, extras: map[key]*sx{}}
}

// snode extras the walker's node does not carry.
type sx struct {
	nodeID int64
	date   int64
}

// root is unit's root node at its frozen version, or nil when the result
// has no version (gone).
func (w *sw) root(unit int, ex map[*node]*sx, nodeIDs map[index.VersionKey]int64, metas map[index.VersionKey]index.VersionMeta) (*node, error) {
	rt := w.s.cur.Roots[unit-1]
	if rt.Blob == "" {
		return nil, nil
	}
	m, err := w.r.open(w.s.b.Write(), w.s.b.WriteMountBranch())
	if err != nil {
		return nil, err
	}
	m.anchor = w.s.cur.Anchor
	vk := index.VersionKey{Path: rt.Path, Blob: rt.Blob}
	meta, ok := metas[vk]
	if !ok {
		return nil, nil
	}
	n := &node{key: key{m.id12, rt.Path}, m: m, commit: rt.Commit, blob: rt.Blob, f: metaFact(rt.Path, meta), included: true}
	ex[n] = &sx{nodeID: nodeIDs[vk]}
	return n, nil
}

// resolveLevel reads the refs of every node of one BFS level, in batches:
// per repo, ONE join for the edges out of the level's versions (with the
// targets' facts rows), ONE object read for the targets with no row, and
// per foreign repo ONE live-version read at its frozen tip.
func (w *sw) resolveLevel(level []*node, ex map[*node]*sx, opts Options) error {
	byMount := map[*mount][]*node{}
	var mounts []*mount
	for _, n := range level {
		if n.resolved {
			continue
		}
		if _, ok := byMount[n.m]; !ok {
			mounts = append(mounts, n.m)
		}
		byMount[n.m] = append(byMount[n.m], n)
	}
	edges := map[*node]map[string]index.StreamEdge{}
	for _, m := range mounts {
		ns := byMount[m]
		// Node ids: known for a node adopted through an edge; looked up
		// (one query) for the rest (a root, a cross-KB entry).
		var keys []index.VersionKey
		for _, n := range ns {
			if ex[n].nodeID == 0 && n.blob != "" {
				keys = append(keys, index.VersionKey{Path: n.rel, Blob: n.blob})
			}
		}
		if len(keys) > 0 {
			ids, err := m.svc.GraphStore().NodeIDsByBlob(w.ctx, keys)
			if err != nil {
				return err
			}
			w.s.Stats.IndexQueries += w.s.chunks(len(keys))
			for _, n := range ns {
				if ex[n].nodeID == 0 {
					ex[n].nodeID = ids[index.VersionKey{Path: n.rel, Blob: n.blob}]
				}
			}
		}
		var ids []int64
		byID := map[int64][]*node{}
		for _, n := range ns {
			if id := ex[n].nodeID; id != 0 && hasLocalRef(n) {
				if len(byID[id]) == 0 {
					ids = append(ids, id)
				}
				byID[id] = append(byID[id], n)
			}
		}
		if len(ids) == 0 {
			continue
		}
		out, err := m.svc.GraphStore().DerivedFromOfNodes(w.ctx, ids, w.meta)
		if err != nil {
			return err
		}
		w.s.Stats.IndexQueries += w.s.chunks(len(ids))
		// Orphan targets (no facts row): their metadata from the blob.
		var orphan []string
		for _, e := range out {
			if w.meta && !e.HasRow {
				orphan = append(orphan, e.Blob)
			}
		}
		var parsed map[string]index.VersionMeta
		if len(orphan) > 0 {
			blobs, err := m.svc.FactQuery().BlobsByHash(w.ctx, orphan)
			if err != nil {
				return err
			}
			w.s.Stats.IndexQueries += w.s.chunks(len(orphan))
			w.s.Stats.ObjectReads += len(orphan)
			parsed = map[string]index.VersionMeta{}
			for _, e := range out {
				if e.HasRow {
					continue
				}
				if data, ok := blobs[e.Blob]; ok {
					f, err := fact.ParseFact(e.Path, string(data))
					if err != nil {
						return fmt.Errorf("parse %s@%s: %w", e.Path, e.Blob, err)
					}
					parsed[e.Blob] = index.VersionMeta{Blob: e.Blob, Title: f.Title, Kind: string(f.Kind), Type: string(f.Type), Confidence: f.Confidence, Refs: f.Refs}
				}
			}
		}
		for _, e := range out {
			if !e.HasRow {
				if pm, ok := parsed[e.Blob]; ok {
					e.Meta, e.HasRow = pm, true
				}
			}
			for _, n := range byID[e.Source] {
				pm := edges[n]
				if pm == nil {
					pm = map[string]index.StreamEdge{}
					edges[n] = pm
				}
				// Several ref-events can wire one target path: the lowest
				// edge id wins (rows come ordered by edge id), as reader.pins
				// keeps the first.
				if _, dup := pm[e.Path]; !dup {
					pm[e.Path] = e
				}
			}
		}
	}
	// Cross-KB refs: per foreign repo, one live-version read at its frozen tip.
	foreign := map[string]map[string]*index.LiveVersion{}
	if opts.CrossKB {
		want := map[string][]string{}
		for _, n := range level {
			if n.resolved {
				continue
			}
			for _, raw := range n.f.Refs {
				c := fact.ClassifyRef(raw, n.m.id12)
				if c.Kind == fact.RefForeignFact {
					want[c.RepoID] = append(want[c.RepoID], c.Path)
				}
			}
		}
		for id, paths := range want {
			m, err := w.r.foreign(w.ctx, id)
			if err != nil {
				return err
			}
			if m == nil {
				continue
			}
			got := map[string]*index.LiveVersion{}
			head, err := m.svc.HeadCommit(w.ctx, m.branch)
			if err != nil {
				return err
			}
			if head == m.anchor {
				lv, err := m.svc.FactQuery().LiveVersions(w.ctx, m.branch, paths)
				if err != nil {
					return err
				}
				w.s.Stats.IndexQueries += w.s.chunks(len(paths))
				for p, v := range lv {
					v := v
					got[p] = &v
				}
			} else {
				// The repo moved past its frozen tip: the per-path read at the
				// tip (the existing walker's), counted as per-fact ops.
				for _, p := range paths {
					f, commit, blob, ok, err := w.r.latest(w.ctx, m, p)
					if err != nil {
						return err
					}
					w.s.Stats.PerFactOps++
					if ok {
						var date int64
						if revs, err := m.svc.HistoryQuery().RevisionsBefore(w.ctx, m.branch, p, commit, 1); err == nil && len(revs) > 0 {
							date = revs[0].CommittedAt
						}
						got[p] = &index.LiveVersion{Commit: commit, Date: date, Meta: index.VersionMeta{Blob: blob, Title: f.Title, Kind: string(f.Kind), Type: string(f.Type), Confidence: f.Confidence, Refs: f.Refs}}
					}
				}
			}
			foreign[id] = got
		}
	}
	for _, n := range level {
		if n.resolved {
			continue
		}
		n.resolved = true
		w.classify(n, edges[n], foreign, opts, ex)
	}
	return nil
}

func hasLocalRef(n *node) bool {
	for _, raw := range n.f.Refs {
		if c := fact.ClassifyRef(raw, n.m.id12); c.Kind == fact.RefLocalFact && c.Path != n.rel {
			return true
		}
	}
	return false
}

// classify is walker.resolve over the batched reads: n's refs in the order
// written, a fact once per target, a source once per label.
func (w *sw) classify(n *node, edges map[string]index.StreamEdge, foreign map[string]map[string]*index.LiveVersion, opts Options, ex map[*node]*sx) {
	seen := map[key]bool{}
	labels := map[string]int{}
	wire := func(k key) string {
		if k.repo == w.s.writeID {
			return k.rel
		}
		return fact.KBScheme + k.repo + "/" + k.rel
	}
	for _, raw := range n.f.Refs {
		c := fact.ClassifyRef(raw, n.m.id12)
		switch c.Kind {
		case fact.RefLocalFact, fact.RefForeignFact:
			k := key{c.RepoID, c.Path}
			if c.Kind == fact.RefLocalFact {
				k.repo = n.m.id12
			}
			if seen[k] || k == n.key {
				continue
			}
			seen[k] = true
			if c.Kind == fact.RefForeignFact {
				if !opts.CrossKB {
					n.refs = append(n.refs, resolved{kind: refOmit, omit: Omission{Path: wire(k), Reason: ReasonCrossKBOff}})
					continue
				}
				got, ok := foreign[c.RepoID]
				if !ok {
					n.refs = append(n.refs, resolved{kind: refOmit, omit: Omission{Path: wire(k), Reason: ReasonKBUnavail}})
					continue
				}
				v := got[c.Path]
				if v == nil {
					n.refs = append(n.refs, resolved{kind: refOmit, omit: Omission{Path: wire(k), Reason: ReasonNotInThatKB}})
					continue
				}
				m := w.r.mounts[c.RepoID]
				r := resolved{kind: refNode, target: k, m: m, commit: v.Commit, blob: v.Meta.Blob, f: metaFact(c.Path, v.Meta), latest: true}
				w.extras[k] = &sx{date: v.Date}
				n.refs = append(n.refs, r)
				continue
			}
			e, ok := edges[c.Path]
			if !ok || (w.meta && !e.HasRow) {
				n.refs = append(n.refs, resolved{kind: refOmit, omit: Omission{Path: wire(k), Reason: ReasonNoVersion}})
				continue
			}
			r := resolved{kind: refNode, target: k, m: n.m, commit: e.Commit, blob: e.Blob, f: metaFact(c.Path, e.Meta)}
			if w.extras[k] == nil {
				w.extras[k] = &sx{nodeID: e.Target, date: e.Date}
			}
			n.refs = append(n.refs, r)
		case fact.RefLocalSystemFile, fact.RefForeignSystemFile:
			k := key{"\x00file:" + c.RepoID, c.Path}
			if seen[k] {
				continue
			}
			seen[k] = true
			p := c.Path
			if c.Kind == fact.RefForeignSystemFile {
				p = fact.KBScheme + c.RepoID + "/" + c.Path
			}
			n.refs = append(n.refs, resolved{kind: refOmit, omit: Omission{Path: p, Reason: ReasonSystemFile}})
		case fact.RefSourceCode:
			label := sourceCodeLabel(c)
			if labels[label] > 0 {
				continue
			}
			labels[label]++
			n.refs = append(n.refs, resolved{kind: refSource, source: Source{Label: label}})
		case fact.RefExternalURL:
			label := urlLabel(raw)
			kk := labels[label]
			labels[label]++
			if kk > 0 {
				label = fmt.Sprintf("%s %d", label, kk+1)
			}
			n.refs = append(n.refs, resolved{kind: refSource, source: Source{Label: label, URL: raw}})
		}
	}
}

// ---- reservation (first level) ------------------------------------------------

// reserve builds the reserved map: for each result in order, its direct
// references in refs order that resolve, up to what its own limits show
// (max_nodes counts the root; link nodes count), each not a result and not
// taken by an earlier unit reserved as <unit>.<k>. One first-level join per
// KB for all results.
func (s *StreamFactSet) reserve(ctx context.Context) error {
	s.reserved = map[uint64]reservation{}
	opts := s.cur.Options
	if opts.Depth < 1 || opts.MaxNodes < 2 || len(s.cur.Roots) == 0 {
		return nil
	}
	w := s.newWalk(ctx)
	defer w.r.close()
	w.meta = false
	roots, ex, err := w.roots()
	if err != nil {
		return err
	}
	var level []*node
	for _, n := range roots {
		if n != nil {
			level = append(level, n)
		}
	}
	if err := w.resolveLevel(level, ex, opts); err != nil {
		return err
	}
	for i, n := range roots {
		if n == nil {
			continue
		}
		unit := int32(i + 1)
		included, k := 1, int32(0)
		for _, r := range n.refs {
			if r.kind != refNode {
				continue
			}
			if included >= opts.MaxNodes {
				break
			}
			included++
			k++
			h := FactHash(r.target.repo, r.target.rel)
			if _, isResult := s.resultUnit[h]; isResult {
				continue
			}
			if _, taken := s.reserved[h]; taken {
				continue
			}
			s.reserved[h] = reservation{unit, k}
		}
	}
	return nil
}

// roots reads every result's root node (one facts-row read and one node-id
// read for all of them); nil for a gone result or a frame (the prototype
// does not walk frames).
func (w *sw) roots() ([]*node, map[*node]*sx, error) {
	m, err := w.r.open(w.s.b.Write(), w.s.b.WriteMountBranch())
	if err != nil {
		return nil, nil, err
	}
	m.anchor = w.s.cur.Anchor
	var keys []index.VersionKey
	for _, rt := range w.s.cur.Roots {
		if rt.Blob != "" {
			keys = append(keys, index.VersionKey{Path: rt.Path, Blob: rt.Blob})
		}
	}
	metas, err := m.svc.FactQuery().VersionsByBlob(w.ctx, keys)
	if err != nil {
		return nil, nil, err
	}
	w.s.Stats.IndexQueries += w.s.chunks(len(keys))
	ids, err := m.svc.GraphStore().NodeIDsByBlob(w.ctx, keys)
	if err != nil {
		return nil, nil, err
	}
	w.s.Stats.IndexQueries += w.s.chunks(len(keys))
	ex := map[*node]*sx{}
	out := make([]*node, len(w.s.cur.Roots))
	for i := range w.s.cur.Roots {
		n, err := w.root(i+1, ex, ids, metas)
		if err != nil {
			return nil, nil, err
		}
		if n != nil && n.f.Type == fact.Frame {
			n = nil
		}
		out[i] = n
	}
	return out, ex, nil
}

// ---- one unit -------------------------------------------------------------------

// walkUnit plans unit (no bodies): BFS with one batched read per level,
// the reading order, numbers, links. It reads the written set and does not
// change it; apply does.
func (s *StreamFactSet) walkUnit(ctx context.Context, unit int) ([]StreamSection, error) {
	opts := s.cur.Options
	w := s.newWalk(ctx)
	defer w.r.close()
	rt := s.cur.Roots[unit-1]
	m, err := w.r.open(s.b.Write(), s.b.WriteMountBranch())
	if err != nil {
		return nil, err
	}
	m.anchor = s.cur.Anchor
	var root *node
	ex := map[*node]*sx{}
	if rt.Blob != "" {
		vk := index.VersionKey{Path: rt.Path, Blob: rt.Blob}
		metas, err := m.svc.FactQuery().VersionsByBlob(ctx, []index.VersionKey{vk})
		if err != nil {
			return nil, err
		}
		s.Stats.IndexQueries++
		if meta, ok := metas[vk]; ok {
			root = &node{key: key{m.id12, rt.Path}, m: m, commit: rt.Commit, blob: rt.Blob, f: metaFact(rt.Path, meta), included: true}
			ex[root] = &sx{date: rt.Date}
		}
	}
	s.Stats.UnitsWalked++
	if root == nil {
		sec := StreamSection{Unit: unit, Number: strconv.Itoa(unit), Anchor: AnchorOf(FactHash(s.writeID, rt.Path)),
			Path: rt.Path, Repo: s.writeID, Title: rt.Title, Unavailable: true}
		return []StreamSection{sec}, nil
	}
	wk := &walker{ctx: ctx, opts: opts, writeID: s.writeID, nodes: map[key]*node{root.key: root}, rootKey: root.key, pinDirect: true}
	wk.incl = []*node{root}
	level := []*node{root}
	for len(level) > 0 {
		if err := w.resolveLevel(level, ex, opts); err != nil {
			return nil, err
		}
		var next []*node
		for _, u := range level {
			if u.dist >= opts.Depth {
				continue
			}
			for _, r := range u.refs {
				if r.kind != refNode {
					continue
				}
				if _, seen := wk.nodes[r.target]; seen {
					continue
				}
				t := &node{key: r.target, m: r.m, commit: r.commit, blob: r.blob, f: r.f, dist: u.dist + 1, latest: r.latest}
				wk.nodes[t.key] = t
				x := w.extras[r.target]
				if x == nil {
					x = &sx{}
				}
				ex[t] = &sx{nodeID: x.nodeID, date: x.date}
				if len(wk.incl) < opts.MaxNodes {
					t.included = true
					wk.incl = append(wk.incl, t)
					t.link = s.visited(t, unit)
				}
				if t.expands() {
					next = append(next, t)
				}
			}
		}
		level = next
	}
	order := wk.order(root, nil)
	return s.sections(wk, w, root, order, unit, ex)
}

// visited is the streamed visited lookup: written by an earlier unit (a back
// link), another unit's result, or reserved for another unit (a forward link
// with its fixed number).
func (s *StreamFactSet) visited(n *node, unit int) *factsetTarget {
	h := FactHash(n.repo, n.rel)
	if _, ok := s.written[h]; ok {
		return &factsetTarget{unit: 0, anchor: AnchorOf(h)}
	}
	if ru, ok := s.resultUnit[h]; ok {
		if int(ru) == unit {
			return nil
		}
		return &factsetTarget{unit: int(ru), number: strconv.Itoa(int(ru)), anchor: AnchorOf(h)}
	}
	if rv, ok := s.reserved[h]; ok && int(rv.unit) != unit {
		return &factsetTarget{unit: int(rv.unit), number: fmt.Sprintf("%d.%d", rv.unit, rv.k), anchor: AnchorOf(h)}
	}
	return nil
}

// sections numbers the ordered nodes and builds the unit's sections.
func (s *StreamFactSet) sections(wk *walker, w *sw, root *node, order []*node, unit int, ex map[*node]*sx) ([]StreamSection, error) {
	pos := make(map[*node]int, len(order))
	for i, n := range order {
		pos[n] = i
	}
	number := make([]string, len(order))
	depth := make([]int, len(order))
	kids := make([]int, len(order))
	out := make([]StreamSection, len(order))
	cut := map[string]map[key]bool{ReasonDepth: {}, ReasonSize: {}}
	for i, n := range order {
		if n == root {
			number[i] = strconv.Itoa(unit)
		} else {
			p := pos[n.parent]
			kids[p]++
			number[i] = number[p] + "." + strconv.Itoa(kids[p])
			depth[i] = depth[p] + 1
		}
		h := FactHash(n.repo, n.rel)
		sec := StreamSection{Unit: unit, Number: number[i], TreeDepth: depth[i], Depth: n.dist, Anchor: AnchorOf(h),
			Path: wk.wire(n.key), Repo: n.repo, Commit: n.commit, Blob: n.blob, Title: n.f.Title, Type: string(n.f.Type),
			Kind: kindOf(n.f), Confidence: n.f.Confidence, Written: n.link == nil}
		if x := ex[n]; x != nil && x.date != 0 {
			sec.Date = dateOf(x.date)
		}
		if n.latest {
			sec.Marks = []string{MarkLatestOtherKB}
		}
		if n.link == nil && n != root {
			if rv, ok := s.reserved[h]; ok {
				if int(rv.unit) != unit || fmt.Sprintf("%d.%d", rv.unit, rv.k) != number[i] {
					return nil, fmt.Errorf("factset: %s reserved as %d.%d was walked as %s in unit %d", n.rel, rv.unit, rv.k, number[i], unit)
				}
			}
		}
		if t := n.link; t != nil {
			dir := LinkForward
			if t.unit == 0 || t.unit < unit {
				dir = LinkBack
			}
			sec.Link = &StreamLink{Anchor: t.anchor, Direction: dir, Unit: t.unit, Number: t.number, CitedBlob: n.blob}
			if t.unit == 0 {
				sec.Link.Unit = 0
			}
			out[i] = sec
			continue
		}
		for _, r := range n.refs {
			switch r.kind {
			case refSource:
				sec.Sources = append(sec.Sources, r.source)
			case refOmit:
				sec.NotIncluded = append(sec.NotIncluded, r.omit)
			case refNode:
				t := wk.nodes[r.target]
				if t != nil && t.included {
					sec.BuildsOn = append(sec.BuildsOn, StreamRef{Anchor: AnchorOf(FactHash(t.repo, t.rel)), CitedBlob: r.blob})
					continue
				}
				reason := ReasonDepth
				if t != nil {
					reason = ReasonSize
				}
				cut[reason][r.target] = true
				sec.NotIncluded = append(sec.NotIncluded, Omission{Title: r.f.Title, ID: shortID(r.target.rel), Path: wk.wire(r.target), Reason: reason})
			}
		}
		out[i] = sec
	}
	tr := Truncation{Depth: len(cut[ReasonDepth]), Nodes: len(cut[ReasonSize])}
	out[0].Truncated = &tr
	return out, nil
}

func dateOf(unix int64) string {
	return time.Unix(unix, 0).UTC().Format("2006-01-02")
}

// apply adds a planned unit's written facts to the written set.
func (s *StreamFactSet) apply(unit int, secs []StreamSection) {
	for _, sec := range secs {
		if sec.Written || sec.Unavailable {
			h, _ := strconv.ParseUint(sec.Anchor[2:], 16, 64)
			s.written[h] = struct{}{}
		}
	}
	s.walked, s.last = unit, secs
}

// plan returns unit's sections (no bodies), replaying from the start when
// the cache is not right before it.
func (s *StreamFactSet) plan(ctx context.Context, unit int) ([]StreamSection, error) {
	if s.walked == unit && s.last != nil {
		return s.last, nil
	}
	if s.walked >= unit {
		// A seek backwards: rebuild from the frozen inputs.
		s.written, s.walked, s.last = map[uint64]struct{}{}, 0, nil
	}
	for s.walked < unit {
		u := s.walked + 1
		secs, err := s.walkUnit(ctx, u)
		if err != nil {
			return nil, err
		}
		s.apply(u, secs)
	}
	return s.last, nil
}

// fillBodies reads the bodies of the written sections, in one batch per
// repo, by blob hash.
func (s *StreamFactSet) fillBodies(ctx context.Context, secs []StreamSection) error {
	w := s.newWalk(ctx)
	defer w.r.close()
	byRepo := map[string][]int{}
	for i, sec := range secs {
		if sec.Written && sec.Blob != "" {
			byRepo[sec.Repo] = append(byRepo[sec.Repo], i)
		}
	}
	for repo, idx := range byRepo {
		m, err := w.r.mountOf(repo)
		if err != nil {
			return err
		}
		hashes := make([]string, len(idx))
		for j, i := range idx {
			hashes[j] = secs[i].Blob
		}
		blobs, err := m.svc.FactQuery().BlobsByHash(ctx, hashes)
		if err != nil {
			return err
		}
		s.Stats.IndexQueries += s.chunks(len(hashes))
		s.Stats.ObjectReads += len(hashes)
		for _, i := range idx {
			data, ok := blobs[secs[i].Blob]
			if !ok {
				return fmt.Errorf("blob %s of %s is not in the store", secs[i].Blob, secs[i].Path)
			}
			f, err := fact.ParseFact(relOf(secs[i].Path), string(data))
			if err != nil {
				return err
			}
			secs[i].Body = f.Body
		}
	}
	return nil
}

// Page serves sections from (unit, section): whole sections, at most limit,
// stopping at PageCap bytes once one is in. It returns the next position
// (unit 0 at the end). Positions before unit replay without bodies.
func (s *StreamFactSet) Page(ctx context.Context, unit, section, limit int) ([]StreamSection, int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []StreamSection
	size := 0
	for unit >= 1 && unit <= len(s.cur.Roots) {
		secs, err := s.plan(ctx, unit)
		if err != nil {
			return nil, 0, 0, err
		}
		end := section
		for end < len(secs) && len(out)+(end-section) < limit {
			end++
		}
		page := append([]StreamSection(nil), secs[section:end]...)
		if err := s.fillBodies(ctx, page); err != nil {
			return nil, 0, 0, err
		}
		for _, sec := range page {
			b, _ := json.Marshal(sec)
			if len(out) > 0 && size+len(b) > PageCap {
				return out, unit, section, nil
			}
			size += len(b)
			out = append(out, sec)
			section++
			s.Stats.Sections++
		}
		if section >= len(secs) {
			unit, section = unit+1, 0
		}
		if len(out) >= limit {
			break
		}
	}
	if unit > len(s.cur.Roots) {
		unit = 0
	}
	return out, unit, section, nil
}

// Export streams every section, bodies included, to emit.
func (s *StreamFactSet) Export(ctx context.Context, emit func(StreamSection) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written, s.walked, s.last = map[uint64]struct{}{}, 0, nil
	for u := 1; u <= len(s.cur.Roots); u++ {
		secs, err := s.plan(ctx, u)
		if err != nil {
			return err
		}
		for lo := 0; lo < len(secs); lo += 400 {
			part := append([]StreamSection(nil), secs[lo:min(len(secs), lo+400)]...)
			if err := s.fillBodies(ctx, part); err != nil {
				return err
			}
			for _, sec := range part {
				if err := emit(sec); err != nil {
					return err
				}
				s.Stats.Sections++
			}
		}
	}
	return nil
}
