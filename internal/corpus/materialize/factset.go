package materialize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/sessions"
)

// A FACTSET materializes a search: every result is one unit, called a "Fact",
// in search order, and each unit is that result's materialization, walked by
// the same walk as Plan (walkUnit) with ONE visited set carried from unit to
// unit. Before any unit is walked, the set RESERVES two levels (user
// rulings, 2026-10-29): the frozen search results, each for its own unit,
// and each result's direct references, each for the first unit that shows
// it at depth 1 (reserveDirectRefs). Then:
//   - a reached fact that is a search result is a link "(see N)" to its own
//     unit, where it is written as the root; the walk continues past it;
//   - a reached fact reserved as another unit's direct reference, not
//     walked yet, is a link "(see N.k)" forward to its fixed number there,
//     and stops there;
//   - a reached fact an earlier unit already wrote is a link "(see N.x)"
//     back to that first appearance, and its leftover children are not
//     written here (the link node is terminal);
//   - every other fact is written where the walk first reaches it.
//
// Units are walked lazily, in order, as pages need them, and each walked
// unit is appended to the factset's structure (sessions.FactSet: in memory,
// spilled to a file of its own only when it must): rows, the visits of what
// it wrote, its anchors. A seek to unit N walks the units before N first if
// they are not walked yet.

// FactSet bounds. Values and reasons: the experiment's decisions fact and the
// PR report.
const (
	// DefaultFactSetFacts is the Facts control's default: every result, up to
	// the cap (design §4).
	DefaultFactSetFacts = MaxFactSetFacts
	// MaxFactSetFacts caps the units, the same ceiling as max_nodes.
	MaxFactSetFacts = 500
	// MaxFactSetRows caps a factset's sections (written and links). The walk
	// that would pass it is refused (ErrFactSetTooLarge); the units already
	// walked still page.
	MaxFactSetRows = 100_000
	// MaxConcurrentFactSetBuilds caps the calls WALKING factset units at once,
	// server-wide (a first call, or a page, seek or export that needs
	// unwalked units); a call over it is refused at once (ErrFactSetBusy,
	// retry-able). A page served from walked units takes no slot, and an
	// export gives its slot back before it streams. This cap, not the
	// in-memory factset budget (session.factset_memory_mib, which bounds factsets at
	// rest), is what bounds the walks' working memory: readers and version
	// caches, measured at 2-3x the accounted size of what they append.
	MaxConcurrentFactSetBuilds = 4
)

// The factset's tool-session value: "materialize-factset:<limit>".
func factsetTool(limit int) string { return sessions.FactSetToolPrefix + strconv.Itoa(limit) }

func parseFactSetTool(tool string) (int, bool) {
	rest, ok := strings.CutPrefix(tool, sessions.FactSetToolPrefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(rest)
	return n, err == nil && n >= 1
}

var (
	// ErrFactSetBusy: MaxConcurrentFactSetBuilds calls are walking factsets; retry.
	ErrFactSetBusy = errors.New("too many factsets are being built right now; retry shortly")
	// ErrFactSetTooLarge: walking on would pass MaxFactSetRows.
	ErrFactSetTooLarge = errors.New("factset too large")
	// ErrCursorInUse: another call is reading this cursor right now (a 409, not
	// pages.go's ErrCursorBusy, a busy store). A
	// cursor has one reader at a time.
	ErrCursorInUse = errors.New("the cursor is being read by another call")
	// ErrStoreBusy: the session store's write lock could not be taken in
	// time; nothing changed, retry.
	ErrStoreBusy = sessions.ErrBusy
)

// buildSlots is the server-wide cap on calls walking factset units.
var buildSlots = make(chan struct{}, MaxConcurrentFactSetBuilds)

// maxFactSetRows is MaxFactSetRows; a test lowers it to drive the cap, or raises
// it to measure a factset past it.
var maxFactSetRows = MaxFactSetRows

// ReserveFactSetBuildsForTest takes n build slots and returns their release,
// so a test can drive the cap without timing. Never called in production.
func ReserveFactSetBuildsForTest(n int) func() {
	for range n {
		buildSlots <- struct{}{}
	}
	return func() {
		for range n {
			<-buildSlots
		}
	}
}

// FactSetRequest is a factset's first call.
type FactSetRequest struct {
	Binding *repos.Binding
	Manager *repos.Manager
	MayRead func(repo string) bool
	Search  FactSetSearch
	// PathsIn narrows the search to an explicit set (since_fork); NoResults
	// says the set is empty, so the search is not run at all.
	PathsIn   []string
	NoResults bool
	// Facts is the most units (search results), 1..MaxFactSetFacts.
	Facts   int
	Options Options
	// Embed, when set, embeds the text query (the REST search's server
	// embedder); nil leaves it to the store, as knomit_query does.
	Embed func(ctx context.Context, text string) ([]float32, error)
	// Limit is the page size, frozen in the cursor (1..HumanMaxLimit).
	Limit int
}

// FactSetEnv is what every page of a factset repeats.
type FactSetEnv struct {
	Mode                string        `json:"mode"`
	Title               string        `json:"title"`
	SearchLine          string        `json:"search_line"`
	Search              FactSetSearch `json:"search"`
	Anchor              string        `json:"anchor"`
	Audience            string        `json:"audience"`
	Unpinned            bool          `json:"unpinned"`
	CrossKB             bool          `json:"cross_kb"`
	Depth               int           `json:"depth"`
	MaxNodes            int           `json:"max_nodes"`
	FactsLimit          int           `json:"facts_limit"`
	Units               int           `json:"units"`
	InstructionsVersion string        `json:"instructions_version"`
}

// FactSetMode is FactSetEnv.Mode.
const FactSetMode = "factset"

// FactSetPage is one page of a factset: whole sections in factset order.
type FactSetPage struct {
	FactSetEnv
	// UnitsWalked and Sections are how far the factset is walked so far
	// (units are walked as pages need them): Sections is exact for the
	// walked units only.
	UnitsWalked  int           `json:"units_walked"`
	Sections     int           `json:"sections"`
	Instructions string        `json:"instructions,omitempty"`
	Facts        []FactSetNode `json:"facts"`
	SeqFrom      int           `json:"seq_from"`
	SeqTo        int           `json:"seq_to"`
	UnitFrom     int           `json:"unit_from"`
	UnitTo       int           `json:"unit_to"`
	// Cursor stays set on every page of a non-empty factset, the last one
	// included, so a reader can still seek (unit); a plain resume past the
	// end is refused like an exhausted cursor.
	Cursor  *string `json:"cursor"`
	HasMore bool    `json:"has_more"`
}

// FactSetNode is one appearance of a fact. Node is the unit's walk node: n is
// its ordinal inside the unit, builds_on point inside the unit (their
// anchor_id is where the fact is written). A written node carries the body;
// a link node carries no body, no builds_on, not_included or sources (it is
// terminal: the walk did not expand it here), and Link.
type FactSetNode struct {
	Node
	Unit      int    `json:"unit"`
	Seq       int    `json:"seq"`
	Number    string `json:"number"`
	TreeDepth int    `json:"tree_depth"`
	// Parent is the n of the node's tree parent in its unit; 0 for the unit
	// root and a frame's ancestors.
	Parent  int          `json:"parent,omitempty"`
	Written bool         `json:"written"`
	Link    *FactSetLink `json:"link,omitempty"`
	// Unavailable is a unit root whose search result has no version at the
	// factset's anchor (deleted after the search): a heading alone, under the
	// title the search listed, "(no longer available)"; written is false
	// and there is no link. Other units' links to it still point here.
	Unavailable bool `json:"unavailable,omitempty"`
	// UnitTruncated is the unit's own walk cuts, on its root.
	UnitTruncated *Truncation `json:"unit_truncated,omitempty"`
}

// FactSetLink is where a heading-only appearance's fact is written. A forward
// link to a unit not walked yet knows its unit, number and anchor; its n
// and seq are 0 until that unit is walked (seek with the unit).
type FactSetLink struct {
	Unit             int    `json:"unit"`
	N                int    `json:"n,omitempty"`
	Seq              int    `json:"seq,omitempty"`
	Number           string `json:"number"`
	AnchorID         string `json:"anchor_id"`
	Direction        string `json:"direction"`
	DifferentVersion bool   `json:"different_version,omitempty"`
}

// Link directions.
const (
	LinkForward = "forward"
	LinkBack    = "back"
)

// factsetTarget is a visited-set hit: where the fact is (or will be) written,
// and the version written there.
type factsetTarget struct {
	unit, n, seq   int
	number, anchor string
	commit, blob   string
}

// FactSetBuildStats is what one call's walking measured.
type FactSetBuildStats struct {
	UnitsWalked int
	Rows        int
	Walk        WalkStats
}

// factset is one call's handle on a factset: its cursor scope, file and reader.
type factset struct {
	ctx     context.Context
	sc      CursorScope
	id      string
	env     FactSetEnv
	limit   int
	f       *sessions.FactSet
	r       *reader
	b       *repos.Binding
	req     Request // the per-unit walk request (File set per unit)
	results []sessions.FactSetResult
	byPath  map[string]sessions.FactSetResult
	walked  int
	rows    int
	slot    bool
	stats   FactSetBuildStats
}

func (bk *factset) close() {
	bk.releaseSlot()
	if bk.r != nil {
		bk.r.close()
	}
	if bk.f != nil {
		_ = bk.f.Close()
	}
}

// FactSetFirstPage runs the search, freezes its results in a new factset (the
// owner's previous factset is replaced), walks as many units as page 1 needs
// and returns page 1 under a cursor owned by sc.
func FactSetFirstPage(ctx context.Context, sc CursorScope, req FactSetRequest) (FactSetPage, FactSetBuildStats, error) {
	if err := req.Options.Validate(); err != nil {
		return FactSetPage{}, FactSetBuildStats{}, err
	}
	if req.Facts < 1 || req.Facts > MaxFactSetFacts {
		return FactSetPage{}, FactSetBuildStats{}, fmt.Errorf("%w: facts must be between 1 and %d, got %d", ErrInvalid, MaxFactSetFacts, req.Facts)
	}
	if req.Limit < 1 || req.Limit > HumanMaxLimit {
		return FactSetPage{}, FactSetBuildStats{}, fmt.Errorf("%w: limit must be between 1 and %d, got %d", ErrInvalid, HumanMaxLimit, req.Limit)
	}
	if err := req.Search.validate(); err != nil {
		return FactSetPage{}, FactSetBuildStats{}, err
	}
	if req.Binding.IsLens() {
		return FactSetPage{}, FactSetBuildStats{}, fmt.Errorf("%w: a factset materializes one repository's search; this binding is a lens", ErrInvalid)
	}
	if !takeSlot() {
		return FactSetPage{}, FactSetBuildStats{}, ErrFactSetBusy
	}
	r := newReader(req.Binding, req.Manager, req.MayRead)
	bk := &factset{ctx: ctx, sc: sc, r: r, b: req.Binding, limit: req.Limit, slot: true}
	defer bk.close()

	wm, err := r.open(req.Binding.Write(), req.Binding.WriteMountBranch())
	if err != nil {
		return FactSetPage{}, bk.stats, err
	}
	hits, err := runFactSetSearch(ctx, wm, req)
	if err != nil {
		return FactSetPage{}, bk.stats, err
	}
	anchor, err := wm.svc.HeadCommit(ctx, wm.branch)
	if err != nil {
		return FactSetPage{}, bk.stats, fmt.Errorf("resolve HEAD of %s: %w", wm.branch, err)
	}
	write := req.Binding.Write()
	bk.env = FactSetEnv{
		Mode: FactSetMode, Title: FactSetTitle(req.Search, write.OntologyRoot(), write.Name()),
		SearchLine: SearchLine(req.Search), Search: req.Search, Anchor: anchor, Audience: AudienceHuman,
		Unpinned: req.Options.Unpin, CrossKB: req.Options.CrossKB, Depth: req.Options.Depth,
		MaxNodes: req.Options.MaxNodes, FactsLimit: req.Facts, Units: len(hits), InstructionsVersion: FactSetInstructionsVersion,
	}
	if len(hits) == 0 {
		return FactSetPage{FactSetEnv: bk.env, Facts: []FactSetNode{}}, bk.stats, nil
	}
	envJSON, _ := json.Marshal(bk.env)
	results, used := reserveRootAnchors(hits)
	unitReq := Request{
		Binding: req.Binding, Manager: req.Manager, MayRead: req.MayRead, Commit: anchor,
		Options: Options{Depth: req.Options.Depth, MaxNodes: req.Options.MaxNodes, Unpin: req.Options.Unpin, CrossKB: req.Options.CrossKB},
	}
	reserved, err := reserveDirectRefs(ctx, r, unitReq, results, used)
	if err != nil {
		return FactSetPage{}, bk.stats, err
	}
	id, err := sc.Sessions.CreateFactSet(ctx, factsetTool(req.Limit), sc.Branch, sc.Pin, sc.ReadSet, sc.Owner, string(envJSON), results, reserved)
	if err != nil {
		return FactSetPage{}, bk.stats, err
	}
	bk.id = id
	f, ok := sc.Sessions.OpenFactSet(id)
	if !ok {
		_ = sc.Sessions.DeleteFactSet(context.WithoutCancel(ctx), id)
		return FactSetPage{}, bk.stats, ErrCursorInUse
	}
	bk.f = f
	if err := bk.load(req.Manager, req.MayRead); err != nil {
		return FactSetPage{}, bk.stats, err
	}
	page, err := bk.page(1, 1, true)
	if err != nil {
		// The caller never got the cursor: nothing may stay behind.
		_ = sc.Sessions.DeleteFactSet(context.WithoutCancel(ctx), id)
	}
	return page, bk.stats, err
}

func takeSlot() bool {
	select {
	case buildSlots <- struct{}{}:
		return true
	default:
		return false
	}
}

// reserveRootAnchors is every unit root's anchor, "f-<id8>", suffixed -2,
// -3 in unit order when results share an id8: reserved up front so a
// forward link to a unit not walked yet already knows its anchor.
func reserveRootAnchors(hits []factsetHit) ([]sessions.FactSetResult, map[string]int) {
	used := map[string]int{}
	out := make([]sessions.FactSetResult, len(hits))
	for i, h := range hits {
		out[i] = sessions.FactSetResult{Unit: i + 1, Path: h.path, Anchor: nextAnchor(used, h.path), Title: h.title}
	}
	return out, used
}

// nextAnchor is "f-<id8>" of wire, suffixed -2, -3 when an earlier
// reservation took it.
func nextAnchor(used map[string]int, wire string) string {
	a := anchorIDPrefix + shortID(relOf(wire))
	used[a]++
	if c := used[a]; c > 1 {
		a = fmt.Sprintf("%s-%d", a, c)
	}
	return a
}

// reserveDirectRefs is the second level of the factset's reservation (user
// ruling, 2026-10-29), computed before any unit is walked: every unit
// root's DIRECT references, the ones its own walk will show at depth 1, are
// reserved for that unit, so an earlier unit's deeper walk links forward
// to them instead of writing them.
//   - Owner: the first unit, in result order, whose walk shows the fact at
//     depth 1. A unit shows only what its own limits admit: nothing at
//     Depth 0, and with max_nodes = k + 1 only the first k depth-1 entries
//     (fact refs that resolve, in refs order; link nodes count, as in the
//     walk). This mirrors walker.bfs's first level exactly, so a reserved
//     fact is always written by its owner, never linked to and written
//     nowhere.
//   - A search result is never reserved: it keeps its own unit.
//   - Number: "<unit>.<k>", k its place among the root's depth-1 entries;
//     the walk pins every depth-1 entry as a child of the root, in refs
//     order (walker.pinDirect), so the number holds once the unit is
//     walked.
//   - A frame root, or a result gone at the anchor, reserves nothing.
//
// Below the direct references nothing is reserved: the first unit to reach
// a fact writes it.
func reserveDirectRefs(ctx context.Context, r *reader, req Request, results []sessions.FactSetResult, used map[string]int) ([]sessions.FactSetReserve, error) {
	if req.Options.Depth < 1 || req.Options.MaxNodes < 2 {
		return nil, nil
	}
	isResult := make(map[string]bool, len(results))
	for _, res := range results {
		isResult[res.Path] = true
	}
	taken := map[string]bool{}
	var out []sessions.FactSetReserve
	writeID := fact12(req.Binding)
	for _, res := range results {
		w := &walker{ctx: ctx, r: r, opts: req.Options, writeID: writeID, nodes: map[key]*node{}}
		ureq := req
		ureq.File = res.Path
		root, err := w.root(ureq)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if root.f.Type == fact.Frame {
			continue
		}
		refs, err := w.resolve(root)
		if err != nil {
			return nil, err
		}
		included := len(w.incl)
		k := 0
		for _, ref := range refs {
			if ref.kind != refNode {
				continue
			}
			if included >= req.Options.MaxNodes {
				break
			}
			included++
			k++
			wire := w.wire(ref.target)
			if isResult[wire] || taken[wire] {
				continue
			}
			taken[wire] = true
			out = append(out, sessions.FactSetReserve{
				Path: wire, Unit: res.Unit, Number: fmt.Sprintf("%d.%d", res.Unit, k),
				Anchor: nextAnchor(used, wire), Commit: ref.commit, Blob: ref.blob,
			})
		}
		r.forget()
	}
	return out, nil
}

// load reads the factset's frozen results, walked extent and cross-KB tips.
func (bk *factset) load(mgr *repos.Manager, mayRead func(string) bool) error {
	var err error
	if bk.results, err = bk.f.Results(bk.ctx); err != nil {
		return err
	}
	bk.byPath = make(map[string]sessions.FactSetResult, len(bk.results))
	for _, res := range bk.results {
		bk.byPath[res.Path] = res
	}
	if bk.walked, bk.rows, err = bk.f.Walked(bk.ctx); err != nil {
		return err
	}
	if bk.r.tips, err = bk.f.Tips(bk.ctx); err != nil {
		return err
	}
	bk.req = Request{
		Binding: bk.b, Manager: mgr, MayRead: mayRead, Commit: bk.env.Anchor,
		Options: Options{Depth: bk.env.Depth, MaxNodes: bk.env.MaxNodes, Unpin: bk.env.Unpinned, CrossKB: bk.env.CrossKB},
	}
	return nil
}

// walkTo walks units until at least rows sections exist and units are
// walked, or the factset is fully walked. Walking takes a build slot, once per
// call.
func (bk *factset) walkTo(units, rows int) error {
	for bk.walked < len(bk.results) && (bk.walked < units || bk.rows < rows) {
		if !bk.slot {
			if !takeSlot() {
				return ErrFactSetBusy
			}
			bk.slot = true
		}
		if err := bk.walkUnit(bk.walked + 1); err != nil {
			return err
		}
	}
	return nil
}

// walkUnit walks unit and appends it to the factset file.
func (bk *factset) walkUnit(unit int) error {
	res := bk.results[unit-1]
	req := bk.req
	req.File = res.Path
	uw, err := walkUnit(bk.ctx, bk.r, req, func(n *node) (*factsetTarget, error) {
		return bk.visited(n, unit)
	})
	bk.r.forget()
	if errors.Is(err, ErrNotFound) {
		// The result is gone at the anchor (deleted between the search and
		// the HEAD the factset is walked at). Its unit is kept, as a heading
		// alone, the way the walk keeps a cited fact with no version as a
		// title with its reason: a result is never dropped, and the unit
		// numbers stay the result positions.
		uw, err = goneUnit(res, fact12(bk.b)), nil
	}
	if err != nil {
		return err
	}
	if bk.rows+len(uw.doc.Nodes) > maxFactSetRows {
		return fmt.Errorf("%w: it would hold more than %d sections (unit %d of %d); lower Facts, Depth or References",
			ErrFactSetTooLarge, maxFactSetRows, unit, len(bk.results))
	}
	rows, visits, anchors, err := bk.unitRows(uw, unit, res)
	if err != nil {
		return err
	}
	tips := map[string]string{}
	for id12, m := range bk.r.mounts {
		if m != nil && id12 != fact12(bk.b) && m.anchor != "" {
			tips[id12] = m.anchor
		}
	}
	if err := bk.f.AppendUnit(bk.ctx, unit, rows, visits, anchors, tips); err != nil {
		if errors.Is(err, sessions.ErrFactSetUnitTaken) {
			return ErrCursorInUse
		}
		return err
	}
	bk.walked, bk.rows = unit, bk.rows+len(rows)
	bk.stats.UnitsWalked++
	bk.stats.Rows += len(rows)
	bk.stats.Walk = bk.r.stats
	if h := factsetUnitWalkedHook; h != nil {
		h(bk.id, unit)
	}
	return nil
}

// goneUnit is the unit of a result that has no version at the anchor: its
// root alone, unavailable, under the title the search listed.
func goneUnit(res sessions.FactSetResult, writeID string) *unitWalk {
	n := Node{N: 1, Path: res.Path, Repo: writeID, Role: RoleRoot, Title: res.Title}
	return &unitWalk{doc: &Document{Root: res.Path, RootTitle: res.Title, Nodes: []Node{n}},
		parent: []int{-1}, blobs: []string{""}, links: []*factsetTarget{nil}, gone: true}
}

// factsetUnitWalkedHook, when set, runs after each unit is appended (a test
// seam: SetFactSetUnitWalkedHookForTest). Never set in production.
var factsetUnitWalkedHook func(cursor string, unit int)

// SetFactSetUnitWalkedHookForTest sets the hook run after every unit a factset
// call appends, and returns its restore. Never called in production.
func SetFactSetUnitWalkedHookForTest(h func(cursor string, unit int)) (restore func()) {
	old := factsetUnitWalkedHook
	factsetUnitWalkedHook = h
	return func() { factsetUnitWalkedHook = old }
}

// releaseSlot gives the call's build slot back (the call has finished
// walking).
func (bk *factset) releaseSlot() {
	if bk.slot {
		<-buildSlots
		bk.slot = false
	}
}

// visited is the factset's visited-set lookup for a node the walk of unit
// reached: its first written appearance if an earlier unit wrote it, else
// the unit of a search result (written there as the root), else nil.
func (bk *factset) visited(n *node, unit int) (*factsetTarget, error) {
	wire := (&walker{writeID: fact12(bk.b)}).wire(n.key)
	v, ok, err := bk.f.Visited(bk.ctx, wire)
	if err != nil {
		return nil, err
	}
	if ok {
		return &factsetTarget{unit: v.Unit, n: v.N, seq: v.Seq, number: v.Number, anchor: v.Anchor, commit: v.Commit, blob: v.Blob}, nil
	}
	res, isResult := bk.byPath[wire]
	if !isResult {
		// A direct reference another unit reserved: a forward link to its
		// fixed section there (it was reserved before any unit was walked;
		// had its unit been walked, the visited set would have answered).
		rv, reserved, err := bk.f.Reserved(bk.ctx, wire)
		if err != nil {
			return nil, err
		}
		if !reserved || rv.Unit == unit {
			return nil, nil
		}
		t := &factsetTarget{unit: rv.Unit, number: rv.Number, anchor: rv.Anchor}
		if !bk.env.Unpinned {
			t.commit, t.blob = rv.Commit, rv.Blob
		}
		return t, nil
	}
	if res.Unit == unit {
		return nil, nil
	}
	// A result not walked yet: written as its unit's root, at its version
	// live at the anchor.
	t := &factsetTarget{unit: res.Unit, number: strconv.Itoa(res.Unit), anchor: res.Anchor}
	if !bk.env.Unpinned && n.m != nil {
		if _, c, blob, ok, err := bk.r.latest(bk.ctx, n.m, n.rel); err == nil && ok {
			t.commit, t.blob = c, blob
		}
	}
	return t, nil
}

// fact12 is the binding's write repo id, the repo whose paths are bare.
func fact12(b *repos.Binding) string { return fact.ID12(b.Write().ID()) }

// unitRows turns one walked unit into rows from bk.rows+1: section numbers
// from the walk tree (the root is the unit number, its children unit.1,
// unit.2, ...; a frame's ancestors are unnumbered), anchors (a root's is
// reserved; a written fact's "f-<id8>" is unique in the factset; a link's is
// "u<unit>-<id8>"), links, and the visits of the facts it writes.
func (bk *factset) unitRows(uw *unitWalk, unit int, res sessions.FactSetResult) ([]sessions.FactSetRow, []sessions.FactSetVisit, []string, error) {
	nodes := uw.doc.Nodes
	number := make([]string, len(nodes))
	depth := make([]int, len(nodes))
	kids := make([]int, len(nodes))
	anchor := make([]string, len(nodes))
	usedHere := map[string]bool{}
	var newAnchors []string
	byN := make(map[int]int, len(nodes))
	for i, n := range nodes {
		byN[n.N] = i
		switch p := uw.parent[i]; {
		case n.Role == RoleRoot || n.Role == RoleFocal:
			number[i] = strconv.Itoa(unit)
		case p < 0:
			// A frame's ancestor: an unnumbered preamble, as in the single
			// view.
		default:
			kids[p]++
			number[i] = number[p] + "." + strconv.Itoa(kids[p])
			depth[i] = depth[p] + 1
		}
		id8 := shortID(relOf(n.Path))
		switch {
		case uw.links[i] != nil:
			anchor[i] = uniqueAnchor("u"+strconv.Itoa(unit)+"-"+id8, func(a string) (bool, error) { return usedHere[a], nil })
		case n.Role == RoleRoot || n.Role == RoleFocal:
			anchor[i] = res.Anchor
		default:
			rv, reserved, err := bk.f.Reserved(bk.ctx, n.Path)
			if err != nil {
				return nil, nil, nil, err
			}
			if reserved {
				// Written by the unit it was reserved for, at the number and
				// under the anchor forward links already carry.
				if rv.Unit != unit || rv.Number != number[i] {
					return nil, nil, nil, fmt.Errorf("factset: %s reserved as %s (unit %d) was walked as %s in unit %d", n.Path, rv.Number, rv.Unit, number[i], unit)
				}
				anchor[i] = rv.Anchor
				break
			}
			a, err := uniqueAnchorErr(anchorIDPrefix+id8, func(a string) (bool, error) {
				if usedHere[a] {
					return true, nil
				}
				return bk.f.AnchorTaken(bk.ctx, a)
			})
			if err != nil {
				return nil, nil, nil, err
			}
			anchor[i] = a
			newAnchors = append(newAnchors, a)
		}
		usedHere[anchor[i]] = true
	}
	rows := make([]sessions.FactSetRow, 0, len(nodes))
	var visits []sessions.FactSetVisit
	for i, n := range nodes {
		seq := bk.rows + i + 1
		bn := FactSetNode{Node: n, Unit: unit, Seq: seq, Number: number[i], TreeDepth: depth[i],
			Written: uw.links[i] == nil && !uw.gone, Unavailable: uw.gone}
		bn.AnchorID = anchor[i]
		if p := uw.parent[i]; p >= 0 {
			bn.Parent = nodes[p].N
		}
		if (n.Role == RoleRoot || n.Role == RoleFocal) && !uw.gone {
			t := uw.doc.Truncated
			bn.UnitTruncated = &t
		}
		for j := range bn.BuildsOn {
			l := &bn.BuildsOn[j]
			k := byN[l.N]
			l.AnchorID = anchor[k]
			if t := uw.links[k]; t != nil {
				l.AnchorID = t.anchor
			}
		}
		if t := uw.links[i]; t != nil {
			l := &FactSetLink{Unit: t.unit, N: t.n, Seq: t.seq, Number: t.number, AnchorID: t.anchor, Direction: LinkForward}
			if t.unit < unit {
				l.Direction = LinkBack
			}
			if !bk.env.Unpinned && t.commit != "" && n.Commit != t.commit {
				m, err := bk.r.mountOf(n.Repo)
				if err != nil {
					return nil, nil, nil, err
				}
				differ, err := bk.r.bodiesDiffer(bk.ctx, m, relOf(n.Path), n.Commit, uw.blobs[i], t.commit, t.blob)
				if err != nil {
					return nil, nil, nil, err
				}
				l.DifferentVersion = differ
			}
			bn.Link = l
			bn.BuildsOn, bn.NotIncluded, bn.Sources = nil, nil, nil
		} else {
			visits = append(visits, sessions.FactSetVisit{Path: n.Path, Unit: unit, N: n.N, Seq: seq, Number: number[i],
				Anchor: anchor[i], Commit: n.Commit, Blob: uw.blobs[i]})
		}
		state, err := json.Marshal(bn)
		if err != nil {
			return nil, nil, nil, err
		}
		rows = append(rows, sessions.FactSetRow{Seq: seq, Unit: unit, N: n.N, Path: n.Path, Written: bn.Written, State: string(state)})
	}
	return rows, visits, newAnchors, nil
}

func uniqueAnchor(base string, taken func(string) (bool, error)) string {
	a, _ := uniqueAnchorErr(base, taken)
	return a
}

// uniqueAnchorErr is base, or base-2, base-3, ... the first not taken.
func uniqueAnchorErr(base string, taken func(string) (bool, error)) (string, error) {
	a := base
	for k := 2; ; k++ {
		t, err := taken(a)
		if err != nil {
			return "", err
		}
		if !t {
			return a, nil
		}
		a = fmt.Sprintf("%s-%d", base, k)
	}
}

// UIListLimit is how many results the web UI's results list shows: the
// limit it sends to the repo /search route and to the /facts collection
// (web/src/api.ts, search and recent). A factset's first UIListLimit units are
// exactly that list.
const UIListLimit = 50

// factsetHit is one search result as the factset freezes it.
type factsetHit struct{ path, title string }

// runFactSetSearch runs the frozen search on the write mount and returns the
// ordered results, at most req.Facts.
//
// THE FIRST UIListLimit RESULTS ARE THE UI'S LIST, EXACTLY. A text search
// takes a nearest-neighbour window that grows with its limit, and its
// filters apply after that window, so the same search at limit 500 can rank
// results the 50-row list never showed, or list more than it did. The factset
// therefore runs the very call the UI makes (limit UIListLimit) and freezes
// its answer as the prefix. Only when that list is full and Facts asks for
// more does it search again at limit Facts and append, in that call's
// order, every result the prefix does not hold. A list the UI shows short
// (fewer than UIListLimit) is the whole factset: "you found 10, you see 10".
// Ties are ordered by path in the store (index.FactQuery.Search), so both
// calls are deterministic. A Facts below UIListLimit truncates the list.
func runFactSetSearch(ctx context.Context, m *mount, req FactSetRequest) ([]factsetHit, error) {
	if req.NoResults {
		return nil, nil
	}
	var vec []float32
	if req.Search.Sort != SortRecent && req.Search.Text != "" && req.Embed != nil {
		v, err := req.Embed(ctx, req.Search.Text)
		if err != nil {
			return nil, fmt.Errorf("search: embed query: %w", err)
		}
		if len(v) == 0 {
			return nil, errors.New("search: embed query: embedder returned an empty vector")
		}
		vec = v
	}
	hits, err := factsetSearchOnce(ctx, m, req, UIListLimit, vec)
	if err != nil {
		return nil, err
	}
	if len(hits) == UIListLimit && req.Facts > UIListLimit {
		more, err := factsetSearchOnce(ctx, m, req, req.Facts, vec)
		if err != nil {
			return nil, err
		}
		seen := make(map[string]bool, len(hits))
		for _, h := range hits {
			seen[h.path] = true
		}
		for _, h := range more {
			if !seen[h.path] {
				seen[h.path] = true
				hits = append(hits, h)
			}
		}
	}
	if len(hits) > req.Facts {
		hits = hits[:req.Facts]
	}
	return hits, nil
}

// factsetSearchOnce is one search call at limit, as the UI's list makes it:
// the repo /search route (sort relevance) or the /facts collection's
// sort=recent. The search returns the facts' rows (bodies included, a store
// detail); only their paths and titles are kept.
func factsetSearchOnce(ctx context.Context, m *mount, req FactSetRequest, limit int, vec []float32) ([]factsetHit, error) {
	q := req.Search.options(limit, req.PathsIn)
	var hits []factsetHit
	if req.Search.Sort == SortRecent {
		out, _, err := m.svc.FactQuery().RecentFacts(ctx, m.branch, q)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		for _, e := range out {
			hits = append(hits, factsetHit{e.Path, e.Title})
		}
		return hits, nil
	}
	q.QueryVec = vec
	out, err := m.svc.FactQuery().Search(ctx, m.branch, q)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	for _, e := range out {
		hits = append(hits, factsetHit{e.Path, e.Title})
	}
	return hits, nil
}

// openFactSet checks cursor against sc exactly as NextPage does (every
// mismatch is ErrCursorExpired) and opens the factset for this call.
func openFactSet(ctx context.Context, sc CursorScope, b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool, cursor string) (*factset, *sessions.FactSetInfo, error) {
	limit, info, err := ownedFactSet(ctx, sc, cursor)
	if err != nil {
		return nil, nil, err
	}
	bk := &factset{ctx: ctx, sc: sc, id: cursor, limit: limit, b: b, r: newReader(b, mgr, mayRead)}
	if err := json.Unmarshal([]byte(info.Env), &bk.env); err != nil {
		bk.close()
		return nil, nil, fmt.Errorf("factset envelope: %w", err)
	}
	f, ok := sc.Sessions.OpenFactSet(cursor)
	if !ok {
		// Swept or evicted: the cursor reads as expired, never an error.
		bk.close()
		return nil, nil, ErrCursorExpired
	}
	bk.f = f
	if err := bk.load(mgr, mayRead); err != nil {
		bk.close()
		return nil, nil, err
	}
	return bk, info, nil
}

// ownedFactSet is the factset cursor's frozen page size and position, with the
// refusals every cursor makes (NextPage, explain.go): a cursor is a frozen
// view of ONE binding's read set, owned by one endpoint and principal, and
// every mismatch, a cursor of another tool included, is ErrCursorExpired.
func ownedFactSet(ctx context.Context, sc CursorScope, cursor string) (int, *sessions.FactSetInfo, error) {
	session, err := sc.Sessions.GetToolSession(ctx, cursor)
	if err != nil {
		return 0, nil, fmt.Errorf("session lookup error: %w", err)
	}
	if session == nil || session.Status != "active" || session.Binding == "" ||
		session.Binding != sc.Pin || session.ReadSet != sc.ReadSet || session.Owner != sc.Owner {
		return 0, nil, ErrCursorExpired
	}
	limit, ok := parseFactSetTool(session.Tool)
	if !ok {
		return 0, nil, ErrCursorExpired
	}
	info, err := sc.Sessions.GetFactSet(ctx, cursor)
	if err != nil {
		return 0, nil, err
	}
	if info == nil {
		return 0, nil, ErrCursorExpired
	}
	return limit, info, nil
}

// IsFactSetCursor reports whether cursor names a factset THIS caller may read
// (sc's owner, pin and read set: the checks of every factset call), so an edge
// can route a resume to FactSetNextPage. Another owner's factset cursor is false,
// exactly like one that does not exist, so the edge's refusals cannot tell
// the two apart (fact d870cc94: an unauthorized cursor reads as expired).
func IsFactSetCursor(ctx context.Context, sc CursorScope, cursor string) (bool, error) {
	_, _, err := ownedFactSet(ctx, sc, cursor)
	if errors.Is(err, ErrCursorExpired) {
		return false, nil
	}
	return err == nil, err
}

// FactSetNextPage serves the next page of a factset cursor, or, with unit > 0,
// seeks: the page starts at that unit's first section (1..units; anything
// else is ErrInvalid), walking the units before it first if they are not
// walked yet. Every cursor this caller cannot be served is ErrCursorExpired
// and moves nothing; a call racing another on the same cursor is
// ErrCursorInUse.
func FactSetNextPage(ctx context.Context, sc CursorScope, b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool, cursor string, unit int) (FactSetPage, error) {
	page, _, err := FactSetNextPageStats(ctx, sc, b, mgr, mayRead, cursor, unit)
	return page, err
}

// FactSetNextPageStats is FactSetNextPage with what the call walked.
func FactSetNextPageStats(ctx context.Context, sc CursorScope, b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool, cursor string, unit int) (FactSetPage, FactSetBuildStats, error) {
	bk, info, err := openFactSet(ctx, sc, b, mgr, mayRead, cursor)
	if err != nil {
		return FactSetPage{}, FactSetBuildStats{}, err
	}
	defer bk.close()
	from := info.NextSeq
	if unit != 0 {
		if unit < 1 || unit > len(bk.results) {
			return FactSetPage{}, bk.stats, fmt.Errorf("%w: unit must be between 1 and %d, got %d", ErrInvalid, len(bk.results), unit)
		}
		if err := bk.walkTo(unit, 0); err != nil {
			return FactSetPage{}, bk.stats, err
		}
		if from, err = bk.f.UnitStart(ctx, unit); err != nil {
			return FactSetPage{}, bk.stats, err
		}
	} else {
		if err := bk.walkTo(0, from); err != nil {
			return FactSetPage{}, bk.stats, err
		}
		if from > bk.rows {
			return FactSetPage{}, bk.stats, ErrCursorExpired
		}
	}
	page, err := bk.page(info.NextSeq, from, false)
	return page, bk.stats, err
}

// page serves the page starting at seq from (walking units until the page
// can be filled), moving the cursor from at (its position when the call
// began) to after the page.
func (bk *factset) page(at, from int, first bool) (FactSetPage, error) {
	if err := bk.walkTo(0, from+bk.limit-1); err != nil {
		// At the row cap the units already walked still page: a short page
		// is served, and the call that needs the next unit gets the 422.
		if !errors.Is(err, ErrFactSetTooLarge) || bk.rows < from {
			return FactSetPage{}, err
		}
	}
	rows, err := bk.f.Rows(bk.ctx, from, bk.limit)
	if err != nil {
		return FactSetPage{}, err
	}
	if len(rows) == 0 {
		return FactSetPage{}, ErrCursorExpired
	}
	nodes, err := bk.inflate(rows)
	if err != nil {
		return FactSetPage{}, err
	}
	page, used, err := fillFactSetPage(bk.env, nodes, bk.limit, first)
	if err != nil {
		return FactSetPage{}, err
	}
	next := nodes[used-1].Seq + 1
	moved, err := bk.sc.Sessions.AdvanceFactSet(bk.ctx, bk.id, at, next)
	if err != nil {
		return FactSetPage{}, err
	}
	if !moved {
		// The position moved under this call (another reader: 409), or the
		// cursor is gone (reaped, replaced or deleted mid-call: the 404).
		info, err := bk.sc.Sessions.GetFactSet(bk.ctx, bk.id)
		if err != nil {
			return FactSetPage{}, err
		}
		if info == nil {
			return FactSetPage{}, ErrCursorExpired
		}
		return FactSetPage{}, ErrCursorInUse
	}
	id := bk.id
	page.Cursor = &id
	page.UnitsWalked, page.Sections = bk.walked, bk.rows
	page.HasMore = next <= bk.rows || bk.walked < len(bk.results)
	return page, nil
}

// inflate turns stored rows into page nodes with the written nodes' bodies
// read at their frozen commits. A row in a repo this caller cannot read
// makes the whole call ErrCursorExpired: the frozen view is not this
// caller's.
func (bk *factset) inflate(rows []sessions.FactSetRow) ([]FactSetNode, error) {
	out := make([]FactSetNode, len(rows))
	var bodies []int
	for i, row := range rows {
		if err := json.Unmarshal([]byte(row.State), &out[i]); err != nil {
			return nil, fmt.Errorf("factset row decode: %w", err)
		}
		if _, _, ok := bk.r.target(out[i].Repo); !ok {
			return nil, ErrCursorExpired
		}
		if l := out[i].Link; l != nil && l.Seq == 0 {
			// A forward link stored before its unit was walked: fill in
			// where the root landed once it has.
			v, ok, err := bk.f.Visited(bk.ctx, row.Path)
			if err != nil {
				return nil, err
			}
			if ok {
				l.N, l.Seq = v.N, v.Seq
			}
		}
		if row.Written {
			bodies = append(bodies, i)
		}
	}
	if len(bodies) > 0 {
		ns := make([]Node, len(bodies))
		for k, i := range bodies {
			ns[k] = out[i].Node
		}
		if err := bk.r.readBodies(bk.ctx, ns); err != nil {
			// The frozen version can no longer be read for this caller:
			// answered like expiry, naming nothing.
			return nil, ErrCursorExpired
		}
		for k, i := range bodies {
			out[i].Body = ns[k].Body
		}
	}
	return out, nil
}

// mountOf is the open mount of the repo with id12 (reach applies).
func (r *reader) mountOf(id12 string) (*mount, error) {
	ri, branch, ok := r.target(id12)
	if !ok {
		return nil, ErrCursorExpired
	}
	return r.open(ri, branch)
}

// fillFactSetPage is fillPage for a factset: whole sections, at most limit, at
// least one, measured against PageCap with a cursor present.
func fillFactSetPage(env FactSetEnv, nodes []FactSetNode, limit int, first bool) (FactSetPage, int, error) {
	build := func(k int) FactSetPage {
		p := FactSetPage{FactSetEnv: env, Facts: nodes[:k], SeqFrom: nodes[0].Seq, SeqTo: nodes[k-1].Seq,
			UnitFrom: nodes[0].Unit, UnitTo: nodes[k-1].Unit}
		if first {
			p.Instructions = FactSetInstructions
		}
		return p
	}
	placeholder := strings.Repeat("0", 36)
	used := 1
	for k := 2; k <= min(limit, len(nodes)); k++ {
		p := build(k)
		p.Cursor, p.HasMore = &placeholder, true
		p.UnitsWalked, p.Sections = MaxFactSetFacts, MaxFactSetRows
		out, err := json.Marshal(p)
		if err != nil {
			return FactSetPage{}, 0, err
		}
		if len(out) > PageCap {
			break
		}
		used = k
	}
	return build(used), used, nil
}
