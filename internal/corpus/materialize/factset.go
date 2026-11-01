package materialize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/model"
	"knomit/internal/engine/store/sessions"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/config"
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
// creation, as one session-DB row. The reserved map and the planned PREFIX
// (each written fact with the unit that first wrote it) live in an in-memory
// CACHE under a budget; a page is a pure function of (FactSet, position): a
// unit within the prefix is re-planned alone against it, any number of calls
// at once, and only extending the prefix is done once, by one call. A lost or
// evicted entry is planned again up to what the next call needs. The reading
// position travels in the cursor token, so pages never write the session DB
// (only a heartbeat, at most once per quarter of the idle TTL).

// FactSet bounds.
const (
	// DefaultFactSetFacts is the Facts control's default: every result, up
	// to the cap.
	DefaultFactSetFacts = MaxFactSetFacts
	// MaxFactSetFacts caps the units, the same ceiling as max_nodes.
	MaxFactSetFacts = 500
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
	// ErrFactSetBusy: the FactSet memory budget (session.factset_memory_mib)
	// stayed full for as long as a call waits for planning memory
	// (config.FactSetStallLimit); retry.
	ErrFactSetBusy = errors.New("the server stayed busy building other factsets; retry shortly")
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
	// UnitsWalked is the highest unit THIS page walked: what sequential
	// paging reports at this position, never the cache's planned extent
	// (which depends on what other calls read). Sections counts the
	// sections of units 1..UnitsWalked.
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
	V     int        `json:"v"`
	Env   FactSetEnv `json:"env"`
	Limit int        `json:"limit"`
	// HomeRepo is the repo every root lives in (id12), read at Branch as of
	// Env.Anchor: it keys the fact hashes, the classification of local refs,
	// the read-ahead and the unavailable roots. The JSON name predates the
	// home/wire split: for a repo FactSet the home IS the write repo.
	HomeRepo string `json:"write_repo"`
	Branch   string `json:"branch"`
	// WireRepo is the repo whose paths go out bare, when it is not the home
	// (a lens sub-FactSet over a read member: its paths are kb://<home>/...).
	// Empty means the home: a repo FactSet never sets it, so its frozen
	// bytes are unchanged.
	WireRepo string               `json:"wire_repo,omitempty"`
	Tips     map[string]frozenTip `json:"tips,omitempty"`
	Roots    []frozenRoot         `json:"roots"`
}

// wireID is the repo whose paths go out bare (see WireRepo).
func (fr *factsetFrozen) wireID() string {
	if fr.WireRepo != "" {
		return fr.WireRepo
	}
	return fr.HomeRepo
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

// ---- heartbeat ---------------------------------------------------------------

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

// factsetAnchorWait is session.factset_anchor_wait: how long a first call
// waits for a settled watermark before ErrIndexMoved.
var factsetAnchorWait atomic.Int64

func init() { factsetAnchorWait.Store(int64(config.DefaultFactSetAnchorWait)) }

// SetFactSetAnchorWait sets how long a first call waits for its branch's
// index to settle (session.factset_anchor_wait); 0 tries once; a negative
// value is ignored.
func SetFactSetAnchorWait(d time.Duration) {
	if d >= 0 {
		factsetAnchorWait.Store(int64(d))
	}
}

// settlePoll is the first and the longest pause between two looks at an
// unsettled watermark: short against a sync's write window, doubling, so a
// burst of syncs costs a handful of meta reads, not a spin.
const (
	settlePollMin = 5 * time.Millisecond
	settlePollMax = 100 * time.Millisecond
)

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
// the caller can reach, writes the ONE session row (a new FactSet beside any
// the owner already holds), builds the reservation in memory and returns
// page 1. sc.Owner must be FactSetOwner: a FactSet belongs to the principal
// alone, whichever surface or endpoint it was made on.
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
	// No planning memory yet: searchAnchored reserves it once the index has
	// settled (a first call waiting for that holds none, review R3-1).
	c := &fsCall{ctx: ctx, b: req.Binding, mgr: req.Manager, mayRead: req.MayRead,
		reserve: fsPlanReserve(req.Facts, req.Options.MaxNodes, false)}
	c.r = newReader(req.Binding, req.Manager, req.MayRead, &c.st)
	defer c.close()

	wm, err := c.r.open(req.Binding.Write(), req.Binding.WriteMountBranch())
	if err != nil {
		return FactSetPage{}, c.st, err
	}
	if err := c.r.checkReady(ctx, wm); err != nil {
		return FactSetPage{}, c.st, err
	}
	searchStart := time.Now()
	anchor, hits, err := c.searchAnchored(wm, req)
	c.st.Search = time.Since(searchStart)
	if err != nil {
		return FactSetPage{}, c.st, err
	}
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
	// The repo path of the seam: home = wire = the write mount.
	fr := freezePinned(pinnedHome{ID: wm.id12, Branch: wm.branch}, wm.id12, env, req.Limit, hits)
	if req.Options.CrossKB {
		if fr.Tips, err = c.freezeTips(); err != nil {
			return FactSetPage{}, c.st, err
		}
	}
	if err := c.startPinned(fr, sc.Sessions); err != nil {
		return FactSetPage{}, c.st, err
	}
	page, err := c.createPinned(sc)
	return page, c.st, err
}

// ---- the pinned seam -------------------------------------------------------
//
// A FactSet over SUPPLIED pinned roots: the second half of FactSetFirstPage,
// split from the search so a lens facade (F3) can build one sub-FactSet per
// lens member from that member's slice of the fused lens search, without a
// search of its own. The repo path is search + anchor (FactSetFirstPage),
// then freezePinned, startPinned and createPinned with home = wire = the
// write mount; its frozen bytes and pages are what they were before the
// split. A sub calls freezePinned and startPinned only: it is not a session
// row of its own.

// pinnedHome is the repo a FactSet's roots live in and the branch it is read
// at.
type pinnedHome struct {
	ID     string // id12
	Branch string
}

// freezePinned is the frozen row of a FactSet over pinned roots: every hit a
// version of a fact in home, at env.Anchor (the home's settled watermark the
// hits were read at), in the order given. wireID is the repo whose paths go
// out bare (the binding's write repo); a home that is not it emits
// kb://<home>/... paths. Tips are the caller's (cross_kb only); a sub-FactSet
// has none.
func freezePinned(home pinnedHome, wireID string, env FactSetEnv, limit int, hits []factsetHit) *factsetFrozen {
	fr := &factsetFrozen{V: frozenVersion, Env: env, Limit: limit, HomeRepo: home.ID, Branch: home.Branch}
	if wireID != home.ID {
		fr.WireRepo = wireID
	}
	for _, h := range hits {
		fr.Roots = append(fr.Roots, frozenRoot{Path: h.path, Commit: h.commit, Blob: h.blob, Title: h.title, Date: h.date})
	}
	return fr
}

// startPinned makes c the call on a NEW FactSet over fr, not yet a session
// row: it takes fr's tips, registers fr's home in c's reader, makes the
// cache entry (not yet in the cache) and builds the reservation (the results
// and reserved tables) in memory.
func (c *fsCall) startPinned(fr *factsetFrozen, ti *sessions.ToolIndex) error {
	c.r.tips = fr.Tips
	if c.r.tips == nil {
		c.r.tips = map[string]frozenTip{}
	}
	c.e = newFSEntry("", ti, fr)
	if err := c.registerHome(); err != nil {
		return err
	}
	return c.ensureState()
}

// createPinned writes the ONE session row of c's started FactSet (a new
// FactSet beside any the owner already holds), caches its entry and returns
// page 1. A failure after the row was written deletes it: the caller never
// got the cursor.
func (c *fsCall) createPinned(sc CursorScope) (FactSetPage, error) {
	ctx, e := c.ctx, c.e
	frozen, err := json.Marshal(e.fr)
	if err != nil {
		return FactSetPage{}, err
	}
	id, err := sc.Sessions.CreateFactSet(ctx, factsetTool(e.fr.Limit), sc.Branch, sc.Pin, sc.ReadSet, sc.Owner, frozen)
	if err != nil {
		return FactSetPage{}, err
	}
	e.id = id
	fsCachePut(e)
	defer fsCacheRelease(e)
	page, err := c.page(fpos{1, 0}, true)
	if err != nil {
		// The caller never got the cursor: nothing may stay behind.
		fsCacheDrop(id)
		_ = sc.Sessions.DeleteFactSet(context.WithoutCancel(ctx), id)
		return FactSetPage{}, err
	}
	return page, nil
}

// registerHome registers the FactSet's home repo in c's reader (see
// reader.registerHome): every call on a FactSet does it when it opens, before
// any read. A repo FactSet's home is the write repo, read at the binding's
// write mount branch exactly as before the split; any other home is read at
// its frozen branch.
func (c *fsCall) registerHome() error {
	fr := c.e.fr
	branch := fr.Branch
	if fr.HomeRepo == fact.ID12(c.b.Write().ID()) {
		branch = c.b.WriteMountBranch()
	}
	return c.r.registerHome(fr.HomeRepo, branch, fr.Env.Anchor)
}

// home is this call's mount of the FactSet's home repo, at its branch, read
// as of its anchor: the ONE place planning, the reservation and the
// read-ahead open the repo the roots live in.
func (c *fsCall) home() (*mount, error) {
	m, ok, err := c.r.homeMount(c.e.fr.HomeRepo)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("factset: home repo %s not registered for this call", c.e.fr.HomeRepo)
	}
	return m, nil
}

// searchAnchored runs the search between two reads of the write mount's
// index watermark (the commit branch_facts reflects): when they agree, every
// hit's (commit, blob) is its version AT that anchor, provided both reads are
// settled (HistoryQuery.SettledWatermark). Otherwise it waits and tries again
// until session.factset_anchor_wait passes, then returns ErrIndexMoved.
func (c *fsCall) searchAnchored(wm *mount, req FactSetRequest) (string, []factsetHit, error) {
	// The hits are branch_facts rows, AT the watermark only when no sync was
	// rewriting them past it on either side of the search and the watermark
	// did not move (a sync moves it last, review N1). Until then the call
	// waits, bounded by session.factset_anchor_wait and the request's
	// context, and searches only once the watermark has settled; past the
	// deadline it is ErrIndexMoved (503, Retry-After).
	deadline := time.Now().Add(time.Duration(factsetAnchorWait.Load()))
	pause := settlePollMin
	wait := func() error {
		left := time.Until(deadline)
		if left <= 0 {
			return ErrIndexMoved
		}
		t := time.NewTimer(min(pause, left))
		defer t.Stop()
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-t.C:
		}
		pause = min(2*pause, settlePollMax)
		return nil
	}
	// Planning memory is held only from the search on: a first call waiting
	// for its index to settle holds none, so a sync burst cannot starve the
	// pages, seeks and exports of existing FactSets (review R3-1). It is
	// reserved (waiting for memory when the budget is full) only once the
	// watermark reads settled, and given back before every wait.
	for {
		w1, gen1, settled1, err := wm.svc.HistoryQuery().SettledWatermark(c.ctx, wm.branch)
		if err != nil {
			return "", nil, err
		}
		c.st.IndexQueries++
		if settled1 {
			if err := c.needBudget(); err != nil {
				return "", nil, err
			}
			hits, err := runFactSetSearch(c.ctx, wm, req)
			if err != nil {
				return "", nil, err
			}
			if h := factsetSearchHook; h != nil {
				h()
			}
			w2, gen2, settled2, err := wm.svc.HistoryQuery().SettledWatermark(c.ctx, wm.branch)
			if err != nil {
				return "", nil, err
			}
			c.st.IndexQueries += 2
			// gen: a whole Rebuild at the same head between the two reads
			// (review R3-2) leaves w1 == w2 but bumps the generation.
			if w1 == w2 && gen1 == gen2 && settled1 && settled2 {
				return w1, hits, nil
			}
			c.releaseBudget()
		}
		if err := wait(); err != nil {
			return "", nil, err
		}
	}
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

// fsCall is one call's handle: its reader, its stats, its planning memory
// and, while it extends the prefix, the entry's extension token.
type fsCall struct {
	ctx     context.Context
	b       *repos.Binding
	mgr     *repos.Manager
	mayRead func(string) bool
	r       *reader
	e       *fsEntry
	st      Stats
	// res is the planning memory this call holds (nil: none); reserve is
	// how much it takes when it must plan (0: fsPlanReserve of its entry).
	res     *fsReservation
	reserve int64
	token   bool
	// pre: the reads made ahead for the window of units being planned; next:
	// the following window, reading in the background.
	pre  *prefetch
	next *pending
	// noPipeline: no window is read in the background, so every PLANNING
	// read is made while the call holds its planning memory (the export: it
	// gives the memory back between units, and a background window would
	// read outside it; its inflate reads run without it either way).
	noPipeline bool
}

// prefetchWindow is how many units are read ahead at once: a far seek or an
// export of 500 units makes 84 windows of batched reads instead of a root
// read and a level read per unit.
var prefetchWindow = 6

// prefetchParallel is how many statements a window's large reads are split
// into and run at once (the store's read pool has four connections). The
// reads are primary-key probes over a store far larger than one connection's
// page cache, so they are bound by page access, and parallel readers overlap
// it.
const prefetchParallel = 4

// windowUnits is the window of units that starts at u: u and the units after
// it, at most prefetchWindow, all of this FactSet's home.
func windowUnits(fr *factsetFrozen, u int) []int {
	hi := min(len(fr.Roots)+1, u+prefetchWindow)
	units := make([]int, 0, max(0, hi-u))
	for v := u; v < hi; v++ {
		units = append(units, v)
	}
	return units
}

// readAhead reads ahead for a window of units (ascending, all of one home
// repo, the repo lv reads): their roots'
// metadata, their roots' ref rows, and then, in parallel, the metadata of
// those rows' local targets (pinned) and the ref rows of those targets (level
// 1, read speculatively: a target the unit links instead of expanding wastes
// only its read). None of it depends on the written set, so it is what the
// units' plans would read, batched and overlapped.
//
// It touches no field of c (it may run beside the planner): it returns its
// window and what it read.
func (c *fsCall) readAhead(fr *factsetFrozen, lv *index.LevelReader, units []int) (pre *prefetch, st Stats, err error) {
	pre = &prefetch{units: units, rows: map[string][]index.LevelRow{}, metas: map[index.TargetVersion]index.TargetMeta{}}
	var roots []index.TargetVersion
	for _, u := range units {
		if root := fr.Roots[u-1]; root.Blob != "" {
			roots = append(roots, index.TargetVersion{Path: root.Path, Blob: root.Blob, Commit: root.Commit})
		}
	}
	if len(roots) == 0 {
		return pre, st, nil
	}
	metas, parsed, err := lv.Versions(roots)
	if err != nil {
		return nil, st, err
	}
	st.IndexQueries++
	if parsed {
		st.ParsedReads++
	}
	var blobs []string
	for k, v := range metas {
		pre.metas[k] = v
		if v.HasMeta && len(v.Meta.Refs) > 0 {
			blobs = append(blobs, k.Blob)
		}
	}
	if len(blobs) == 0 || fr.Env.Unpinned {
		// Unpinned units read no ref rows: their targets are their refs, at
		// the versions live at the anchor (LiveAt at plan time).
		return pre, st, nil
	}
	rows, err := lv.Refs(blobs)
	if err != nil {
		return nil, st, err
	}
	st.IndexQueries++
	for _, b := range blobs {
		pre.rows[b] = nil
	}
	var targets []index.TargetVersion
	var children []string
	seenChild := map[string]bool{}
	for _, r := range rows {
		pre.rows[r.SourceBlob] = append(pre.rows[r.SourceBlob], r)
		if r.Repo == "" && r.TargetBlob != "" {
			targets = append(targets, index.TargetVersion{Path: r.Path, Blob: r.TargetBlob, Commit: r.TargetCommit})
			if _, root := pre.rows[r.TargetBlob]; !root && !seenChild[r.TargetBlob] {
				seenChild[r.TargetBlob] = true
				children = append(children, r.TargetBlob)
			}
		}
	}
	if fr.Env.Unpinned {
		targets = nil // versioned by LiveAt at the anchor instead
	}
	type part struct {
		metas  map[index.TargetVersion]index.TargetMeta
		rows   []index.LevelRow
		parsed bool
		err    error
	}
	split := func(n int) [][2]int {
		var out [][2]int
		if n == 0 {
			return out
		}
		step := (n + prefetchParallel - 1) / prefetchParallel
		for lo := 0; lo < n; lo += step {
			out = append(out, [2]int{lo, min(n, lo+step)})
		}
		return out
	}
	// Level by level, speculatively (the written set is not known here: a
	// node the unit will link instead of expanding only wastes its read), the
	// versions of the next level's nodes and the rows of this level's nodes,
	// in parallel chunks, to the unit's depth and within a node budget of
	// max_nodes per unit. Pinned only: unpinned and cross-KB versions are
	// read by LiveAt at plan time.
	budget := len(units) * fr.Env.MaxNodes
	read := len(targets)
	for dist := 1; ; dist++ {
		if dist > fr.Env.Depth {
			children = nil // their rows are never read: beyond the depth
		}
		tParts, cParts := split(len(targets)), split(len(children))
		results := make([]part, len(tParts)+len(cParts))
		var wg sync.WaitGroup
		for i, r := range tParts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i].metas, results[i].parsed, results[i].err = lv.Versions(targets[r[0]:r[1]])
			}()
		}
		for i, r := range cParts {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[len(tParts)+i].rows, results[len(tParts)+i].err = lv.Refs(children[r[0]:r[1]])
			}()
		}
		wg.Wait()
		for _, b := range children {
			pre.rows[b] = nil
		}
		var newRows []index.LevelRow
		for _, r := range results {
			if r.err != nil {
				return nil, st, r.err
			}
			st.IndexQueries++
			if r.parsed {
				st.ParsedReads++
			}
			for k, v := range r.metas {
				pre.metas[k] = v
			}
			for _, row := range r.rows {
				pre.rows[row.SourceBlob] = append(pre.rows[row.SourceBlob], row)
				newRows = append(newRows, row)
			}
		}
		if fr.Env.Unpinned || len(newRows) == 0 || dist >= fr.Env.Depth+1 {
			break
		}
		// The next level: the targets of the rows just read, their versions
		// and, in the same round, their rows (a target with no refs costs one
		// empty probe; waiting for its version first would cost a round).
		targets, children = targets[:0], nil
		for _, r := range newRows {
			if r.Repo != "" || r.TargetBlob == "" {
				continue
			}
			tv := index.TargetVersion{Path: r.Path, Blob: r.TargetBlob, Commit: r.TargetCommit}
			if _, ok := pre.metas[tv]; !ok {
				targets = append(targets, tv)
			}
			if _, done := pre.rows[r.TargetBlob]; !done && !seenChild[r.TargetBlob] {
				seenChild[r.TargetBlob] = true
				children = append(children, r.TargetBlob)
			}
		}
		read += len(targets)
		if read > budget || (len(targets) == 0 && len(children) == 0) {
			break
		}
	}
	return pre, st, nil
}

// backgroundReads counts the windows started in the background (a test reads
// it to see that an export starts none).
var backgroundReads atomic.Int64

// pending is a window being read ahead in the background.
type pending struct {
	lo   int
	done chan struct{}
	pre  *prefetch
	st   Stats
	err  error
}

// startReadAhead reads the window of units in the background.
func (c *fsCall) startReadAhead(fr *factsetFrozen, lv *index.LevelReader, units []int) *pending {
	backgroundReads.Add(1)
	pd := &pending{lo: units[0], done: make(chan struct{})}
	go func() {
		defer close(pd.done)
		pd.pre, pd.st, pd.err = c.readAhead(fr, lv, units)
	}()
	return pd
}

// window makes c.pre cover unit u: the window read in the background when it
// is the one u starts, else read now; then the NEXT window starts reading in
// the background while u's window is planned (reads overlap planning). m is
// the home mount: every unit of a FactSet is of its home repo.
func (c *fsCall) window(fr *factsetFrozen, m *mount, u int) error {
	if err := c.r.checkReady(c.ctx, m); err != nil {
		return err
	}
	lv, err := c.r.level(c.ctx, m)
	if err != nil {
		return err
	}
	c.pre = nil
	var pre *prefetch
	var st Stats
	if pd := c.next; pd != nil && pd.lo == u {
		<-pd.done
		pre, st, err = pd.pre, pd.st, pd.err
	} else {
		if pd != nil {
			<-pd.done // a window nobody will plan: let it finish
		}
		pre, st, err = c.readAhead(fr, lv, windowUnits(fr, u))
	}
	c.next = nil
	c.st.add(st)
	if err != nil {
		return err
	}
	c.pre = pre
	if next := pre.last() + 1; next <= len(fr.Roots) && !c.noPipeline {
		c.next = c.startReadAhead(fr, lv, windowUnits(fr, next))
	}
	return nil
}

func (c *fsCall) close() {
	if c.next != nil {
		<-c.next.done // its statements must finish before the reader closes
		c.next = nil
	}
	c.giveToken()
	c.releaseBudget()
	if c.r != nil {
		c.r.close()
	}
}

func (c *fsCall) releaseBudget() {
	if c.res != nil {
		c.res.release()
		c.res = nil
	}
}

// needBudget reserves this call's planning memory, once, waiting for it
// while the budget is full (fsReserve). A call reserves BEFORE it takes the
// extension token, never after: a token holder never waits for memory.
func (c *fsCall) needBudget() error {
	if c.res != nil {
		return nil
	}
	n := c.reserve
	if n == 0 && c.e != nil {
		n = fsPlanReserve(len(c.e.fr.Roots), c.e.fr.Env.MaxNodes, c.e.st.built.Load())
	}
	res, err := fsReserve(c.ctx, n)
	if err != nil {
		return err
	}
	c.res = res
	return nil
}

func (c *fsCall) giveToken() {
	if c.token {
		<-c.e.ext
		c.token = false
		c.e.st.wake() // a waiter takes it, or finds its unit planned
	}
}

// openFactSet checks a cursor's id against sc exactly as every cursor is
// checked (every mismatch is ErrCursorExpired: unknown, another tool's,
// another binding's or read set's, another owner's), writes the bounded
// heartbeat, and returns the call holding its cache entry (given back by
// done; other calls on the FactSet run alongside). The ROW decides on every
// call, cache hit or miss.
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
		if fr.V != frozenVersion || len(fr.Roots) > MaxFactSetFacts {
			// Another version, or a row no first call could have written
			// (the prefix keeps units in 16 bits): it reads as expired.
			c.close()
			return nil, ErrCursorExpired
		}
		c.st.CacheMisses++
		e = fsCachePutIfAbsent(newFSEntry(id, sc.Sessions, &fr))
	}
	// No up-front reach check of the frozen tips: a repo this caller can no
	// longer read (archived, removed, access revoked, a narrower credential)
	// is handled by the read path itself. reader.foreign re-checks target /
	// mayRead (89b26b07) whenever content of that repo is first read in a
	// call, and an unreachable repo reads as kb_unavailable, like a missing
	// target. Units already planned and cached are not re-read.
	c.r.tips = e.fr.Tips
	if c.r.tips == nil {
		c.r.tips = map[string]frozenTip{}
	}
	c.e = e
	// The home is registered before any read: a page served from kept plans
	// plans nothing, yet reads its bodies and RULE 1 marks from the home.
	if err := c.registerHome(); err != nil {
		c.done()
		return nil, err
	}
	return c, nil
}

// done ends the call: its token and memory back, its entry re-measured and
// given back.
func (c *fsCall) done() {
	c.close()
	if c.e != nil {
		fsCacheRelease(c.e)
	}
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

// FactSetOwner is who owns a FactSet: the caller's principal alone
// (auth.Principal.String(), the identity the auth edge verified), with no
// endpoint or route. A FactSet made over MCP is therefore paged, exported and
// closed over REST by the same principal, and the reverse, and across MCP
// endpoints, as long as the binding pin and read set match too (the row
// checks all three). Every other cursor (query, explain, single-fact
// materialize) stays owned by endpoint + principal. A context with no
// principal (an in-process caller) owns as "", which never creates or
// resumes a FactSet.
func FactSetOwner(ctx context.Context) string {
	p, ok := auth.FromContext(ctx)
	if !ok || p.IsZero() {
		return ""
	}
	return p.String()
}

// CloseFactSet ends the FactSet a cursor names (any cursor of it: the position
// is ignored): its session row and frozen inputs are deleted and its cache
// entry dropped. A FactSet of another owner, pin or read set, and an unknown
// one, are ErrCursorExpired alike and nothing is deleted. A cursor that is not
// a FactSet cursor at all (by its syntax) is ErrInvalid. A page already being
// served when the close lands finishes; its cursor then reads as expired.
func CloseFactSet(ctx context.Context, sc CursorScope, cursor string) error {
	id, _, ok := parseCursor(cursor)
	if !ok {
		return fmt.Errorf("%w: close takes a factset cursor (<id>.<unit>.<section>)", ErrInvalid)
	}
	if sc.Pin == "" || sc.Owner == "" {
		return ErrCursorExpired
	}
	closed, err := sc.Sessions.CloseFactSet(ctx, id, sc.Pin, sc.ReadSet, sc.Owner)
	if err != nil {
		return err
	}
	if !closed {
		return ErrCursorExpired
	}
	fsCacheDrop(id)
	return nil
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
	defer c.done()
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

// ensureState builds the FactSet's results and reserved tables (once per
// cache entry, by the call holding the extension token): the two-level
// reservation.
func (c *fsCall) ensureState() error {
	e := c.e
	if e.st.built.Load() {
		return nil
	}
	if err := c.acquireToken(func() bool { return e.st.built.Load() }); err != nil {
		return err
	}
	if !c.token {
		return nil // built by the call that held the token before us
	}
	defer c.giveToken()
	// Unit 1's window does not depend on the reservation: read it while the
	// reservation is built (not for a call that reads nothing in the
	// background: the export).
	if c.next == nil && !c.noPipeline && (c.pre == nil || c.pre.first() != 1) {
		m, err := c.home()
		if err != nil {
			return err
		}
		if err := c.r.checkReady(c.ctx, m); err != nil {
			return err
		}
		lv, err := c.r.level(c.ctx, m)
		if err != nil {
			return err
		}
		c.next = c.startReadAhead(e.fr, lv, windowUnits(e.fr, 1))
	}
	results, reserved, err := c.reserveTables(e.fr)
	if err != nil {
		return err
	}
	e.st.setTables(results, reserved)
	fsCacheAccount(e) // the tables are charged now, not when the call ends
	return nil
}

// acquireToken takes the entry's extension token (the caller must not hold
// it), holding planning memory, unless done() reports the work already done:
// then it returns holding the memory but no token. A call that must wait for another call's token gives
// its memory back while it waits and reserves again once woken: a waiter
// holds no planning memory. Memory is always reserved BEFORE the token is
// taken, so a token holder never waits for memory.
func (c *fsCall) acquireToken(done func() bool) error {
	for {
		if err := c.needBudget(); err != nil {
			return err
		}
		_, woke := c.e.st.extent()
		if done() {
			return nil
		}
		select {
		case c.e.ext <- struct{}{}:
			c.token = true
			if done() { // the holder before us did it
				c.giveToken()
			}
			return nil
		default:
		}
		c.releaseBudget()
		select {
		case <-woke: // the prefix grew, the tables were built, or the token came free
		case <-c.ctx.Done():
			return c.ctx.Err()
		}
	}
}

// factsetExtendHook runs before the call holding the extension token plans
// each unit past the prefix (a test seam: a test blocks an extension in it).
// nil in production.
var factsetExtendHook func(unit int)

// planUnitOf returns unit u's plan (no bodies). A unit within the planned
// prefix is a READ: its kept plan, or the unit re-planned alone against the
// prefix (what units before it wrote), never a replay, and any number of
// calls do this at once. A unit past the prefix is planned by EXTENDING it:
// the call holding the extension token plans the units up to u one by one,
// applying each to the prefix; a call that needs the prefix extended waits,
// on its own context, until another call's extension reaches u or the token
// is free.
func (c *fsCall) planUnitOf(u int) (*plannedUnit, error) {
	if err := c.ensureState(); err != nil {
		return nil, err
	}
	e := c.e
	if pu := e.plan(u); pu != nil {
		return pu, nil
	}
	if err := c.acquireToken(func() bool { planned, _ := e.st.extent(); return u <= planned }); err != nil {
		return nil, err
	}
	if !c.token {
		pu, err := c.planUnit(e.fr, &e.st, u)
		if err != nil {
			return nil, err
		}
		e.keepPlan(u, pu)
		return pu, nil
	}
	defer c.giveToken()
	planned, _ := e.st.extent()
	var pu *plannedUnit
	for next := planned + 1; next <= u; next++ {
		if h := factsetExtendHook; h != nil {
			h(next)
		}
		var err error
		if pu, err = c.planUnit(e.fr, &e.st, next); err != nil {
			return nil, err
		}
		if err := e.st.apply(next, pu); err != nil {
			return nil, err
		}
		e.keepPlan(next, pu)
		// Charge the prefix's growth as it happens: an export or a far seek
		// holds the entry (never evicted) for its whole run.
		fsCacheAccount(e)
	}
	return pu, nil
}

// planUnit plans unit u over the cross-unit state st (no bodies).
func (c *fsCall) planUnit(fr *factsetFrozen, st *fsState, u int) (*plannedUnit, error) {
	root := fr.Roots[u-1]
	m, err := c.home()
	if err != nil {
		return nil, err
	}
	if c.pre == nil || !c.pre.covers(u) {
		if err := c.window(fr, m, u); err != nil {
			return nil, err
		}
	}
	p := &planner{ctx: c.ctx, r: c.r, opts: fr.Env.options(), homeID: fr.HomeRepo, wireID: fr.wireID(), fs: st, unit: u, st: &c.st, pre: c.pre}
	before := c.st.IndexQueries
	defer func() { c.st.MaxUnitQueries = max(c.st.MaxUnitQueries, c.st.IndexQueries-before) }()
	n, err := p.rootNode(m, root.Path, root.Commit, root.Blob, root.Date)
	if err != nil {
		return nil, err
	}
	if n == nil {
		c.st.UnitsPlanned++
		k := key{fr.HomeRepo, root.Path}
		return &plannedUnit{secs: []section{{h: factHash(k.repo, k.rel), repo: k.repo, rel: k.rel, wire: p.wire(k), title: root.Title,
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

// reserveTables builds the results map and the reserved map from the frozen roots
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
func (c *fsCall) reserveTables(fr *factsetFrozen) (results, reserved ftab, err error) {
	collision := func(err error) (ftab, ftab, error) {
		logCollision(err)
		return nil, nil, fmt.Errorf("%w: %w", ErrFactIdentity, err)
	}
	roots := make([]fentry, len(fr.Roots))
	for i, root := range fr.Roots {
		roots[i] = fentry{h: factHash(fr.HomeRepo, root.Path), check: uint32(factCheck(fr.HomeRepo, root.Path)), blob: blob64(root.Blob), unit: uint16(i + 1)}
	}
	if results, err = freeze(roots, func(e fentry) (string, string) { return fr.HomeRepo, fr.Roots[int(e.unit)-1].Path }); err != nil {
		return collision(err)
	}
	opts := fr.Env.options()
	if opts.Depth < 1 || opts.MaxNodes < 2 {
		return results, nil, nil
	}
	// Every depth-1 entry a unit shows is a candidate, in unit order; freeze
	// keeps the first unit's (the earliest owner wins). A result is never a
	// candidate.
	var cands []fentry
	m, err := c.home()
	if err != nil {
		return nil, nil, err
	}
	if err := c.r.checkReady(c.ctx, m); err != nil {
		return nil, nil, err
	}
	const window = 25
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
			return nil, nil, err
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
		var targets []index.RefTarget
		if opts.Unpin {
			// Unpinned, a unit's depth-1 entries are its root's fact refs as
			// written (row or not), exactly as the planner classifies them:
			// in order, each (repo, path) once, the root itself skipped.
			for _, k := range keys {
				mt, ok := metas[k]
				if !ok || fact.Type(mt.Type) == fact.Frame {
					continue
				}
				seen := map[[2]string]bool{}
				for ord, raw := range mt.Refs {
					cr := fact.ClassifyRef(raw, fr.HomeRepo)
					var t index.RefTarget
					switch cr.Kind {
					case fact.RefLocalFact:
						t = index.RefTarget{SourceBlob: k.Blob, Ord: ord, Path: cr.Path}
					case fact.RefForeignFact:
						t = index.RefTarget{SourceBlob: k.Blob, Ord: ord, Repo: cr.RepoID, Path: cr.Path}
					default:
						continue
					}
					if seen[[2]string{t.Repo, t.Path}] || (t.Repo == "" && t.Path == k.Path) {
						continue
					}
					seen[[2]string{t.Repo, t.Path}] = true
					targets = append(targets, t)
				}
			}
		} else {
			if targets, err = m.svc.GraphStore().RefTargetsOfBlobs(c.ctx, blobs); err != nil {
				return nil, nil, err
			}
			c.st.IndexQueries++
		}
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
				return nil, nil, err
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
				return nil, nil, err
			}
			if fm == nil {
				continue
			}
			if err := c.r.checkReady(c.ctx, fm); err != nil {
				return nil, nil, err
			}
			live, hist, err := fm.svc.HistoryQuery().LiveAt(c.ctx, fm.branch, fm.anchor, foreignPaths[id])
			if err != nil {
				return nil, nil, err
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
			unit := i + 1
			included, k := 1, 0
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
					repo = fr.HomeRepo
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
				if ri := results.find(h); ri >= 0 {
					if results[ri].check != uint32(chk) {
						return collision(errHashCollision{repo, t.path})
					}
					continue
				}
				cands = append(cands, fentry{h: h, check: uint32(chk), blob: blob64(blob), unit: uint16(unit), k: uint16(k)})
			}
		}
	}
	if reserved, err = freeze(cands, func(e fentry) (string, string) {
		return "", fmt.Sprintf("FactHash %016x", e.h)
	}); err != nil {
		return collision(err)
	}
	return results, reserved, nil
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
		// A unit shrank since this position was issued (it was re-planned,
		// or planned again after an eviction, without a repo that has since
		// gone): the cursor was valid, so continue at the next unit's start;
		// past the last unit is the normal end-of-FactSet answer.
		from = fpos{from.unit + 1, 0}
		if from.unit > units {
			return FactSetPage{}, fmt.Errorf("%w: the cursor is past the end of the factset; seek with unit (1 to %d)", ErrInvalid, units)
		}
		if pu, err = c.planUnitOf(from.unit); err != nil {
			return FactSetPage{}, err
		}
	}
	type cand struct {
		unit int
		sec  section
		next fpos // the position after this section
	}
	var cands []cand
	at := from
	// walked is the highest unit this page plans: what sequential paging
	// had planned when it served this page (a page's candidates always reach
	// the previous page's last unit), so UnitsWalked and Sections depend on
	// the position only, never on what other calls planned.
	walked := from.unit
	for len(cands) < fr.Limit && at.unit <= units {
		pu, err := c.planUnitOf(at.unit)
		if err != nil {
			return FactSetPage{}, err
		}
		walked = max(walked, at.unit)
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
	page.UnitsWalked = walked
	page.Sections = c.e.st.sectionsThrough(walked)
	return page, nil
}

// inflate turns planned sections into page nodes: bodies of the written ones
// by blob (one batched object read per repo) and the RULE 1 marks (one
// batched branch_facts read per repo, against the reading branch NOW).
func (c *fsCall) inflate(secs []section, unitOf func(int) int) ([]FactSetNode, error) {
	// A section of a foreign repo this caller can no longer read (archived,
	// removed, access revoked: reader.target / mayRead, 89b26b07) is served
	// as unavailable, a heading alone, like a root gone at the anchor. The
	// planned cache is not re-planned, so numbering and structure hold.
	secs = slices.Clone(secs)
	home := c.e.fr.HomeRepo
	gone := map[string]bool{}
	for i, s := range secs {
		if s.unavailable || s.repo == home {
			continue
		}
		g, seen := gone[s.repo]
		if !seen {
			m, err := c.r.foreign(c.ctx, s.repo)
			if err != nil {
				return nil, err
			}
			g = m == nil
			gone[s.repo] = g
		}
		if g {
			secs[i].unavailable = true
		}
	}
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
