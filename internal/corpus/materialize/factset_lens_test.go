package materialize

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/domain/modelparams"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/storetest"
)

// The lens facade (F3). Fixtures are three real stores: alpha (the write
// repo W), beta (R1) and gamma (R2), whose stores embed with lensCosEmbedder,
// so a text search ranks each mount's facts by the cos=<c> marker in their
// bodies and the fused order is known in advance: RRF over per-mount ranks,
// ties broken by mount order (alpha, beta, gamma).

// lensCosEmbedder: a query embeds to e0; a document with cos=<c> in its title
// or body embeds to (c, sqrt(1-c^2), 0...), so its cosine with every query is
// c (no marker: 0, below every recall floor).
type lensCosEmbedder struct{}

const lensCosDim = 768

var lensCosMarker = regexp.MustCompile(`cos=([0-9.]+[0-9])`)

func (lensCosEmbedder) doc(title, body string) []float32 {
	out := make([]float32, lensCosDim)
	m := lensCosMarker.FindStringSubmatch(title + "\n" + body)
	if m == nil {
		out[1] = 1
		return out
	}
	c, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		panic(err)
	}
	out[0], out[1] = float32(c), float32(math.Sqrt(1-c*c))
	return out
}

func (lensCosEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	out := make([]float32, lensCosDim)
	out[0] = 1
	return out, nil
}

func (e lensCosEmbedder) EmbedDocument(_ context.Context, title, body string) ([]float32, error) {
	return e.doc(title, body), nil
}

func (e lensCosEmbedder) EmbedDocuments(_ context.Context, titles, bodies []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range titles {
		out[i] = e.doc(titles[i], bodies[i])
	}
	return out, nil
}

func (e lensCosEmbedder) EmbedShortStrings(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, s := range texts {
		out[i] = e.doc(s, "")
	}
	return out, nil
}

func (lensCosEmbedder) Dim() int                           { return lensCosDim }
func (lensCosEmbedder) ID() string                         { return "lens-cos-test" }
func (lensCosEmbedder) Thresholds() modelparams.Thresholds { return modelparams.Defaults() }

// newLensMember is one lens member: its own store (embedding with
// lensCosEmbedder) and its own repo binding.
func newLensMember(t testing.TB, name string) *factsetFixture {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), name+".db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetEmbedder(lensCosEmbedder{})
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: name, UID: "uid-" + name, AgentBranch: "agent/test", Svc: svc, OntologyRoot: "kb", Embedder: lensCosEmbedder{},
	})
	f := &factsetFixture{ri: ri, svc: svc, b: repos.NewBindingOfRepo(ri, "agent/test")}
	// One fact, so the member's index has synced once and its watermark is
	// set: a never-synced index has an empty watermark, which never reads
	// settled (production syncs every store when it opens). No cos marker:
	// no text search ever finds it.
	f.write(t, "kb/seed/"+name+".md", "Seed", "seed of "+name+".")
	return f
}

// lensTrio is the three-member lens W (alpha), R1 (beta), R2 (gamma).
type lensTrio struct {
	w, r1, r2 *factsetFixture
	lens      *repos.Binding
}

func newLensTrio(t testing.TB) *lensTrio {
	t.Helper()
	lt := &lensTrio{w: newLensMember(t, "alpha"), r1: newLensMember(t, "beta"), r2: newLensMember(t, "gamma")}
	lt.lens = repos.NewBindingForTest(lt.w.ri,
		repos.ReadTarget{RI: lt.w.ri, Branch: "agent/test"},
		repos.ReadTarget{RI: lt.r1.ri, Branch: "agent/test"},
		repos.ReadTarget{RI: lt.r2.ri, Branch: "agent/test"})
	require.True(t, lt.lens.IsLens())
	return lt
}

func (lt *lensTrio) members() []*factsetFixture { return []*factsetFixture{lt.w, lt.r1, lt.r2} }

// scopeOf is a lens cursor scope over binding b: W's session DB, b's pin and
// b's read-set fingerprint, as the edges build it.
func (lt *lensTrio) scopeOf(b *repos.Binding) CursorScope {
	return CursorScope{Sessions: lt.w.svc.ToolSession(), Branch: "agent/test", Pin: b.PinID(),
		ReadSet: federate.ReadSetFingerprint(b), Owner: "o"}
}

func (lt *lensTrio) scope() CursorScope { return lt.scopeOf(lt.lens) }

func (lt *lensTrio) request(text, path string) FactSetRequest {
	return FactSetRequest{Binding: lt.lens, Search: FactSetSearch{Text: text, Path: path, Sort: SortRelevance},
		Facts: DefaultFactSetFacts, Options: DefaultOptions(), Limit: HumanMaxLimit}
}

func (lt *lensTrio) first(t testing.TB, req FactSetRequest) FactSetPage {
	t.Helper()
	p, _, err := FactSetFirstPage(context.Background(), lt.scope(), req)
	require.NoError(t, err)
	return p
}

// cosBody is a body whose cosine with every query is c.
func cosBody(c float64, rest string) string {
	return "cos=" + strconv.FormatFloat(c, 'f', -1, 64) + ". " + rest
}

// unitRoots is every unit root of the pages, in order: the node whose number
// is its unit.
func unitRoots(pages ...FactSetPage) []FactSetNode {
	var out []FactSetNode
	for _, p := range pages {
		for _, n := range p.Facts {
			if n.Number == strconv.Itoa(n.Unit) {
				out = append(out, n)
			}
		}
	}
	return out
}

// allPages pages a FactSet from p to its end over binding b.
func allPages(t testing.TB, sc CursorScope, b *repos.Binding, p FactSetPage) []FactSetPage {
	t.Helper()
	out := []FactSetPage{p}
	for p.HasMore {
		var err error
		p, err = FactSetNextPage(context.Background(), sc, b, nil, nil, *p.Cursor, 0)
		require.NoError(t, err)
		out = append(out, p)
	}
	return out
}

func wireOf(b *repos.Binding, ri *repos.RepoInstance, rel string) string {
	if ri == b.Write() {
		return rel
	}
	return fact.KBScheme + fact.ID12(ri.ID()) + "/" + rel
}

func paths(nodes []FactSetNode) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Path
	}
	return out
}

// seedAlternating writes two results per member under kb/u/, ranked within
// each member by cos, so RRF alternates mounts: w1 a1 b1 w2 a2 b2.
func (lt *lensTrio) seedAlternating(t testing.TB) {
	t.Helper()
	lt.w.write(t, "kb/u/w1.md", "W1", cosBody(0.95, "w1 body."))
	lt.w.write(t, "kb/u/w2.md", "W2", cosBody(0.7, "w2 body."))
	lt.r1.write(t, "kb/u/a1.md", "A1", cosBody(0.9, "a1 body."))
	lt.r1.write(t, "kb/u/a2.md", "A2", cosBody(0.6, "a2 body."))
	lt.r2.write(t, "kb/u/b1.md", "B1", cosBody(0.85, "b1 body."))
	lt.r2.write(t, "kb/u/b2.md", "B2", cosBody(0.65, "b2 body."))
}

// fusedWire is the lens list computed independently of the FactSet:
// federate.PlanFused + FusedSearch over the lens with legs that read every
// mount's live index, every mount asked for 500 (the REST lens search's
// depth), as wire paths.
func fusedWire(t testing.TB, b *repos.Binding, text, path string) []string {
	t.Helper()
	plan, err := federate.PlanFused(context.Background(), federate.FusedQuery{Binding: b, Path: path,
		Opts: index.SearchOptions{Text: text, Limit: 500, MotifMatch: index.MotifMatchExact}})
	require.NoError(t, err)
	res, err := federate.FusedSearch(context.Background(), plan, func(ctx context.Context, tg federate.Target, q index.SearchOptions) ([]index.SearchResult, error) {
		var out []index.SearchResult
		var err error
		require.NoError(t, tg.RT.RI.WithRead(func(svc *store.Service) { out, err = svc.FactQuery().Search(ctx, tg.RT.Branch, q) }))
		return out, err
	}, federate.FusedOptions{})
	require.NoError(t, err)
	var out []string
	for _, h := range res.Hits {
		out = append(out, wireOf(b, h.Target.RT.RI, h.Row.Path))
	}
	return out
}

func currentBlob(t testing.TB, f *factsetFixture, rel string) string {
	t.Helper()
	cur, err := f.svc.FactQuery().CurrentBlobs(context.Background(), "agent/test", []string{rel})
	require.NoError(t, err)
	v, ok := cur[rel]
	require.True(t, ok, rel)
	return v.Blob
}

// lensFrozenOf is a lens cursor's v2 row, as stored.
func lensFrozenOf(t testing.TB, sc CursorScope, cursor string) *lensFrozen {
	t.Helper()
	row, err := sc.Sessions.GetFactSet(context.Background(), mustID(t, cursor))
	require.NoError(t, err)
	require.NotNil(t, row)
	lf, err := decodeLensFrozen(row.Frozen)
	require.NoError(t, err)
	return lf
}

// Item 1 (3-mount lens, text results alternating mounts). The roots are the
// fused lens list, in order, and each root is its own mount's version at that
// mount's frozen anchor:
//   - roots == federate.FusedSearch's list (computed apart), which is the
//     literal w1 a1 b1 w2 a2 b2, W bare and the read members kb://<id12>/;
//   - each root's blob is its own member's current blob, and env.anchors
//     maps every member to its watermark (Lens 4), env.anchor = W's;
//   - each unit root carries its repo name (Lens 7);
//   - ONE session row in W, none in R1 or R2; frozen v2 holds the order
//     (sub per global unit) and one sub per member with its anchor.
//
// Sabotage (search the write mount only: planLensSearch keeps
// plan.Targets[:1]): the roots are w1 w2 and the list comparison fails.
func TestLensFactSet_RootsAreTheFusedLensList(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	lt.seedAlternating(t)
	before := []int64{lt.w.svc.ToolSession().FactSetWrites(), lt.r1.svc.ToolSession().FactSetWrites(), lt.r2.svc.ToolSession().FactSetWrites()}

	req := lt.request("anything", "kb/u/")
	req.Options.Depth = 0
	p := lt.first(t, req)
	pages := allPages(t, lt.scope(), lt.lens, p)
	roots := unitRoots(pages...)

	want := []string{
		"kb/u/w1.md", wireOf(lt.lens, lt.r1.ri, "kb/u/a1.md"), wireOf(lt.lens, lt.r2.ri, "kb/u/b1.md"),
		"kb/u/w2.md", wireOf(lt.lens, lt.r1.ri, "kb/u/a2.md"), wireOf(lt.lens, lt.r2.ri, "kb/u/b2.md"),
	}
	require.Equal(t, want, fusedWire(t, lt.lens, "anything", "kb/u/"), "the fused lens list itself")
	require.Equal(t, want, paths(roots), "the roots are the fused lens list, in order")
	require.Equal(t, 6, p.Units)
	for i, n := range roots {
		require.Equal(t, i+1, n.Unit, "global unit numbers")
	}

	owner := []*factsetFixture{lt.w, lt.r1, lt.r2, lt.w, lt.r1, lt.r2}
	names := []string{"alpha", "beta", "gamma", "alpha", "beta", "gamma"}
	for i, n := range roots {
		require.Equal(t, currentBlob(t, owner[i], relOf(n.Path)), n.Blob, "unit %d: its own member's version", i+1)
		require.Equal(t, fact.ID12(owner[i].ri.ID()), n.Repo)
		require.Equal(t, names[i], n.RepoName, "Lens 7: the unit root names its repo")
	}
	for _, n := range p.Facts {
		if n.Number != strconv.Itoa(n.Unit) {
			require.Empty(t, n.RepoName, "only unit roots carry repo_name")
		}
	}

	require.Len(t, p.Anchors, 3, "Lens 4: one anchor per member")
	for _, f := range lt.members() {
		wm, err := f.svc.HistoryQuery().Watermark(context.Background(), "agent/test")
		require.NoError(t, err)
		require.Equal(t, wm, p.Anchors[f.id12()], "member %s's anchor is its watermark", f.ri.Name())
	}
	require.Equal(t, p.Anchors[lt.w.id12()], p.Anchor, "anchor stays the write member's")

	require.Equal(t, before[0]+1, lt.w.svc.ToolSession().FactSetWrites(), "ONE row, in W's session DB")
	require.Equal(t, before[1], lt.r1.svc.ToolSession().FactSetWrites(), "no row in R1")
	require.Equal(t, before[2], lt.r2.svc.ToolSession().FactSetWrites(), "no row in R2")

	lf := lensFrozenOf(t, lt.scope(), *p.Cursor)
	require.Equal(t, frozenVersionLens, lf.V)
	require.Equal(t, lt.w.id12(), lf.WireRepo)
	require.Equal(t, []int{0, 1, 2, 0, 1, 2}, lf.Order)
	require.Len(t, lf.Subs, 3)
	for i, f := range lt.members() {
		require.Equal(t, f.id12(), lf.Subs[i].Repo)
		require.Equal(t, f.ri.Name(), lf.Subs[i].Name)
		require.Equal(t, p.Anchors[f.id12()], lf.Subs[i].Anchor)
		require.Len(t, lf.Subs[i].Roots, 2)
	}
	require.Equal(t, "kb/u/a1.md", lf.Subs[1].Roots[0].Path, "a sub's roots are repo-relative, in fused order")
}

// seedRemap: R1 (beta) roots A (unit 2 globally) and B (unit 4 globally).
// A -> X -> Y, and B -> Y directly, so Y is reserved as B's "2.1" in R1's
// local numbering and appears in A's unit as a FORWARD link to it. W holds
// the same paths with other bodies (other blobs). W's roots w1, w2 are units
// 1 and 3; gamma has no result.
func (lt *lensTrio) seedRemap(t testing.TB) {
	t.Helper()
	for _, f := range []*factsetFixture{lt.w, lt.r1} {
		tag := f.ri.Name()
		f.write(t, "kb/z/y.md", "Y", "y body in "+tag+".")
		f.write(t, "kb/z/x.md", "X", "x body in "+tag+".", "kb/z/y.md")
	}
	lt.w.write(t, "kb/u/w1.md", "W1", cosBody(0.95, "w1."))
	lt.w.write(t, "kb/u/w2.md", "W2", cosBody(0.7, "w2."))
	lt.r1.write(t, "kb/u/a.md", "A", cosBody(0.9, "a."), "kb/z/x.md")
	lt.r1.write(t, "kb/u/b.md", "B", cosBody(0.6, "b."), "kb/z/y.md")
}

// Remap (exactly four fields) and item 2 (an R1 root's local refs are R1
// facts). R1's sub numbers A as its unit 1 and B as its unit 2; globally they
// are units 2 and 4. In A's unit (global 2):
//   - every node's unit is 2 and its number starts with "2" (fields 1, 2);
//   - Y, reached at depth 2, is a FORWARD link to B's reserved Y: link.unit 4,
//     link.number "4.1" (fields 3, 4);
//   - X and Y are R1 facts: wire kb://R1/kb/z/..., at R1's blobs (not W's).
//
// Sabotage, one run each (remap in lensEntry.remap): drop the Unit remap
// (unit 1), drop the Number remap ("1", "1.1"), drop the Link.Unit remap
// (link unit 2), drop the Link.Number remap (link number "2.1"). Sabotage
// (wire by home: the planner's wireID = fr.HomeRepo): X's path is bare.
func TestLensFactSet_RemapsTheFourNumberFields(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	lt.seedRemap(t)
	req := lt.request("anything", "kb/u/")
	req.Options.Depth = 3
	p := lt.first(t, req)
	require.Equal(t, 4, p.Units)
	pages := allPages(t, lt.scope(), lt.lens, p)
	// Roots by tree position, not by number (the numbers are under test).
	var roots []FactSetNode
	for _, pg := range pages {
		for _, n := range pg.Facts {
			if n.TreeDepth == 0 && n.Parent == 0 {
				roots = append(roots, n)
			}
		}
	}
	require.Equal(t, []string{"kb/u/w1.md", wireOf(lt.lens, lt.r1.ri, "kb/u/a.md"), "kb/u/w2.md", wireOf(lt.lens, lt.r1.ri, "kb/u/b.md")},
		paths(roots))

	var unitA []FactSetNode
	for _, pg := range pages {
		for _, n := range pg.Facts {
			if strings.HasPrefix(n.Number, "2") || n.Unit == 2 {
				unitA = append(unitA, n)
			}
		}
	}
	r1 := func(rel string) string { return wireOf(lt.lens, lt.r1.ri, rel) }
	byPath := map[string]FactSetNode{}
	for _, n := range unitA {
		require.Equal(t, 2, n.Unit, "field 1: %s is in global unit 2", n.Path)
		require.True(t, n.Number == "2" || strings.HasPrefix(n.Number, "2."), "field 2: %s numbered %q", n.Path, n.Number)
		byPath[n.Path] = n
	}
	require.Contains(t, byPath, r1("kb/u/a.md"))
	require.Equal(t, "2", byPath[r1("kb/u/a.md")].Number)
	x, ok := byPath[r1("kb/z/x.md")]
	require.True(t, ok, "X is written in A's unit as an R1 fact on the wire (item 2): %v", paths(unitA))
	require.True(t, x.Written)
	require.Equal(t, "2.1", x.Number)
	require.Equal(t, currentBlob(t, lt.r1, "kb/z/x.md"), x.Blob, "X at R1's blob")
	require.NotEqual(t, currentBlob(t, lt.w, "kb/z/x.md"), x.Blob)
	y, ok := byPath[r1("kb/z/y.md")]
	require.True(t, ok, "Y appears in A's unit: %v", paths(unitA))
	require.NotNil(t, y.Link, "Y is a link: it is reserved for B")
	require.Equal(t, LinkForward, y.Link.Direction)
	require.Equal(t, 4, y.Link.Unit, "field 3: the link names B's GLOBAL unit")
	require.Equal(t, "4.1", y.Link.Number, "field 4: the link's number in global units")
	require.Equal(t, currentBlob(t, lt.r1, "kb/z/y.md"), y.Link.CitedBlob, "the N.k target is R1's Y at R1's blob")

	// B's unit writes Y as 4.1.
	var yWritten *FactSetNode
	for _, pg := range pages {
		for i, n := range pg.Facts {
			if n.Path == r1("kb/z/y.md") && n.Written {
				yWritten = &pg.Facts[i]
			}
		}
	}
	require.NotNil(t, yWritten)
	require.Equal(t, 4, yWritten.Unit)
	require.Equal(t, "4.1", yWritten.Number)
	require.Equal(t, y.AnchorID, yWritten.AnchorID)
}

// The export renders global numbers: "## 2. A", "### 2.1 X", the forward
// link "(see [4.1](#<Y>))", "## 4. B" and "### 4.1 Y"; nothing local leaks
// ("## 1. A", "### 2.1 Y").
//
// Sabotage: the export skips lensEntry.remap: A's heading is "## 1. A" and
// the link reads "(see [2.1](...".
func TestLensFactSet_ExportHasGlobalNumbers(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	lt.seedRemap(t)
	req := lt.request("anything", "kb/u/")
	req.Options.Depth = 3
	p := lt.first(t, req)
	var buf bytes.Buffer
	require.NoError(t, FactSetExport(context.Background(), lt.scope(), lt.lens, nil, nil, *p.Cursor, func(FactSetEnv) io.Writer { return &buf }))
	doc := buf.String()
	yAnchor := AnchorOf(FactHash(lt.r1.id12(), "kb/z/y.md"))
	for _, s := range []string{"\n## 1. W1\n", "\n## 2. A\n", "### 2.1 X", "(see [4.1](#" + yAnchor + "))", "\n## 3. W2\n", "\n## 4. B\n", "### 4.1 Y"} {
		require.Contains(t, doc, s)
	}
	require.NotContains(t, doc, "\n## 1. A\n")
	require.NotContains(t, doc, "(see [2.1](#"+yAnchor+"))")
	require.Less(t, strings.Index(doc, "## 2. A"), strings.Index(doc, "## 3. W2"), "global order")
}

// Lens 1 and Lens 2. W's root cites R1's X (kb://R1/kb/z/x.md) and a repo
// outside the lens:
//   - the R1 ref is a not-followed leaf: not_included {path kb://R1/...,
//     reason lens_repo, repo_name "beta"}; nothing of R1 is written or linked
//     in W's unit;
//   - the ref outside the lens is cross_kb_off, as on a repo FactSet;
//   - cross_kb=true on the lens is ErrInvalid, and writes nothing.
//
// Sabotage (drop the lens-member branch in classify): the R1 ref is
// cross_kb_off with no repo_name.
func TestLensFactSet_RefIntoAnotherMemberIsANotFollowedLeaf(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	lt.r1.write(t, "kb/z/x.md", "X", "x in beta.")
	outside := "kb://0123456789ab/kb/z/far.md"
	lt.w.write(t, "kb/u/w1.md", "W1", cosBody(0.9, "w1."), wireOf(lt.lens, lt.r1.ri, "kb/z/x.md"), outside)
	req := lt.request("anything", "kb/u/")
	p := lt.first(t, req)
	require.Equal(t, 1, p.Units)
	root := p.Facts[0]
	require.Equal(t, "kb/u/w1.md", root.Path)
	require.Equal(t, []Omission{
		{Path: wireOf(lt.lens, lt.r1.ri, "kb/z/x.md"), Reason: ReasonLensRepo, RepoName: "beta"},
		{Path: outside, Reason: ReasonCrossKBOff},
	}, root.NotIncluded)
	for _, n := range allPages(t, lt.scope(), lt.lens, p) {
		for _, f := range n.Facts {
			require.NotContains(t, f.Path, lt.r1.id12(), "nothing of R1 under W's unit")
		}
	}
	raw := mustJSON(t, root)
	require.Contains(t, raw, `{"path":"`+wireOf(lt.lens, lt.r1.ri, "kb/z/x.md")+`","reason":"lens_repo","repo_name":"beta"}`, "the leaf's wire shape")

	before := lt.w.svc.ToolSession().FactSetWrites()
	req.Options.CrossKB = true
	_, _, err := FactSetFirstPage(context.Background(), lt.scope(), req)
	require.ErrorIs(t, err, ErrInvalid)
	require.EqualError(t, err, "invalid materialize request: cross_kb is not accepted on a lens factset")
	require.Equal(t, before, lt.w.svc.ToolSession().FactSetWrites())
}

// Item 3. R1 indexes a new commit after creation: pages, a seek and a
// replay after the cache is dropped still show R1's FROZEN state. Unpinned,
// A's ref target X is read at R1's ANCHOR (LiveAt), so the new X (written
// after creation) never shows; A's root keeps its frozen blob.
//
// Sabotage (re-read R1's watermark per call: attachLens registers each home
// at its current watermark instead of its frozen anchor): the replay shows
// the new X.
func TestLensFactSet_R1MovingAfterCreationChangesNothing(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	lt.w.write(t, "kb/u/w1.md", "W1", cosBody(0.95, "w1."))
	lt.r1.write(t, "kb/z/x.md", "X", "x OLD.")
	lt.r1.write(t, "kb/u/a.md", "A", cosBody(0.9, "a."), "kb/z/x.md")
	oldX, oldA := currentBlob(t, lt.r1, "kb/z/x.md"), currentBlob(t, lt.r1, "kb/u/a.md")
	req := lt.request("anything", "kb/u/")
	req.Options.Unpin, req.Limit = true, 1
	p := lt.first(t, req)
	sc := lt.scope()
	before := strings.ReplaceAll(strings.Join(func() (out []string) {
		for _, pg := range allPages(t, sc, lt.lens, p) {
			out = append(out, mustJSON(t, pg))
		}
		return
	}(), "\n"), mustID(t, *p.Cursor), "ID")

	lt.r1.write(t, "kb/z/x.md", "X", "x NEW.")
	lt.r1.write(t, "kb/u/a.md", "A", cosBody(0.9, "a, edited."), "kb/z/x.md")
	require.NotEqual(t, oldX, currentBlob(t, lt.r1, "kb/z/x.md"))
	DropFactSetCacheForTest()

	after := strings.ReplaceAll(strings.Join(func() (out []string) {
		for _, pg := range allPages(t, sc, lt.lens, p) {
			out = append(out, mustJSON(t, pg))
		}
		return
	}(), "\n"), mustID(t, *p.Cursor), "ID")
	// RULE 1 marks are about NOW: they may say "changed since cited"; strip
	// them, everything else is the frozen FactSet.
	strip := regexp.MustCompile(`,"marks":\["changed_since_cited"\]`)
	require.Equal(t, before, strip.ReplaceAllString(after, ""), "the replay after R1 moved is the frozen FactSet")
	DropFactSetCacheForTest()
	seek, err := FactSetNextPage(context.Background(), sc, lt.lens, nil, nil, *p.Cursor, 2)
	require.NoError(t, err)
	require.Equal(t, 2, seek.UnitFrom)
	var sawX, sawA bool
	for _, pg := range allPages(t, sc, lt.lens, seek) {
		for _, n := range pg.Facts {
			switch n.Path {
			case wireOf(lt.lens, lt.r1.ri, "kb/u/a.md"):
				sawA = true
				require.Equal(t, oldA, n.Blob)
			case wireOf(lt.lens, lt.r1.ri, "kb/z/x.md"):
				sawX = true
				require.Equal(t, oldX, n.Blob, "X at R1's anchor, not R1's new tip")
				require.Contains(t, n.Body, "x OLD.")
			}
		}
	}
	require.True(t, sawA)
	require.True(t, sawX)
}

// Item 4. A sync lands on R2 during every fan-out (the search hook writes to
// R2, which moves its watermark): the call waits and searches again under
// ONE deadline, then answers ErrIndexMoved (busy) with nothing written.
//
// Sabotage (sandwich W only: lensSearchAnchored reads mounts[:1]): the call
// is served and ErrIndexMoved is not returned.
func TestLensFactSet_SyncOnR2DuringTheFanOutIsBusy(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	lt.seedAlternating(t)
	defer SetFactSetAnchorWaitForTest(300 * time.Millisecond)()
	var n atomic.Int64
	defer SetFactSetSearchHookForTest(func() {
		k := n.Add(1)
		lt.r2.write(t, "kb/v/sync"+strconv.FormatInt(k, 10)+".md", "S", "a sync on R2.")
	})()
	before := lt.w.svc.ToolSession().FactSetWrites()
	start := time.Now()
	_, _, err := FactSetFirstPage(context.Background(), lt.scope(), lt.request("anything", "kb/u/"))
	require.ErrorIs(t, err, ErrIndexMoved)
	require.GreaterOrEqual(t, time.Since(start), 300*time.Millisecond, "it waited the whole deadline")
	require.GreaterOrEqual(t, n.Load(), int64(2), "it searched again after the first move")
	require.Equal(t, before, lt.w.svc.ToolSession().FactSetWrites(), "nothing written")
}

// Item 7. Membership change between pages (R2 removed): the next page, a
// seek, the export and a close all answer the SAME ErrCursorExpired as an
// unknown cursor, never another error. The edge's read-set fingerprint
// differs, so the row check refuses it; with the old fingerprint (a forged
// scope), the home registration refuses R2 the same way.
//
// Sabotage (registerHome accepts any home: drop the ByID check): the forged
// scope's page is served or fails otherwise, not ErrCursorExpired.
func TestLensFactSet_MembershipChangeIsExpired(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	lt.seedAlternating(t)
	req := lt.request("anything", "kb/u/")
	req.Options.Depth, req.Limit = 0, 1
	p := lt.first(t, req)
	require.True(t, p.HasMore)
	_, err := FactSetNextPage(context.Background(), lt.scope(), lt.lens, nil, nil, *p.Cursor, 0)
	require.NoError(t, err)

	smaller := repos.NewBindingForTest(lt.w.ri,
		repos.ReadTarget{RI: lt.w.ri, Branch: "agent/test"},
		repos.ReadTarget{RI: lt.r1.ri, Branch: "agent/test"})
	sc := lt.scopeOf(smaller)
	const ghost = "00000000-0000-4000-8000-000000000000.1.0"
	_, ghostErr := FactSetNextPage(context.Background(), sc, smaller, nil, nil, ghost, 0)
	require.ErrorIs(t, ghostErr, ErrCursorExpired)
	_, err = FactSetNextPage(context.Background(), sc, smaller, nil, nil, *p.Cursor, 0)
	require.ErrorIs(t, err, ErrCursorExpired)
	require.Equal(t, ghostErr.Error(), err.Error(), "the same answer as an unknown cursor")
	_, err = FactSetNextPage(context.Background(), sc, smaller, nil, nil, *p.Cursor, 3)
	require.ErrorIs(t, err, ErrCursorExpired)
	require.ErrorIs(t, FactSetExport(context.Background(), sc, smaller, nil, nil, *p.Cursor, func(FactSetEnv) io.Writer { return io.Discard }), ErrCursorExpired)

	// A forged scope carrying the OLD fingerprint over the smaller binding:
	// registering R2 as a home refuses it, as an expired cursor.
	DropFactSetCacheForTest()
	forged := lt.scope()
	forged.Pin = smaller.PinID()
	_, err = FactSetNextPage(context.Background(), forged, smaller, nil, nil, *p.Cursor, 0)
	require.ErrorIs(t, err, ErrCursorExpired)
	require.Contains(t, err.Error(), "repo "+lt.r2.id12()+" is not available")

	require.ErrorIs(t, CloseFactSet(context.Background(), sc, *p.Cursor), ErrCursorExpired)
	require.NoError(t, CloseFactSet(context.Background(), lt.scope(), *p.Cursor), "the lens's own scope still closes it")
}

// Item 9 (the per-unit read bound, roots in three mounts). Per unit at most
// Depth+2 index reads beyond the read-ahead, no per-fact git read and no
// OutgoingAtCommit/LiveRevision, on pages, a seek and the export; and the
// facade's window really reads ahead: the most reads one unit makes is no
// more than in each member's own repo FactSet over the same results (the
// Depth+2 bound alone cannot see a lost read-ahead, 714e784c).
//
// Sabotage (the facade's cover reads nothing ahead: c.pre = nil): units read
// their roots themselves and the baseline comparison fails.
func TestLensFactSet_PerUnitReadBoundOverThreeMounts(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	for _, f := range lt.members() {
		tag := f.ri.Name()
		seedABCBodies(t, f, tag)
	}
	// Make the kb/u/ results text-searchable in every member.
	for i, f := range lt.members() {
		for j, leaf := range []string{"a", "b", "c"} {
			cos := 0.9 - 0.1*float64(j) - 0.01*float64(i)
			refs := map[string][]string{"a": {"kb/z/x.md", "kb/z/y.md"}, "b": {"kb/z/y.md", "kb/z/z.md", "kb/u/" + f.ri.Name() + "-c.md"}, "c": {"kb/z/w.md", "kb/z/x.md"}}[leaf]
			f.write(t, "kb/u/"+f.ri.Name()+"-"+leaf+".md", strings.ToUpper(leaf), cosBody(cos, leaf+" in "+f.ri.Name()+"."), refs...)
		}
	}
	ctx := context.Background()
	var mu sync.Mutex
	var bad []string
	defer storetest.SetGitPathReadHook(func(name string) {
		mu.Lock()
		bad = append(bad, name)
		mu.Unlock()
	})()
	for _, f := range lt.members() {
		defer f.svc.Search().SetOverridesForTest(index.Overrides{Reads: func(m string) error {
			if m == "OutgoingAtCommit" || m == "LiveRevision" {
				mu.Lock()
				bad = append(bad, m)
				mu.Unlock()
			}
			return nil
		}})()
	}
	for _, depth := range []int{1, 3} {
		// Baseline: each member's own repo FactSet over its kb/u/ results.
		base := 0
		for _, f := range lt.members() {
			r := f.request("kb/u/")
			r.Search = FactSetSearch{Text: "anything", Path: "kb/u/", Sort: SortRelevance}
			r.Options.Depth, r.Limit = depth, 1
			bp, st, err := FactSetFirstPage(ctx, f.scope("o"), r)
			require.NoError(t, err)
			require.Equal(t, 3, bp.Units)
			mq := st.MaxUnitQueries
			for bp.HasMore {
				bp, st, err = FactSetNextPageStats(ctx, f.scope("o"), f.b, nil, nil, *bp.Cursor, 0)
				require.NoError(t, err)
				mq = max(mq, st.MaxUnitQueries)
			}
			base = max(base, mq)
		}

		req := lt.request("anything", "kb/u/")
		req.Options.Depth, req.Limit = depth, 1
		p, st, err := FactSetFirstPage(ctx, lt.scope(), req)
		require.NoError(t, err)
		require.Equal(t, 9, p.Units)
		maxQ, total := st.MaxUnitQueries, st.IndexQueries
		for p.HasMore {
			p, st, err = FactSetNextPageStats(ctx, lt.scope(), lt.lens, nil, nil, *p.Cursor, 0)
			require.NoError(t, err)
			maxQ, total = max(maxQ, st.MaxUnitQueries), total+st.IndexQueries
		}
		require.Positive(t, total)
		require.LessOrEqual(t, maxQ, depth+2, "depth %d: per unit at most Depth+2 index reads beyond the read-ahead", depth)
		require.LessOrEqual(t, maxQ, base, "depth %d: the facade's window reads ahead as each member's own FactSet does", depth)
		_, st, err = FactSetNextPageStats(ctx, lt.scope(), lt.lens, nil, nil, *p.Cursor, 5)
		require.NoError(t, err)
		require.LessOrEqual(t, st.MaxUnitQueries, depth+2)
		require.NoError(t, FactSetExport(ctx, lt.scope(), lt.lens, nil, nil, *p.Cursor, func(FactSetEnv) io.Writer { return io.Discard }))
	}
	require.Empty(t, bad, "no per-fact git read and no OutgoingAtCommit")
}

// Concurrency: two readers alternate cursors on ONE lens FactSet (each pages
// it to its end, then seeks every unit), concurrently and after a cache
// drop, under -race; every page equals the sequential page at the same
// position.
//
// Each member has 12 results, more than a sub keeps plans for (keptPlans),
// so the readers re-plan units and read windows while the other reads.
//
// Sabotage (the window state on the shared facade entry instead of the call:
// attachLens reuses one lensCall per entry): -race reports a data race.
func TestLensFactSet_TwoReadersAlternateCursors(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	for _, f := range lt.members() {
		seedABCBodies(t, f, f.ri.Name())
		for j := 0; j < 12; j++ {
			leaf := string(rune('a' + j))
			f.write(t, "kb/u/"+f.ri.Name()+"-"+leaf+".md", strings.ToUpper(leaf), cosBody(0.9-0.03*float64(j), leaf+" in "+f.ri.Name()+"."), "kb/z/x.md", "kb/z/y.md")
		}
	}
	req := lt.request("anything", "kb/u/")
	req.Options.Depth, req.Limit = 3, 2
	p := lt.first(t, req)
	sc := lt.scope()
	id := mustID(t, *p.Cursor)
	seq := map[string]string{}
	for _, pg := range allPages(t, sc, lt.lens, p) {
		if pg.Cursor != nil {
			seq[*pg.Cursor] = ""
		}
	}
	cursors := []string{id + ".1.0"}
	for c := range seq {
		cursors = append(cursors, c)
	}
	want := map[string]string{}
	for _, c := range cursors {
		pg, err := FactSetNextPage(context.Background(), sc, lt.lens, nil, nil, c, 0)
		if err != nil {
			require.ErrorIs(t, err, ErrInvalid) // the end cursor
			continue
		}
		want[c] = mustJSON(t, pg)
	}
	for u := 1; u <= p.Units; u++ {
		pg, err := FactSetNextPage(context.Background(), sc, lt.lens, nil, nil, *p.Cursor, u)
		require.NoError(t, err)
		want["seek"+strconv.Itoa(u)] = mustJSON(t, pg)
	}
	DropFactSetCacheForTest()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for r := 0; r < 2; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for k := 0; k < 3; k++ {
				for i := range cursors {
					c := cursors[(i+r*3)%len(cursors)]
					w, ok := want[c]
					if !ok {
						continue
					}
					pg, err := FactSetNextPage(context.Background(), sc, lt.lens, nil, nil, c, 0)
					if err != nil {
						errs <- err
						return
					}
					if got := mustJSON(t, pg); got != w {
						errs <- errors.New("page at " + c + " differs from the sequential page")
						return
					}
				}
				for u := 1; u <= p.Units; u++ {
					uu := u
					if r == 1 {
						uu = p.Units + 1 - u
					}
					pg, err := FactSetNextPage(context.Background(), sc, lt.lens, nil, nil, *p.Cursor, uu)
					if err != nil {
						errs <- err
						return
					}
					if mustJSON(t, pg) != want["seek"+strconv.Itoa(uu)] {
						errs <- errors.New("seek to unit " + strconv.Itoa(uu) + " differs")
						return
					}
				}
			}
		}(r)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
}

// Lens 3: no blob dedupe across forks. The same PATH in W and R1 is one root,
// W's copy (write-first, even though R1's ranks first); the same BLOB at two
// different paths (R1 and R2 hold byte-identical files) stays two roots.
//
// Sabotage (dedupe roots by blob in runLensSearch): the R2 copy is dropped.
func TestLensFactSet_NoBlobDedupeAcrossForks(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	lt.w.write(t, "kb/u/same.md", "Same", cosBody(0.5, "W's copy."))
	lt.r1.write(t, "kb/u/same.md", "Same", cosBody(0.99, "R1's copy."))
	lt.r1.write(t, "kb/u/twin1.md", "Twin", cosBody(0.8, "identical."))
	lt.r2.write(t, "kb/u/twin2.md", "Twin", cosBody(0.8, "identical."))
	require.Equal(t, currentBlob(t, lt.r1, "kb/u/twin1.md"), currentBlob(t, lt.r2, "kb/u/twin2.md"), "one blob at two paths")
	req := lt.request("anything", "kb/u/")
	req.Options.Depth = 0
	p := lt.first(t, req)
	roots := unitRoots(allPages(t, lt.scope(), lt.lens, p)...)
	require.ElementsMatch(t, []string{"kb/u/same.md", wireOf(lt.lens, lt.r1.ri, "kb/u/twin1.md"), wireOf(lt.lens, lt.r2.ri, "kb/u/twin2.md")}, paths(roots))
	for _, n := range roots {
		if n.Path == "kb/u/same.md" {
			require.Equal(t, currentBlob(t, lt.w, "kb/u/same.md"), n.Blob, "W's version wins the path")
		}
	}
}

// Lens 5 and Lens 6. The no-filter title is "All in lens <lens>"; repo=
// narrowing equals the narrowed lens list (only beta's roots), an unknown
// repo is invalid; since_fork reads only the write member, limited to its
// fork's paths (the edge's PathsIn), the other members contribute nothing.
//
// Sabotage (since_fork does not skip the read members in runLensSearch): R1
// and R2 roots appear under since_fork.
func TestLensFactSet_TitleNarrowingAndSinceFork(t *testing.T) {
	isolateFactSetGlobals(t)
	lt := newLensTrio(t)
	lt.seedAlternating(t)
	ctx := context.Background()

	req := lt.request("", "")
	req.Search.Sort, req.Options.Depth = SortRecent, 0
	p := lt.first(t, req)
	require.Equal(t, "All in lens alpha", p.Title, "Lens 5 (the test binding is named after its write repo)")
	require.Equal(t, "All in lens eng", FactSetLensTitle(FactSetSearch{Sort: SortRecent}, "kb", "eng"))
	require.Equal(t, "Policies", FactSetLensTitle(FactSetSearch{Types: []string{"policy"}}, "kb", "eng"), "filters keep the repo rules")

	req = lt.request("anything", "kb/u/")
	req.Search.Repos, req.Options.Depth = []string{"beta"}, 0
	p = lt.first(t, req)
	require.Equal(t, []string{wireOf(lt.lens, lt.r1.ri, "kb/u/a1.md"), wireOf(lt.lens, lt.r1.ri, "kb/u/a2.md")},
		paths(unitRoots(allPages(t, lt.scope(), lt.lens, p)...)))
	require.Contains(t, p.SearchLine, "repo beta")
	req.Search.Repos = []string{"nope"}
	_, _, err := FactSetFirstPage(ctx, lt.scope(), req)
	require.ErrorIs(t, err, ErrInvalid)
	require.EqualError(t, err, `invalid materialize request: no mount named "nope" in lens "alpha"`)

	req = lt.request("anything", "kb/u/")
	req.Search.Repos, req.Search.SinceFork, req.Options.Depth = nil, true, 0
	req.PathsIn = []string{"kb/u/w2.md", "kb/u/a1.md"}
	p = lt.first(t, req)
	require.Equal(t, []string{"kb/u/w2.md"}, paths(unitRoots(allPages(t, lt.scope(), lt.lens, p)...)),
		"only the write member's fork; R1's a1 is not read although it is in PathsIn")
	req.PathsIn, req.NoResults = nil, true
	p = lt.first(t, req)
	require.Zero(t, p.Units)
}

// The pinned seam's repo path keeps refusing repo= narrowing it cannot honour:
// a repository's own FactSet has no members, and a one-member lens accepts
// only its one member's name (the lens list's own rule).
func TestFactSet_RepoNarrowingOnTheRepoPath(t *testing.T) {
	isolateFactSetGlobals(t)
	f := newFactSetFixture(t)
	f.write(t, "kb/u/a.md", "A", "a.")
	req := f.request("kb/u/")
	req.Search.Repos = []string{"alpha"}
	_, _, err := FactSetFirstPage(context.Background(), f.scope("o"), req)
	require.ErrorIs(t, err, ErrInvalid)
	require.EqualError(t, err, "invalid materialize request: repo narrows a lens factset to some of its repos; this binding is one repository")
}

// lensEntry.remap's number rule, alone: the first component only, a frame
// ancestor's "" untouched.
func TestLensFactSet_RemapNumber(t *testing.T) {
	g := []int{2, 4, 9}
	for in, want := range map[string]string{"1": "2", "2.1": "4.1", "3.1.2": "9.1.2", "": ""} {
		require.Equal(t, want, remapNumber(in, g), in)
	}
}
