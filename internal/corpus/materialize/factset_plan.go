package materialize

import (
	"context"
	"fmt"
	"hash/fnv"
	"strconv"

	"github.com/rs/zerolog/log"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/index"
)

// The planner: ONE walk for every materialization, a single fact
// (Plan, a unit with no FactSet around it) and every unit of a FactSet. It
// reads only the index, in batches: every level of the walk is one set-based
// read per KB (index.GraphStore.RefLevel: the ref_edges rows of the level's
// blobs joined to their targets' facts rows and dates), a version is its BLOB
// (a root: the blob branch_facts held at the anchor; a pinned ref: its
// ref_edges target_blob; unpinned and cross-KB: the blob live at the anchor
// or the frozen tip, index.HistoryQuery.LiveAt), and a body is read by blob
// hash only for a section a page emits. No git object is decoded per fact
// and OutgoingAtCommit is never called.
//
// Within a unit: a breadth-first pass to Options.Depth decides inclusion
// (shortest distance from the root, cut at MaxNodes in BFS order; the refs
// of the nodes AT the depth limit are read too, so a ref cut by depth still
// has a title), then a pre-order depth-first pass in the author's refs order
// (ref_edges.ord) decides the reading order. A frame root's ancestors come
// first (own data only, any distance, bounded by MaxNodes), topologically by
// a post-order walk over frame refs, then the frame, then the ordinary walk.
//
// In a FactSet each newly reached fact is classified in refs order: in the
// WRITTEN set (an earlier unit wrote it) it is a back link, terminal; a
// result of another unit, or reserved for a later unit, it is a forward link,
// terminal; otherwise it is included. Links count toward MaxNodes. A unit's
// depth-1 entries stay its root's children in the reading order (pinDirect),
// so a reserved fact's number "N.k" holds.

// Stats counts what one call did, for the measurements and the query-bound
// tests.
type Stats struct {
	// IndexQueries is the index reads (one per batched read call; a chunked
	// read of a very large set counts once per call).
	IndexQueries int
	// ObjectReads is the blobs read from the object table (bodies).
	ObjectReads int
	// UnitsPlanned is the units planned (replays included).
	UnitsPlanned int
	// HistoricalLiveAt is the LiveAt reads that ran the historical path
	// (the branch had moved past the anchor or tip).
	HistoricalLiveAt int
	// CacheMisses is the FactSets rebuilt from their frozen row (a replay).
	CacheMisses int
}

func (s *Stats) add(o Stats) {
	s.IndexQueries += o.IndexQueries
	s.ObjectReads += o.ObjectReads
	s.UnitsPlanned += o.UnitsPlanned
	s.HistoricalLiveAt += o.HistoricalLiveAt
	s.CacheMisses += o.CacheMisses
}

// FactHash is a fact's identity in a FactSet: 64 bits of FNV-1a over (repo
// id12, NUL, repo-relative path). Same fact = same path (user ruling). It is
// the written set's key, the reserved map's key and the output anchor
// f-<16 hex>. Collision odds are about 3e-10 at 100k facts, and a collision
// is DETECTED (factCheck), never silently resolved.
func FactHash(repo, rel string) uint64 { return factHash(repo, rel) }

// factHash is FactHash; a test seam replaces it to force a collision.
var factHash = fnv1a

func fnv1a(repo, rel string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(repo))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(rel))
	return h.Sum64()
}

// factCheck is a second, independent 64-bit hash of (repo, path) (FNV-1a of
// the path first, under another separator), stored beside every FactHash
// key: a lookup whose check differs is a COLLISION of FactHash, which fails
// the call instead of dropping a subtree. It costs 8 bytes per entry where
// the (repo, path) itself would cost about 60 (the cache budget).
func factCheck(repo, rel string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(rel))
	_, _ = h.Write([]byte{0xff})
	_, _ = h.Write([]byte(repo))
	return h.Sum64()
}

// AnchorOf is the output anchor of a fact hash.
func AnchorOf(h uint64) string { return fmt.Sprintf("f-%016x", h) }

// blob64 is the first 64 bits of a blob hash: what the written set and the
// reserved map keep of a written version, so a link can say whether the
// version it cites differs (different_version) without holding the blob.
// 0 for "" or a malformed hash.
func blob64(blob string) uint64 {
	if len(blob) < 16 {
		return 0
	}
	v, err := strconv.ParseUint(blob[:16], 16, 64)
	if err != nil {
		return 0
	}
	return v
}

// errHashCollision is a FactHash collision between two different facts.
type errHashCollision struct{ repo, rel string }

func (e errHashCollision) Error() string {
	return fmt.Sprintf("factset: fact identity collision on %s in repo %s", e.rel, e.repo)
}

// ---- FactSet state (the cache's content) ---------------------------------

// rootRec is a result's unit.
type rootRec struct {
	unit  int32
	check uint64
	blob  uint64
}

// resRec is a reserved direct reference: its owning unit and its place k
// among that unit's depth-1 entries, and the version that unit writes.
type resRec struct {
	unit, k int32
	check   uint64
	blob    uint64
}

// wRec is a written fact: its check hash and the version written.
type wRec struct{ check, blob uint64 }

// fsState is a FactSet's cross-unit state: the results, the reserved map and
// the written set. Nothing else crosses units.
type fsState struct {
	results  map[uint64]rootRec
	reserved map[uint64]resRec
	written  map[uint64]wRec
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
	if w, ok := s.written[h]; ok {
		if w.check != check {
			return nil, errHashCollision{repo, rel}
		}
		return &plink{back: true, blob: w.blob}, nil
	}
	if r, ok := s.results[h]; ok {
		if r.check != check {
			return nil, errHashCollision{repo, rel}
		}
		if int(r.unit) == unit {
			return nil, nil
		}
		return &plink{unit: int(r.unit), number: strconv.Itoa(int(r.unit)), back: int(r.unit) < unit, blob: r.blob}, nil
	}
	if r, ok := s.reserved[h]; ok {
		if r.check != check {
			return nil, errHashCollision{repo, rel}
		}
		if int(r.unit) == unit {
			return nil, nil
		}
		return &plink{unit: int(r.unit), number: fmt.Sprintf("%d.%d", r.unit, r.k), back: int(r.unit) < unit, blob: r.blob}, nil
	}
	return nil, nil
}

// ---- one unit -----------------------------------------------------------

// pnode is one fact in a unit's plan.
type pnode struct {
	key
	h, check     uint64
	m            *mount
	commit, blob string
	meta         index.VersionMeta
	date         int64
	dist         int
	latest       bool // reached cross-KB: its repo's version at the frozen tip
	ancestor     bool
	included     bool
	resolved     bool
	refs         []pref
	parent       *pnode
	link         *plink
}

func (n *pnode) expands() bool { return n.included && n.link == nil }

// pref is one of a node's refs, classified.
type pref struct {
	kind         refKind
	target       key
	m            *mount
	commit, blob string
	meta         index.VersionMeta
	date         int64
	latest       bool
	omit         Omission
	source       Source
}

// planner plans one unit.
type planner struct {
	ctx     context.Context
	r       *reader
	opts    Options
	writeID string
	fs      *fsState // nil for a single fact
	unit    int
	nodes   map[key]*pnode
	incl    []*pnode
	// pinDirect keeps every depth-1 node its root's child in the reading
	// order (a FactSet unit that is not a frame).
	pinDirect bool
	st        *Stats
}

// wire renders a key as addressed on the wire: bare in the binding's write
// repo, kb://<id12>/… everywhere else.
func (p *planner) wire(k key) string {
	if k.repo == p.writeID {
		return k.rel
	}
	return fact.KBScheme + k.repo + "/" + k.rel
}

func (p *planner) newNode(k key, m *mount, commit, blob string, meta index.VersionMeta, date int64, dist int, latest bool) *pnode {
	return &pnode{key: k, h: factHash(k.repo, k.rel), check: factCheck(k.repo, k.rel), m: m, commit: commit,
		blob: blob, meta: meta, date: date, dist: dist, latest: latest}
}

// adopt records the target of r, first reached from a node at distance
// dist-1, with the version THIS ref cites (a path has one version in a unit,
// the one it was first reached at). Under the cap it is included; in a
// FactSet it may be a link (terminal).
func (p *planner) adopt(r pref, dist int) (*pnode, error) {
	n := p.newNode(r.target, r.m, r.commit, r.blob, r.meta, r.date, dist, r.latest)
	p.nodes[n.key] = n
	if len(p.incl) < p.opts.MaxNodes {
		n.included = true
		p.incl = append(p.incl, n)
		if p.fs != nil {
			l, err := p.fs.lookup(n.h, n.check, n.repo, n.rel, p.unit)
			if err != nil {
				return nil, err
			}
			n.link = l
		}
	}
	return n, nil
}

// levelReads is what one batched level read returned for one mount.
type levelReads struct {
	rows map[string]map[[2]string]index.RefLevelRow // by source blob, then (repo, path)
	live map[string]index.LiveVersion              // unpin: local targets at the anchor
}

// resolveLevel reads and classifies the refs of every unresolved node of one
// level: per mount ONE level read (pinned: RefLevel; unpinned: the ref
// targets, then LiveAt at the anchor), plus one LiveAt per foreign repo at its
// frozen tip for the level's cross-KB refs.
func (p *planner) resolveLevel(level []*pnode) error {
	byMount := map[*mount][]*pnode{}
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
	reads := map[*mount]*levelReads{}
	for _, m := range mounts {
		ns := byMount[m]
		var blobs []string
		for _, n := range ns {
			if hasFactRef(n) {
				blobs = append(blobs, n.blob)
			}
		}
		lr := &levelReads{rows: map[string]map[[2]string]index.RefLevelRow{}}
		reads[m] = lr
		if len(blobs) == 0 {
			continue
		}
		if err := p.r.checkReady(p.ctx, m); err != nil {
			return err
		}
		if !p.opts.Unpin {
			rows, err := m.svc.GraphStore().RefLevel(p.ctx, blobs)
			if err != nil {
				return err
			}
			p.st.IndexQueries++
			// A target with no facts row (a pinned OLD version often has
			// none): its metadata parsed from its blob, one batched read.
			var orphans []index.VersionKey
			for _, r := range rows {
				if r.Repo == "" && r.TargetBlob != "" && !r.HasMeta {
					orphans = append(orphans, index.VersionKey{Path: r.Path, Blob: r.TargetBlob})
				}
			}
			var parsed map[index.VersionKey]index.VersionMeta
			if len(orphans) > 0 {
				if parsed, err = m.svc.FactQuery().VersionsByBlob(p.ctx, orphans); err != nil {
					return err
				}
				p.st.IndexQueries++
			}
			for _, r := range rows {
				if r.Repo == "" && r.TargetBlob != "" && !r.HasMeta {
					if pm, ok := parsed[index.VersionKey{Path: r.Path, Blob: r.TargetBlob}]; ok {
						r.Meta, r.HasMeta = pm, true
					}
				}
				put(lr.rows, r)
			}
			continue
		}
		// Unpinned: the same target SET (ref_edges rows, the author's order),
		// each at its version live at the anchor.
		targets, err := m.svc.GraphStore().RefTargetsOfBlobs(p.ctx, blobs)
		if err != nil {
			return err
		}
		p.st.IndexQueries++
		var paths []string
		for _, t := range targets {
			put(lr.rows, index.RefLevelRow{RefEdgeRow: index.RefEdgeRow{SourceBlob: t.SourceBlob, Ord: t.Ord, Repo: t.Repo, Path: t.Path, TargetBlob: t.TargetBlob}})
			if t.Repo == "" {
				paths = append(paths, t.Path)
			}
		}
		if len(paths) > 0 {
			live, hist, err := m.svc.HistoryQuery().LiveAt(p.ctx, m.branch, m.anchor, paths)
			if err != nil {
				return err
			}
			p.st.IndexQueries++
			if hist {
				p.st.HistoricalLiveAt++
			}
			lr.live = live
		}
	}

	// Cross-KB refs: per foreign repo, one LiveAt at its frozen tip.
	foreign := map[string]map[string]index.LiveVersion{}
	if p.opts.CrossKB {
		want := map[string][]string{}
		var repos []string
		for _, n := range level {
			if n.resolved {
				continue
			}
			for _, raw := range n.meta.Refs {
				c := fact.ClassifyRef(raw, n.m.id12)
				if c.Kind != fact.RefForeignFact {
					continue
				}
				if _, ok := want[c.RepoID]; !ok {
					repos = append(repos, c.RepoID)
				}
				want[c.RepoID] = append(want[c.RepoID], c.Path)
			}
		}
		for _, id := range repos {
			fm, err := p.r.foreign(p.ctx, id)
			if err != nil {
				return err
			}
			if fm == nil {
				continue
			}
			if err := p.r.checkReady(p.ctx, fm); err != nil {
				return err
			}
			live, hist, err := fm.svc.HistoryQuery().LiveAt(p.ctx, fm.branch, fm.anchor, want[id])
			if err != nil {
				return err
			}
			p.st.IndexQueries++
			if hist {
				p.st.HistoricalLiveAt++
			}
			foreign[id] = live
		}
	}
	for _, n := range level {
		if n.resolved {
			continue
		}
		n.resolved = true
		p.classify(n, reads[n.m], foreign)
	}
	return nil
}

func put(rows map[string]map[[2]string]index.RefLevelRow, r index.RefLevelRow) {
	byKey := rows[r.SourceBlob]
	if byKey == nil {
		byKey = map[[2]string]index.RefLevelRow{}
		rows[r.SourceBlob] = byKey
	}
	k := [2]string{r.Repo, r.Path}
	if _, dup := byKey[k]; !dup {
		byKey[k] = r
	}
}

// hasFactRef reports whether n has a fact ref other than itself.
func hasFactRef(n *pnode) bool {
	for _, raw := range n.meta.Refs {
		c := fact.ClassifyRef(raw, n.m.id12)
		if (c.Kind == fact.RefLocalFact && c.Path != n.rel) || c.Kind == fact.RefForeignFact {
			return true
		}
	}
	return false
}

// classify turns n's refs, in the order written, into its classified refs: a
// fact once per target, a source once per label. A local ref with no
// ref_edges row (unresolved when its version was written, or its target blob
// missing: the real-blob invariant) is "not included" (no recorded version)
// and never a node. Under unpin a target gone at the anchor is dropped without
// a mark.
func (p *planner) classify(n *pnode, lr *levelReads, foreign map[string]map[string]index.LiveVersion) {
	seen := map[key]bool{}
	labels := map[string]int{}
	var rows map[[2]string]index.RefLevelRow
	if lr != nil {
		rows = lr.rows[n.blob]
	}
	for _, raw := range n.meta.Refs {
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
				if !p.opts.CrossKB {
					n.refs = append(n.refs, pref{kind: refOmit, omit: Omission{Path: p.wire(k), Reason: ReasonCrossKBOff}})
					continue
				}
				live, ok := foreign[c.RepoID]
				if !ok {
					n.refs = append(n.refs, pref{kind: refOmit, omit: Omission{Path: p.wire(k), Reason: ReasonKBUnavail}})
					continue
				}
				v, ok := live[c.Path]
				if !ok || !v.HasMeta {
					if p.opts.Unpin {
						continue
					}
					n.refs = append(n.refs, pref{kind: refOmit, omit: Omission{Path: p.wire(k), Reason: ReasonNotInThatKB}})
					continue
				}
				n.refs = append(n.refs, pref{kind: refNode, target: k, m: p.r.mounts[c.RepoID], commit: v.Commit, blob: v.Blob,
					meta: v.Meta, date: v.Date, latest: true})
				continue
			}
			row, ok := rows[[2]string{"", c.Path}]
			if !ok {
				n.refs = append(n.refs, pref{kind: refOmit, omit: Omission{Path: p.wire(k), Reason: ReasonNoVersion}})
				continue
			}
			if p.opts.Unpin {
				v, ok := lr.live[c.Path]
				if !ok || !v.HasMeta {
					continue // gone at the anchor: not rendered, not mentioned
				}
				n.refs = append(n.refs, pref{kind: refNode, target: k, m: n.m, commit: v.Commit, blob: v.Blob, meta: v.Meta, date: v.Date})
				continue
			}
			if row.TargetBlob == "" || !row.HasMeta {
				n.refs = append(n.refs, pref{kind: refOmit, omit: Omission{Path: p.wire(k), Reason: ReasonNoVersion}})
				continue
			}
			n.refs = append(n.refs, pref{kind: refNode, target: k, m: n.m, commit: row.TargetCommit, blob: row.TargetBlob,
				meta: row.Meta, date: row.Date})
		case fact.RefLocalSystemFile, fact.RefForeignSystemFile:
			k := key{"\x00file:" + c.RepoID, c.Path}
			if seen[k] {
				continue
			}
			seen[k] = true
			path := c.Path
			if c.Kind == fact.RefForeignSystemFile {
				path = fact.KBScheme + c.RepoID + "/" + c.Path
			}
			n.refs = append(n.refs, pref{kind: refOmit, omit: Omission{Path: path, Reason: ReasonSystemFile}})
		case fact.RefSourceCode:
			label := sourceCodeLabel(c)
			if labels[label] > 0 {
				continue
			}
			labels[label]++
			n.refs = append(n.refs, pref{kind: refSource, source: Source{Label: label}})
		case fact.RefExternalURL:
			label := urlLabel(raw)
			kk := labels[label]
			labels[label]++
			if kk > 0 {
				label = fmt.Sprintf("%s %d", label, kk+1)
			}
			n.refs = append(n.refs, pref{kind: refSource, source: Source{Label: label, URL: raw}})
		}
		// fact.RefMalformed: no write path stores one; nothing to render.
	}
}

// ancestors is the frame root's ancestors in reading order (user ruling
// 2026-10-28, 16ccefe6 item 2): INCLUSION breadth-first over frame refs from
// the root, closest first against MaxNodes, any distance, each ancestor
// terminal (none of its other refs is expanded); ORDER a depth-first
// POST-ORDER over frame refs in refs order (every parent before the frames
// that cite it; a ref to a frame already visited, the root included, is a
// back-reference: how a mutual pair settles, the frame you are at being the
// child, with no cycle detection). In a FactSet an ancestor already written
// is a back link and one reserved for a later unit (or another unit's
// result) a forward link (ruling Q4); a link ancestor is terminal.
func (p *planner) ancestors(start *pnode) ([]*pnode, error) {
	level := []*pnode{start}
	for len(level) > 0 {
		if err := p.resolveLevel(level); err != nil {
			return nil, err
		}
		var next []*pnode
		for _, f := range level {
			for _, r := range f.refs {
				if r.kind != refNode || fact.Type(r.meta.Type) != fact.Frame {
					continue
				}
				if _, seen := p.nodes[r.target]; seen {
					continue
				}
				a, err := p.adopt(r, f.dist+1)
				if err != nil {
					return nil, err
				}
				a.ancestor = true
				if a.expands() {
					next = append(next, a)
				}
			}
		}
		level = next
	}
	var out []*pnode
	visited := map[key]bool{start.key: true}
	var visit func(f *pnode)
	visit = func(f *pnode) {
		for _, r := range f.refs {
			if r.kind != refNode || visited[r.target] {
				continue
			}
			a := p.nodes[r.target]
			if a == nil || !a.ancestor || !a.included {
				continue
			}
			visited[a.key] = true
			if a.link == nil {
				visit(a)
			}
			out = append(out, a)
		}
	}
	visit(start)
	return out, nil
}

// bfs decides the included set, level by level (the FIFO order exactly), one
// batched read per level. Ancestors are already in the set and terminal.
func (p *planner) bfs(root *pnode) error {
	level := []*pnode{root}
	for len(level) > 0 {
		if err := p.resolveLevel(level); err != nil {
			return err
		}
		var next []*pnode
		for _, u := range level {
			if u.dist >= p.opts.Depth {
				continue
			}
			for _, r := range u.refs {
				if r.kind != refNode {
					continue
				}
				if _, seen := p.nodes[r.target]; seen {
					continue
				}
				t, err := p.adopt(r, u.dist+1)
				if err != nil {
					return err
				}
				if t.expands() {
					next = append(next, t)
				}
			}
		}
		level = next
	}
	return nil
}

// order is the reading order: ancestors, then the root, then a pre-order
// depth-first walk from the root in refs order writing each included node at
// its first visit. An ancestor or a link node is never expanded.
func (p *planner) order(root *pnode, ancestors []*pnode) []*pnode {
	out := append([]*pnode{}, ancestors...)
	out = append(out, root)
	written := map[key]bool{root.key: true}
	for _, a := range ancestors {
		written[a.key] = true
	}
	var visit func(u *pnode)
	visit = func(u *pnode) {
		for _, r := range u.refs {
			if r.kind != refNode || written[r.target] {
				continue
			}
			t := p.nodes[r.target]
			if t == nil || !t.included {
				continue
			}
			if p.pinDirect && u != root && t.dist == 1 {
				continue // the root's own child, placed in its turn
			}
			written[t.key] = true
			t.parent = u
			out = append(out, t)
			if t.expands() {
				visit(t)
			}
		}
	}
	visit(root)
	return out
}

// plannedUnit is one planned unit: its sections, no bodies.
type plannedUnit struct {
	secs  []section
	frame bool
}

// section is one planned appearance of a fact in a unit.
type section struct {
	h            uint64
	repo, rel    string
	wire         string
	commit, blob string
	title, typ   string
	kind         string
	confidence   float64
	date         int64
	dist         int
	treeDepth    int
	n            int // 1-based ordinal in the unit
	number       string
	parent       int // index of the tree parent; -1 for the root and ancestors
	role         string
	heading      string // a frame's group heading, on the first node of a group
	latest       bool
	unavailable  bool
	link         *plink
	buildsOn     []Link
	notIncluded  []Omission
	sources      []Source
	truncated    *Truncation
}

// planRoot plans one unit from its root (a node already read at its
// version) and returns its sections.
func (p *planner) planRoot(root *pnode) (*plannedUnit, error) {
	p.st.UnitsPlanned++
	root.included = true
	p.nodes = map[key]*pnode{root.key: root}
	p.incl = []*pnode{root}
	frame := fact.Type(root.meta.Type) == fact.Frame
	p.pinDirect = p.fs != nil && !frame
	var ancestors []*pnode
	if frame {
		var err error
		if ancestors, err = p.ancestors(root); err != nil {
			return nil, err
		}
	}
	if err := p.bfs(root); err != nil {
		return nil, err
	}
	order := p.order(root, ancestors)
	return p.sections(root, order, frame)
}

// sections numbers the ordered nodes (the root is the unit number, its
// children "N.1", "N.2", …; ancestors unnumbered) and builds the sections.
func (p *planner) sections(root *pnode, order []*pnode, frame bool) (*plannedUnit, error) {
	pos := make(map[*pnode]int, len(order))
	for i, n := range order {
		pos[n] = i
	}
	out := make([]section, len(order))
	kids := make([]int, len(order))
	cut := map[string]map[key]bool{ReasonDepth: {}, ReasonSize: {}}
	prevHeading := ""
	for i, n := range order {
		s := section{h: n.h, repo: n.repo, rel: n.rel, wire: p.wire(n.key), commit: n.commit, blob: n.blob,
			title: n.meta.Title, typ: n.meta.Type, kind: kindOfMeta(n.meta), confidence: n.meta.Confidence,
			date: n.date, dist: n.dist, n: i + 1, parent: -1, role: RoleRef, latest: n.latest, link: n.link}
		switch {
		case n.ancestor:
			s.role = RoleAncestor
		case n == root && frame:
			s.role = RoleFocal
		case n == root:
			s.role = RoleRoot
		}
		switch {
		case n == root:
			if p.unit > 0 {
				s.number = strconv.Itoa(p.unit)
			}
		case n.parent != nil:
			pi := pos[n.parent]
			s.parent = pi
			kids[pi]++
			s.treeDepth = out[pi].treeDepth + 1
			if out[pi].number != "" {
				s.number = out[pi].number + "." + strconv.Itoa(kids[pi])
			}
		}
		if frame {
			g := sectionStandsOn
			switch {
			case n.ancestor:
				g = sectionAncestors
			case n == root:
				g = sectionThisFrame
			}
			if g != prevHeading {
				s.heading = g
			}
			prevHeading = g
		}
		if p.fs != nil && n.link == nil && n != root && !n.ancestor {
			// A reserved fact is written by its owner at its fixed number.
			if r, ok := p.fs.reserved[n.h]; ok {
				if want := fmt.Sprintf("%d.%d", r.unit, r.k); int(r.unit) != p.unit || want != s.number {
					return nil, fmt.Errorf("factset: %s reserved as %s was planned as %s in unit %d", n.rel, want, s.number, p.unit)
				}
			}
		}
		if n.link == nil {
			for _, r := range n.refs {
				switch r.kind {
				case refSource:
					s.sources = append(s.sources, r.source)
				case refOmit:
					if !n.ancestor {
						s.notIncluded = append(s.notIncluded, r.omit)
					}
				case refNode:
					t := p.nodes[r.target]
					if t != nil && t.included {
						l := Link{N: pos[t] + 1, Title: t.meta.Title, ID: shortID(t.rel), AnchorID: AnchorOf(t.h)}
						// different_version: the version cited here vs the
						// version written (blob identity, ruling Q3: ANY change).
						switch {
						case t.link == nil:
							l.DifferentVersion = r.blob != t.blob
						case t.link.blob != 0:
							l.DifferentVersion = blob64(r.blob) != t.link.blob
						}
						s.buildsOn = append(s.buildsOn, l)
						continue
					}
					if n.ancestor {
						continue
					}
					reason := ReasonDepth
					if t != nil {
						reason = ReasonSize
					}
					cut[reason][r.target] = true
					s.notIncluded = append(s.notIncluded, Omission{Title: r.meta.Title, ID: shortID(r.target.rel), Path: p.wire(r.target), Reason: reason})
				}
			}
		}
		out[i] = s
	}
	tr := Truncation{Depth: len(cut[ReasonDepth]), Nodes: len(cut[ReasonSize])}
	for i := range out {
		if out[i].role == RoleRoot || out[i].role == RoleFocal {
			out[i].truncated = &tr
		}
	}
	return &plannedUnit{secs: out, frame: frame}, nil
}

func kindOfMeta(m index.VersionMeta) string {
	if m.Kind == "" {
		return string(fact.DefaultKind)
	}
	return m.Kind
}

// rootNode reads a root's metadata at its version (one batched read); nil
// when its blob has no metadata (gone at the anchor).
func (p *planner) rootNode(m *mount, rel, commit, blob string, date int64) (*pnode, error) {
	if blob == "" {
		return nil, nil
	}
	vk := index.VersionKey{Path: rel, Blob: blob}
	metas, err := m.svc.FactQuery().VersionsByBlob(p.ctx, []index.VersionKey{vk})
	if err != nil {
		return nil, err
	}
	p.st.IndexQueries++
	meta, ok := metas[vk]
	if !ok {
		return nil, nil
	}
	return p.newNode(key{m.id12, rel}, m, commit, blob, meta, date, 0, false), nil
}

// logCollision logs a FactHash collision (the call fails with a 500).
func logCollision(err error) {
	log.Error().Err(err).Msg("materialize factset: fact identity collision; the call is refused rather than drop a subtree")
}
