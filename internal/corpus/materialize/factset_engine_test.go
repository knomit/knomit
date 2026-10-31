package materialize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/storetest"
)

func (f *factsetFixture) id12() string { return fact.ID12(f.b.Write().ID()) }

func (f *factsetFixture) first(t testing.TB, owner string, req FactSetRequest) FactSetPage {
	t.Helper()
	p, _, err := FactSetFirstPage(context.Background(), f.scope(owner), req)
	require.NoError(t, err)
	return p
}

func reservedNumber(t testing.TB, cursor, repo, path string) string {
	t.Helper()
	r, ok := cachedEntry(t, cursor).st.reserved[FactHash(repo, path)]
	if !ok {
		return ""
	}
	return itoa(r.unit) + "." + itoa(r.k)
}

// pagesJSON pages a FactSet to its end and returns every page's JSON with the
// cursor id normalized (two FactSets of one search compare byte for byte).
func pagesJSON(t testing.TB, f *factsetFixture, owner string, p FactSetPage) []string {
	t.Helper()
	id, _, _ := parseCursor(*p.Cursor)
	var out []string
	for {
		raw, err := json.Marshal(p)
		require.NoError(t, err)
		out = append(out, strings.ReplaceAll(string(raw), id, "ID"))
		if !p.HasMore {
			return out
		}
		p, err = FactSetNextPage(context.Background(), f.scope(owner), f.b, nil, nil, *p.Cursor, 0)
		require.NoError(t, err)
	}
}

// F2. The k limit: the root of unit 1 cites [x, y, z] and max_nodes is 3, so
// unit 1 shows only x and y at depth 1: they are reserved as 1.1 and 1.2,
// and z (beyond unit 1's limits) is reserved for unit 2, whose root cites it,
// and written ONCE, there, as 2.1.
//
// Sabotage: ignore the k limit (reserve every direct ref), and z is reserved
// as 1.3 for a unit that never shows it: unit 2 links to a z written nowhere.
func TestFactSet_F2_ReservationHonoursTheKLimit(t *testing.T) {
	f := newFactSetFixture(t)
	for _, l := range []string{"x", "y", "z"} {
		f.write(t, "kb/z/"+l+".md", strings.ToUpper(l), l+" body.")
	}
	f.write(t, "kb/u/b.md", "B", "b body.", "kb/z/z.md")
	f.write(t, "kb/u/a.md", "A", "a body.", "kb/z/x.md", "kb/z/y.md", "kb/z/z.md")
	req := f.request("kb/u/")
	req.Options.MaxNodes, req.Limit = 3, 5
	p := f.first(t, "o", req)
	require.Equal(t, "1.1", reservedNumber(t, *p.Cursor, f.id12(), "kb/z/x.md"))
	require.Equal(t, "1.2", reservedNumber(t, *p.Cursor, f.id12(), "kb/z/y.md"))
	require.Equal(t, "2.1", reservedNumber(t, *p.Cursor, f.id12(), "kb/z/z.md"))
	nodes := readAll(t, f, "o", p)
	require.Equal(t, []row{{"1", "u/a", ""}, {"1.1", "z/x", ""}, {"1.2", "z/y", ""}, {"2", "u/b", ""}, {"2.1", "z/z", ""}}, rowsOf(nodes))
	require.Contains(t, omitted(nodes[0]), "Z|size", "unit 1 lists z as cut by its own size limit")
}

func omitted(n FactSetNode) []string {
	var out []string
	for _, o := range n.NotIncluded {
		l := o.Title
		if l == "" {
			l = o.Path
		}
		out = append(out, l+"|"+o.Reason)
	}
	return out
}

// F3. Depth 0 or max_nodes 1: nothing is reserved and every unit is its root
// alone.
//
// Sabotage: drop the guard in reserve, and at Depth 0 the direct refs are
// reserved (the map is not empty).
func TestFactSet_F3_NothingReservedAtDepth0OrMaxNodes1(t *testing.T) {
	f := newFactSetFixture(t)
	seedABC(t, f)
	for _, c := range []struct {
		owner           string
		depth, maxNodes int
	}{{"d0", 0, MaxNodesLimit}, {"m1", 3, 1}} {
		req := f.request("kb/u/")
		req.Options.Depth, req.Options.MaxNodes, req.Limit = c.depth, c.maxNodes, 5
		p := f.first(t, c.owner, req)
		require.Empty(t, cachedEntry(t, *p.Cursor).st.reserved, "%s: nothing reserved", c.owner)
		require.Equal(t, []row{{"1", "u/a", ""}, {"2", "u/b", ""}, {"3", "u/c", ""}}, rowsOf(readAll(t, f, c.owner, p)), c.owner)
	}
}

// F4. A frame root: its ancestors come first, with their own data only
// (unnumbered, in post-order: every parent before the frame citing it), the
// mutual pair F <-> M settled by first reach (M is F's parent; M's ref back to
// F is a back-reference, never a loop). An ancestor an earlier unit wrote is a
// back link; one reserved for a LATER unit is a forward link (ruling Q4: the
// reservation wins). A frame root reserves nothing.
//
// Sabotage: let a frame root reserve, and B and M are reserved for unit F.
func TestFactSet_F4_FrameAncestors(t *testing.T) {
	seed := func(t *testing.T, other string) *factsetFixture {
		f := newFactSetFixture(t)
		f.writeOn(t, "agent/test", "kb/f/a.md", "A", "a frame.", 0.7, fact.Frame)
		f.writeOn(t, "agent/test", "kb/f/b.md", "B", "b frame.", 0.7, fact.Frame, "kb/f/a.md")
		f.writeOn(t, "agent/test", "kb/f/m.md", "M", "m frame.", 0.7, fact.Frame)
		f.writeOn(t, "agent/test", "kb/u/f.md", "F", "f frame.", 0.7, fact.Frame, "kb/f/b.md", "kb/f/m.md")
		// M cites F back (the mutual pair), rewritten after F so F pins it.
		f.writeOn(t, "agent/test", "kb/f/m.md", "M", "m frame.", 0.7, fact.Frame, "kb/u/f.md")
		f.writeOn(t, "agent/test", "kb/u/f.md", "F", "f frame.", 0.7, fact.Frame, "kb/f/b.md", "kb/f/m.md")
		// The other result cites a directly: a is reserved for its unit. All
		// in one second, so the search lists by path (e before f before n).
		f.write(t, other, "N", "n body.", "kb/f/a.md")
		return f
	}
	check := func(t *testing.T, f *factsetFixture, want []row, frameUnit int32) {
		req := f.request("kb/u/")
		req.Limit = 5
		p := f.first(t, "o", req)
		nodes := readAll(t, f, "o", p)
		require.Equal(t, want, rowsOf(nodes))
		var roles []string
		for _, n := range nodes {
			if strings.HasPrefix(n.Path, "kb/f/") && n.Number == "" {
				roles = append(roles, n.Role)
			}
		}
		require.Equal(t, []string{RoleAncestor, RoleAncestor, RoleAncestor}, roles)
		e := cachedEntry(t, *p.Cursor)
		for _, r := range e.st.reserved {
			require.NotEqual(t, frameUnit, r.unit, "the frame unit reserves nothing")
		}
		require.Len(t, e.st.reserved, 1, "only the other unit's a")
	}
	t.Run("ancestor written earlier is a back link", func(t *testing.T) {
		f := seed(t, "kb/u/e.md")
		check(t, f, []row{
			{"1", "u/e", ""}, {"1.1", "f/a", ""},
			{"", "f/a", "back 1.1"}, {"", "f/b", ""}, {"", "f/m", ""}, {"2", "u/f", ""},
		}, 2)
	})
	t.Run("ancestor reserved for a later unit is a forward link", func(t *testing.T) {
		f := seed(t, "kb/u/n.md")
		check(t, f, []row{
			{"", "f/a", "forward 2.1"}, {"", "f/b", ""}, {"", "f/m", ""}, {"1", "u/f", ""},
			{"2", "u/n", ""}, {"2.1", "f/a", ""},
		}, 1)
	})
}

// F5. A fact reached in units 2 and 3 is written in 2; in 3 it is a back
// link with the fact's anchor, terminal: its leftover children are not
// repeated there.
//
// Sabotage: keep expanding a link node, and unit 3 repeats y under x.
func TestFactSet_F5_BackLinkIsTerminal(t *testing.T) {
	f := newFactSetFixture(t)
	f.write(t, "kb/z/y.md", "Y", "y body.")
	f.write(t, "kb/z/x.md", "X", "x body.", "kb/z/y.md")
	f.write(t, "kb/u/c.md", "C", "c body.", "kb/z/x.md")
	f.write(t, "kb/u/b.md", "B", "b body.", "kb/z/x.md")
	f.write(t, "kb/u/a.md", "A", "a body.")
	req := f.request("kb/u/")
	req.Limit = 5
	nodes := readAll(t, f, "o", f.first(t, "o", req))
	require.Equal(t, []row{
		{"1", "u/a", ""},
		{"2", "u/b", ""}, {"2.1", "z/x", ""}, {"2.1.1", "z/y", ""},
		{"3", "u/c", ""}, {"3.1", "z/x", "back 2.1"},
	}, rowsOf(nodes))
	require.Equal(t, AnchorOf(FactHash(f.id12(), "kb/z/x.md")), nodes[5].Link.AnchorID)
}

// F6. The order of a unit's children is the AUTHOR's refs order (ref_edges
// ord), not the target path: r cites [url, z, b], so z is 1.1 and b 1.2.
//
// Sabotage: order RefTargetsOfBlobs by target_path, and the reservation numbers
// b 1.1 against a plan that writes z first (the unit is refused).
func TestFactSet_F6_RefsOrderIsTheAuthors(t *testing.T) {
	f := newFactSetFixture(t)
	f.write(t, "kb/z/b.md", "B", "b body.")
	f.write(t, "kb/z/z.md", "Z", "z body.")
	f.write(t, "kb/u/r.md", "R", "r body.", "https://example.com/x", "kb/z/z.md", "kb/z/b.md")
	req := f.request("kb/u/")
	req.Limit = 5
	p := f.first(t, "o", req)
	require.Equal(t, []row{{"1", "u/r", ""}, {"1.1", "z/z", ""}, {"1.2", "z/b", ""}}, rowsOf(p.Facts))
	require.Equal(t, "1.1", reservedNumber(t, *p.Cursor, f.id12(), "kb/z/z.md"))
}

// F7. different_version is BLOB identity (ruling Q3): unit 1 writes x, y, z
// at the versions A cites; unit 2's B cited the OLDER versions: x's body
// changed (true), y changed by whitespace only (true: a body compare would
// say false), and z was changed and then restored to the same bytes, so B
// cites the same BLOB at another commit (false: a commit compare would say
// true).
//
// Sabotage: compare commits instead of blobs, and z's link is flagged.
func TestFactSet_F7_DifferentVersionIsBlobIdentity(t *testing.T) {
	f := newFactSetFixture(t)
	f.write(t, "kb/z/x.md", "X", "x body.")
	f.write(t, "kb/z/y.md", "Y", "y body.")
	f.write(t, "kb/z/z.md", "Z", "z body.")
	f.write(t, "kb/u/b.md", "B", "b body.", "kb/z/x.md", "kb/z/y.md", "kb/z/z.md")
	f.write(t, "kb/z/x.md", "X", "x body, changed.")
	f.write(t, "kb/z/y.md", "Y", "y  body.")
	f.write(t, "kb/z/z.md", "Z", "z body, for a while.")
	f.write(t, "kb/z/z.md", "Z", "z body.")
	f.write(t, "kb/u/a.md", "A", "a body.", "kb/z/x.md", "kb/z/y.md", "kb/z/z.md")
	req := f.request("kb/u/")
	req.Limit = 5
	nodes := readAll(t, f, "o", f.first(t, "o", req))
	require.Equal(t, []row{
		{"1", "u/a", ""}, {"1.1", "z/x", ""}, {"1.2", "z/y", ""}, {"1.3", "z/z", ""},
		{"2", "u/b", ""}, {"2.1", "z/x", "back 1.1"}, {"2.2", "z/y", "back 1.2"}, {"2.3", "z/z", "back 1.3"},
	}, rowsOf(nodes))
	got := []bool{nodes[5].Link.DifferentVersion, nodes[6].Link.DifferentVersion, nodes[7].Link.DifferentVersion}
	require.Equal(t, []bool{true, true, false}, got)
	require.Equal(t, nodes[3].Blob, nodes[7].Link.CitedBlob, "z: the same blob")
	require.NotEqual(t, nodes[3].Commit, nodes[7].Commit, "z: cited at another commit")
}

// F8. RULE 1 marks are about the READING branch NOW, by blob: R changed only
// on branch b2, so the same search shows no mark on agent/test and
// changed_since_cited on b2; R retracted on b3 is still rendered (historical)
// and marked deleted; and R changed on agent/test AFTER the FactSet was
// created is marked on the next page (the marks are computed at page time,
// never against the anchor).
//
// Sabotage: compare against the anchor instead of branch_facts now, and the
// after-creation change is unmarked.
func TestFactSet_F8_Rule1MarksAgainstTheReadingBranchNow(t *testing.T) {
	f := newFactSetFixture(t)
	ctx := context.Background()
	f.write(t, "kb/z/r.md", "R", "r body.")
	f.write(t, "kb/u/a.md", "A", "a body.", "kb/z/r.md")
	require.NoError(t, f.svc.CreateBranch(ctx, "b2", "agent/test"))
	require.NoError(t, f.svc.CreateBranch(ctx, "b3", "agent/test"))
	require.NoError(t, f.svc.IndexManager().Sync(ctx, "b2"))
	require.NoError(t, f.svc.IndexManager().Sync(ctx, "b3"))
	f.writeOn(t, "b2", "kb/z/r.md", "R", "r body on b2.", 0.7, fact.Observation)
	_, err := f.svc.Facts().DeleteFact(ctx, "b3", "kb/z/r.md", "retract r")
	require.NoError(t, err)
	require.NoError(t, f.svc.IndexManager().Sync(ctx, "b2"))
	require.NoError(t, f.svc.IndexManager().Sync(ctx, "b3"))

	on := func(branch string) FactSetNode {
		b := repos.NewBindingOfRepo(f.ri, branch)
		sc := CursorScope{Sessions: f.svc.ToolSession(), Branch: branch, Pin: b.PinID(), ReadSet: "rs", Owner: "o-" + branch}
		req := f.request("kb/u/")
		req.Binding, req.Limit = b, 5
		p, _, err := FactSetFirstPage(ctx, sc, req)
		require.NoError(t, err)
		require.Len(t, p.Facts, 2)
		return p.Facts[1]
	}
	main := on("agent/test")
	require.Empty(t, main.Marks, "unchanged on the reading branch: no mark")
	b2 := on("b2")
	require.Equal(t, []string{MarkChangedSinceCited}, b2.Marks)
	require.Equal(t, main.Blob, b2.Blob, "the structure is historical: b2 shows the pinned version")
	b3 := on("b3")
	require.Equal(t, []string{MarkDeleted}, b3.Marks)
	require.Equal(t, "r body.", b3.Body, "a pinned target retracted on the reading branch is still rendered")

	req := f.request("kb/u/")
	req.Limit = 1
	p := f.first(t, "late", req)
	f.write(t, "kb/z/r.md", "R", "r body, changed after creation.")
	p2, err := FactSetNextPage(ctx, f.scope("late"), f.b, nil, nil, *p.Cursor, 0)
	require.NoError(t, err)
	require.Equal(t, "z/r", strings.TrimSuffix(strings.TrimPrefix(p2.Facts[0].Path, "kb/"), ".md"))
	require.Equal(t, main.Blob, p2.Facts[0].Blob, "the frozen version is shown")
	require.Equal(t, []string{MarkChangedSinceCited}, p2.Facts[0].Marks, "marked against the branch NOW")
}

// F9. OPTION 1 / F2: X's bytes cite R on brA (where R is at v2) and the SAME
// bytes are written on brC (never merged, R still at v1). X's refs were
// resolved once, at its earliest introduction (brA), so a FactSet on brC
// follows X -> R at brA's pin (R's blob = v2's), and RULE 1 marks it against
// brC.
//
// Sabotage: filter the ref rows by the reading branch's commits, and R is "no
// recorded version" on brC.
func TestFactSet_F9_RefsAreResolvedOncePerBlob(t *testing.T) {
	f := newFactSetFixture(t)
	ctx := context.Background()
	f.write(t, "kb/z/r.md", "R", "r v1.")
	require.NoError(t, f.svc.CreateBranch(ctx, "bra", "agent/test"))
	require.NoError(t, f.svc.CreateBranch(ctx, "brc", "agent/test"))
	require.NoError(t, f.svc.IndexManager().Sync(ctx, "bra"))
	require.NoError(t, f.svc.IndexManager().Sync(ctx, "brc"))
	f.writeOn(t, "bra", "kb/z/r.md", "R", "r v2.", 0.7, fact.Observation)
	f.writeOn(t, "bra", "kb/u/x.md", "X", "x body.", 0.7, fact.Observation, "kb/z/r.md")
	require.NoError(t, f.svc.IndexManager().Sync(ctx, "bra"))
	// The earliest introduction is by (committed_at, hash): a second apart, so
	// brA's is the earliest whatever the hashes.
	time.Sleep(1100 * time.Millisecond)
	f.writeOn(t, "brc", "kb/u/x.md", "X", "x body.", 0.7, fact.Observation, "kb/z/r.md")
	require.NoError(t, f.svc.IndexManager().Sync(ctx, "bra"))
	require.NoError(t, f.svc.IndexManager().Sync(ctx, "brc"))

	read := func(branch string) []FactSetNode {
		b := repos.NewBindingOfRepo(f.ri, branch)
		sc := CursorScope{Sessions: f.svc.ToolSession(), Branch: branch, Pin: b.PinID(), ReadSet: "rs", Owner: "o-" + branch}
		req := f.request("kb/u/")
		req.Binding, req.Limit = b, 5
		p, _, err := FactSetFirstPage(ctx, sc, req)
		require.NoError(t, err)
		return p.Facts
	}
	a, c := read("bra"), read("brc")
	require.Equal(t, a[0].Blob, c[0].Blob, "the same bytes: one blob")
	require.Len(t, c, 2, "brC follows X -> R")
	require.Equal(t, a[1].Blob, c[1].Blob, "at brA's pin")
	require.Equal(t, "r v2.", c[1].Body)
	require.Empty(t, a[1].Marks)
	require.Equal(t, []string{MarkChangedSinceCited}, c[1].Marks, "marked against brC, which holds v1")
}

// F10. Unpin: T, gone at the anchor, vanishes with its subtree and without a
// mark or a count, and k is unaffected (a and b are 1.1 and 1.2). Then a
// commit changes a after the FactSet was created and the cache is evicted:
// the replay serves byte-identical pages (a at its version AT THE ANCHOR), and
// the historical LiveAt path ran.
//
// Sabotage: make LiveAt always answer from branch_facts, and the replay shows
// a's new version.
func TestFactSet_F10_UnpinIsHistoricalAndReplayable(t *testing.T) {
	f := newFactSetFixture(t)
	ctx := context.Background()
	f.write(t, "kb/z/s.md", "S", "s body.")
	f.write(t, "kb/z/t.md", "T", "t body.", "kb/z/s.md")
	f.write(t, "kb/z/a.md", "A", "a body.")
	f.write(t, "kb/z/b.md", "B", "b body.")
	f.write(t, "kb/u/u.md", "U", "u body.", "kb/z/t.md", "kb/z/a.md", "kb/z/b.md")
	_, err := f.svc.Facts().DeleteFact(ctx, "agent/test", "kb/z/t.md", "retract t")
	require.NoError(t, err)
	req := f.request("kb/u/")
	req.Options.Unpin, req.Limit = true, 1
	whole := pagesJSON(t, f, "whole", f.first(t, "whole", req))

	p := f.first(t, "o", req)
	f.write(t, "kb/z/a.md", "A", "a body, changed after creation.")
	DropFactSetCacheForTest()
	got := []string{strings.ReplaceAll(mustJSON(t, p), mustID(t, *p.Cursor), "ID")}
	var hist int
	for p.HasMore {
		var st Stats
		p, st, err = FactSetNextPageStats(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 0)
		require.NoError(t, err)
		hist += st.HistoricalLiveAt
		got = append(got, strings.ReplaceAll(mustJSON(t, p), mustID(t, *p.Cursor), "ID"))
	}
	// The RULE 1 marks are page-time annotations (ruling Q2): a moved a is
	// marked now. Everything else, structure and versions, is byte-identical.
	require.Contains(t, got[1], `"marks":["changed_since_cited"]`, "a changed after creation: marked now")
	require.Equal(t, whole, stripMarks(got), "the replay after the branch moved serves the same bytes")
	require.Positive(t, hist, "the historical LiveAt path ran")
	var all []FactSetNode
	for _, raw := range whole {
		var pg FactSetPage
		require.NoError(t, json.Unmarshal([]byte(raw), &pg))
		all = append(all, pg.Facts...)
	}
	require.Equal(t, []row{{"1", "u/u", ""}, {"1.1", "z/a", ""}, {"1.2", "z/b", ""}}, rowsOf(all))
	require.Empty(t, all[0].NotIncluded, "a gone target is not mentioned")
	require.Equal(t, "a body.", all[1].Body)
}

// stripMarks drops the RULE 1 marks from page JSON.
func stripMarks(pages []string) []string {
	out := make([]string, len(pages))
	for i, p := range pages {
		out[i] = strings.NewReplacer(`"marks":["changed_since_cited"],`, "", `"marks":["deleted"],`, "").Replace(p)
	}
	return out
}

func mustJSON(t testing.TB, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func mustID(t testing.TB, cursor string) string {
	t.Helper()
	id, _, ok := parseCursor(cursor)
	require.True(t, ok)
	return id
}

// F12. Replay: evicting the cache after unit k serves the pages after it byte
// for byte as an uninterrupted run, with exactly one cache miss.
//
// Sabotage: rebuild an evicted entry without its reserved map, and the pages
// after the eviction write Z in unit 1 (no forward link).
func TestFactSet_F12_ReplayAfterEviction(t *testing.T) {
	f := newFactSetFixture(t)
	seedABC(t, f)
	ctx := context.Background()
	req := f.request("kb/u/")
	req.Options.Depth, req.Limit = 3, 1
	whole := pagesJSON(t, f, "whole", f.first(t, "whole", req))

	p := f.first(t, "o", req)
	got := []string{strings.ReplaceAll(mustJSON(t, p), mustID(t, *p.Cursor), "ID")}
	misses, i := 0, 1
	for p.HasMore {
		if i == 2 {
			DropFactSetCacheForTest()
		}
		var st Stats
		var err error
		p, st, err = FactSetNextPageStats(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 0)
		require.NoError(t, err)
		misses += st.CacheMisses
		got = append(got, strings.ReplaceAll(mustJSON(t, p), mustID(t, *p.Cursor), "ID"))
		i++
	}
	require.Equal(t, whole, got)
	require.Equal(t, 1, misses, "one rebuild")
}

// F13. Session writes: a creation, 40 pages and a seek write the session DB
// exactly once (the heartbeat clock frozen); moving the clock a quarter of
// the idle TTL ahead adds exactly one write (the heartbeat).
//
// Sabotage: write the heartbeat on every page, and the count is 42.
func TestFactSet_F13_OneSessionWritePerFactSet(t *testing.T) {
	f := newFactSetFixture(t)
	ctx := context.Background()
	for i := range 45 {
		f.write(t, "kb/u/r"+strings.Repeat("0", 3-len(itoa(int32(i))))+itoa(int32(i))+".md", "R"+itoa(int32(i)), "body.")
	}
	now := time.Now().UTC().Add(time.Hour)
	defer SetFactSetClockForTest(func() time.Time { return now })()
	ti := f.svc.ToolSession()
	before := ti.FactSetWrites()
	req := f.request("kb/u/")
	req.Limit = 1
	p := f.first(t, "o", req)
	// The creation row was stamped with the wall clock; the frozen clock sits
	// an hour ahead, so the first page would heartbeat: pin it to "now".
	require.Equal(t, int64(1), ti.FactSetWrites()-before, "the creation is one write")
	require.NoError(t, ti.TouchFactSet(ctx, mustID(t, *p.Cursor), now))
	before = ti.FactSetWrites()
	var err error
	for range 40 {
		p, err = FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 0)
		require.NoError(t, err)
	}
	_, err = FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 3)
	require.NoError(t, err)
	require.Equal(t, int64(0), ti.FactSetWrites()-before, "40 pages and a seek write nothing")
	now = now.Add(time.Duration(factsetIdleTTL.Load()) / 4)
	_, err = FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 0)
	require.NoError(t, err)
	_, err = FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 0)
	require.NoError(t, err)
	require.Equal(t, int64(1), ti.FactSetWrites()-before, "a quarter TTL later: one heartbeat")
}

// F14. The query bound and no git: every unit's plan makes at most Depth+2
// index reads, and no per-path git read (ResolveActiveCommitForPath,
// ReadBlobHashAtCommit, ReadFileAtCommit) nor OutgoingAtCommit nor
// LiveRevision runs on a creation, the pages, a seek or an export.
//
// Sabotage: call OutgoingAtCommit for one level, and the probe fails.
func TestFactSet_F14_QueryBoundAndNoGit(t *testing.T) {
	f := newFactSetFixture(t)
	seedABC(t, f)
	ctx := context.Background()
	var mu sync.Mutex
	var bad []string
	defer storetest.SetGitPathReadHook(func(name string) {
		mu.Lock()
		bad = append(bad, name)
		mu.Unlock()
	})()
	defer f.svc.Search().SetOverridesForTest(index.Overrides{Reads: func(m string) error {
		if m == "OutgoingAtCommit" || m == "LiveRevision" {
			mu.Lock()
			bad = append(bad, m)
			mu.Unlock()
		}
		return nil
	}})()
	for _, depth := range []int{1, 3} {
		req := f.request("kb/u/")
		req.Options.Depth, req.Limit = depth, 1
		p, st, err := FactSetFirstPage(ctx, f.scope("o"), req)
		require.NoError(t, err)
		maxQ := st.MaxUnitQueries
		for p.HasMore {
			p, st, err = FactSetNextPageStats(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 0)
			require.NoError(t, err)
			maxQ = max(maxQ, st.MaxUnitQueries)
		}
		require.Positive(t, maxQ)
		require.LessOrEqual(t, maxQ, depth+2, "depth %d: per unit at most Depth+2 index reads", depth)
		_, _, err = FactSetNextPageStats(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 2)
		require.NoError(t, err)
		require.NoError(t, FactSetExport(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, func(FactSetEnv) io.Writer { return io.Discard }))
	}
	_, _, err := PlanStats(ctx, Request{Binding: f.b, File: "kb/u/b.md", Options: DefaultOptions()})
	require.NoError(t, err)
	require.Empty(t, bad, "no per-fact git read and no OutgoingAtCommit")
}

// F15. A fact a merge delivered unchanged: the FactSet root's (blob, commit)
// is the single-fact root's, and the commit is the one that WROTE the blob
// (recordedCommit), not the merge; after a conflict-resolving merge, the root
// is the merge's new blob.
//
// Sabotage: take the root's commit from its latest commit_log row, and the
// merge commit shows.
func TestFactSet_F15_RootVersionAfterAMerge(t *testing.T) {
	f := newFactSetFixture(t)
	ctx := context.Background()
	f.write(t, "kb/u/base.md", "Base", "base.")
	require.NoError(t, f.svc.CreateBranch(ctx, "feature", "agent/test"))
	require.NoError(t, f.svc.IndexManager().Sync(ctx, "feature"))
	wrote := f.writeOn(t, "feature", "kb/u/m.md", "M", "m body.", 0.7, fact.Observation)
	f.write(t, "kb/u/other.md", "Other", "other.")
	require.NoError(t, f.svc.Merge().MergeBranch(ctx, "feature", "agent/test", "local_wins"))
	require.NoError(t, f.svc.IndexManager().Sync(ctx, "agent/test"))

	req := f.request("kb/u/m.md")
	req.Limit = 5
	p := f.first(t, "o", req)
	require.Len(t, p.Facts, 1)
	doc, err := Plan(ctx, Request{Binding: f.b, File: "kb/u/m.md", Options: DefaultOptions()})
	require.NoError(t, err)
	require.Equal(t, doc.Nodes[0].Blob, p.Facts[0].Blob)
	require.Equal(t, doc.Nodes[0].Commit, p.Facts[0].Commit)
	require.Equal(t, wrote, p.Facts[0].Commit, "the commit that wrote the blob, not the merge")
}

// F18. The watermark race: the index advancing during the search is retried
// once; advancing again is ErrIndexMoved (503), and nothing is written.
//
// Sabotage: skip the second watermark read, and the moving index is served.
func TestFactSet_F18_WatermarkRace(t *testing.T) {
	f := newFactSetFixture(t)
	f.write(t, "kb/u/a.md", "A", "a body.")
	n := 0
	restore := SetFactSetSearchHookForTest(func() {
		if n < 1 {
			f.write(t, "kb/x/m"+itoa(int32(n))+".md", "M", "m.")
		}
		n++
	})
	p := f.first(t, "o", f.request("kb/u/"))
	restore()
	require.Equal(t, 2, n, "retried once")
	wm, err := f.svc.HistoryQuery().Watermark(context.Background(), "agent/test")
	require.NoError(t, err)
	require.Equal(t, wm, p.Anchor, "anchored at the watermark that held still")

	n = 0
	defer SetFactSetSearchHookForTest(func() {
		f.write(t, "kb/x/k"+itoa(int32(n))+".md", "K", "k.")
		n++
	})()
	writes := f.svc.ToolSession().FactSetWrites()
	_, _, err = FactSetFirstPage(context.Background(), f.scope("o2"), f.request("kb/u/"))
	require.ErrorIs(t, err, ErrIndexMoved)
	require.Equal(t, 2, n)
	require.Equal(t, writes, f.svc.ToolSession().FactSetWrites(), "nothing written")
}

// F19. Before the ref_edges backfill (sentinel unset, table empty) the
// FactSet is byte-identical to the backfilled one when the repo id resolves;
// with the repo id unresolved too, it is ErrNotReady (503), never a 500.
//
// Sabotage: drop the fallback (read the empty table), and the pages lose
// every reference.
func TestFactSet_F19_BeforeTheBackfill(t *testing.T) {
	f := newFactSetFixture(t)
	seedABC(t, f)
	req := f.request("kb/u/")
	req.Options.Depth, req.Limit = 3, 2
	backfilled := pagesJSON(t, f, "a", f.first(t, "a", req))

	db := storetest.RawDB(f.svc)
	_, err := db.Exec(`DELETE FROM meta WHERE key = 'ref_edges_backfilled'`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM ref_edges`)
	require.NoError(t, err)
	DropFactSetCacheForTest()
	require.Equal(t, backfilled, pagesJSON(t, f, "b", f.first(t, "b", req)), "the fallback derives the same rows")

	f.svc.GraphStore().SetRootCommitForTest(func(context.Context, string) (string, error) {
		return "", errors.New("unresolved")
	})
	defer f.svc.GraphStore().SetRootCommitForTest(nil)
	_, _, err = FactSetFirstPage(context.Background(), f.scope("c"), req)
	require.ErrorIs(t, err, ErrNotReady)
}

// F20. The real-blob invariant: a ref whose target blob is missing has no
// ref_edges row; it is "not included (no recorded version)" and the structure
// is unaffected (the next ref is still 1.1).
//
// Sabotage: count the refs list instead of the rows when reserving, and a is
// reserved 1.2 against a plan that writes it as 1.1 (the unit is refused).
func TestFactSet_F20_MissingTargetBlobIsNotANode(t *testing.T) {
	f := newFactSetFixture(t)
	ctx := context.Background()
	f.write(t, "kb/z/a.md", "A", "a body.")
	f.write(t, "kb/z/t.md", "T", "t body.")
	res, err := f.svc.Facts().ReadFact(ctx, "agent/test", "kb/z/t.md", nil)
	require.NoError(t, err)
	_ = res
	var blob string
	require.NoError(t, storetest.RawDB(f.svc).QueryRow(`SELECT f.blob_hash FROM branch_facts bf JOIN facts f ON f.id = bf.fact_id WHERE bf.path = 'kb/z/t.md'`).Scan(&blob))
	require.NoError(t, storetest.DeleteObject(f.svc, blob))
	f.write(t, "kb/u/r.md", "R", "r body.", "kb/z/t.md", "kb/z/a.md")
	req := f.request("kb/u/")
	req.Limit = 5
	p := f.first(t, "o", req)
	require.Equal(t, []row{{"1", "u/r", ""}, {"1.1", "z/a", ""}}, rowsOf(p.Facts))
	require.Equal(t, []string{"kb/z/t.md|" + ReasonNoVersion}, omitted(p.Facts[0]))
}

// F21. The cursor: another owner's cursor is ErrCursorExpired, the same as
// one that does not exist; a position past the extent is ErrInvalid; two
// concurrent calls on one cursor both succeed with the same bytes.
//
// Sabotage: accept another owner, and the stranger is served.
func TestFactSet_F21_Cursor(t *testing.T) {
	f := newFactSetFixture(t)
	seedABC(t, f)
	ctx := context.Background()
	req := f.request("kb/u/")
	req.Limit = 1
	p := f.first(t, "o", req)
	_, err := FactSetNextPage(ctx, f.scope("stranger"), f.b, nil, nil, *p.Cursor, 0)
	require.ErrorIs(t, err, ErrCursorExpired)
	_, err = FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, "00000000-0000-0000-0000-000000000000.1.0", 0)
	require.ErrorIs(t, err, ErrCursorExpired)
	id := mustID(t, *p.Cursor)
	_, err = FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, id+".4.0", 0)
	require.ErrorIs(t, err, ErrInvalid, "unit 4 of 3")
	_, err = FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, id+".1.99", 0)
	require.ErrorIs(t, err, ErrInvalid, "section 99 of unit 1")

	var wg sync.WaitGroup
	out := make([][]byte, 8)
	errs := make([]error, 8)
	for i := range out {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pg, err := FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 0)
			errs[i] = err
			out[i], _ = json.Marshal(pg)
		}(i)
	}
	wg.Wait()
	for i := range out {
		require.NoError(t, errs[i])
		require.True(t, bytes.Equal(out[0], out[i]), "call %d: the same bytes", i)
	}
}

// F22. A FactHash collision (the seam forces x and y onto one hash) fails the
// call (ErrFactIdentity), never drops a subtree.
//
// Sabotage: ignore the check hash, and y is served as a back link to x.
func TestFactSet_F22_HashCollisionIsRefused(t *testing.T) {
	f := newFactSetFixture(t)
	f.write(t, "kb/z/y.md", "Y", "y body.")
	f.write(t, "kb/z/x.md", "X", "x body.")
	f.write(t, "kb/u/b.md", "B", "b body.", "kb/z/y.md")
	f.write(t, "kb/u/a.md", "A", "a body.", "kb/z/x.md")
	defer SetFactHashForTest(func(repo, rel string) uint64 {
		if rel == "kb/z/x.md" || rel == "kb/z/y.md" {
			return 42
		}
		return fnv1a(repo, rel)
	})()
	req := f.request("kb/u/")
	req.Limit = 5
	_, _, err := FactSetFirstPage(context.Background(), f.scope("o"), req)
	require.ErrorIs(t, err, ErrFactIdentity)
}

// Export: one document, every section once, links resolved to the numbers
// written (back links from the anchors the export rendered), the RULE 1 note,
// and no planning slot held while it writes.
//
// Sabotage: keep the planning slot while writing, and the writer sees one in
// use.
func TestFactSet_ExportStreamsWithoutAPlanningSlot(t *testing.T) {
	f := newFactSetFixture(t)
	seedABC(t, f)
	ctx := context.Background()
	req := f.request("kb/u/")
	req.Options.Depth, req.Limit = 3, 1
	p := f.first(t, "o", req)
	var buf bytes.Buffer
	var inUse []int
	w := writerFunc(func(b []byte) (int, error) {
		inUse = append(inUse, BuildSlotsInUse())
		return buf.Write(b)
	})
	require.NoError(t, FactSetExport(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, func(FactSetEnv) io.Writer { return w }))
	for _, n := range inUse {
		require.Zero(t, n, "no planning slot is held while the export writes")
	}
	doc := buf.String()
	require.Contains(t, doc, "### 1.1.2 Z (see [2.2](#"+AnchorOf(FactHash(f.id12(), "kb/z/z.md"))+"))")
	require.Contains(t, doc, "### 2.1 Y (see [1.2](#"+AnchorOf(FactHash(f.id12(), "kb/z/y.md"))+"))")
	require.Equal(t, 1, strings.Count(doc, "<a id=\""+AnchorOf(FactHash(f.id12(), "kb/z/z.md"))+"\">"), "one target per fact")
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(b []byte) (int, error) { return w(b) }
