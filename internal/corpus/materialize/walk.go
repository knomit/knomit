package materialize

import (
	"context"
	"fmt"
	"path"
	"strings"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/index"
)

// Roles a node plays in the reading order.
const (
	RoleRoot     = "root"     // the fact asked for, when it is not a frame
	RoleAncestor = "ancestor" // a frame the starting frame stands under
	RoleFocal    = "focal"    // the fact asked for, when it is a frame
	RoleRef      = "ref"      // everything the walk reached from the start
)

// Reasons a ref is listed as "not included" instead of being written.
const (
	ReasonDepth       = "depth"          // beyond Options.Depth
	ReasonSize        = "size"           // cut by Options.MaxNodes
	ReasonNoVersion   = "no_version"     // a local ref with no recorded cited version
	ReasonCrossKBOff  = "cross_kb_off"   // a cross-KB ref, and Options.CrossKB is off
	ReasonKBUnavail   = "kb_unavailable" // no repo registered here has that id
	ReasonNotInThatKB = "not_in_that_kb" // the other repo does not hold the fact
	ReasonSystemFile  = "system_file"    // a .knomit/ file: read it with knomit_explain
)

// MarkLatestOtherKB marks a node reached through a cross-KB ref: such a ref
// records no cited version, so the node is its repo's latest version.
const MarkLatestOtherKB = "latest_other_kb"

// Document.Order values.
const (
	OrderRefs  = "refs"  // pre-order depth-first from the root
	OrderFrame = "frame" // ancestors, the frame, then the depth-first walk
)

// Frame section headings, the only frame-specific text in either output.
const (
	sectionAncestors = "Ancestors"
	sectionThisFrame = "This frame"
	sectionStandsOn  = "What it stands on"
)

const (
	anchorIDPrefix     = "f-"
	shortIDLen         = 8
	errRootNotFoundFmt = "%w: could not read %s at %s"
)

// Request is one materialization: the fact (bare, or kb://<id12>/… for a
// mount of the binding), the anchor commit ("" = the tip of the fact's
// mount branch, resolved now) and the options.
//
// Cross-KB, the binding's own mounts are always readable. Any other repo
// registered in Manager is read only when MayRead says the CALLER may read
// it by name (the edge answers from the caller's credential); a nil Manager
// or a nil MayRead reaches no repo outside the binding. A repo refused here
// reads exactly like one that does not exist (kb_unavailable), so the output
// never tells a caller which repos it may not read.
type Request struct {
	Binding *repos.Binding
	Manager *repos.Manager
	MayRead func(repo string) bool
	File    string
	Commit  string
	Options Options
}

// Document is a planned materialization: every node in reading order, with
// its version frozen. Bodies are filled by Plan; a paged caller stores the
// nodes without them and reads them back with ReadBodies.
type Document struct {
	Root      string     `json:"root"`
	RootTitle string     `json:"root_title"`
	Anchor    string     `json:"anchor"`
	Order     string     `json:"order"`
	Options   Options    `json:"options"`
	Nodes     []Node     `json:"nodes"`
	Truncated Truncation `json:"truncated"`
}

// Truncation counts the distinct facts a written node cites that were not
// written, by reason. Gone targets under unpin are never counted.
type Truncation struct {
	Depth int `json:"depth"`
	Nodes int `json:"nodes"`
}

// Node is one written fact. JSON is the shape the human pages carry and the
// state a paged cursor stores (without Body).
type Node struct {
	N           int        `json:"n"`
	Path        string     `json:"path"`
	Repo        string     `json:"repo"`
	Commit      string     `json:"commit"`
	Depth       int        `json:"depth"`
	Role        string     `json:"role"`
	Title       string     `json:"title"`
	Type        string     `json:"type"`
	Kind        string     `json:"kind"`
	Confidence  float64    `json:"confidence"`
	Date        string     `json:"date,omitempty"`
	Body        string     `json:"body,omitempty"`
	BuildsOn    []Link     `json:"builds_on,omitempty"`
	NotIncluded []Omission `json:"not_included,omitempty"`
	Sources     []Source   `json:"sources,omitempty"`
	Marks       []string   `json:"marks,omitempty"`
	// AnchorID is the in-document link target (f-<id8>, suffixed when two
	// facts share an id8).
	AnchorID string `json:"anchor_id"`
	// Section is the heading written before this node, for a frame's three
	// groups; "" for none.
	Section string `json:"section,omitempty"`
}

// Link is a ref to a fact written in the same document, in either direction.
// DifferentVersion: the citing fact cited a version whose BODY differs from
// the one written (pinned mode only; a commit difference alone is not a
// change).
type Link struct {
	N                int    `json:"n"`
	Title            string `json:"title"`
	ID               string `json:"id"`
	AnchorID         string `json:"anchor_id"`
	DifferentVersion bool   `json:"different_version,omitempty"`
}

// Omission is a ref that was not written, with the reason.
type Omission struct {
	Title  string `json:"title,omitempty"`
	ID     string `json:"id,omitempty"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Source is a non-fact ref: an external URL or a source-code ref.
type Source struct {
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
}

// key names one fact across repos: the repo's 12-hex root-commit id and the
// repo-relative path.
type key struct{ repo, rel string }

type node struct {
	key
	m      *mount
	commit string
	// blob is the version's blob hash, "" when the walk read it through the
	// blob fallback; f carries no body (the walk reads metadata only).
	blob string
	f    fact.Fact
	dist int
	// latest: reached through a cross-KB hop, so read at its repo's latest
	// version rather than a cited one.
	latest   bool
	ancestor bool
	included bool
	refs     []resolved
	resolved bool
	// parent is the node whose refs first reached this one in the reading
	// order (the walk tree's edge); nil for the root and frame ancestors.
	parent *node
	// link, set only in a factset, says this fact is in the factset's visited set
	// (a search result, or a fact an earlier unit wrote): the node is a
	// heading with a link there, and is TERMINAL (its refs are never read,
	// so its leftover children are not written here).
	link *factsetTarget
}

type refKind int

const (
	refNode refKind = iota
	refOmit
	refSource
)

// resolved is one of a node's refs, classified and (for a fact) read at the
// version this node cites.
type resolved struct {
	kind   refKind
	target key
	m      *mount
	commit string
	blob   string
	f      fact.Fact
	latest bool
	omit   Omission
	source Source
}

// walker carries one Plan.
type walker struct {
	ctx     context.Context
	r       *reader
	opts    Options
	writeID string
	nodes   map[key]*node
	incl    []*node
	// visited, set only in a factset, looks a newly reached fact up in the
	// factset's visited set (seeded with the search results, grown by every
	// unit walked). nil in a single materialization.
	visited func(n *node) (*factsetTarget, error)
	rootKey key
	// pinDirect, set only for a factset unit (not a frame), keeps every node at
	// BFS distance 1 a child of the root in the reading order: the DFS never
	// places a direct reference under a sibling, so the root's children are
	// exactly its depth-1 entries in refs order. A single materialization
	// keeps the plain pre-order DFS.
	pinDirect bool
}

// Plan runs the walk and returns the document, bodies included.
func Plan(ctx context.Context, req Request) (*Document, error) {
	if err := req.Options.Validate(); err != nil {
		return nil, err
	}
	r := newReader(req.Binding, req.Manager, req.MayRead)
	defer r.close()
	u, err := walkUnit(ctx, r, req, nil)
	if err != nil {
		return nil, err
	}
	// The walk read metadata only; the one-shot document carries bodies.
	if err := r.readBodies(ctx, u.doc.Nodes); err != nil {
		return nil, err
	}
	return u.doc, nil
}

// unitWalk is one walked materialization: its document WITHOUT bodies, and
// the walk tree as positions in doc.Nodes (parent[i] is the index of node
// i's tree parent, -1 for the root and frame ancestors).
type unitWalk struct {
	doc    *Document
	parent []int
	// blobs[i] is node i's blob hash ("" when read through the blob fallback).
	blobs []string
	// links[i] is node i's link target in a factset; nil when it is written.
	links []*factsetTarget
	// gone: a factset unit whose result has no version at the anchor; its one
	// node is the root, a heading only (goneUnit).
	gone bool
}

// walkUnit runs one materialization's walk on r: metadata and edges only,
// never a body. It is the walk of Plan and of every unit of a factset.
// visited is nil for Plan; a factset passes its visited-set lookup, so a
// reached fact that is in the set becomes a terminal link node.
func walkUnit(ctx context.Context, r *reader, req Request, visited func(n *node) (*factsetTarget, error)) (*unitWalk, error) {
	w := &walker{ctx: ctx, r: r, opts: req.Options, writeID: fact.ID12(req.Binding.Write().ID()), nodes: map[key]*node{}, visited: visited}

	root, err := w.root(req)
	if err != nil {
		return nil, err
	}
	w.rootKey = root.key
	frame := root.f.Type == fact.Frame
	// A factset unit's direct references are its root's children in refs order,
	// their numbers fixed before any unit is walked (reserveDirectRefs).
	w.pinDirect = visited != nil && !frame

	var ancestors []*node
	if frame {
		if ancestors, err = w.ancestors(root); err != nil {
			return nil, err
		}
	}
	if err := w.bfs(root); err != nil {
		return nil, err
	}
	order := w.order(root, ancestors)
	doc, err := w.document(root, order, frame)
	if err != nil {
		return nil, err
	}
	pos := make(map[*node]int, len(order))
	for i, n := range order {
		pos[n] = i
	}
	parent := make([]int, len(order))
	for i, n := range order {
		parent[i] = -1
		if n.parent != nil {
			parent[i] = pos[n.parent]
		}
	}
	blobs := make([]string, len(order))
	links := make([]*factsetTarget, len(order))
	for i, n := range order {
		blobs[i], links[i] = n.blob, n.link
	}
	return &unitWalk{doc: doc, parent: parent, blobs: blobs, links: links}, nil
}

// root resolves the requested fact to its mount and reads it at its version
// live at the anchor.
func (w *walker) root(req Request) (*node, error) {
	if req.File == "" {
		return nil, fmt.Errorf("%w: file is required", ErrInvalid)
	}
	id, rel, qualified, err := fact.ParseKBPath(req.File)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	rt := repos.ReadTarget{RI: req.Binding.Write(), Branch: req.Binding.WriteMountBranch()}
	if qualified {
		var ok bool
		if rt, ok = req.Binding.ByID(id); !ok {
			return nil, fmt.Errorf("%w: repo %s is not mounted in this binding", ErrNotFound, id)
		}
	}
	rel = fact.NormalizePath(rt.RI.OntologyRoot(), rel)
	if !fact.IsFactFilePath(rt.RI.OntologyRoot(), rel) {
		return nil, fmt.Errorf("%w: %s is not a fact path: materialize takes a fact under %s/", ErrInvalid, req.File, rt.RI.OntologyRoot())
	}
	m, err := w.r.open(rt.RI, rt.Branch)
	if err != nil {
		return nil, err
	}
	m.anchor = req.Commit
	if m.anchor == "" {
		if m.anchor, err = m.svc.HeadCommit(w.ctx, m.branch); err != nil {
			return nil, fmt.Errorf("resolve HEAD of %s: %w", m.branch, err)
		}
	}
	commit, err := w.r.liveRevision(w.ctx, m, rel)
	if err != nil {
		return nil, fmt.Errorf("resolve the version of %s at %s: %w", rel, m.anchor, err)
	}
	if commit == "" {
		return nil, fmt.Errorf(errRootNotFoundFmt, ErrNotFound, req.File, m.anchor)
	}
	f, used, blob, ok, err := w.r.readAt(w.ctx, m, rel, commit)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf(errRootNotFoundFmt, ErrNotFound, req.File, m.anchor)
	}
	n := &node{key: key{m.id12, rel}, m: m, commit: used, blob: blob, f: f, included: true}
	w.nodes[n.key] = n
	w.incl = append(w.incl, n)
	return n, nil
}

// resolve classifies and reads n's refs, once, in the order they are
// written. A fact ref appears once per target; a source once per label.
func (w *walker) resolve(n *node) ([]resolved, error) {
	if n.resolved {
		return n.refs, nil
	}
	n.resolved = true
	if err := w.prefetch(n); err != nil {
		return nil, err
	}
	var pins map[string]string
	seen := map[key]bool{}
	labels := map[string]int{}
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
			res, keep, err := w.resolveFact(n, c, k, &pins)
			if err != nil {
				return nil, err
			}
			if keep {
				n.refs = append(n.refs, res)
			}
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
			k := labels[label]
			labels[label]++
			if k > 0 {
				label = fmt.Sprintf("%s %d", label, k+1)
			}
			n.refs = append(n.refs, resolved{kind: refSource, source: Source{Label: label, URL: raw}})
		}
		// fact.RefMalformed: no write path stores one; nothing to render.
	}
	return n.refs, nil
}

// resolveFact reads the target of one fact ref at the version n cites.
// keep is false for a target that must not be mentioned at all: under
// unpin, one that is gone at the anchor.
func (w *walker) resolveFact(n *node, c fact.Ref, k key, pins *map[string]string) (resolved, bool, error) {
	wire := w.wire(k)
	if c.Kind == fact.RefForeignFact {
		if !w.opts.CrossKB {
			return resolved{kind: refOmit, omit: Omission{Path: wire, Reason: ReasonCrossKBOff}}, true, nil
		}
		m, err := w.r.foreign(w.ctx, c.RepoID)
		if err != nil {
			return resolved{}, false, err
		}
		if m == nil {
			return resolved{kind: refOmit, omit: Omission{Path: wire, Reason: ReasonKBUnavail}}, true, nil
		}
		k.repo = m.id12
		f, commit, blob, ok, err := w.r.latest(w.ctx, m, c.Path)
		if err != nil {
			return resolved{}, false, err
		}
		if !ok {
			if w.opts.Unpin {
				return resolved{}, false, nil
			}
			return resolved{kind: refOmit, omit: Omission{Path: w.wire(k), Reason: ReasonNotInThatKB}}, true, nil
		}
		return resolved{kind: refNode, target: k, m: m, commit: commit, blob: blob, f: f, latest: true}, true, nil
	}

	if w.opts.Unpin {
		f, commit, blob, ok, err := w.r.latest(w.ctx, n.m, c.Path)
		if err != nil {
			return resolved{}, false, err
		}
		if !ok {
			return resolved{}, false, nil
		}
		return resolved{kind: refNode, target: k, m: n.m, commit: commit, blob: blob, f: f}, true, nil
	}

	if *pins == nil {
		p, err := w.r.pins(w.ctx, n.m, n.rel, n.commit)
		if err != nil {
			return resolved{}, false, err
		}
		*pins = p
	}
	pinned, ok := (*pins)[c.Path]
	if !ok {
		return resolved{kind: refOmit, omit: Omission{Path: wire, Reason: ReasonNoVersion}}, true, nil
	}
	f, used, blob, ok, err := w.r.readAt(w.ctx, n.m, c.Path, pinned)
	if err != nil {
		return resolved{}, false, err
	}
	if !ok {
		return resolved{kind: refOmit, omit: Omission{Path: wire, Reason: ReasonNoVersion}}, true, nil
	}
	return resolved{kind: refNode, target: k, m: n.m, commit: used, blob: blob, f: f}, true, nil
}

// versionPrefetch turns walker.prefetch on (always, in production); a test
// turns it off to compare the batched path with the per-ref one.
var versionPrefetch = true

// SetVersionPrefetchForTest turns the batched version read on or off and
// returns the restore. Never called in production.
func SetVersionPrefetchForTest(on bool) (restore func()) {
	old := versionPrefetch
	versionPrefetch = on
	return func() { versionPrefetch = old }
}

// prefetch reads, in ONE batch, the index rows of every version n's local
// fact refs will read (the version each cites, or the latest at the anchor
// when unpinned) into the reader's version cache, so resolve's per-ref
// readAt answers from the cache. It changes no answer: the batch returns,
// for each version, exactly what VersionMetaAt would (index.VersionMetasAt),
// and it asks for exactly the versions resolve reads (the same refs, the
// same pins, fetched only when a local ref exists, as resolve does).
// Cross-KB refs keep their per-ref read.
func (w *walker) prefetch(n *node) error {
	if !versionPrefetch {
		return nil
	}
	var local []string
	seen := map[key]bool{}
	for _, raw := range n.f.Refs {
		c := fact.ClassifyRef(raw, n.m.id12)
		if c.Kind != fact.RefLocalFact {
			continue
		}
		k := key{n.m.id12, c.Path}
		if seen[k] || k == n.key {
			continue
		}
		seen[k] = true
		local = append(local, c.Path)
	}
	if len(local) < 2 {
		return nil // nothing to batch
	}
	pairs := make([]index.PathAtCommit, 0, len(local))
	if w.opts.Unpin {
		for _, p := range local {
			pairs = append(pairs, index.PathAtCommit{Path: p, Commit: n.m.anchor})
		}
	} else {
		pins, err := w.r.pins(w.ctx, n.m, n.rel, n.commit)
		if err != nil {
			return err
		}
		for _, p := range local {
			if c, ok := pins[p]; ok {
				pairs = append(pairs, index.PathAtCommit{Path: p, Commit: c})
			}
		}
	}
	return w.r.prefetchMetas(w.ctx, n.m, pairs)
}

// adopt records the target of r as a node first reached from a node at
// distance dist-1. Its version is the one THIS ref cites: a path has one
// version in a document, the one it was first reached at.
//
// In a factset, an included node whose fact is in the factset's visited set is a
// link (n.link set): it counts as a section of this unit, and it is never
// expanded.
func (w *walker) adopt(r resolved, dist int) (*node, error) {
	n := &node{key: r.target, m: r.m, commit: r.commit, blob: r.blob, f: r.f, dist: dist, latest: r.latest}
	w.nodes[n.key] = n
	if len(w.incl) < w.opts.MaxNodes {
		n.included = true
		w.incl = append(w.incl, n)
		if w.visited != nil && n.key != w.rootKey {
			t, err := w.visited(n)
			if err != nil {
				return nil, err
			}
			n.link = t
		}
	}
	return n, nil
}

// expands reports whether an adopted node's refs are walked: it is
// included and not a factset link.
func (n *node) expands() bool { return n.included && n.link == nil }

// bfs decides the included set: shortest distance from the root, in refs
// order, cut at MaxNodes. Ancestors are already included and are terminal.
// Every included node's refs are resolved, the ones at the depth limit too,
// so a ref cut by depth still has a title to show.
//
// It runs level by level, which is the FIFO order exactly (a level is the
// queue's run of nodes at one distance, in the order they were queued), so
// that each level's reads can be prefetched in batches first (prefetchLevel).
func (w *walker) bfs(root *node) error {
	level := []*node{root}
	for len(level) > 0 {
		if err := w.prefetchLevel(level); err != nil {
			return err
		}
		var next []*node
		for _, u := range level {
			refs, err := w.resolve(u)
			if err != nil {
				return err
			}
			if u.dist >= w.opts.Depth {
				continue
			}
			for _, r := range refs {
				if r.kind != refNode {
					continue
				}
				if _, seen := w.nodes[r.target]; seen {
					continue
				}
				t, err := w.adopt(r, u.dist+1)
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

// prefetchLevel reads, in batches, what resolving a BFS level will read:
// the pins of every node in it with a local fact ref (GraphStore
// .OutgoingAtCommits), then the index rows of every version those refs
// cite (FactQuery.VersionMetasAt), per mount. resolve then answers from the
// reader's caches. It changes no answer (both batches equal their per-item
// reads) and reads nothing resolve would not.
func (w *walker) prefetchLevel(level []*node) error {
	if !versionPrefetch {
		return nil
	}
	type work struct {
		n     *node
		local []string
	}
	byMount := map[*mount][]work{}
	var mounts []*mount
	for _, n := range level {
		if n.resolved || n.m == nil {
			continue
		}
		var local []string
		seen := map[string]bool{}
		for _, raw := range n.f.Refs {
			c := fact.ClassifyRef(raw, n.m.id12)
			if c.Kind != fact.RefLocalFact || c.Path == n.rel || seen[c.Path] {
				continue
			}
			seen[c.Path] = true
			local = append(local, c.Path)
		}
		if len(local) == 0 {
			continue
		}
		if _, ok := byMount[n.m]; !ok {
			mounts = append(mounts, n.m)
		}
		byMount[n.m] = append(byMount[n.m], work{n, local})
	}
	for _, m := range mounts {
		ws := byMount[m]
		var pairs []index.PathAtCommit
		if w.opts.Unpin {
			for _, x := range ws {
				for _, p := range x.local {
					pairs = append(pairs, index.PathAtCommit{Path: p, Commit: m.anchor})
				}
			}
		} else {
			roots := make([]index.PathAtCommit, len(ws))
			for i, x := range ws {
				roots[i] = index.PathAtCommit{Path: x.n.rel, Commit: x.n.commit}
			}
			if err := w.r.prefetchPins(w.ctx, m, roots); err != nil {
				return err
			}
			for _, x := range ws {
				pins, err := w.r.pins(w.ctx, m, x.n.rel, x.n.commit)
				if err != nil {
					return err
				}
				for _, p := range x.local {
					if c, ok := pins[p]; ok {
						pairs = append(pairs, index.PathAtCommit{Path: p, Commit: c})
					}
				}
			}
		}
		if len(pairs) > readerCacheMax/2 {
			continue // too many to hold until used: resolve reads them per node
		}
		if err := w.r.prefetchMetas(w.ctx, m, pairs); err != nil {
			return err
		}
	}
	return nil
}

// order is the reading order: ancestors topologically, then the root, then
// a pre-order depth-first walk from the root in refs order that writes each
// included node at its first visit. An ancestor is never revisited or
// expanded: a ref to one is a back-reference.
func (w *walker) order(root *node, ancestors []*node) []*node {
	out := append([]*node{}, ancestors...)
	out = append(out, root)
	written := map[key]bool{root.key: true}
	for _, a := range ancestors {
		written[a.key] = true
	}
	var visit func(u *node)
	visit = func(u *node) {
		for _, r := range u.refs {
			if r.kind != refNode || written[r.target] {
				continue
			}
			t := w.nodes[r.target]
			if t == nil || !t.included {
				continue
			}
			if w.pinDirect && u != root && t.dist == 1 {
				continue // the root's own child, placed in its turn
			}
			written[t.key] = true
			t.parent = u
			out = append(out, t)
			visit(t)
		}
	}
	visit(root)
	return out
}

// wire renders a key as addressed on the wire: bare in the binding's write
// repo, kb://<id12>/… everywhere else.
func (w *walker) wire(k key) string {
	if k.repo == w.writeID {
		return k.rel
	}
	return fact.KBScheme + k.repo + "/" + k.rel
}

// document turns the ordered nodes into the output shape.
func (w *walker) document(root *node, order []*node, frame bool) (*Document, error) {
	doc := &Document{
		Root:      w.wire(root.key),
		RootTitle: root.f.Title,
		Anchor:    root.m.anchor,
		Order:     OrderRefs,
		Options:   w.opts,
	}
	if frame {
		doc.Order = OrderFrame
	}
	ord := make(map[key]int, len(order))
	anchors := make(map[key]string, len(order))
	usedAnchor := map[string]int{}
	for i, n := range order {
		ord[n.key] = i + 1
		id := anchorIDPrefix + shortID(n.rel)
		usedAnchor[id]++
		if c := usedAnchor[id]; c > 1 {
			id = fmt.Sprintf("%s-%d", id, c)
		}
		anchors[n.key] = id
	}
	cut := map[string]map[key]bool{ReasonDepth: {}, ReasonSize: {}}
	prevSection := ""

	for i, n := range order {
		out := Node{
			N:          i + 1,
			Path:       w.wire(n.key),
			Repo:       n.repo,
			Commit:     n.commit,
			Depth:      n.dist,
			Role:       RoleRef,
			Title:      n.f.Title,
			Type:       string(n.f.Type),
			Kind:       kindOf(n.f),
			Confidence: n.f.Confidence,
			Date:       w.r.date(w.ctx, n.m, n.rel, n.commit),
			AnchorID:   anchors[n.key],
		}
		switch {
		case n.ancestor:
			out.Role = RoleAncestor
		case n == root && frame:
			out.Role = RoleFocal
		case n == root:
			out.Role = RoleRoot
		}
		if n.latest {
			out.Marks = []string{MarkLatestOtherKB}
		}
		if frame {
			g := sectionStandsOn
			switch {
			case n.ancestor:
				g = sectionAncestors
			case n == root:
				g = sectionThisFrame
			}
			if g != prevSection {
				out.Section = g
			}
			prevSection = g
		}
		for _, r := range n.refs {
			switch r.kind {
			case refSource:
				out.Sources = append(out.Sources, r.source)
			case refOmit:
				if !n.ancestor {
					out.NotIncluded = append(out.NotIncluded, r.omit)
				}
			case refNode:
				t := w.nodes[r.target]
				if t != nil && t.included {
					l := Link{N: ord[t.key], Title: t.f.Title, ID: shortID(t.rel), AnchorID: anchors[t.key]}
					// One version per path: a citation of another version is
					// marked only when the BODY differs (a commit difference
					// alone is the 77%-false-alarm proxy).
					if !w.opts.Unpin && r.commit != t.commit {
						differ, err := w.r.bodiesDiffer(w.ctx, t.m, t.rel, r.commit, r.blob, t.commit, t.blob)
						if err != nil {
							return nil, err
						}
						l.DifferentVersion = differ
					}
					out.BuildsOn = append(out.BuildsOn, l)
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
				out.NotIncluded = append(out.NotIncluded, Omission{
					Title: r.f.Title, ID: shortID(r.target.rel), Path: w.wire(r.target), Reason: reason,
				})
			}
		}
		doc.Nodes = append(doc.Nodes, out)
	}
	doc.Truncated = Truncation{Depth: len(cut[ReasonDepth]), Nodes: len(cut[ReasonSize])}
	return doc, nil
}

func kindOf(f fact.Fact) string {
	if f.Kind == "" {
		return string(fact.DefaultKind)
	}
	return string(f.Kind)
}

// shortID is a fact's short id: its file name without .md, at most 8
// characters (fact files are named by an 8-hex uuid prefix).
func shortID(rel string) string {
	id := strings.TrimSuffix(path.Base(rel), ".md")
	if len(id) > shortIDLen {
		id = id[:shortIDLen]
	}
	return id
}
