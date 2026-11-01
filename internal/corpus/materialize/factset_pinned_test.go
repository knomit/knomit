package materialize

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/storetest"
)

// The pinned seam (F2): a FactSet built from SUPPLIED pinned roots
// (freezePinned + startPinned + createPinned), with a home repo (where the
// roots live) and a wire repo (whose paths go out bare). The repo path is
// home = wire = write; a lens sub-FactSet (F3) has home = a read member R and
// wire = the lens write repo. Until F3, home != wire is reachable only here.

// newBetaFixture is a second repo ("beta") in its own store, with a binding
// of its own (for its repo FactSet) and the same writers as the alpha
// fixture.
func newBetaFixture(t testing.TB) *factsetFixture {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "beta.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "beta", UID: "uid-beta", AgentBranch: "agent/test", Svc: svc, OntologyRoot: "kb",
	})
	return &factsetFixture{ri: ri, svc: svc, b: repos.NewBindingOfRepo(ri, "agent/test")}
}

// seedABCBodies is seedABC with every body tagged, so one repo's blobs differ
// from another's for the same paths and refs.
func seedABCBodies(t testing.TB, f *factsetFixture, tag string) {
	t.Helper()
	for _, leaf := range []string{"p", "r", "s"} {
		f.write(t, "kb/z/"+leaf+".md", strings.ToUpper(leaf), leaf+" body "+tag+".")
	}
	f.write(t, "kb/z/q.md", "Q", "q body "+tag+".", "kb/z/r.md")
	f.write(t, "kb/z/z.md", "Z", "z body "+tag+".", "kb/z/p.md", "kb/z/s.md")
	f.write(t, "kb/z/w.md", "W", "w body "+tag+".", "kb/z/q.md", "kb/z/r.md")
	f.write(t, "kb/z/y.md", "Y", "y body "+tag+".", "kb/z/q.md")
	f.write(t, "kb/z/x.md", "X", "x body "+tag+".", "kb/z/p.md", "kb/z/z.md")
	f.write(t, "kb/u/c.md", "C", "c body "+tag+".", "kb/z/w.md", "kb/z/x.md")
	f.write(t, "kb/u/b.md", "B", "b body "+tag+".", "kb/z/y.md", "kb/z/z.md", "kb/u/c.md")
	f.write(t, "kb/u/a.md", "A", "a body "+tag+".", "kb/z/x.md", "kb/z/y.md")
}

// frozenOf is the frozen row of a FactSet cursor, as stored.
func frozenOf(t testing.TB, sc CursorScope, cursor string) ([]byte, *factsetFrozen) {
	t.Helper()
	row, err := sc.Sessions.GetFactSet(context.Background(), mustID(t, cursor))
	require.NoError(t, err)
	require.NotNil(t, row)
	var fr factsetFrozen
	require.NoError(t, json.Unmarshal(row.Frozen, &fr))
	return row.Frozen, &fr
}

// hitsOf turns frozen roots back into the hits a search returned.
func hitsOf(fr *factsetFrozen) []factsetHit {
	hits := make([]factsetHit, len(fr.Roots))
	for i, r := range fr.Roots {
		hits[i] = factsetHit{path: r.Path, title: r.Title, commit: r.Commit, blob: r.Blob, date: r.Date}
	}
	return hits
}

// firstPinned is the seam as a caller other than FactSetFirstPage uses it:
// freezePinned with the given home and wire, then startPinned and
// createPinned on a call over b. It returns page 1 and what the call read.
func firstPinned(t testing.TB, sc CursorScope, b *repos.Binding, home pinnedHome, wire string, env FactSetEnv, limit int, hits []factsetHit) (FactSetPage, Stats, error) {
	t.Helper()
	ctx := context.Background()
	c := &fsCall{ctx: ctx, b: b}
	c.r = newReader(b, nil, nil, &c.st)
	defer c.close()
	fr := freezePinned(home, wire, env, limit, hits)
	if err := c.startPinned(fr, sc.Sessions); err != nil {
		return FactSetPage{}, c.st, err
	}
	p, err := c.createPinned(sc)
	return p, c.st, err
}

// pagesWith pages a FactSet to its end over binding b and returns every
// page's JSON with the cursor id normalized, plus the most index reads one
// unit made and the index reads of the later pages.
func pagesWith(t testing.TB, sc CursorScope, b *repos.Binding, p FactSetPage) (out []string, maxQ, later int) {
	t.Helper()
	id := mustID(t, *p.Cursor)
	for {
		raw, err := json.Marshal(p)
		require.NoError(t, err)
		out = append(out, strings.ReplaceAll(string(raw), id, "ID"))
		if !p.HasMore {
			return out, maxQ, later
		}
		var st Stats
		p, st, err = FactSetNextPageStats(context.Background(), sc, b, nil, nil, *p.Cursor, 0)
		require.NoError(t, err)
		maxQ, later = max(maxQ, st.MaxUnitQueries), later+st.IndexQueries
	}
}

// exportWith is the export of a FactSet over binding b.
func exportWith(t testing.TB, sc CursorScope, b *repos.Binding, cursor string) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, FactSetExport(context.Background(), sc, b, nil, nil, cursor, func(FactSetEnv) io.Writer { return &buf }))
	return buf.String()
}

// The seam equals FactSetFirstPage. The repo path (FactSetFirstPage: search,
// then the seam with home = wire = write) and the seam called directly with
// the same home, wire, env and the roots the search returned make the SAME
// frozen row byte for byte (no wire_repo field: omitempty) and the same first
// page, later pages, seek and export. Both sides run the seam, so this does
// NOT show identity with the code before the split: that is held by the
// existing suite (F1-F22, random access) and by
// TestFactSet_RepoFrozenRowAndFirstPageGolden, whose goldens were generated
// on the pre-seam code.
//
// Sabotage: make freezePinned always set WireRepo (drop the wireID != home
// guard), and the repo path's own row carries "wire_repo": its frozen bytes
// are no longer what they were before the split.
func TestFactSet_Pinned_RepoPathIsByteIdentical(t *testing.T) {
	isolateFactSetGlobals(t)
	f := newFactSetFixture(t)
	seedABC(t, f)
	sc := f.scope("o")
	req := f.request("kb/u/")
	req.Options.Depth, req.Options.MaxNodes, req.Limit = 3, MaxFactSetFacts, 2

	pa, _, err := FactSetFirstPage(context.Background(), sc, req)
	require.NoError(t, err)
	rawA, frA := frozenOf(t, sc, *pa.Cursor)
	require.NotContains(t, string(rawA), "wire_repo", "a repo FactSet's row has no wire field")
	require.Equal(t, f.id12(), frA.HomeRepo)
	require.Equal(t, 3, len(frA.Roots))

	pb, _, err := firstPinned(t, sc, f.b, pinnedHome{ID: frA.HomeRepo, Branch: frA.Branch}, f.id12(), frA.Env, frA.Limit, hitsOf(frA))
	require.NoError(t, err)
	require.NotEqual(t, mustID(t, *pa.Cursor), mustID(t, *pb.Cursor), "two FactSets")
	rawB, _ := frozenOf(t, sc, *pb.Cursor)
	require.Equal(t, string(rawA), string(rawB), "the frozen row is byte-identical")

	pagesA, _, _ := pagesWith(t, sc, f.b, pa)
	pagesB, _, _ := pagesWith(t, sc, f.b, pb)
	require.Greater(t, len(pagesA), 3, "several pages")
	require.Equal(t, pagesA, pagesB, "first and later pages are byte-identical")

	seek := func(p FactSetPage) string {
		s, err := FactSetNextPage(context.Background(), sc, f.b, nil, nil, *p.Cursor, 2)
		require.NoError(t, err)
		return strings.ReplaceAll(mustJSON(t, s), mustID(t, *p.Cursor), "ID")
	}
	require.Equal(t, seek(pa), seek(pb), "a seek is byte-identical")
	require.Equal(t, exportWith(t, sc, f.b, *pa.Cursor), exportWith(t, sc, f.b, *pb.Cursor), "the export is byte-identical")
}

// lensPair is a two-member lens binding over alpha (the write repo W) and
// beta (R), both seeded with the ABC example under distinct bodies, plus
// beta's own repo FactSet over the same search: the baseline the sub-FactSet
// is compared against.
type lensPair struct {
	alpha, beta *factsetFixture
	lens        *repos.Binding
	sc          CursorScope // the lens's cursor scope: W's session DB
	betaSC      CursorScope // beta's repo FactSet's scope
	base        FactSetPage // beta's repo FactSet, page 1
	baseFR      *factsetFrozen
	baseSt      Stats
	r           string // beta's id12
}

func newLensPair(t testing.TB, depth, limit int) *lensPair {
	t.Helper()
	alpha, beta := newFactSetFixture(t), newBetaFixture(t)
	seedABCBodies(t, alpha, "in W")
	seedABCBodies(t, beta, "in R")
	lp := &lensPair{alpha: alpha, beta: beta, r: beta.id12()}
	lp.lens = repos.NewBindingForTest(alpha.ri,
		repos.ReadTarget{RI: alpha.ri, Branch: "agent/test"},
		repos.ReadTarget{RI: beta.ri, Branch: "agent/test"})
	require.True(t, lp.lens.IsLens())
	lp.sc = CursorScope{Sessions: alpha.svc.ToolSession(), Branch: "agent/test", Pin: lp.lens.PinID(), ReadSet: "rs", Owner: "o"}
	lp.betaSC = beta.scope("o")
	req := beta.request("kb/u/")
	req.Options.Depth, req.Options.MaxNodes, req.Limit = depth, MaxFactSetFacts, limit
	var err error
	lp.base, lp.baseSt, err = FactSetFirstPage(context.Background(), lp.betaSC, req)
	require.NoError(t, err)
	_, lp.baseFR = frozenOf(t, lp.betaSC, *lp.base.Cursor)
	require.Equal(t, lp.r, lp.baseFR.HomeRepo)
	return lp
}

// sub builds the sub-FactSet over the lens: home R (beta, at the anchor and
// with the roots beta's own search froze), wire W (the lens write repo).
func (lp *lensPair) sub(t testing.TB) (FactSetPage, Stats) {
	t.Helper()
	p, st, err := firstPinned(t, lp.sc, lp.lens, pinnedHome{ID: lp.r, Branch: "agent/test"}, lp.alpha.id12(),
		lp.baseFR.Env, lp.baseFR.Limit, hitsOf(lp.baseFR))
	require.NoError(t, err)
	return p, st
}

// asWire is a repo FactSet page's JSON with its home's bare paths rewritten
// to kb://<home>/…: what the same FactSet is on a lens whose write repo is
// another repo. The frozen search (env.search.path) is the request's, not a
// fact's path: it stays as it was.
func asWire(pages []string, home string) []string {
	out := make([]string, len(pages))
	q := fact.KBScheme + home + "/kb/"
	for i, p := range pages {
		p = strings.ReplaceAll(p, `"path":"kb/`, `"path":"`+q)
		out[i] = strings.ReplaceAll(p, `"search":{"path":"`+q, `"search":{"path":"kb/`)
	}
	return out
}

// Home != wire: a sub-FactSet whose home is a lens read member R (beta) and
// whose wire is the lens write repo W (alpha).
//   - every section's path is kb://R/… (wire by W, not by home): the sub's
//     pages are beta's own repo FactSet's pages with only the paths
//     rewritten, byte for byte;
//   - the "N.k" reservations are R facts at R's pinned blobs: the reserved
//     table is keyed by FactHash(R, path) and holds blob64 of R's version,
//     which differs from W's version of the same path;
//   - bodies are R's.
//
// Sabotage 1 (wire by home): build the planner with wireID = fr.HomeRepo, and
// the sub's paths are bare (the asWire comparison fails).
// Sabotage 2 (classify against the write repo): reserveTables keys a local
// direct reference by fr.wireID() instead of fr.HomeRepo, and nothing of R is
// reserved: Z is not reserved as 2.2 and unit 1 writes it instead of linking
// forward.
func TestFactSet_Pinned_HomeIsNotTheWireRepo(t *testing.T) {
	isolateFactSetGlobals(t)
	lp := newLensPair(t, 3, 5)
	p, _ := lp.sub(t)
	_, fr := frozenOf(t, lp.sc, *p.Cursor)
	require.Equal(t, lp.r, fr.HomeRepo)
	require.Equal(t, lp.alpha.id12(), fr.WireRepo, "the wire repo is frozen beside the home")

	// The reservation: R facts at R's pinned blobs, nothing of W.
	e := cachedEntry(t, *p.Cursor)
	cur, err := lp.beta.svc.FactQuery().CurrentBlobs(context.Background(), "agent/test", []string{"kb/z/x.md", "kb/z/y.md", "kb/z/z.md", "kb/z/w.md"})
	require.NoError(t, err)
	curW, err := lp.alpha.svc.FactQuery().CurrentBlobs(context.Background(), "agent/test", []string{"kb/z/z.md"})
	require.NoError(t, err)
	require.NotEqual(t, cur["kb/z/z.md"].Blob, curW["kb/z/z.md"].Blob, "W's and R's versions differ")
	for path, want := range map[string]string{"kb/z/x.md": "1.1", "kb/z/y.md": "1.2", "kb/z/z.md": "2.2", "kb/z/w.md": "3.1"} {
		i := e.st.reserved.find(FactHash(lp.r, path))
		require.GreaterOrEqual(t, i, 0, "%s is reserved as an R fact", path)
		got := e.st.reserved[i]
		require.Equal(t, want, itoa(int32(got.unit))+"."+itoa(int32(got.k)), path)
		require.Equal(t, blob64(cur[path].Blob), got.blob, "%s is reserved at R's pinned blob", path)
		require.Negative(t, e.st.reserved.find(FactHash(lp.alpha.id12(), path)), "%s: nothing of W is reserved", path)
	}

	basePages, _, _ := pagesWith(t, lp.betaSC, lp.beta.b, lp.base)
	subPages, _, _ := pagesWith(t, lp.sc, lp.lens, p)
	require.Equal(t, asWire(basePages, lp.r), subPages, "the sub is R's repo FactSet with kb://R/ paths")

	nodes := readAll(t, &factsetFixture{b: lp.lens, svc: lp.alpha.svc}, "o", p)
	require.Len(t, nodes, 18)
	prefix := fact.KBScheme + lp.r + "/"
	for _, n := range nodes {
		require.True(t, strings.HasPrefix(n.Path, prefix), "%s %s is addressed kb://R/…", n.Number, n.Path)
		require.Equal(t, lp.r, n.Repo)
		require.Equal(t, AnchorOf(FactHash(lp.r, relOf(n.Path))), n.AnchorID, "%s: the anchor is R's fact", n.Number)
		if n.Written {
			require.Contains(t, n.Body, "in R.", "%s: the body is R's", n.Number)
		}
	}
	// Unit 1 links FORWARD to Z at its reserved number 2.2 (it is R's
	// reservation that the planner honours).
	require.Equal(t, "1.1.2", nodes[3].Number)
	require.Equal(t, prefix+"kb/z/z.md", nodes[3].Path)
	require.NotNil(t, nodes[3].Link)
	require.Equal(t, LinkForward, nodes[3].Link.Direction)
	require.Equal(t, "2.2", nodes[3].Link.Number)
}

// Home != wire, B9: a page served from KEPT plans plans nothing, so nothing
// opens the home through planning; its bodies and RULE 1 marks still read
// through R, because every call registers its home when it opens.
//
// Sabotage: register the home only when planning opens it (delete the
// registerHome call in openFactSet and call it from fsCall.home instead),
// and the seek fails with "repo <R> is not available": nothing is planned,
// so bodies and RULE 1 reach R through the FactSet reader's foreign route,
// which admits no repo without a frozen tip. (Deleting the call alone fails
// earlier and louder: fsCall.home refuses an unregistered home.)
func TestFactSet_Pinned_KeptPlanPageReadsTheHome(t *testing.T) {
	isolateFactSetGlobals(t)
	lp := newLensPair(t, 3, 1)
	p, _ := lp.sub(t)
	require.Equal(t, "1", p.Facts[0].Number)

	// Change R's version of the root a after the anchor: RULE 1 reads R NOW.
	lp.beta.write(t, "kb/u/a.md", "A", "a body changed in R.", "kb/z/x.md", "kb/z/y.md")

	s, st, err := FactSetNextPageStats(context.Background(), lp.sc, lp.lens, nil, nil, *p.Cursor, 1)
	require.NoError(t, err)
	require.Zero(t, st.UnitsPlanned, "served from the kept plan of unit 1: nothing planned")
	require.Zero(t, st.CacheMisses)
	require.Len(t, s.Facts, 1)
	n := s.Facts[0]
	require.Equal(t, fact.KBScheme+lp.r+"/kb/u/a.md", n.Path)
	require.Contains(t, n.Body, "a body in R.", "the body is R's version at the anchor")
	require.Equal(t, []string{MarkChangedSinceCited}, n.Marks, "RULE 1 read R's reading branch now")
}

// Home != wire: the per-unit read bound (F14 shape) holds on R. The read-ahead
// is keyed by the HOME, so a sub over R reads exactly what beta's own repo
// FactSet reads per unit and per page (same data, same plan): at most
// Depth+2 index reads per unit, and no per-fact git read, OutgoingAtCommit or
// LiveRevision on R.
//
// Sabotage: key the planner's use of the read-ahead by wireID instead of
// homeID (factset_plan.go resolveLevel), and R's units read their level-0
// rows themselves: the sub's per-unit reads exceed the baseline's.
func TestFactSet_Pinned_ReadBoundHoldsOnTheHome(t *testing.T) {
	isolateFactSetGlobals(t)
	var mu sync.Mutex
	var bad []string
	for _, depth := range []int{1, 3} {
		lp := newLensPair(t, depth, 1) // seeding writes through git: before the probe
		restoreGit := storetest.SetGitPathReadHook(func(name string) {
			mu.Lock()
			bad = append(bad, name)
			mu.Unlock()
		})
		restore := lp.beta.svc.Search().SetOverridesForTest(index.Overrides{Reads: func(m string) error {
			if m == "OutgoingAtCommit" || m == "LiveRevision" {
				mu.Lock()
				bad = append(bad, m)
				mu.Unlock()
			}
			return nil
		}})
		p, st := lp.sub(t)
		_, baseMax, baseLater := pagesWith(t, lp.betaSC, lp.beta.b, lp.base)
		_, subMax, subLater := pagesWith(t, lp.sc, lp.lens, p)
		baseMax, subMax = max(baseMax, lp.baseSt.MaxUnitQueries), max(subMax, st.MaxUnitQueries)
		require.Positive(t, subLater, "depth %d: the sub's reads are counted", depth)
		require.LessOrEqual(t, subMax, depth+2, "depth %d: per unit at most Depth+2 index reads on R", depth)
		require.Equal(t, baseMax, subMax, "depth %d: per unit, the sub reads what R's repo FactSet reads", depth)
		require.Equal(t, baseLater, subLater, "depth %d: per page, the sub reads what R's repo FactSet reads", depth)
		_, _, err := FactSetNextPageStats(context.Background(), lp.sc, lp.lens, nil, nil, *p.Cursor, 2)
		require.NoError(t, err)
		exportWith(t, lp.sc, lp.lens, *p.Cursor)
		restore()
		restoreGit()
	}
	require.Empty(t, bad, "no per-fact git read and no OutgoingAtCommit on R")
}

// A home outside the binding reads like a cursor that does not exist
// (ErrCursorExpired, a 404 at the edges), never a 500.
func TestFactSet_Pinned_HomeOutsideTheBindingIsExpired(t *testing.T) {
	f := newFactSetFixture(t)
	r := newReader(f.b, nil, nil, nil)
	defer r.close()
	err := r.registerHome("0123456789ab", "agent/test", "")
	require.ErrorIs(t, err, ErrCursorExpired)
	require.Contains(t, err.Error(), "repo 0123456789ab is not available")
	require.NoError(t, r.registerHome(f.id12(), "agent/test", ""), "the write repo is a home")
}

// A pinned FactSet with NO roots is legal (a lens member whose slice of the
// fused search is empty): it starts (reservation built, an empty read-ahead
// window), its first page is the ordinary "past the end" 400, and, once it
// is a session row, a resume is the same 400 and the export writes nothing.
// None of it panics.
//
// Sabotage: build the background window's pending as pending{lo: units[0]}
// (no empty-window guard), and startPinned panics with index out of range.
func TestFactSet_Pinned_ZeroRootsIsLegal(t *testing.T) {
	isolateFactSetGlobals(t)
	f := newFactSetFixture(t)
	f.write(t, "kb/u/a.md", "A", "a body.")
	ctx := context.Background()
	anchor, err := f.svc.HistoryQuery().Watermark(ctx, "agent/test")
	require.NoError(t, err)
	env := FactSetEnv{Mode: FactSetMode, Title: "empty", Anchor: anchor, Audience: AudienceHuman,
		Depth: 3, MaxNodes: MaxFactSetFacts, FactsLimit: DefaultFactSetFacts, InstructionsVersion: FactSetInstructionsVersion}
	fr := freezePinned(pinnedHome{ID: f.id12(), Branch: "agent/test"}, f.id12(), env, 5, nil)
	require.Empty(t, fr.Roots)

	c := &fsCall{ctx: ctx, b: f.b}
	c.r = newReader(f.b, nil, nil, &c.st)
	require.NoError(t, c.startPinned(fr, f.svc.ToolSession()))
	require.True(t, c.e.st.built.Load(), "the (empty) reservation was built")
	_, err = c.page(fpos{1, 0}, true)
	require.ErrorIs(t, err, ErrInvalid)
	c.close()

	frozen, err := json.Marshal(fr)
	require.NoError(t, err)
	sc := f.scope("o")
	id, err := sc.Sessions.CreateFactSet(ctx, factsetTool(5), sc.Branch, sc.Pin, sc.ReadSet, sc.Owner, frozen)
	require.NoError(t, err)
	cursor := cursorToken(id, fpos{1, 0})
	_, err = FactSetNextPage(ctx, sc, f.b, nil, nil, cursor, 0)
	require.ErrorIs(t, err, ErrInvalid)
	started := false
	require.NoError(t, FactSetExport(ctx, sc, f.b, nil, nil, cursor, func(FactSetEnv) io.Writer { started = true; return io.Discard }))
	require.False(t, started, "nothing to export")
}

// The read-ahead window is per HOME: a unit number names a unit only within
// one FactSet, so one call that plans the sub-FactSets of two homes (F3's
// facade does, on one reader) must not take sub A's window for sub B's unit
// 1. Here one call starts and plans unit 1 of a sub over W (alpha), then
// starts and plans unit 1 of a sub over R (beta): B's unit reads exactly
// what it reads in a call of its own, and the window it was planned with is
// R's.
//
// Sabotage: drop the home from prefetch.covers (compare the unit only), and
// B's unit 1 is "covered" by A's window: it is planned without a read-ahead
// of its own and reads its level-0 rows itself.
func TestFactSet_Pinned_WindowIsPerHome(t *testing.T) {
	isolateFactSetGlobals(t)
	lp := newLensPair(t, 1, 5)
	ctx := context.Background()
	reqA := lp.alpha.request("kb/u/")
	reqA.Options.Depth, reqA.Options.MaxNodes, reqA.Limit = 1, MaxFactSetFacts, 5
	pa, _, err := FactSetFirstPage(ctx, lp.alpha.scope("o"), reqA)
	require.NoError(t, err)
	_, frAlpha := frozenOf(t, lp.alpha.scope("o"), *pa.Cursor)
	w, r := lp.alpha.id12(), lp.r
	frA := func() *factsetFrozen {
		return freezePinned(pinnedHome{ID: w, Branch: "agent/test"}, w, frAlpha.Env, 5, hitsOf(frAlpha))
	}
	frB := func() *factsetFrozen {
		return freezePinned(pinnedHome{ID: r, Branch: "agent/test"}, w, lp.baseFR.Env, 5, hitsOf(lp.baseFR))
	}
	ti := lp.alpha.svc.ToolSession()
	newCall := func() *fsCall {
		c := &fsCall{ctx: ctx, b: lp.lens}
		c.r = newReader(lp.lens, nil, nil, &c.st)
		return c
	}

	// B's unit 1 in a call of its own: the baseline.
	solo := newCall()
	require.NoError(t, solo.startPinned(frB(), ti))
	want, err := solo.planUnitOf(1)
	require.NoError(t, err)
	wantQ := solo.st.MaxUnitQueries
	solo.close()

	c := newCall()
	defer c.close()
	require.NoError(t, c.startPinned(frA(), ti))
	_, err = c.planUnitOf(1)
	require.NoError(t, err)
	require.True(t, c.pre.covers(w, 1), "A's window covers A's unit 1")
	preA := c.pre

	require.NoError(t, c.startPinned(frB(), ti))
	c.st.MaxUnitQueries = 0
	got, err := c.planUnitOf(1)
	require.NoError(t, err)
	require.Equal(t, wantQ, c.st.MaxUnitQueries, "B's unit 1 reads what it reads alone")
	require.Equal(t, r, c.pre.home, "B's unit 1 was planned with R's window")
	require.True(t, c.pre.covers(r, 1))
	require.False(t, preA.covers(r, 1), "A's window does not cover B's unit 1")
	require.Equal(t, len(want.secs), len(got.secs))
	for i := range want.secs {
		require.Equal(t, want.secs[i].wire, got.secs[i].wire)
		require.Equal(t, want.secs[i].number, got.secs[i].number)
	}
}
