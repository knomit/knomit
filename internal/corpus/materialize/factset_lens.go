package materialize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/sessions"
)

// THE LENS FACADE (F3 of the lens FactSet plan, design section 7 and the
// rulings of section 8). A FactSet on a binding that reads more than its
// write repo (Binding.IsLens) materializes the LENS's own result list:
//
//   - ONE fused lens search (federate.PlanFused + FusedSearch/FusedRecent,
//     the function REST /lenses/{lens}/search and MCP knomit_query call),
//     every mount asked for MaxFactSetFacts results the way the REST lens
//     search asks for 500, fused, write-first deduped and truncated to
//     Facts, so the first UIListLimit roots ARE the lens list, in order;
//   - read between two reads of EVERY mount's settled index watermark under
//     ONE deadline (session.factset_anchor_wait): any mount unsettled or
//     moving re-runs the whole fan-out (RRF needs every list), and past the
//     deadline the call is ErrIndexMoved, nothing written;
//   - the fused list partitioned by repo, fused order kept: one SUB-FactSet
//     per lens mount (the F2 seam: freezePinned + the attach/ensureState of
//     startPinned, home = that mount, wire = the lens write repo, CrossKB
//     off), so each sub only ever holds facts of its own repo and the subs'
//     written sets never meet;
//   - ONE session row (frozen v2, lensFrozen) holding the global order (the
//     sub of each global unit) and every sub's (repo, name, branch, anchor,
//     roots). Subs are not session rows. The owner, pin and read-set checks
//     run on that row before any sub home is registered (084b9245);
//   - ONE cache entry holding the N sub entries (summed accounting);
//   - pages, seeks and the export walk GLOBAL units, planning in global
//     order (the planned extent is a global prefix), with one read-ahead
//     window of prefetchWindow GLOBAL units per call, grouped by sub;
//   - unit numbers remapped from a sub's local units to global units at
//     emission, in exactly four fields: FactSetNode.Unit, the first
//     component of FactSetNode.Number, FactSetLink.Unit and the first
//     component of FactSetLink.Number. A back link carries the anchor only
//     and needs nothing; anchors are repo-qualified already.
//
// Rulings: a ref into another lens member is a not-followed leaf with the
// member's repo name and the fact path (Lens 1); cross_kb=true is invalid
// (Lens 2); no blob dedupe across forks, same path is write-first (Lens 3);
// pages carry an anchors map (Lens 4); the no-filter title is "All in lens
// <lens>" (Lens 5); since_fork means the write member's fork only and the
// lens list's repo= narrowing is a search field (Lens 6); every unit root
// carries its repo name (Lens 7).

// frozenVersionLens is a lens facade row's version (frozenVersion is a repo
// FactSet's, which stays readable).
const frozenVersionLens = 2

// lensFrozen is the ONE row a lens FactSet writes (factsets.frozen, v2).
type lensFrozen struct {
	V     int        `json:"v"`
	Env   FactSetEnv `json:"env"`
	Limit int        `json:"limit"`
	// WireRepo is the lens write repo: its paths go out bare, every other
	// member's kb://<id12>/....
	WireRepo string `json:"wire_repo"`
	// Order is the sub of each global unit: Order[g-1] indexes Subs.
	Order []int     `json:"order"`
	Subs  []lensSub `json:"subs"`
}

// lensSub is one lens member's sub-FactSet as frozen: its home repo, the
// branch it was read at, its anchor (the settled watermark its roots were
// read at), and its roots in fused order. Name is the member's repo name at
// creation (the Lens 1 leaves and the Lens 7 unit roots show it), frozen so a
// rename changes no page.
type lensSub struct {
	Repo   string       `json:"repo"`
	Name   string       `json:"name"`
	Branch string       `json:"branch"`
	Anchor string       `json:"anchor"`
	Roots  []frozenRoot `json:"roots"`
}

// frozenVersionOf is a frozen row's version (0 when unreadable).
func frozenVersionOf(raw []byte) int {
	var v struct {
		V int `json:"v"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return 0
	}
	return v.V
}

// decodeLensFrozen reads a v2 row. A row no first call could have written
// (an order naming no sub, a sub whose roots and order disagree, more than
// MaxFactSetFacts units) reads as expired.
func decodeLensFrozen(raw []byte) (*lensFrozen, error) {
	var lf lensFrozen
	if err := json.Unmarshal(raw, &lf); err != nil {
		return nil, fmt.Errorf("factset frozen row: %w", err)
	}
	if lf.V != frozenVersionLens || len(lf.Subs) == 0 || len(lf.Order) > MaxFactSetFacts {
		return nil, ErrCursorExpired
	}
	// One sub per member: two subs of one repo would register one home twice
	// and number two FactSets' units as one.
	seen := make(map[string]bool, len(lf.Subs))
	for _, sub := range lf.Subs {
		if sub.Repo == "" || seen[sub.Repo] {
			return nil, ErrCursorExpired
		}
		seen[sub.Repo] = true
	}
	counts := make([]int, len(lf.Subs))
	for _, s := range lf.Order {
		if s < 0 || s >= len(lf.Subs) {
			return nil, ErrCursorExpired
		}
		counts[s]++
	}
	for i, sub := range lf.Subs {
		if counts[i] != len(sub.Roots) {
			return nil, ErrCursorExpired
		}
	}
	return &lf, nil
}

// lensEntry is a facade cache entry's lens state: the frozen row, one entry
// per sub (never cached on its own) and the unit maps, a pure function of
// Order.
type lensEntry struct {
	fr   *lensFrozen
	subs []*fsEntry
	// globalOf[s][l-1] is sub s's local unit l as a global unit; localOf[g-1]
	// is global unit g's local unit in its sub, fr.Order[g-1].
	globalOf [][]int
	localOf  []int
}

// newLensEntry is the facade cache entry of lf (id "" until the row is
// written): one sub entry per member, each a v1 FactSet over its home with
// the lens write repo as its wire.
func newLensEntry(id string, ti *sessions.ToolIndex, lf *lensFrozen) *fsEntry {
	facade := newFSEntry(id, ti, nil)
	le := &lensEntry{fr: lf, globalOf: make([][]int, len(lf.Subs)), localOf: make([]int, len(lf.Order))}
	for g, s := range lf.Order {
		le.globalOf[s] = append(le.globalOf[s], g+1)
		le.localOf[g] = len(le.globalOf[s])
	}
	members := make(map[string]string, len(lf.Subs))
	for _, s := range lf.Subs {
		members[s.Repo] = s.Name
	}
	for i, s := range lf.Subs {
		env := lf.Env
		env.Anchor, env.Anchors, env.Units, env.CrossKB = s.Anchor, nil, len(s.Roots), false
		fr := &factsetFrozen{V: frozenVersion, Env: env, Limit: lf.Limit, HomeRepo: s.Repo, Branch: s.Branch,
			Roots: s.Roots, sub: i, lensRepos: members}
		if lf.WireRepo != s.Repo {
			fr.WireRepo = lf.WireRepo
		}
		sub := newFSEntry("", ti, fr)
		sub.parent = facade
		le.subs = append(le.subs, sub)
	}
	facade.lens = le
	return facade
}

// at is global unit g's sub and local unit.
func (le *lensEntry) at(g int) (sub, local int) { return le.fr.Order[g-1], le.localOf[g-1] }

// ---- creation -------------------------------------------------------------------

// lensFactSetFirstPage is FactSetFirstPage on a lens (Binding.IsLens): the
// anchored fused search, the partition into one sub per member, the ONE
// facade row and page 1.
func lensFactSetFirstPage(ctx context.Context, sc CursorScope, req FactSetRequest) (FactSetPage, Stats, error) {
	if req.Options.CrossKB {
		// Ruling Lens 2: refs between the lens's own repos are never
		// followed, and nothing else is either.
		return FactSetPage{}, Stats{}, fmt.Errorf("%w: cross_kb is not accepted on a lens factset", ErrInvalid)
	}
	b := req.Binding
	c := &fsCall{ctx: ctx, b: b, mgr: req.Manager, mayRead: req.MayRead,
		reserve: fsPlanReserve(req.Facts, req.Options.MaxNodes, false)}
	c.r = newReader(b, req.Manager, req.MayRead, &c.st)
	defer c.close()

	write := b.Write()
	writeID := fact.ID12(write.ID())
	searchStart := time.Now()
	plan, err := planLensSearch(ctx, req)
	if err != nil {
		return FactSetPage{}, c.st, err
	}
	// Only the mounts the search actually reads are opened, checked and
	// sandwiched: the plan's targets (a member narrowed out by repo= or skipped
	// by the path filter's topic is not one), and under since_fork the write
	// member alone. A member not searched has no roots and needs no anchor, so
	// an unsettled one (never synced, mid-sync) cannot make this call busy.
	//
	// A member whose index has NEVER synced is EMPTY (user ruling,
	// 2026-11-01): it is dropped from the searched targets before the
	// fan-out, so no leg reads its unsettled index, the sandwich does not wait
	// on it and it has no anchor; it stays an empty sub (its name still labels
	// refs into it). See neverSynced for the predicate. A member that has been
	// settled once and is mid-sync still waits.
	reads := b.Reads()
	mounts := make([]*mount, len(reads)) // by Reads() index; nil: not searched
	if !req.NoResults {
		searched := make([]federate.Target, 0, len(plan.Targets))
		for _, t := range plan.Targets {
			if req.Search.SinceFork && fact.ID12(t.RT.RI.ID()) != writeID {
				continue
			}
			m, err := c.r.open(t.RT.RI, t.RT.Branch)
			if err != nil {
				return FactSetPage{}, c.st, err
			}
			empty, err := c.neverSynced(m)
			if err != nil {
				return FactSetPage{}, c.st, err
			}
			if empty {
				continue
			}
			if err := c.r.checkReady(ctx, m); err != nil {
				return FactSetPage{}, c.st, err
			}
			mounts[t.Mount] = m
			searched = append(searched, t)
		}
		plan.Targets = searched
	}
	anchors, per, order, err := c.lensSearchAnchored(mounts, plan, req)
	c.st.Search = time.Since(searchStart)
	if err != nil {
		return FactSetPage{}, c.st, err
	}
	anchorMap := map[string]string{}
	for i, m := range mounts {
		if m != nil {
			anchorMap[m.id12] = anchors[i]
		}
	}
	env := FactSetEnv{
		Mode: FactSetMode, Title: FactSetLensTitle(req.Search, write.OntologyRoot(), b.Name()),
		SearchLine: SearchLine(req.Search), Search: req.Search, Anchor: anchorMap[writeID], Anchors: anchorMap,
		Audience: AudienceHuman, Unpinned: req.Options.Unpin, Depth: req.Options.Depth, MaxNodes: req.Options.MaxNodes,
		FactsLimit: req.Facts, Units: len(order), InstructionsVersion: FactSetInstructionsVersion,
	}
	if len(order) == 0 {
		return FactSetPage{FactSetEnv: env, Facts: []FactSetNode{}}, c.st, nil
	}
	lf := &lensFrozen{V: frozenVersionLens, Env: env, Limit: req.Limit, WireRepo: writeID, Order: order}
	for i, rt := range reads {
		// The F2 seam, once per member: that member's slice of the fused
		// list, its own anchor, the lens write repo as the wire. A member the
		// search did not read is an empty sub with no anchor; it stays a sub
		// so its name labels refs into it (ruling Lens 1).
		id := fact.ID12(rt.RI.ID())
		subEnv := env
		subEnv.Anchor, subEnv.Anchors, subEnv.Units = anchors[i], nil, len(per[i])
		fr := freezePinned(pinnedHome{ID: id, Branch: rt.Branch}, writeID, subEnv, req.Limit, per[i])
		roots := fr.Roots
		if roots == nil {
			roots = []frozenRoot{}
		}
		lf.Subs = append(lf.Subs, lensSub{Repo: id, Name: rt.RI.Name(), Branch: rt.Branch, Anchor: anchors[i], Roots: roots})
	}
	facade := newLensEntry("", sc.Sessions, lf)
	if err := c.attachLens(facade); err != nil {
		return FactSetPage{}, c.st, err
	}
	// startPinned's second half per sub: the reservation tables, built now,
	// as a repo FactSet builds its own at creation. A member with no roots
	// has nothing to build (a legal empty sub).
	for _, sub := range facade.lens.subs {
		if len(sub.fr.Roots) == 0 {
			continue
		}
		c.e = sub
		if err := c.ensureState(); err != nil {
			return FactSetPage{}, c.st, err
		}
	}
	page, err := c.lc.create(c, sc)
	return page, c.st, err
}

// neverSynced reports whether lens member m's index has NEVER reached a
// settled state on its branch: HistoryQuery.SettledWatermark answers an empty
// watermark AND an empty rebuild generation. The watermark is set by the
// first sync to finish and only Index.Rebuild clears it again, and Rebuild
// bumps the generation BEFORE clearing it, so a member that was settled once
// and is now rebuilding (empty watermark, generation set) or syncing (the
// watermark set, a pending marker naming another head) is NOT never-synced:
// it waits in the sandwich as before. A first sync still in flight (no
// watermark yet, no generation) is never-synced: it has no settled state to
// read. Lens members only; a repo FactSet on a never-synced repo keeps the
// repo path's wait.
func (c *fsCall) neverSynced(m *mount) (bool, error) {
	w, gen, _, err := m.svc.HistoryQuery().SettledWatermark(c.ctx, m.branch)
	if err != nil {
		return false, err
	}
	c.st.IndexQueries++
	return w == "" && gen == "", nil
}

// checkRepoNarrowing validates a repo= narrowing on the REPO path. A
// one-member lens is still a lens to its caller: its only member is the write
// repo, and the lens list accepts exactly that name (narrowing nothing) and
// refuses any other with the lens list's own text. A repository's own
// FactSet has no members to narrow to.
func checkRepoNarrowing(b *repos.Binding, names []string) error {
	if len(names) == 0 {
		return nil
	}
	if !b.FromLens() {
		return fmt.Errorf("%w: repo is only accepted on a lens", ErrInvalid)
	}
	for _, n := range names {
		if n != b.Write().Name() {
			return fmt.Errorf("%w: %v", ErrInvalid, &federate.UnknownRepoError{Name: n, Lens: b.Name()})
		}
	}
	return nil
}

// planLensSearch plans the fused lens search of req exactly as the lens list
// plans it (federate.PlanFused: motif widening over the full read set, the
// targets of the path filter, repo= narrowing, one query vector), every mount
// asked for MaxFactSetFacts results, as the REST lens search asks every
// mount for its 500 candidates. A malformed path or an unknown repo is the
// caller's to fix (ErrInvalid); anything else is an error of the read.
func planLensSearch(ctx context.Context, req FactSetRequest) (*federate.FusedPlan, error) {
	q := req.Search.options(MaxFactSetFacts, nil)
	q.Path = "" // set per leg, to the target's repo-relative path filter
	var embed func(context.Context, string) ([]float32, error)
	if req.Embed != nil {
		embed = func(ctx context.Context, text string) ([]float32, error) {
			if text == "" {
				return nil, nil
			}
			v, err := req.Embed(ctx, text)
			if err != nil {
				return nil, fmt.Errorf("search: embed query: %w", err)
			}
			if v == nil {
				return nil, nil // no embedder: every mount embeds for itself
			}
			if len(v) == 0 {
				return nil, errors.New("search: embed query: embedder returned an empty vector")
			}
			return v, nil
		}
	}
	plan, err := federate.PlanFused(ctx, federate.FusedQuery{
		Binding: req.Binding, Path: req.Search.Path, Repos: req.Search.Repos, Opts: q,
		Clusters: req.Clusters, Embed: embed,
	})
	if err != nil {
		var pe *federate.PlanError
		if errors.As(err, &pe) && (pe.Stage == federate.StageTargets || pe.Stage == federate.StageRepos) {
			return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
		}
		return nil, err
	}
	return plan, nil
}

// lensSearchAnchored runs the fused search between two reads of EVERY
// mount's settled index watermark, under ONE deadline
// (session.factset_anchor_wait). When every mount is settled on both sides
// and none moved, each hit's (commit, blob) is its version at its own
// mount's anchor. Any mount unsettled or moving re-runs the whole fan-out
// (fusion needs every list); past the deadline it is ErrIndexMoved. It
// returns each mount's anchor (by Binding.Reads() index), the hits of each
// mount in fused order, and the mount of each global unit.
func (c *fsCall) lensSearchAnchored(mounts []*mount, plan *federate.FusedPlan, req FactSetRequest) ([]string, [][]factsetHit, []int, error) {
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
	// gen: a whole Rebuild at the same head between the two reads leaves the
	// watermark equal but bumps the generation (review R3-2 of the repo path).
	type mark struct {
		w, gen  string
		settled bool
	}
	look := func() ([]mark, bool, error) {
		out := make([]mark, len(mounts))
		all := true
		for i, m := range mounts {
			if m == nil {
				continue // not searched: no roots, no anchor (see lensFactSetFirstPage)
			}
			w, gen, settled, err := m.svc.HistoryQuery().SettledWatermark(c.ctx, m.branch)
			if err != nil {
				return nil, false, err
			}
			c.st.IndexQueries++
			out[i] = mark{w, gen, settled}
			all = all && settled
		}
		return out, all, nil
	}
	for {
		before, settled, err := look()
		if err != nil {
			return nil, nil, nil, err
		}
		if settled {
			// Planning memory from the search on, never while waiting for the
			// indexes to settle (review R3-1 of the repo path).
			if err := c.needBudget(); err != nil {
				return nil, nil, nil, err
			}
			per, order, err := c.runLensSearch(mounts, plan, req)
			if err != nil {
				return nil, nil, nil, err
			}
			if h := factsetSearchHook; h != nil {
				h()
			}
			after, settledAfter, err := look()
			if err != nil {
				return nil, nil, nil, err
			}
			same := settledAfter
			for i := range mounts {
				same = same && before[i] == after[i]
			}
			if same {
				anchors := make([]string, len(mounts))
				for i := range mounts {
					anchors[i] = before[i].w
				}
				return anchors, per, order, nil
			}
			c.releaseBudget()
		}
		if err := wait(); err != nil {
			return nil, nil, nil, err
		}
	}
}

// runLensSearch is ONE fused fan-out over the opened mounts: each leg reads
// its own mount (keyed by the target's repo, never its plan position), the
// union is fused, write-first deduped and truncated to req.Facts, then
// partitioned by mount (Target.Mount, the stable Binding.Reads() index) in
// fused order. Under since_fork only the write member's leg reads, limited
// to its fork's changed paths (ruling Lens 6 / Q-L7); every other member
// contributes nothing, as on the lens list.
func (c *fsCall) runLensSearch(mounts []*mount, plan *federate.FusedPlan, req FactSetRequest) ([][]factsetHit, []int, error) {
	byID := make(map[string]*mount, len(mounts))
	for _, m := range mounts {
		if m != nil {
			byID[m.id12] = m
		}
	}
	writeID := fact.ID12(c.b.Write().ID())
	legMount := func(t federate.Target, q *index.SearchOptions) (*mount, bool, error) {
		id := fact.ID12(t.RT.RI.ID())
		if req.NoResults {
			return nil, false, nil
		}
		if req.Search.SinceFork {
			if id != writeID {
				return nil, false, nil
			}
			q.PathsIn = req.PathsIn
		}
		m := byID[id]
		if m == nil {
			return nil, false, fmt.Errorf("lens mount %s was not opened", t.RT.RI.Name())
		}
		return m, true, nil
	}
	type hit struct {
		mount int
		h     factsetHit
	}
	var hits []hit
	if req.Search.Sort == SortRecent {
		// The lens list's recency read, merge-horizon deepening included
		// (federate.FusedRecentDeep, shared with REST /lenses/{lens}/facts):
		// write-first dedupe can spend a fork's whole depth on copies, so a
		// text-less listing runs deeper until req.Facts merged rows are
		// provably complete. With text it runs once.
		res, _, err := federate.FusedRecentDeep(c.ctx, plan, func(ctx context.Context, t federate.Target, q index.SearchOptions) ([]index.RecentFactEntry, int, error) {
			m, read, err := legMount(t, &q)
			if err != nil || !read {
				return nil, 0, err
			}
			return m.svc.FactQuery().RecentFacts(ctx, m.branch, q)
		}, federate.FusedOptions{}, MaxFactSetFacts, req.Facts, federate.MaxRecencyDepth)
		if err != nil {
			return nil, nil, fmt.Errorf("search: %w", err)
		}
		for _, h := range res.Hits {
			e := h.Row
			hits = append(hits, hit{h.Target.Mount, factsetHit{e.Path, e.Title, e.CommitHash, e.BlobHash, e.CommittedAt}})
		}
	} else {
		res, err := federate.FusedSearch(c.ctx, plan, func(ctx context.Context, t federate.Target, q index.SearchOptions) ([]index.SearchResult, error) {
			m, read, err := legMount(t, &q)
			if err != nil || !read {
				return nil, err
			}
			return m.svc.FactQuery().Search(ctx, m.branch, q)
		}, federate.FusedOptions{})
		if err != nil {
			return nil, nil, fmt.Errorf("search: %w", err)
		}
		for _, h := range res.Hits {
			e := h.Row
			hits = append(hits, hit{h.Target.Mount, factsetHit{e.Path, e.Title, e.CommitHash, e.BlobHash, e.CommittedAt}})
		}
	}
	c.st.IndexQueries += len(plan.Targets)
	if len(hits) > req.Facts {
		hits = hits[:req.Facts]
	}
	per := make([][]factsetHit, len(mounts))
	order := make([]int, 0, len(hits))
	for _, h := range hits {
		if h.mount < 0 || h.mount >= len(mounts) {
			return nil, nil, fmt.Errorf("lens search: hit of mount %d outside the binding's %d mounts", h.mount, len(mounts))
		}
		per[h.mount] = append(per[h.mount], h.h)
		order = append(order, h.mount)
	}
	return per, order, nil
}

// ---- a call on a lens facade -------------------------------------------------------

// lensCall is a facade call's state: the facade entry, the reads of the
// current window of global units (one prefetch per sub home it touches),
// and the next window, read in the background.
type lensCall struct {
	le     *lensEntry
	facade *fsEntry
	pres   map[string]*prefetch
	next   *lensPending
}

// lensPending is a window of global units being read ahead in the
// background.
type lensPending struct {
	lo   int
	done chan struct{}
	pres map[string]*prefetch
	st   Stats
	err  error
}

// attachLens makes the facade entry e the FactSet c works on: no tips (a sub
// never follows a foreign ref) and EVERY sub's home registered before any
// read (084b9245: a page served from kept plans reads bodies and RULE 1
// marks from several homes). The caller has checked the row's owner, pin
// and read set first; registration refuses a home the binding no longer
// mounts as an expired cursor.
func (c *fsCall) attachLens(e *fsEntry) error {
	le := e.lens
	c.lc = &lensCall{le: le, facade: e}
	c.e = e
	c.r.tips = map[string]frozenTip{}
	writeID := fact.ID12(c.b.Write().ID())
	// The row must describe THIS binding: its wire is the binding's write
	// repo, and every read member is mounted at the branch the row froze. The
	// read-set fingerprint already covers both; this keeps a row that
	// disagrees (a forged or stale scope) from being served at all.
	if le.fr.WireRepo != writeID {
		return ErrCursorExpired
	}
	built := true
	for _, sub := range le.subs {
		fr := sub.fr
		branch := fr.Branch
		if fr.HomeRepo == writeID {
			branch = c.b.WriteMountBranch()
		} else if rt, ok := c.b.ByID(fr.HomeRepo); ok {
			if rt.Branch != fr.Branch {
				return ErrCursorExpired
			}
			branch = rt.Branch
		}
		if err := c.r.registerHome(fr.HomeRepo, branch, fr.Env.Anchor); err != nil {
			return err
		}
		if len(fr.Roots) > 0 && !sub.st.built.Load() {
			built = false
		}
	}
	if c.reserve == 0 {
		// ONE planning reservation for the whole facade call, sized for all
		// its units, never one per sub.
		c.reserve = fsPlanReserve(len(le.fr.Order), le.fr.Env.MaxNodes, built)
	}
	return nil
}

// wait lets a background window finish (its statements must end before the
// reader closes).
func (lc *lensCall) wait() {
	if lc.next != nil {
		<-lc.next.done
		lc.next = nil
	}
}

// globalPlanned is the planned GLOBAL prefix: the highest g such that every
// global unit 1..g is planned in its sub.
func (lc *lensCall) globalPlanned() int {
	ext := make([]int, len(lc.le.subs))
	for i, sub := range lc.le.subs {
		if sub.st.built.Load() {
			ext[i], _ = sub.st.extent()
		}
	}
	for g := 1; g <= len(lc.le.fr.Order); g++ {
		s, l := lc.le.at(g)
		if l > ext[s] {
			return g - 1
		}
	}
	return len(lc.le.fr.Order)
}

// planGlobal returns global unit g's plan (its sub's local unit, planned by
// the sub exactly as a repo FactSet plans a unit). It plans in GLOBAL order:
// every global unit before g is planned first, so the planned extent is a
// global prefix and units_walked / sections keep their meaning in global
// units. The subs are independent (no fact of one is in another), so the
// order changes no byte, only which units are planned.
func (lc *lensCall) planGlobal(c *fsCall, g int) (*plannedUnit, error) {
	for gg := lc.globalPlanned() + 1; gg < g; gg++ {
		s, l := lc.le.at(gg)
		c.e = lc.le.subs[s]
		if _, err := c.planUnitOf(l); err != nil {
			return nil, err
		}
	}
	s, l := lc.le.at(g)
	c.e = lc.le.subs[s]
	return c.planUnitOf(l)
}

// lensGroup is one sub's local units within a window of global units.
type lensGroup struct {
	sub   int
	units []int
}

// groups is the window of global units that starts at g (at most
// prefetchWindow), grouped by sub in order of first appearance, each sub's
// local units ascending; hi is the first global unit after it.
func (lc *lensCall) groups(g int) (out []lensGroup, hi int) {
	hi = min(len(lc.le.fr.Order)+1, g+prefetchWindow)
	idx := map[int]int{}
	for gg := g; gg < hi; gg++ {
		s, l := lc.le.at(gg)
		i, ok := idx[s]
		if !ok {
			i = len(out)
			idx[s] = i
			out = append(out, lensGroup{sub: s})
		}
		out[i].units = append(out[i].units, l)
	}
	return out, hi
}

// levels opens, on the caller's goroutine, the home mount and level reader
// of every sub a window touches (the reader's maps are not shared with a
// background read).
func (lc *lensCall) levels(c *fsCall, gs []lensGroup) ([]*index.LevelReader, error) {
	lvs := make([]*index.LevelReader, len(gs))
	for i, gr := range gs {
		home := lc.le.subs[gr.sub].fr.HomeRepo
		m, ok, err := c.r.homeMount(home)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("factset: home repo %s not registered for this call", home)
		}
		if err := c.r.checkReady(c.ctx, m); err != nil {
			return nil, err
		}
		if lvs[i], err = c.r.level(c.ctx, m); err != nil {
			return nil, err
		}
	}
	return lvs, nil
}

// read reads a window ahead: one readAhead per sub it touches, each on its
// own home's level reader (the prefetch is keyed by (home, unit)). It
// touches no field of c or lc.
func (lc *lensCall) read(c *fsCall, gs []lensGroup, lvs []*index.LevelReader) (map[string]*prefetch, Stats, error) {
	pres := make(map[string]*prefetch, len(gs))
	var st Stats
	for i, gr := range gs {
		fr := lc.le.subs[gr.sub].fr
		pre, s, err := c.readAhead(fr, lvs[i], gr.units)
		st.add(s)
		if err != nil {
			return nil, st, err
		}
		pres[fr.HomeRepo] = pre
	}
	return pres, st, nil
}

// cover makes c.pre cover sub fr's local unit u: the current window's reads
// of fr's home when they cover it, else the window of global units that
// starts at u's global unit (the one read in the background when it is that
// window, else read now); then the NEXT window starts reading in the
// background (not for the export).
func (lc *lensCall) cover(c *fsCall, fr *factsetFrozen, u int) error {
	if pre := lc.pres[fr.HomeRepo]; pre != nil && pre.covers(fr.HomeRepo, u) {
		c.pre = pre
		return nil
	}
	g := lc.le.globalOf[fr.sub][u-1]
	gs, hi := lc.groups(g)
	var pres map[string]*prefetch
	var st Stats
	var err error
	if pd := lc.next; pd != nil && pd.lo == g {
		<-pd.done
		pres, st, err = pd.pres, pd.st, pd.err
	} else {
		lc.wait() // a window nobody will plan: let it finish
		var lvs []*index.LevelReader
		if lvs, err = lc.levels(c, gs); err == nil {
			pres, st, err = lc.read(c, gs, lvs)
		}
	}
	lc.next = nil
	c.st.add(st)
	if err != nil {
		return err
	}
	lc.pres = pres
	c.pre = pres[fr.HomeRepo]
	if hi <= len(lc.le.fr.Order) && !c.noPipeline {
		next, _ := lc.groups(hi)
		lvs, err := lc.levels(c, next)
		if err != nil {
			return err
		}
		backgroundReads.Add(1)
		pd := &lensPending{lo: hi, done: make(chan struct{})}
		go func() {
			defer close(pd.done)
			pd.pres, pd.st, pd.err = lc.read(c, next, lvs)
		}()
		lc.next = pd
	}
	return nil
}

// remapNumber rewrites the first component of a section number ("3",
// "3.1.2") from a sub's local unit to its global unit; "" (a frame's
// unnumbered ancestors) stays "".
func remapNumber(num string, globalOf []int) string {
	if num == "" {
		return num
	}
	head, rest, dotted := strings.Cut(num, ".")
	u, err := strconv.Atoi(head)
	if err != nil || u < 1 || u > len(globalOf) {
		return num
	}
	out := strconv.Itoa(globalOf[u-1])
	if dotted {
		out += "." + rest
	}
	return out
}

// remap turns a node of sub s, as the sub emitted it (local units), into the
// facade's node: EXACTLY four fields change, FactSetNode.Unit, the first
// component of FactSetNode.Number, FactSetLink.Unit and the first component
// of FactSetLink.Number (a forward link's target is always in the same sub,
// so the same map applies; a back link carries the anchor only). Then a unit
// root carries its repo's name (ruling Lens 7).
func (le *lensEntry) remap(s int, n *FactSetNode) {
	g := le.globalOf[s]
	n.Unit = g[n.Unit-1]
	n.Number = remapNumber(n.Number, g)
	if l := n.Link; l != nil && l.Unit > 0 {
		l.Unit = g[l.Unit-1]
		l.Number = remapNumber(l.Number, g)
	}
	if n.Number == strconv.Itoa(n.Unit) {
		n.RepoName = le.fr.Subs[s].Name
	}
}

// sectionsThrough is the sections of global units 1..g: each sub's sections
// of its local units among them (planned: the global prefix covers g).
func (le *lensEntry) sectionsThrough(g int) int {
	locals := make([]int, len(le.subs))
	for gg := 1; gg <= g; gg++ {
		s, l := le.at(gg)
		locals[s] = l
	}
	n := 0
	for s, sub := range le.subs {
		if locals[s] > 0 {
			n += sub.st.sectionsThrough(locals[s])
		}
	}
	return n
}

// create writes the facade's ONE session row, caches its entry and returns
// page 1; a failure after the row was written deletes it.
func (lc *lensCall) create(c *fsCall, sc CursorScope) (FactSetPage, error) {
	ctx, facade := c.ctx, lc.facade
	frozen, err := json.Marshal(lc.le.fr)
	if err != nil {
		return FactSetPage{}, err
	}
	id, err := sc.Sessions.CreateFactSet(ctx, factsetTool(lc.le.fr.Limit), sc.Branch, sc.Pin, sc.ReadSet, sc.Owner, frozen)
	if err != nil {
		return FactSetPage{}, err
	}
	facade.id = id
	fsCachePut(facade)
	defer fsCacheRelease(facade)
	page, err := lc.page(c, fpos{1, 0}, true)
	if err != nil {
		fsCacheDrop(id)
		_ = sc.Sessions.DeleteFactSet(context.WithoutCancel(ctx), id)
		return FactSetPage{}, err
	}
	return page, nil
}

// nextPage is FactSetNextPage on the facade: the page at the cursor's GLOBAL
// position, or a seek to global unit (1..units).
func (lc *lensCall) nextPage(c *fsCall, at fpos, unit int) (FactSetPage, error) {
	units := len(lc.le.fr.Order)
	if unit != 0 {
		if unit < 1 || unit > units {
			return FactSetPage{}, fmt.Errorf("%w: unit must be between 1 and %d, got %d", ErrInvalid, units, unit)
		}
		at = fpos{unit, 0}
	}
	return lc.page(c, at, false)
}

// page is fsCall.page over GLOBAL units (the shared buildPage): each global
// unit planned through its sub, inflated with the sub's local unit, then
// remapped; one inflate covers sections of any number of homes.
func (lc *lensCall) page(c *fsCall, from fpos, first bool) (FactSetPage, error) {
	le := lc.le
	return c.buildPage(pageSource{
		env: le.fr.Env, limit: le.fr.Limit, units: len(le.fr.Order), id: lc.facade.id,
		plan:     func(g int) (*plannedUnit, error) { return lc.planGlobal(c, g) },
		unitOf:   func(g int) int { return le.localOf[g-1] },
		emit:     func(g int, n *FactSetNode) { le.remap(le.fr.Order[g-1], n) },
		sections: le.sectionsThrough,
	}, from, first)
}

// export is FactSetExport on the facade: every GLOBAL unit in order, each
// planned by its sub (extending the global prefix as a page would), its
// sections inflated exportChunk at a time and remapped, so the document's
// "## <unit>." headings, "### <number>" headings and "(see N)" links all
// carry global numbers. Planning memory is given back after each unit,
// before anything is written.
func (lc *lensCall) export(c *fsCall, start func(env FactSetEnv) io.Writer) error {
	le := lc.le
	var w io.Writer
	nums := factsetNumbers{}
	for g := 1; g <= len(le.fr.Order); g++ {
		if f := factsetExportFault; f != nil && w != nil {
			if err := f(g); err != nil {
				return err
			}
		}
		pu, err := lc.planGlobal(c, g)
		c.releaseBudget()
		if err != nil {
			return err
		}
		s, l := le.at(g)
		for lo := 0; lo < len(pu.secs); lo += exportChunk {
			part := pu.secs[lo:min(len(pu.secs), lo+exportChunk)]
			nodes, err := c.inflate(part, func(int) int { return l })
			if err != nil {
				return err
			}
			for i := range nodes {
				le.remap(s, &nodes[i])
			}
			if w == nil {
				w = start(le.fr.Env)
				if _, err := io.WriteString(w, renderFactSetHeader(le.fr.Env)); err != nil {
					return err
				}
			}
			var sb strings.Builder
			for _, n := range nodes {
				sb.WriteString(renderFactSetNode(n, nums))
			}
			if _, err := io.WriteString(w, sb.String()); err != nil {
				return err
			}
		}
	}
	return nil
}
