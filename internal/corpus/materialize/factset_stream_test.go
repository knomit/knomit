package materialize

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
)

// renderStream is the test renderer of a streamed FactSet: it maps every
// anchor to the number of its written appearance, resolving back links from
// what it has already rendered and forward links once the target arrives,
// and returns the rows in round 3's shape.
func renderStream(t *testing.T, secs []StreamSection) []row {
	t.Helper()
	numberOf := map[string]string{}
	blobOf := map[string]string{}
	for _, s := range secs {
		if s.Written {
			require.NotContains(t, numberOf, s.Anchor, "%s written once", s.Path)
			numberOf[s.Anchor] = s.Number
			blobOf[s.Anchor] = s.Blob
		}
	}
	rendered := map[string]bool{}
	out := make([]row, len(secs))
	for i, s := range secs {
		r := row{number: s.Number, fact: strings.TrimSuffix(strings.TrimPrefix(relOf(s.Path), "kb/"), ".md")}
		if s.Link != nil {
			n, ok := numberOf[s.Link.Anchor]
			require.True(t, ok, "%s %s: its link target is written somewhere", s.Number, s.Path)
			if s.Link.Direction == LinkBack {
				require.True(t, rendered[s.Link.Anchor], "%s: a back link's target is already rendered", s.Number)
			} else {
				require.False(t, rendered[s.Link.Anchor], "%s: a forward link's target comes later", s.Number)
				if s.Link.Number != "" {
					require.Equal(t, n, s.Link.Number, "%s: the number the server announced is where it is written", s.Number)
				}
			}
			r.link = s.Link.Direction + " " + n
		}
		if s.Written {
			rendered[s.Anchor] = true
		}
		out[i] = r
	}
	return out
}

func streamAll(t *testing.T, f *factsetFixture, req FactSetRequest, owner string) (*StreamFactSet, []StreamSection) {
	t.Helper()
	ctx := context.Background()
	s, err := CreateStream(ctx, f.scope(owner), req)
	require.NoError(t, err)
	var all []StreamSection
	unit, sec := 1, 0
	for unit != 0 {
		var page []StreamSection
		page, unit, sec, err = s.Page(ctx, unit, sec, req.Limit)
		require.NoError(t, err)
		require.NotEmpty(t, page)
		all = append(all, page...)
	}
	return s, all
}

// The round-3 A/B/C example, streamed: the same 18 rows, numbers, link
// directions and targets as TestFactSet_TwoLevelReservationExample, with
// links as anchors the renderer resolves; the reserved map is X 1.1, Y 1.2,
// Z 2.2, W 3.1 and nothing else.
func TestStream_TwoLevelReservationExample(t *testing.T) {
	f := newFactSetFixture(t)
	seedABC(t, f)
	req := f.request("kb/u/")
	req.Options.Depth, req.Options.MaxNodes, req.Limit = 3, MaxFactSetFacts, 5
	s, secs := streamAll(t, f, req, "o")
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
	}, renderStream(t, secs))
	id := s.writeID
	require.Equal(t, "1.1", s.ReservedNumber(id, "kb/z/x.md"))
	require.Equal(t, "1.2", s.ReservedNumber(id, "kb/z/y.md"))
	require.Equal(t, "2.2", s.ReservedNumber(id, "kb/z/z.md"))
	require.Equal(t, "3.1", s.ReservedNumber(id, "kb/z/w.md"))
	_, reserved := s.MemoryEntries()
	require.Equal(t, 4, reserved)
	for _, sec := range secs {
		if sec.Written {
			require.NotEmpty(t, sec.Body, "%s: a written section carries its body", sec.Path)
		}
	}
	require.Equal(t, 1, s.Stats.SessionWrites)
	require.Zero(t, s.Stats.PerFactOps)
}

// References = 1 and Depth 0, as TestFactSet_ReservationFollowsTheOwnersLimits.
func TestStream_ReservationFollowsTheOwnersLimits(t *testing.T) {
	f := newFactSetFixture(t)
	seedABC(t, f)
	req := f.request("kb/u/")
	req.Options.Depth, req.Options.MaxNodes, req.Limit = 3, 2, 5
	s, secs := streamAll(t, f, req, "refs1")
	id := s.writeID
	require.Equal(t, "1.1", s.ReservedNumber(id, "kb/z/x.md"))
	require.Equal(t, "2.1", s.ReservedNumber(id, "kb/z/y.md"))
	require.Equal(t, "3.1", s.ReservedNumber(id, "kb/z/w.md"))
	_, reserved := s.MemoryEntries()
	require.Equal(t, 3, reserved)
	require.Equal(t, []row{
		{"1", "u/a", ""}, {"1.1", "z/x", ""},
		{"2", "u/b", ""}, {"2.1", "z/y", ""},
		{"3", "u/c", ""}, {"3.1", "z/w", ""},
	}, renderStream(t, secs))

	req.Options.Depth, req.Options.MaxNodes = 0, MaxFactSetFacts
	s, secs = streamAll(t, f, req, "depth0")
	_, reserved = s.MemoryEntries()
	require.Zero(t, reserved, "Depth 0 reserves nothing")
	require.Equal(t, []row{{"1", "u/a", ""}, {"2", "u/b", ""}, {"3", "u/c", ""}}, renderStream(t, secs))
}

// The streamed FactSet gives #692's placement on every fixture the round-3
// tests use: every row's number, fact, direction and target number.
func TestStream_SamePlacementAsTheStoredFactSet(t *testing.T) {
	cases := map[string]func(t *testing.T, f *factsetFixture){
		"abc": func(t *testing.T, f *factsetFixture) { seedABC(t, f) },
		"tree-depth": func(t *testing.T, f *factsetFixture) {
			f.write(t, "kb/z/d.md", "D", "d body.")
			f.write(t, "kb/z/c.md", "C", "c body.", "kb/z/d.md")
			f.write(t, "kb/z/b.md", "B", "b body.", "kb/z/d.md")
			f.write(t, "kb/z/a.md", "A", "a body.", "kb/z/c.md")
			f.write(t, "kb/u/r.md", "R", "r body.", "kb/z/a.md", "kb/z/b.md")
		},
		"direct-refs": func(t *testing.T, f *factsetFixture) {
			f.write(t, "kb/z/b.md", "B", "b body.")
			f.write(t, "kb/z/a.md", "A", "a body.", "kb/z/b.md")
			f.write(t, "kb/u/r.md", "R", "r body.", "kb/z/a.md", "kb/z/b.md")
		},
		"two": func(t *testing.T, f *factsetFixture) { fixtureTwo(t, f) },
	}
	for name, seed := range cases {
		for _, o := range []Options{{Depth: 3, MaxNodes: MaxFactSetFacts}, {Depth: 3, MaxNodes: 2}, {Depth: 1, MaxNodes: 3}, {Depth: 0, MaxNodes: 5}, DefaultOptions()} {
			t.Run(name, func(t *testing.T) {
				f := newFactSetFixture(t)
				seed(t, f)
				req := f.request("kb/u/")
				req.Options.Depth, req.Options.MaxNodes, req.Limit = o.Depth, o.MaxNodes, 5
				p, _, err := FactSetFirstPage(context.Background(), f.scope("stored"), req)
				require.NoError(t, err)
				want := rowsOf(readAll(t, f, "stored", p))
				_, secs := streamAll(t, f, req, "stream")
				require.Equal(t, want, renderStream(t, secs), "options %+v", o)
			})
		}
	}
}

// Determinism: the state is a cache. Dropped, it is rebuilt from the one
// session row by replay, and a seek to unit 3 serves exactly unit 3 of the
// full read (bodies included); a seek backwards replays from the start.
func TestStream_ReplayGivesTheSameSections(t *testing.T) {
	f := newFactSetFixture(t)
	seedABC(t, f)
	req := f.request("kb/u/")
	req.Options.Depth, req.Options.MaxNodes, req.Limit = 3, MaxFactSetFacts, 5
	s, full := streamAll(t, f, req, "o")
	var unit3 []StreamSection
	for _, sec := range full {
		if sec.Unit == 3 {
			unit3 = append(unit3, sec)
		}
	}
	ctx := context.Background()
	DropStreamCacheForTest()
	s2, err := OpenStream(ctx, f.scope("o"), f.b, nil, nil, s.ID)
	require.NoError(t, err)
	require.NotSame(t, s, s2, "rebuilt, not cached")
	got, next, _, err := s2.Page(ctx, 3, 0, 5)
	require.NoError(t, err)
	require.Zero(t, next)
	require.Equal(t, unit3, got)
	require.Zero(t, s2.Stats.SessionWrites, "a rebuild writes nothing")

	// Backwards, then the whole stream again: identical.
	var again []StreamSection
	unit, sec := 1, 0
	for unit != 0 {
		var page []StreamSection
		page, unit, sec, err = s2.Page(ctx, unit, sec, 2)
		require.NoError(t, err)
		again = append(again, page...)
	}
	require.Equal(t, full, again)

	var exported []StreamSection
	require.NoError(t, s2.Export(ctx, func(sec StreamSection) error { exported = append(exported, sec); return nil }))
	require.Equal(t, full, exported)

	_, err = OpenStream(ctx, f.scope("someone-else"), f.b, nil, nil, "no-such")
	require.ErrorIs(t, err, ErrCursorExpired)
}

// Cross-KB: a ref into a second repo is read at that repo's tip frozen at
// creation. A version written there afterwards is not seen, even after the
// cache is dropped and the state is rebuilt.
func TestStream_CrossKBAtTheFrozenTip(t *testing.T) {
	f := newFactSetFixture(t)
	ctx := context.Background()
	svc2, err := store.Open(filepath.Join(t.TempDir(), "beta.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc2.Close() })
	require.NoError(t, svc2.InitRepo(ctx, map[string]string{}, "agent/test"))
	beta := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "beta", UID: "uid-beta", AgentBranch: "agent/test", Svc: svc2, OntologyRoot: "kb"})
	writeBeta := func(path, title, body string, refs ...string) {
		ff := fact.NewFact(path)
		ff.Title, ff.Body, ff.Type = title, body, fact.Observation
		ff.Domain, ff.Entities, ff.Refs = []string{"testing"}, []string{}, refs
		ff.Confidence, ff.Sources = 0.7, 1
		content, err := fact.SerializeFact(ff)
		require.NoError(t, err)
		_, err = svc2.Facts().WriteFact(ctx, "agent/test", path, content, "write "+path, "")
		require.NoError(t, err)
	}
	writeBeta("kb/o/leaf.md", "Beta leaf", "leaf v1.")
	writeBeta("kb/o/far.md", "Beta far", "far body.", "kb/o/leaf.md")
	betaID := fact.ID12(beta.ID())
	f.b = repos.NewBindingForTest(f.ri, repos.ReadTarget{RI: f.ri, Branch: "agent/test"}, repos.ReadTarget{RI: beta, Branch: "agent/test"})
	f.write(t, "kb/z/x.md", "X", "x body.")
	f.write(t, "kb/u/r.md", "R", "r body.", "kb/z/x.md", "kb://"+betaID+"/kb/o/far.md")

	req := f.request("kb/u/")
	req.Options = Options{Depth: 3, MaxNodes: MaxFactSetFacts, CrossKB: true}
	req.Limit = 5
	s, secs := streamAll(t, f, req, "x")
	require.Equal(t, []row{{"1", "u/r", ""}, {"1.1", "z/x", ""}, {"1.2", "o/far", ""}, {"1.2.1", "o/leaf", ""}}, renderStream(t, secs))
	require.Equal(t, "1.2", s.ReservedNumber(betaID, "kb/o/far.md"), "a cross-KB direct ref is reserved like a local one")
	leaf := secs[3]
	require.Equal(t, "leaf v1.", strings.TrimSpace(leaf.Body))
	require.Equal(t, betaID, leaf.Repo)
	require.Contains(t, s.Cursor().Tips, betaID)

	writeBeta("kb/o/leaf.md", "Beta leaf", "leaf v2.")
	writeBeta("kb/o/far.md", "Beta far v2", "far body 2.")
	DropStreamCacheForTest()
	s2, err := OpenStream(ctx, f.scope("x"), f.b, nil, nil, s.ID)
	require.NoError(t, err)
	got, _, _, err := s2.Page(ctx, 1, 0, 5)
	require.NoError(t, err)
	require.Equal(t, secs, got, "the frozen tip, not beta's new head")
	require.Positive(t, s2.Stats.PerFactOps, "beta moved past its frozen tip: the per-path fallback ran")
}
