package materialize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/model"
	"knomit/internal/engine/store/sessions"
)

// A FACTSET materializes a search: every result is one unit, in search order,
// and each unit is that result's materialization, planned by the streaming
// planner (factset_plan.go). Before any unit is planned, two levels are
// RESERVED (user rulings, 2026-10-29): the results, each for its own unit, and
// each result's direct references, each for the first unit, in result order,
// whose OWN limits show it at depth 1. A fact is written once; every other
// appearance is a link (forward to a later unit, with its number; back to
// where it was written, by anchor).
//
// Streaming (rulings of 2026-10-30): the FROZEN inputs (the results as (repo,
// path, commit, blob), the anchor = the write mount's index watermark, the
// frozen tips of every repo the caller can reach) are written ONCE, at
// creation, as one session-DB row. The written set and the reserved map live
// in an in-memory CACHE under a budget; a lost or evicted entry is rebuilt by
// replaying units without bodies. The reading position travels in the cursor
// token, so pages never write the session DB (only a heartbeat, at most once
// per quarter of the idle TTL).

// FactSet bounds.
const (
	// DefaultFactSetFacts is the Facts control's default: every result, up
	// to the cap.
	DefaultFactSetFacts = MaxFactSetFacts
	// MaxFactSetFacts caps the units, the same ceiling as max_nodes.
	MaxFactSetFacts = 500
	// MaxConcurrentFactSetBuilds caps the calls PLANNING FactSet units at
	// once, server-wide (a first call, or a page, seek or export that must
	// plan); one over it is refused at once (ErrFactSetBusy, retry-able). A
	// page served from a planned unit takes no slot, and an export holds one
	// only while it plans a unit, never while it writes.
	MaxConcurrentFactSetBuilds = 4
	// MaxConcurrentFactSetExports caps the exports streaming at once,
	// server-wide; one over it is refused at once (ErrFactSetExportsBusy).
	MaxConcurrentFactSetExports = 8
	// UIListLimit is how many results the web UI's results list shows: the
	// default limit of the repo /search route and of the /facts collection
	// (which the UI no longer sends). A FactSet's first UIListLimit units are
	// exactly that list.
	UIListLimit = 50
)

// FactSetMode is FactSetEnv.Mode.
const FactSetMode = "factset"

// Link directions.
const (
	LinkForward = "forward"
	LinkBack    = "back"
)

// RULE 1 marks (user ruling 2026-10-31): computed at PAGE time against what
// the READING branch holds NOW (branch_facts), by blob. Annotations only: they
// never change structure, numbers or links.
const (
	// MarkChangedSinceCited: the version shown (a written section) or cited
	// (a link) is not the one the reading branch holds now.
	MarkChangedSinceCited = "changed_since_cited"
	// MarkDeleted: the reading branch holds no live version of the fact now.
	MarkDeleted = "deleted"
)

var (
	// ErrFactSetBusy: MaxConcurrentFactSetBuilds calls are planning; retry.
	ErrFactSetBusy = errors.New("too many factsets are being built right now; retry shortly")
	// ErrFactSetExportsBusy: MaxConcurrentFactSetExports exports are
	// streaming; retry.
	ErrFactSetExportsBusy = errors.New("too many factsets are being exported right now; retry shortly")
	// ErrIndexMoved: the write mount's index advanced during the search,
	// twice; nothing was written; retry.
	ErrIndexMoved = errors.New("the index moved while the search ran; retry shortly")
	// ErrStoreBusy: the session store's write lock could not be taken in
	// time; nothing changed, retry.
	ErrStoreBusy = model.ErrSessionBusy
	// ErrFactIdentity: two different facts hashed to one FactHash. The call is
	// refused (500) rather than drop a subtree.
	ErrFactIdentity = errors.New("factset fact identity collision")
)

// FactSetRequest is a FactSet's first call.
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

// FactSetEnv is what every page of a FactSet repeats.
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

func (env FactSetEnv) options() Options {
	return Options{Depth: env.Depth, MaxNodes: env.MaxNodes, Unpin: env.Unpinned, CrossKB: env.CrossKB}
}

// FactSetPage is one page of a FactSet: whole sections in FactSet order.
type FactSetPage struct {
	FactSetEnv
	// UnitsWalked and Sections are how far the FactSet is planned so far
	// (units are planned as pages need them): Sections is exact for the
	// planned units only.
	UnitsWalked  int           `json:"units_walked"`
	Sections     int           `json:"sections"`
	Instructions string        `json:"instructions,omitempty"`
	Facts        []FactSetNode `json:"facts"`
	UnitFrom     int           `json:"unit_from"`
	UnitTo       int           `json:"unit_to"`
	// Cursor is the position after this page: "<id>.<unit>.<section>". It
	// stays set on every page of a non-empty FactSet, the last one included
	// (pointing past the end, so a reader can still seek with unit); a plain
	// page from the end is a 400.
	Cursor  *string `json:"cursor"`
	HasMore bool    `json:"has_more"`
}

// FactSetNode is one appearance of a fact. Node carries the section's data:
// n is its ordinal inside the unit, builds_on point inside the unit, and
// anchor_id is f-<16 hex> of the fact (the same in every appearance). A
// written node carries the body; a link node carries no body, builds_on,
// not_included or sources (it is terminal), and Link.
type FactSetNode struct {
	Node
	Unit      int    `json:"unit"`
	Number    string `json:"number"`
	TreeDepth int    `json:"tree_depth"`
	// Parent is the n of the node's tree parent in its unit; 0 for the unit
	// root and a frame's ancestors.
	Parent  int          `json:"parent,omitempty"`
	Written bool         `json:"written"`
	Link    *FactSetLink `json:"link,omitempty"`
	// Unavailable is a unit root whose search result has no version at the
	// FactSet's anchor: a heading alone, under the title the search listed.
	Unavailable bool `json:"unavailable,omitempty"`
	// UnitTruncated is the unit's own walk cuts, on its root.
	UnitTruncated *Truncation `json:"unit_truncated,omitempty"`
}

// FactSetLink says where a heading-only appearance's fact is written: its
// anchor always; for a forward link also the unit and the number there ("7"
// for a result, "7.3" for a reserved direct reference). A back link carries
// the anchor only: the reader has already seen the target (links are anchors
// the renderer resolves). CitedBlob is the version cited here;
// DifferentVersion says it is not the version written (ruling Q3: blob
// identity, so ANY change).
type FactSetLink struct {
	Unit             int    `json:"unit,omitempty"`
	Number           string `json:"number,omitempty"`
	AnchorID         string `json:"anchor_id"`
	Direction        string `json:"direction"`
	CitedBlob        string `json:"cited_blob"`
	DifferentVersion bool   `json:"different_version,omitempty"`
}

// ---- frozen inputs ------------------------------------------------------

// frozenTip is a repo's read branch and the indexed commit it was frozen at.
type frozenTip struct {
	Branch string `json:"b"`
	Commit string `json:"c"`
}

// frozenRoot is one result as frozen: its version at the anchor.
type frozenRoot struct {
	Path   string `json:"p"`
	Commit string `json:"c,omitempty"`
	Blob   string `json:"b,omitempty"`
	Title  string `json:"t"`
	Date   int64  `json:"d,omitempty"`
}

// factsetFrozen is the ONE row a FactSet writes (factsets.frozen).
type factsetFrozen struct {
	V         int                  `json:"v"`
	Env       FactSetEnv           `json:"env"`
	Limit     int                  `json:"limit"`
	WriteRepo string               `json:"write_repo"`
	Branch    string               `json:"branch"`
	Tips      map[string]frozenTip `json:"tips,omitempty"`
	Roots     []frozenRoot         `json:"roots"`
}

const frozenVersion = 1

// factsetTool is the FactSet's tool-session value: "materialize-factset:<limit>".
func factsetTool(limit int) string { return sessions.FactSetToolPrefix + strconv.Itoa(limit) }

// ---- cursor token -----------------------------------------------------------

// fpos is a reading position: unit 1..units, section 0..len(unit)-1; unit
// units+1, section 0 is the end.
type fpos struct{ unit, sec int }

// cursorToken is "<id>.<unit>.<section>": opaque to clients, parsed here.
func cursorToken(id string, p fpos) string {
	return id + "." + strconv.Itoa(p.unit) + "." + strconv.Itoa(p.sec)
}

// parseCursor splits a token; ok is false for anything malformed (which
// reads like a cursor that does not exist).
func parseCursor(tok string) (string, fpos, bool) {
	i := strings.LastIndexByte(tok, '.')
	if i <= 0 {
		return "", fpos{}, false
	}
	j := strings.LastIndexByte(tok[:i], '.')
	if j <= 0 {
		return "", fpos{}, false
	}
	u, err1 := strconv.Atoi(tok[j+1 : i])
	s, err2 := strconv.Atoi(tok[i+1:])
	if err1 != nil || err2 != nil || u < 0 || s < 0 {
		return "", fpos{}, false
	}
	return tok[:j], fpos{u, s}, true
}

// ---- slots, heartbeat ---------------------------------------------------------

var (
	buildSlots  = make(chan struct{}, MaxConcurrentFactSetBuilds)
	exportSlots = make(chan struct{}, MaxConcurrentFactSetExports)
)

// BuildSlotsInUse is how many planning slots calls hold right now.
func BuildSlotsInUse() int { return len(buildSlots) }

// ExportSlotsInUse is how many export slots calls hold right now.
func ExportSlotsInUse() int { return len(exportSlots) }

func takeSlot(ch chan struct{}) bool {
	select {
	case ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// factsetIdleTTL is session.tool_idle_ttl: the reaper deletes a FactSet idle
// longer than it, and the heartbeat is written once per quarter of it.
var factsetIdleTTL atomic.Int64

func init() { factsetIdleTTL.Store(int64(15 * time.Minute)) }

// SetFactSetIdleTTL sets the idle TTL the heartbeat keeps ahead of
// (session.tool_idle_ttl); a non-positive value is ignored.
func SetFactSetIdleTTL(d time.Duration) {
	if d > 0 {
		factsetIdleTTL.Store(int64(d))
	}
}

// factsetNow is the heartbeat's clock (a test seam freezes it).
var factsetNow = time.Now

// ---- creation -----------------------------------------------------------------

// factsetHit is one search result as the FactSet freezes it.
type factsetHit struct {
	path, title  string
	commit, blob string
	date         int64
}

// factsetSearchHook runs between the search and the second watermark read (a
// test seam for the race check). nil in production.
var factsetSearchHook func()

// FactSetFirstPage runs the search at the write mount's index watermark,
// freezes the results, the anchor and (with cross_kb) the tips of every repo
// the caller can reach, writes the ONE session row (replacing the owner's
// previous FactSet), builds the reservation in memory and returns page 1.
func FactSetFirstPage(ctx context.Context, sc CursorScope, req FactSetRequest) (FactSetPage, Stats, error) {
	var st Stats
	if err := req.Options.Validate(); err != nil {
		return FactSetPage{}, st, err
	}
	if req.Facts < 1 || req.Facts > MaxFactSetFacts {
		return FactSetPage{}, st, fmt.Errorf("%w: facts must be between 1 and %d, got %d", ErrInvalid, MaxFactSetFacts, req.Facts)
	}
	if req.Limit < 1 || req.Limit > HumanMaxLimit {
		return FactSetPage{}, st, fmt.Errorf("%w: limit must be between 1 and %d, got %d", ErrInvalid, HumanMaxLimit, req.Limit)
	}
	if err := req.Search.validate(); err != nil {
		return FactSetPage{}, st, err
	}
	if req.Binding.IsLens() {
		return FactSetPage{}, st, fmt.Errorf("%w: a factset materializes one repository's search; this binding is a lens", ErrInvalid)
	}
	if sc.Pin == "" || sc.Owner == "" {
		return FactSetPage{}, st, fmt.Errorf("%w: a factset cursor needs an owner and a binding identity", ErrInvalid)
	}
	if !takeSlot(buildSlots) {
		return FactSetPage{}, st, ErrFactSetBusy
	}
	c := &fsCall{ctx: ctx, b: req.Binding, mgr: req.Manager, mayRead: req.MayRead, slot: true}
	c.r = newReader(req.Binding, req.Manager, req.MayRead, &c.st)
	defer c.close()

	wm, err := c.r.open(req.Binding.Write(), req.Binding.WriteMountBranch())
	if err != nil {
		return FactSetPage{}, c.st, err
	}
	if err := c.r.checkReady(ctx, wm); err != nil {
		return FactSetPage{}, c.st, err
	}
	anchor, hits, err := c.searchAnchored(wm, req)
	if err != nil {
		return FactSetPage{}, c.st, err
	}
	wm.anchor = anchor
	write := req.Binding.Write()
	env := FactSetEnv{
		Mode: FactSetMode, Title: FactSetTitle(req.Search, write.OntologyRoot(), write.Name()),
		SearchLine: SearchLine(req.Search), Search: req.Search, Anchor: anchor, Audience: AudienceHuman,
		Unpinned: req.Options.Unpin, CrossKB: req.Options.CrossKB, Depth: req.Options.Depth,
		MaxNodes: req.Options.MaxNodes, FactsLimit: req.Facts, Units: len(hits), InstructionsVersion: FactSetInstructionsVersion,
	}
	if len(hits) == 0 {
		return FactSetPage{FactSetEnv: env, Facts: []FactSetNode{}}, c.st, nil
	}
	fr := &factsetFrozen{V: frozenVersion, Env: env, Limit: req.Limit, WriteRepo: wm.id12, Branch: wm.branch}
	for _, h := range hits {
		fr.Roots = append(fr.Roots, frozenRoot{Path: h.path, Commit: h.commit, Blob: h.blob, Title: h.title, Date: h.date})
	}
	if req.Options.CrossKB {
		if fr.Tips, err = c.freezeTips(); err != nil {
			return FactSetPage{}, c.st, err
		}
	}
	c.r.tips = fr.Tips
	if c.r.tips == nil {
		c.r.tips = map[string]frozenTip{}
	}
	e := &fsEntry{fr: fr, ti: sc.Sessions}
	c.e = e
	if err := c.ensureState(); err != nil {
		return FactSetPage{}, c.st, err
	}
	frozen, err := json.Marshal(fr)
	if err != nil {
		return FactSetPage{}, c.st, err
	}
	id, replaced, err := sc.Sessions.CreateFactSet(ctx, factsetTool(req.Limit), sc.Branch, sc.Pin, sc.ReadSet, sc.Owner, frozen)
	if err != nil {
		return FactSetPage{}, c.st, err
	}
	for _, old := range replaced {
		fsCacheDrop(old)
	}
	e.id = id
	e.mu.Lock()
	defer e.mu.Unlock()
	fsCachePut(e)
	page, err := c.page(fpos{1, 0}, true)
	if err != nil {
		// The caller never got the cursor: nothing may stay behind.
		fsCacheDrop(id)
		_ = sc.Sessions.DeleteFactSet(context.WithoutCancel(ctx), id)
		return FactSetPage{}, c.st, err
	}
	fsCacheAccount(e)
	return page, c.st, nil
}

// searchAnchored runs the search between two reads of the write mount's
// index watermark (the commit branch_facts reflects): when they agree, every
// hit's (commit, blob) is its version AT that anchor. It retries once; a
// second move is ErrIndexMoved.
func (c *fsCall) searchAnchored(wm *mount, req FactSetRequest) (string, []factsetHit, error) {
	for attempt := 0; attempt < 2; attempt++ {
		w1, err := wm.svc.HistoryQuery().Watermark(c.ctx, wm.branch)
		if err != nil {
			return "", nil, err
		}
		hits, err := runFactSetSearch(c.ctx, wm, req)
		if err != nil {
			return "", nil, err
		}
		if h := factsetSearchHook; h != nil {
			h()
		}
		w2, err := wm.svc.HistoryQuery().Watermark(c.ctx, wm.branch)
		if err != nil {
			return "", nil, err
		}
		c.st.IndexQueries += 3
		if w1 == w2 {
			return w1, hits, nil
		}
	}
	return "", nil, ErrIndexMoved
}

// freezeTips is the indexed tip, now, of every repo other than the write
// repo that the caller can reach: the binding's read mounts (at the branch
// the binding pins) and every registered repo mayRead admits (at its read
// branch). A repo whose store cannot be opened, or whose id does not resolve,
// is left out: it reads as not available, like one that does not exist.
func (c *fsCall) freezeTips() (map[string]frozenTip, error) {
	tips := map[string]frozenTip{}
	writeID := fact.ID12(c.b.Write().ID())
	type cand struct {
		ri     *repos.RepoInstance
		branch string
	}
	var cands []cand
	for _, rt := range c.b.Reads() {
		cands = append(cands, cand{rt.RI, rt.Branch})
	}
	if c.mgr != nil && c.mayRead != nil {
		var regs []*repos.RepoInstance
		c.mgr.ForEach(func(_ string, ri *repos.RepoInstance) {
			if ri != nil {
				regs = append(regs, ri)
			}
		})
		for _, ri := range regs {
			if c.mayRead(ri.Name()) {
				cands = append(cands, cand{ri, ri.ReadBranch()})
			}
		}
	}
	for _, cd := range cands {
		id := fact.ID12(cd.ri.ID())
		if id == "" || id == writeID {
			continue
		}
		if _, done := tips[id]; done {
			continue
		}
		svc, release, err := cd.ri.Acquire()
		if err != nil {
			continue
		}
		wm, err := svc.HistoryQuery().Watermark(c.ctx, cd.branch)
		release()
		c.st.IndexQueries++
		if err != nil {
			return nil, fmt.Errorf("freeze the tip of repo %s: %w", id, err)
		}
		if wm == "" {
			continue
		}
		tips[id] = frozenTip{Branch: cd.branch, Commit: wm}
	}
	return tips, nil
}

// runFactSetSearch runs the frozen search on the write mount and returns the
// ordered results, at most req.Facts, each with its version.
//
// THE FIRST UIListLimit RESULTS ARE THE UI'S LIST, EXACTLY. A text search
// takes a nearest-neighbour window that grows with its limit, and its
// filters apply after that window, so the same search at limit 500 can rank
// results the 50-row list never showed, or list more than it did. The
// FactSet therefore runs the very call the UI makes (limit UIListLimit) and
// freezes its answer as the prefix. Only when that list is full and Facts
// asks for more does it search again at limit Facts and append, in that
// call's order, every result the prefix does not hold. A list the UI shows
// short (fewer than UIListLimit) is the whole FactSet: "you found 10, you see
// 10". Ties are ordered by path in the store, so both calls are
// deterministic. A Facts below UIListLimit truncates the list.
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
// sort=recent.
func factsetSearchOnce(ctx context.Context, m *mount, req FactSetRequest, limit int, vec []float32) ([]factsetHit, error) {
	q := req.Search.options(limit, req.PathsIn)
	var hits []factsetHit
	if req.Search.Sort == SortRecent {
		out, _, err := m.svc.FactQuery().RecentFacts(ctx, m.branch, q)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		for _, e := range out {
			hits = append(hits, factsetHit{e.Path, e.Title, e.CommitHash, e.BlobHash, e.CommittedAt})
		}
		return hits, nil
	}
	q.QueryVec = vec
	out, err := m.svc.FactQuery().Search(ctx, m.branch, q)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	for _, e := range out {
		hits = append(hits, factsetHit{e.Path, e.Title, e.CommitHash, e.BlobHash, e.CommittedAt})
	}
	return hits, nil
}

// ---- a call on a FactSet ---------------------------------------------------

// fsCall is one call's handle: its reader, its stats, its planning slot.
type fsCall struct {
	ctx     context.Context
	b       *repos.Binding
	mgr     *repos.Manager
	mayRead func(string) bool
	r       *reader
	e       *fsEntry
	st      Stats
	slot    bool
}

func (c *fsCall) close() {
	c.releaseSlot()
	if c.r != nil {
		c.r.close()
	}
}

func (c *fsCall) releaseSlot() {
	if c.slot {
		<-buildSlots
		c.slot = false
	}
}

// needSlot takes a planning slot for this call, once (refused at once when
// the cap is reached).
func (c *fsCall) needSlot() error {
	if c.slot {
		return nil
	}
	if !takeSlot(buildSlots) {
		return ErrFactSetBusy
	}
	c.slot = true
	return nil
}

// openFactSet checks a cursor's id against sc exactly as every cursor is
// checked (every mismatch is ErrCursorExpired: unknown, another tool's,
// another binding's or read set's, another owner's), writes the bounded
// heartbeat, and returns the call with its cache entry LOCKED (released by
// unlock). The ROW decides on every call, cache hit or replay.
func openFactSet(ctx context.Context, sc CursorScope, b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool, id string) (*fsCall, error) {
	if sc.Pin == "" || sc.Owner == "" || sc.Pin != b.PinID() {
		return nil, ErrCursorExpired
	}
	row, err := sc.Sessions.GetFactSet(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("session lookup error: %w", err)
	}
	if row == nil || row.Status != "active" || row.Binding == "" || row.Binding != sc.Pin ||
		row.ReadSet != sc.ReadSet || row.Owner != sc.Owner {
		return nil, ErrCursorExpired
	}
	if now := factsetNow(); now.Sub(row.LastUsed) >= time.Duration(factsetIdleTTL.Load())/4 {
		if err := sc.Sessions.TouchFactSet(ctx, id, now); err != nil {
			// Not fatal: the next call writes it; only a FactSet idle past
			// the whole TTL is reaped.
			log.Debug().Err(err).Str("factset", id).Msg("factset heartbeat not written")
		}
	}
	c := &fsCall{ctx: ctx, b: b, mgr: mgr, mayRead: mayRead}
	c.r = newReader(b, mgr, mayRead, &c.st)
	e := fsCacheGet(id, sc.Sessions)
	if e == nil {
		var fr factsetFrozen
		if err := json.Unmarshal(row.Frozen, &fr); err != nil {
			c.close()
			return nil, fmt.Errorf("factset frozen row: %w", err)
		}
		if fr.V != frozenVersion {
			c.close()
			return nil, ErrCursorExpired
		}
		e = &fsEntry{id: id, ti: sc.Sessions, fr: &fr}
		c.st.CacheMisses++
		e = fsCachePutIfAbsent(e)
	}
	// Every frozen repo must still be this caller's to read: a narrower
	// credential resuming a wider caller's cursor gets the expiry answer,
	// naming nothing (89b26b07).
	for id12 := range e.fr.Tips {
		if _, _, ok := c.r.target(id12); !ok {
			c.close()
			return nil, ErrCursorExpired
		}
	}
	c.r.tips = e.fr.Tips
	if c.r.tips == nil {
		c.r.tips = map[string]frozenTip{}
	}
	e.mu.Lock()
	c.e = e
	return c, nil
}

func (c *fsCall) unlock() {
	if c.e != nil {
		fsCacheAccount(c.e)
		c.e.mu.Unlock()
	}
	c.close()
}

// IsFactSetCursor reports whether cursor names a FactSet THIS caller may read
// (sc's owner, pin and read set), so an edge can route a resume to
// FactSetNextPage. Another owner's FactSet cursor is false, exactly like one
// that does not exist, so the edge's refusals cannot tell the two apart.
func IsFactSetCursor(ctx context.Context, sc CursorScope, cursor string) (bool, error) {
	id, _, ok := parseCursor(cursor)
	if !ok {
		return false, nil
	}
	row, err := sc.Sessions.GetFactSet(ctx, id)
	if err != nil {
		return false, err
	}
	return row != nil && row.Status == "active" && row.Binding != "" && row.Binding == sc.Pin &&
		row.ReadSet == sc.ReadSet && row.Owner == sc.Owner, nil
}

// FactSetNextPage serves the page at cursor's position, or, with unit > 0,
// seeks: the page starts at that unit's first section (1..units; anything
// else is ErrInvalid). Every cursor this caller cannot be served is
// ErrCursorExpired; a position past the extent is ErrInvalid. Two calls on
// one cursor both succeed and return the same bytes.
func FactSetNextPage(ctx context.Context, sc CursorScope, b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool, cursor string, unit int) (FactSetPage, error) {
	page, _, err := FactSetNextPageStats(ctx, sc, b, mgr, mayRead, cursor, unit)
	return page, err
}

// FactSetNextPageStats is FactSetNextPage with what the call read.
func FactSetNextPageStats(ctx context.Context, sc CursorScope, b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool, cursor string, unit int) (FactSetPage, Stats, error) {
	id, at, ok := parseCursor(cursor)
	if !ok {
		return FactSetPage{}, Stats{}, ErrCursorExpired
	}
	c, err := openFactSet(ctx, sc, b, mgr, mayRead, id)
	if err != nil {
		return FactSetPage{}, Stats{}, err
	}
	defer c.unlock()
	units := len(c.e.fr.Roots)
	if unit != 0 {
		if unit < 1 || unit > units {
			return FactSetPage{}, c.st, fmt.Errorf("%w: unit must be between 1 and %d, got %d", ErrInvalid, units, unit)
		}
		at = fpos{unit, 0}
	}
	page, err := c.page(at, false)
	return page, c.st, err
}

// ensureState builds the FactSet's results and reserved maps (once per cache
// entry): the two-level reservation.
func (c *fsCall) ensureState() error {
	e := c.e
	if e.st.results != nil {
		return nil
	}
	if err := c.needSlot(); err != nil {
		return err
	}
	st, err := c.reserve(e.fr)
	if err != nil {
		return err
	}
	e.st = *st
	e.st.written = map[uint64]wRec{}
	e.planned, e.counts, e.plans = 0, nil, map[int]*plannedUnit{}
	return nil
}

// planUnitOf returns unit u's plan (no bodies), planning and replaying from
// unit 1 as needed: the cache keeps the written set up to the planned extent
// and the plans of the last few units; anything before that is replayed.
func (c *fsCall) planUnitOf(u int) (*plannedUnit, error) {
	if err := c.ensureState(); err != nil {
		return nil, err
	}
	e := c.e
	if pu, ok := e.plans[u]; ok && u <= e.planned {
		return pu, nil
	}
	if e.planned >= u {
		// A seek backwards past the cached plans: replay from the frozen
		// inputs, without bodies.
		e.st.written = map[uint64]wRec{}
		e.planned, e.counts, e.plans = 0, nil, map[int]*plannedUnit{}
	}
	for e.planned < u {
		if err := c.needSlot(); err != nil {
			return nil, err
		}
		next := e.planned + 1
		pu, err := c.planUnit(e.fr, &e.st, next)
		if err != nil {
			return nil, err
		}
		if err := applyUnit(&e.st, pu); err != nil {
			return nil, err
		}
		e.planned = next
		e.counts = append(e.counts, len(pu.secs))
		e.plans[next] = pu
		// The plans a page may still start in: a page spans at most
		// HumanMaxLimit units, so sequential paging never replays.
		delete(e.plans, next-HumanMaxLimit-1)
	}
	return e.plans[u], nil
}

// planUnit plans unit u over the cross-unit state st (no bodies).
func (c *fsCall) planUnit(fr *factsetFrozen, st *fsState, u int) (*plannedUnit, error) {
	root := fr.Roots[u-1]
	m, err := c.r.open(c.b.Write(), c.b.WriteMountBranch())
	if err != nil {
		return nil, err
	}
	m.anchor = fr.Env.Anchor
	p := &planner{ctx: c.ctx, r: c.r, opts: fr.Env.options(), writeID: fr.WriteRepo, fs: st, unit: u, st: &c.st}
	n, err := p.rootNode(m, root.Path, root.Commit, root.Blob, root.Date)
	if err != nil {
		return nil, err
	}
	if n == nil {
		c.st.UnitsPlanned++
		h := factHash(fr.WriteRepo, root.Path)
		return &plannedUnit{secs: []section{{h: h, repo: fr.WriteRepo, rel: root.Path, wire: root.Path, title: root.Title,
			n: 1, number: strconv.Itoa(u), parent: -1, role: RoleRoot, unavailable: true}}}, nil
	}
	pu, err := p.planRoot(n)
	if err != nil {
		var col errHashCollision
		if errors.As(err, &col) {
			logCollision(err)
			return nil, fmt.Errorf("%w: %w", ErrFactIdentity, err)
		}
		return nil, err
	}
	return pu, nil
}

// applyUnit adds a planned unit's written facts (and an unavailable root's
// heading) to the written set.
func applyUnit(st *fsState, pu *plannedUnit) error {
	for _, s := range pu.secs {
		if s.link != nil {
			continue
		}
		chk := factCheck(s.repo, s.rel)
		if w, ok := st.written[s.h]; ok && w.check != chk {
			err := errHashCollision{s.repo, s.rel}
			logCollision(err)
			return fmt.Errorf("%w: %w", ErrFactIdentity, err)
		}
		st.written[s.h] = wRec{check: chk, blob: blob64(s.blob)}
	}
	return nil
}

// reserve builds the results map and the reserved map from the frozen roots
// (#692 round-3 rule, unchanged semantics):
//   - every result owns its own unit;
//   - each result's DIRECT references are reserved for the first unit, in
//     result order, whose OWN limits show them at depth 1, numbered "N.k":
//     nothing at Depth 0 or with max_nodes < 2; with max_nodes = k+1 only the
//     first k depth-1 entries; a depth-1 entry is a fact ref that resolves,
//     in ord order (link entries count: a direct ref that is a result, or
//     already taken); a frame root and a result gone at the anchor reserve
//     nothing; under unpin a direct ref gone at the anchor is absent and does
//     not count; a cross-KB direct ref with cross_kb off, or into a repo not
//     available, does not count.
//
// It reads, per window of results: their metadata (frame detection), their
// ref targets (RefTargetsOfBlobs, no target metadata), and the versions live
// at the anchor (unpin) or at the frozen tips (cross-KB).
func (c *fsCall) reserve(fr *factsetFrozen) (*fsState, error) {
	st := &fsState{results: make(map[uint64]rootRec, len(fr.Roots)), reserved: map[uint64]resRec{}}
	for i, root := range fr.Roots {
		h, chk := factHash(fr.WriteRepo, root.Path), factCheck(fr.WriteRepo, root.Path)
		if old, dup := st.results[h]; dup {
			if old.check != chk {
				err := errHashCollision{fr.WriteRepo, root.Path}
				logCollision(err)
				return nil, fmt.Errorf("%w: %w", ErrFactIdentity, err)
			}
			continue
		}
		st.results[h] = rootRec{unit: int32(i + 1), check: chk, blob: blob64(root.Blob)}
	}
	opts := fr.Env.options()
	if opts.Depth < 1 || opts.MaxNodes < 2 {
		return st, nil
	}
	m, err := c.r.open(c.b.Write(), c.b.WriteMountBranch())
	if err != nil {
		return nil, err
	}
	m.anchor = fr.Env.Anchor
	if err := c.r.checkReady(c.ctx, m); err != nil {
		return nil, err
	}
	const window = 50
	for lo := 0; lo < len(fr.Roots); lo += window {
		hi := min(len(fr.Roots), lo+window)
		var keys []index.VersionKey
		for _, root := range fr.Roots[lo:hi] {
			if root.Blob != "" {
				keys = append(keys, index.VersionKey{Path: root.Path, Blob: root.Blob})
			}
		}
		if len(keys) == 0 {
			continue
		}
		metas, err := m.svc.FactQuery().VersionsByBlob(c.ctx, keys)
		if err != nil {
			return nil, err
		}
		c.st.IndexQueries++
		var blobs []string
		for _, k := range keys {
			if mt, ok := metas[k]; ok && fact.Type(mt.Type) != fact.Frame {
				blobs = append(blobs, k.Blob)
			}
		}
		if len(blobs) == 0 {
			continue
		}
		targets, err := m.svc.GraphStore().RefTargetsOfBlobs(c.ctx, blobs)
		if err != nil {
			return nil, err
		}
		c.st.IndexQueries++
		bySource := map[string][]refTarget{}
		var local []string
		foreignPaths := map[string][]string{}
		var foreignRepos []string
		for _, t := range targets {
			bySource[t.SourceBlob] = append(bySource[t.SourceBlob], refTarget{t.Ord, t.Repo, t.Path, t.TargetBlob})
			if t.Repo == "" {
				local = append(local, t.Path)
			} else if opts.CrossKB {
				if _, ok := foreignPaths[t.Repo]; !ok {
					foreignRepos = append(foreignRepos, t.Repo)
				}
				foreignPaths[t.Repo] = append(foreignPaths[t.Repo], t.Path)
			}
		}
		var liveLocal map[string]string // path -> blob live at the anchor (unpin)
		if opts.Unpin && len(local) > 0 {
			live, hist, err := m.svc.HistoryQuery().LiveAt(c.ctx, m.branch, m.anchor, local)
			if err != nil {
				return nil, err
			}
			c.st.IndexQueries++
			if hist {
				c.st.HistoricalLiveAt++
			}
			liveLocal = make(map[string]string, len(live))
			for p, v := range live {
				if v.HasMeta {
					liveLocal[p] = v.Blob
				}
			}
		}
		liveForeign := map[string]map[string]string{}
		for _, id := range foreignRepos {
			fm, err := c.r.foreign(c.ctx, id)
			if err != nil {
				return nil, err
			}
			if fm == nil {
				continue
			}
			if err := c.r.checkReady(c.ctx, fm); err != nil {
				return nil, err
			}
			live, hist, err := fm.svc.HistoryQuery().LiveAt(c.ctx, fm.branch, fm.anchor, foreignPaths[id])
			if err != nil {
				return nil, err
			}
			c.st.IndexQueries++
			if hist {
				c.st.HistoricalLiveAt++
			}
			got := make(map[string]string, len(live))
			for p, v := range live {
				if v.HasMeta {
					got[p] = v.Blob
				}
			}
			liveForeign[fm.id12] = got
		}
		for i := lo; i < hi; i++ {
			root := fr.Roots[i]
			ts, ok := bySource[root.Blob]
			if !ok || root.Blob == "" {
				continue
			}
			if mt, ok := metas[index.VersionKey{Path: root.Path, Blob: root.Blob}]; !ok || fact.Type(mt.Type) == fact.Frame {
				continue
			}
			unit := int32(i + 1)
			included, k := 1, int32(0)
			for _, t := range ts {
				if included >= opts.MaxNodes {
					break
				}
				var repo, blob string
				switch {
				case t.repo == "":
					if t.path == root.Path {
						continue
					}
					repo = fr.WriteRepo
					blob = t.blob
					if opts.Unpin {
						b, live := liveLocal[t.path]
						if !live {
							continue
						}
						blob = b
					}
					if blob == "" {
						continue
					}
				default:
					if !opts.CrossKB {
						continue
					}
					got, avail := liveForeign[t.repo]
					if !avail {
						continue
					}
					b, present := got[t.path]
					if !present {
						continue
					}
					repo, blob = t.repo, b
				}
				included++
				k++
				h, chk := factHash(repo, t.path), factCheck(repo, t.path)
				if r, isResult := st.results[h]; isResult {
					if r.check != chk {
						err := errHashCollision{repo, t.path}
						logCollision(err)
						return nil, fmt.Errorf("%w: %w", ErrFactIdentity, err)
					}
					continue
				}
				if r, taken := st.reserved[h]; taken {
					if r.check != chk {
						err := errHashCollision{repo, t.path}
						logCollision(err)
						return nil, fmt.Errorf("%w: %w", ErrFactIdentity, err)
					}
					continue
				}
				st.reserved[h] = resRec{unit: unit, k: k, check: chk, blob: blob64(blob)}
			}
		}
	}
	return st, nil
}

// refTarget is one ref target of a root, as the reservation reads it.
type refTarget struct {
	ord        int
	repo, path string
	blob       string
}

// ---- pages ----------------------------------------------------------------------

// page serves the sections from `from`: whole sections, at most the frozen
// limit, at least one, measured against PageCap; bodies are read by blob only
// for the sections it emits, and the RULE 1 marks are computed for them now.
func (c *fsCall) page(from fpos, first bool) (FactSetPage, error) {
	fr := c.e.fr
	units := len(fr.Roots)
	if from.unit < 1 || from.unit > units {
		return FactSetPage{}, fmt.Errorf("%w: the cursor is past the end of the factset; seek with unit (1 to %d)", ErrInvalid, units)
	}
	pu, err := c.planUnitOf(from.unit)
	if err != nil {
		return FactSetPage{}, err
	}
	if from.sec >= len(pu.secs) {
		return FactSetPage{}, fmt.Errorf("%w: the cursor's position is past unit %d's end", ErrInvalid, from.unit)
	}
	type cand struct {
		unit int
		sec  section
		next fpos // the position after this section
	}
	var cands []cand
	at := from
	for len(cands) < fr.Limit && at.unit <= units {
		pu, err := c.planUnitOf(at.unit)
		if err != nil {
			return FactSetPage{}, err
		}
		for at.sec < len(pu.secs) && len(cands) < fr.Limit {
			cd := cand{unit: at.unit, sec: pu.secs[at.sec]}
			at.sec++
			if at.sec >= len(pu.secs) {
				at = fpos{at.unit + 1, 0}
			}
			cd.next = at
			cands = append(cands, cd)
			if at.sec == 0 {
				break
			}
		}
	}
	secs := make([]section, len(cands))
	for i, cd := range cands {
		secs[i] = cd.sec
	}
	nodes, err := c.inflate(secs, func(i int) int { return cands[i].unit })
	if err != nil {
		return FactSetPage{}, err
	}
	env := fr.Env
	build := func(k int) FactSetPage {
		p := FactSetPage{FactSetEnv: env, Facts: nodes[:k], UnitFrom: nodes[0].Unit, UnitTo: nodes[k-1].Unit}
		if first {
			p.Instructions = FactSetInstructions
		}
		return p
	}
	placeholder := strings.Repeat("0", 36) + ".500.99999"
	used := 1
	for k := 2; k <= len(nodes); k++ {
		p := build(k)
		p.Cursor, p.HasMore = &placeholder, true
		p.UnitsWalked, p.Sections = MaxFactSetFacts, MaxFactSetFacts*MaxNodesLimit
		out, err := json.Marshal(p)
		if err != nil {
			return FactSetPage{}, err
		}
		if len(out) > PageCap {
			break
		}
		used = k
	}
	page := build(used)
	next := cands[used-1].next
	tok := cursorToken(c.e.id, next)
	page.Cursor = &tok
	page.HasMore = next.unit <= units
	page.UnitsWalked = c.e.planned
	for _, n := range c.e.counts {
		page.Sections += n
	}
	return page, nil
}

// inflate turns planned sections into page nodes: bodies of the written ones
// by blob (one batched object read per repo) and the RULE 1 marks (one
// batched branch_facts read per repo, against the reading branch NOW).
func (c *fsCall) inflate(secs []section, unitOf func(int) int) ([]FactSetNode, error) {
	var keys []bodyKey
	for _, s := range secs {
		if s.link == nil && !s.unavailable {
			keys = append(keys, bodyKey{s.repo, s.rel, s.blob})
		}
	}
	bodies, err := c.r.bodies(c.ctx, keys)
	if err != nil {
		return nil, err
	}
	marks, err := c.rule1(secs)
	if err != nil {
		return nil, err
	}
	out := make([]FactSetNode, len(secs))
	for i, s := range secs {
		n := s.factsetNode(unitOf(i))
		if n.Written {
			n.Body = bodies[bodyKey{s.repo, s.rel, s.blob}]
		}
		n.Marks = append(n.Marks, marks[i]...)
		out[i] = n
	}
	return out, nil
}

// rule1 is each section's RULE 1 marks: the version it shows (written) or
// cites (link) against what the reading branch holds NOW (the FactSet's
// branch; a foreign repo's read branch for a cross-KB section).
func (c *fsCall) rule1(secs []section) ([][]string, error) {
	out := make([][]string, len(secs))
	byRepo := map[string][]int{}
	var order []string
	for i, s := range secs {
		if s.unavailable || s.blob == "" {
			continue
		}
		if _, ok := byRepo[s.repo]; !ok {
			order = append(order, s.repo)
		}
		byRepo[s.repo] = append(byRepo[s.repo], i)
	}
	for _, repo := range order {
		idx := byRepo[repo]
		m, err := c.r.mountOf(c.ctx, repo)
		if err != nil {
			return nil, err
		}
		paths := make([]string, len(idx))
		for j, i := range idx {
			paths[j] = secs[i].rel
		}
		cur, err := m.svc.FactQuery().CurrentBlobs(c.ctx, m.branch, paths)
		if err != nil {
			return nil, err
		}
		c.st.IndexQueries++
		for _, i := range idx {
			v, ok := cur[secs[i].rel]
			switch {
			case !ok:
				out[i] = []string{MarkDeleted}
			case v.Blob != secs[i].blob:
				out[i] = []string{MarkChangedSinceCited}
			}
		}
	}
	return out, nil
}

// factsetNode is a section in the page shape (no body, no RULE 1 marks).
func (s section) factsetNode(unit int) FactSetNode {
	n := FactSetNode{Node: s.node(), Unit: unit, Number: s.number, TreeDepth: s.treeDepth,
		Written: s.link == nil && !s.unavailable, Unavailable: s.unavailable}
	if s.parent >= 0 {
		n.Parent = s.parent + 1
	}
	if s.unavailable {
		n.Commit, n.Blob, n.Kind = "", "", ""
		return n
	}
	if (s.role == RoleRoot || s.role == RoleFocal) && s.truncated != nil {
		t := *s.truncated
		n.UnitTruncated = &t
	}
	if l := s.link; l != nil {
		fl := &FactSetLink{AnchorID: AnchorOf(s.h), Direction: LinkForward, CitedBlob: s.blob}
		if l.back {
			fl.Direction = LinkBack
		}
		if l.unit > 0 {
			fl.Unit, fl.Number = l.unit, l.number
		}
		fl.DifferentVersion = l.blob != 0 && blob64(s.blob) != l.blob
		n.Link = fl
		n.BuildsOn, n.NotIncluded, n.Sources = nil, nil, nil
	}
	return n
}
