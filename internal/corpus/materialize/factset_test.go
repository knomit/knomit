package materialize

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
)

type factsetFixture struct {
	ri  *repos.RepoInstance
	svc *store.Service
	b   *repos.Binding
}

func newFactSetFixture(t testing.TB) *factsetFixture {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "alpha.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", UID: "uid-alpha", AgentBranch: "agent/test", Svc: svc, OntologyRoot: "kb",
	})
	return &factsetFixture{ri: ri, svc: svc, b: repos.NewBindingOfRepo(ri, "agent/test")}
}

func (f *factsetFixture) writeOn(t testing.TB, branch, path, title, body string, conf float64, typ fact.Type, refs ...string) string {
	t.Helper()
	ff := fact.NewFact(path)
	ff.Title, ff.Body, ff.Type = title, body, typ
	ff.Domain, ff.Entities, ff.Refs = []string{"testing"}, []string{}, refs
	ff.Confidence, ff.Sources = conf, 1
	if typ == fact.Frame {
		ff.Kind = fact.Pragmatic
	}
	content, err := fact.SerializeFact(ff)
	require.NoError(t, err)
	res, err := f.svc.Facts().WriteFact(context.Background(), branch, path, content, "write "+path, "")
	require.NoError(t, err)
	return res.CommitHash
}

func (f *factsetFixture) write(t testing.TB, path, title, body string, refs ...string) string {
	t.Helper()
	return f.writeOn(t, "agent/test", path, title, body, 0.7, fact.Observation, refs...)
}

func (f *factsetFixture) scope(owner string) CursorScope {
	return CursorScope{Sessions: f.svc.ToolSession(), Branch: "agent/test", Pin: f.b.PinID(), ReadSet: "rs", Owner: owner}
}

func (f *factsetFixture) request(path string) FactSetRequest {
	return FactSetRequest{
		Binding: f.b, Search: FactSetSearch{Path: path, Sort: SortRecent}, Facts: DefaultFactSetFacts,
		Options: DefaultOptions(), Limit: HumanDefaultLimit,
	}
}

// seedABC writes the two-level reservation example the user approved:
// results A, B, C (units 1, 2, 3) under kb/u/, and under kb/z/
//
//	A -> X, Y; B -> Y, Z, C; C -> W, X; X -> P, Z; Y -> Q; Z -> P, S;
//	W -> Q, R; Q -> R; P, R, S leaves.
//
// C is written before B and A, so the recent-sorted search lists a, b, c.
func seedABC(t testing.TB, f *factsetFixture) {
	t.Helper()
	for _, leaf := range []string{"p", "r", "s"} {
		f.write(t, "kb/z/"+leaf+".md", strings.ToUpper(leaf), leaf+" body.")
	}
	f.write(t, "kb/z/q.md", "Q", "q body.", "kb/z/r.md")
	f.write(t, "kb/z/z.md", "Z", "z body.", "kb/z/p.md", "kb/z/s.md")
	f.write(t, "kb/z/w.md", "W", "w body.", "kb/z/q.md", "kb/z/r.md")
	f.write(t, "kb/z/y.md", "Y", "y body.", "kb/z/q.md")
	f.write(t, "kb/z/x.md", "X", "x body.", "kb/z/p.md", "kb/z/z.md")
	f.write(t, "kb/u/c.md", "C", "c body.", "kb/z/w.md", "kb/z/x.md")
	f.write(t, "kb/u/b.md", "B", "b body.", "kb/z/y.md", "kb/z/z.md", "kb/u/c.md")
	f.write(t, "kb/u/a.md", "A", "a body.", "kb/z/x.md", "kb/z/y.md")
}

// readAll pages a FactSet from page p to its end, every node in order.
func readAll(t testing.TB, f *factsetFixture, owner string, p FactSetPage) []FactSetNode {
	t.Helper()
	nodes := append([]FactSetNode(nil), p.Facts...)
	for p.HasMore {
		var err error
		p, err = FactSetNextPage(context.Background(), f.scope(owner), f.b, nil, nil, *p.Cursor, 0)
		require.NoError(t, err)
		nodes = append(nodes, p.Facts...)
	}
	return nodes
}

// row is one expected appearance: its number, fact, and either written or a
// link (direction and the target's number; a back link's number is resolved
// from the anchor, as a renderer does).
type row struct {
	number, fact string
	link         string // "" written; "forward 2.2", "back 1.2"
}

func rowsOf(nodes []FactSetNode) []row {
	written := map[string]string{}
	out := make([]row, len(nodes))
	for i, n := range nodes {
		r := row{number: n.Number, fact: strings.TrimSuffix(strings.TrimPrefix(n.Path, "kb/"), ".md")}
		switch {
		case n.Link == nil:
			written[n.AnchorID] = n.Number
		case n.Link.Direction == LinkBack:
			r.link = "back " + written[n.Link.AnchorID]
		default:
			r.link = n.Link.Direction + " " + n.Link.Number
		}
		out[i] = r
	}
	return out
}

// F1. Two-level reservation (user ruling 2026-10-29), the user's own example
// at Depth 3, References unlimited: the results AND each result's direct
// references are reserved before any unit is planned (X, Y -> unit 1, Z ->
// unit 2, W -> unit 3; a direct reference shared by two results goes to the
// EARLIER one), so unit 1's deep walk links FORWARD to Z at its fixed number
// 2.2. Every number, direction and target is asserted, and every link's
// anchor is the written appearance's, which is FactHash of the fact.
//
// Sabotage: reserve by the LAST unit (overwrite a taken reservation), and X is
// reserved as 3.2: unit 1 links forward to it instead of writing it.
func TestFactSet_F1_TwoLevelReservationExample(t *testing.T) {
	f := newFactSetFixture(t)
	seedABC(t, f)
	req := f.request("kb/u/")
	req.Options.Depth, req.Options.MaxNodes, req.Limit = 3, MaxFactSetFacts, 5
	p, _, err := FactSetFirstPage(context.Background(), f.scope("o"), req)
	require.NoError(t, err)
	require.Equal(t, 3, p.Units)
	nodes := readAll(t, f, "o", p)
	require.Equal(t, []row{
		{"1", "u/a", ""},
		{"1.1", "z/x", ""},
		{"1.1.1", "z/p", ""},
		{"1.1.2", "z/z", "forward 2.2"},
		{"1.2", "z/y", ""},
		{"1.2.1", "z/q", ""},
		{"1.2.1.1", "z/r", ""},
		{"2", "u/b", ""},
		{"2.1", "z/y", "back 1.2"},
		{"2.2", "z/z", ""},
		{"2.2.1", "z/p", "back 1.1.1"},
		{"2.2.2", "z/s", ""},
		{"2.3", "u/c", "forward 3"},
		{"3", "u/c", ""},
		{"3.1", "z/w", ""},
		{"3.1.1", "z/q", "back 1.2.1"},
		{"3.1.2", "z/r", "back 1.2.1.1"},
		{"3.2", "z/x", "back 1.1"},
	}, rowsOf(nodes))

	written := map[string]FactSetNode{}
	for _, n := range nodes {
		require.Equal(t, AnchorOf(FactHash(n.Repo, n.Path)), n.AnchorID, "%s: the anchor is the fact's identity", n.Path)
		if n.Written {
			require.NotContains(t, written, n.Path, "%s written once", n.Path)
			require.NotEmpty(t, n.Body, "%s: a written section carries its body", n.Path)
			written[n.Path] = n
		} else {
			require.Empty(t, n.Body, "%s: a link carries no body", n.Path)
		}
	}
	for _, n := range nodes {
		if n.Link == nil {
			continue
		}
		w, ok := written[n.Path]
		require.True(t, ok, "%s is linked to and written", n.Path)
		require.Equal(t, w.AnchorID, n.Link.AnchorID, "%s %s targets the written appearance", n.Number, n.Path)
		if n.Link.Direction == LinkForward {
			require.Equal(t, w.Number, n.Link.Number)
			require.Equal(t, w.Unit, n.Link.Unit)
		} else {
			require.Zero(t, n.Link.Unit, "a back link carries the anchor only")
			require.Empty(t, n.Link.Number)
		}
		require.Equal(t, n.Blob, n.Link.CitedBlob)
	}

	e := cachedEntry(t, *p.Cursor)
	reservedAs := func(path string) string {
		r, ok := e.st.reserved[FactHash(f.b.Write().ID()[:12], path)]
		if !ok {
			return ""
		}
		return itoa(r.unit) + "." + itoa(r.k)
	}
	require.Equal(t, "1.1", reservedAs("kb/z/x.md"), "X: A and C cite it directly, A is earlier")
	require.Equal(t, "1.2", reservedAs("kb/z/y.md"), "Y: A and B cite it directly, A is earlier")
	require.Equal(t, "2.2", reservedAs("kb/z/z.md"))
	require.Equal(t, "3.1", reservedAs("kb/z/w.md"))
	for _, below := range []string{"kb/z/p.md", "kb/z/q.md", "kb/z/r.md", "kb/z/s.md", "kb/u/a.md", "kb/u/b.md", "kb/u/c.md"} {
		require.Empty(t, reservedAs(below), "%s: below the direct references, or a result: not reserved", below)
	}
}

func itoa(n int32) string { return strconv.Itoa(int(n)) }

// cachedEntry is the cache entry of a cursor (the test reads its maps).
func cachedEntry(t testing.TB, cursor string) *fsEntry {
	t.Helper()
	id, _, ok := parseCursor(cursor)
	require.True(t, ok)
	fsCache.mu.Lock()
	defer fsCache.mu.Unlock()
	e, ok := fsCache.byID[id]
	require.True(t, ok, "the FactSet is cached")
	return e
}
