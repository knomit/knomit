package kb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/auth"
)

const factsetBase = "/repos/alpha/branches/agent:test/search/factset"

func getFactSet(t *testing.T, h http.Handler, u string) (int, materialize.FactSetPage, string) {
	t.Helper()
	code, ctype, body := getMaterialize(t, h, u)
	var p materialize.FactSetPage
	if code == http.StatusOK {
		require.Equal(t, "application/json", ctype)
		require.NoError(t, json.Unmarshal([]byte(body), &p), body)
	}
	return code, p, body
}

// readFactSet pages a factset from its first call to the end: every node in factset
// order, and the pages.
func readFactSet(t *testing.T, h http.Handler, first string) ([]materialize.FactSetNode, []materialize.FactSetPage) {
	t.Helper()
	var nodes []materialize.FactSetNode
	var pages []materialize.FactSetPage
	u := first
	for {
		code, p, body := getFactSet(t, h, u)
		require.Equal(t, http.StatusOK, code, body)
		pages = append(pages, p)
		nodes = append(nodes, p.Facts...)
		if !p.HasMore {
			return nodes, pages
		}
		require.NotNil(t, p.Cursor)
		u = factsetBase + "?cursor=" + url.QueryEscape(*p.Cursor)
		require.Less(t, len(pages), 2000, "paging does not end")
	}
}

// searchPaths is what the repo /search route lists for query: the list the
// factset must materialize.
func searchPaths(t *testing.T, h http.Handler, query string) []string {
	t.Helper()
	code, _, body := getMaterialize(t, h, "/repos/alpha/branches/agent:test/search?"+query)
	require.Equal(t, http.StatusOK, code, body)
	var v struct {
		Embedded struct {
			Results []struct {
				Path string `json:"path"`
			} `json:"results"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &v))
	var out []string
	for _, r := range v.Embedded.Results {
		out = append(out, r.Path)
	}
	return out
}

// recentPaths is what the /facts collection lists for query (sort=recent).
func recentPaths(t *testing.T, h http.Handler, query string) []string {
	t.Helper()
	code, _, body := getMaterialize(t, h, "/repos/alpha/branches/agent:test/facts?"+query)
	require.Equal(t, http.StatusOK, code, body)
	var v struct {
		Embedded struct {
			Facts []struct {
				Path string `json:"path"`
			} `json:"facts"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &v))
	var out []string
	for _, r := range v.Embedded.Facts {
		out = append(out, r.Path)
	}
	return out
}

// seedShared writes the factset fixture under kb/u/ (the results) and kb/z/
// (what they build on):
//
//	u/r1 -> z/s, u/r2        z/s -> z/d
//	u/r2 -> z/m              z/m -> z/s
//
// The search lists r1 first (newest commit, and first by path on a tie). At
// depth 2: unit 1 = r1, s, d, r2 (a result: forward link, not expanded);
// unit 2 = r2, m, s (written in unit 1: back link, its child d not written
// again).
func seedShared(t *testing.T, alpha *repos.RepoInstance) {
	t.Helper()
	const br = "agent/test"
	writeMatFact(t, alpha, br, "kb/z/d.md", "D", "d body.")
	writeMatFact(t, alpha, br, "kb/z/s.md", "S", "s body.", "kb/z/d.md")
	writeMatFact(t, alpha, br, "kb/z/m.md", "M", "m body.", "kb/z/s.md")
	writeMatFact(t, alpha, br, "kb/u/r2.md", "R2", "r2 body.", "kb/z/m.md")
	writeMatFact(t, alpha, br, "kb/u/r1.md", "R1", "r1 body.", "kb/z/s.md", "kb/u/r2.md")
}

func byUnitPath(nodes []materialize.FactSetNode) map[string]materialize.FactSetNode {
	out := map[string]materialize.FactSetNode{}
	for _, n := range nodes {
		out[fmt.Sprintf("%d %s", n.Unit, n.Path)] = n
	}
	return out
}

func unitOf(nodes []materialize.FactSetNode, root string) int {
	for _, n := range nodes {
		if n.Path == root && (n.Role == materialize.RoleRoot || n.Role == materialize.RoleFocal) {
			return n.Unit
		}
	}
	return 0
}

// The FactSet over REST (user rulings 2026-10-29): every result has its own
// unit, in search order, rooted at it; a result reached in another unit is a
// forward link "(see N)" and is not expanded there; a fact an earlier unit
// wrote is a back link to its first appearance, by anchor, and its leftover
// children are not written again; every fact is written once, with its body.
//
// Sabotage: drop the results from the lookup (r2 is expanded inside unit 1),
// make a back link expand (d appears in unit 2), or flip the direction
// comparison.
func TestFactSet_SeededVisitedSetAndLinks(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))

	want := searchPaths(t, h, "path=kb/u/")
	require.Equal(t, []string{"kb/u/r1.md", "kb/u/r2.md"}, want)

	nodes, pages := readFactSet(t, h, factsetBase+"?path=kb/u/&depth=2&limit=5")
	require.Equal(t, 2, pages[0].Units)
	last := pages[len(pages)-1]
	require.Equal(t, 2, last.UnitsWalked)
	require.Equal(t, len(nodes), last.Sections)
	for i, p := range want {
		require.Equal(t, i+1, unitOf(nodes, p), "unit %d is search result %d", i+1, i+1)
	}
	var got []string
	written := map[string]int{}
	numberOf := map[string]string{}
	for _, n := range nodes {
		mark := ""
		if n.Written {
			written[n.Path]++
			numberOf[n.AnchorID] = n.Number
			require.NotEmpty(t, n.Body, "a written section carries its body")
			require.Nil(t, n.Link)
		} else {
			num := n.Link.Number
			if n.Link.Direction == materialize.LinkBack {
				num = numberOf[n.Link.AnchorID] // the renderer resolves a back link's anchor
			}
			mark = " (see " + num + ")"
			require.Empty(t, n.Body, "a link carries no body")
			require.Nil(t, n.BuildsOn)
		}
		got = append(got, fmt.Sprintf("%s %s%s", n.Number, n.Title, mark))
	}
	require.Equal(t, []string{
		"1 R1", "1.1 S", "1.1.1 D", "1.2 R2 (see 2)",
		"2 R2", "2.1 M", "2.1.1 S (see 1.1)",
	}, got)
	for p, c := range written {
		require.Equal(t, 1, c, "%s is written once", p)
	}

	at := byUnitPath(nodes)
	fwd, back := at["1 kb/u/r2.md"].Link, at["2 kb/z/s.md"].Link
	r2, s := at["2 kb/u/r2.md"], at["1 kb/z/s.md"]
	require.Equal(t, materialize.LinkForward, fwd.Direction)
	require.Equal(t, 2, fwd.Unit)
	require.Equal(t, r2.AnchorID, fwd.AnchorID)
	require.Equal(t, "2", fwd.Number)
	require.Equal(t, materialize.LinkBack, back.Direction)
	require.Zero(t, back.Unit, "a back link carries the anchor only")
	require.Equal(t, s.AnchorID, back.AnchorID)

	// Builds-on anchors point where the fact is written.
	anchors := map[string]string{}
	for _, l := range at["1 kb/u/r1.md"].BuildsOn {
		anchors[l.Title] = l.AnchorID
	}
	require.Equal(t, s.AnchorID, anchors["S"])
	require.Equal(t, r2.AnchorID, anchors["R2"])
}

// Units are planned lazily: page 1 plans only the units it needs; the rest
// are planned as later pages reach them. The cursor carries the position.
//
// Sabotage: plan every unit on the first call, and page 1 reports 2 units
// walked.
func TestFactSet_UnitsAreWalkedAsPagesNeedThem(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))
	code, p, body := getFactSet(t, h, factsetBase+"?path=kb/u/&depth=2&limit=2")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 1, p.UnitsWalked, "page 1 needed unit 1 only")
	require.Equal(t, 4, p.Sections)
	require.True(t, p.HasMore)
	// Page 3 (r2's forward link, then unit 2) walks unit 2.
	code, p2, body := getFactSet(t, h, factsetBase+"?cursor="+url.QueryEscape(*p.Cursor))
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 1, p2.UnitsWalked, "rows 3-4 are in unit 1")
	require.Equal(t, "2", p2.Facts[1].Link.Number, "a forward link to a unit not planned yet knows its number")
	code, p3, body := getFactSet(t, h, factsetBase+"?cursor="+url.QueryEscape(*p2.Cursor))
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 2, p3.UnitsWalked)
}

// Numbering restarts in each unit under the unit's number, from the walk
// tree, and the tree depth follows.
//
// Sabotage: number children from the unit's first node instead of their
// tree parent, and d reads "1.2" instead of "1.1.1".
func TestFactSet_SectionNumbersPerUnit(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))
	nodes, _ := readFactSet(t, h, factsetBase+"?path=kb/u/&depth=2&limit=5")
	at := byUnitPath(nodes)
	for _, c := range []struct {
		key, num  string
		treeDepth int
	}{
		{"1 kb/u/r1.md", "1", 0}, {"1 kb/z/s.md", "1.1", 1}, {"1 kb/z/d.md", "1.1.1", 2}, {"1 kb/u/r2.md", "1.2", 1},
		{"2 kb/u/r2.md", "2", 0}, {"2 kb/z/m.md", "2.1", 1}, {"2 kb/z/s.md", "2.1.1", 2},
	} {
		n, ok := at[c.key]
		require.True(t, ok, c.key)
		require.Equal(t, c.num, n.Number, c.key)
		require.Equal(t, c.treeDepth, n.TreeDepth, c.key)
	}
	require.Len(t, nodes, 7)
}

// Seek: cursor + unit continues from that unit's first section, forward or
// back, and the seek page's cursor continues after it. Out-of-range units are
// a 400; the original cursor still serves its own position.
//
// Sabotage: ignore unit (serve from the cursor's position), and the seek
// page starts elsewhere.
func TestFactSet_Seek(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))

	code, p1, body := getFactSet(t, h, factsetBase+"?path=kb/u/&depth=2&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	c := url.QueryEscape(*p1.Cursor)
	code, p, body := getFactSet(t, h, factsetBase+"?cursor="+c+"&unit=2")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 2, p.UnitFrom, "the seek page starts at unit 2's first section")
	require.Equal(t, "2", p.Facts[0].Number)
	code, p, body = getFactSet(t, h, factsetBase+"?cursor="+url.QueryEscape(*p.Cursor))
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "2.1", p.Facts[0].Number, "the seek page's cursor continues after it")

	code, p, body = getFactSet(t, h, factsetBase+"?cursor="+c+"&unit=1")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "1", p.Facts[0].Number, "seek goes back too")
	for _, u := range []string{"0", "3", "x", "-1"} {
		code, _, body := getFactSet(t, h, factsetBase+"?cursor="+c+"&unit="+u)
		require.Equal(t, http.StatusBadRequest, code, "unit=%s: %s", u, body)
	}
	code, p, body = getFactSet(t, h, factsetBase+"?cursor="+c)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "1.1", p.Facts[0].Number, "the original cursor serves its own position")
}

// A cursor refuses every argument but unit, and the refusal consumes
// nothing; the cursor belongs to the principal; another principal's first
// call leaves it alone, and so does the owner's own: one principal holds many
// factsets at once.
//
// Sabotage: drop the owner comparison in openFactSet (u2 is served), or put
// the owner-wide DELETE back in CreateFactSet (the old cursor is a 404).
//
// A position past the extent is a 400, not a 404: the cursor is the
// caller's, the position is wrong.
func TestFactSet_CursorOwnershipRefusalAndManyPerOwner(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	router := newTestRouter(m)
	owner, other := asPrincipal("u1", router), asPrincipal("u2", router)

	code, p1, body := getFactSet(t, owner, factsetBase+"?path=kb/u/&depth=2&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	c := url.QueryEscape(*p1.Cursor)
	for _, extra := range []string{"depth=1", "facts=1", "q=x", "limit=2", "path=kb/", "x=1"} {
		code, _, body := getFactSet(t, owner, factsetBase+"?cursor="+c+"&"+extra)
		require.Equal(t, http.StatusBadRequest, code, "%s: %s", extra, body)
	}
	_, _, ghost := getFactSet(t, owner, factsetBase+"?cursor=00000000-0000-4000-8000-000000000000.1.0")
	code, _, foreign := getFactSet(t, other, factsetBase+"?cursor="+c)
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, ghost, foreign, "another principal's cursor reads exactly like none")
	// The single-fact pages route is another tool's cursor.
	code, _, _ = getFactSet(t, owner, "/repos/alpha/branches/agent:test/facts/kb/u/r1.md/materialize/pages?cursor="+c)
	require.Equal(t, http.StatusNotFound, code)

	code, p2, body := getFactSet(t, owner, factsetBase+"?cursor="+c)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "1.1", p2.Facts[0].Number, "the refused calls changed nothing")

	// Neither another principal's factset nor the owner's next one ends it.
	code, _, body = getFactSet(t, other, factsetBase+"?path=kb/u/&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	code, _, _ = getFactSet(t, owner, factsetBase+"?cursor="+c)
	require.Equal(t, http.StatusOK, code, "u2's factset left u1's alone")
	code, pz, body := getFactSet(t, owner, factsetBase+"?path=kb/z/&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	code, again, body := getFactSet(t, owner, factsetBase+"?cursor="+c)
	require.Equal(t, http.StatusOK, code, "the owner's second factset left the first open: %s", body)
	require.Equal(t, "1.1", again.Facts[0].Number)
	code, z2, body := getFactSet(t, owner, factsetBase+"?cursor="+url.QueryEscape(*pz.Cursor))
	require.Equal(t, http.StatusOK, code, "and the second pages too: %s", body)
	require.Contains(t, z2.SearchLine, "path kb/z/", "the second cursor serves its own search")

	code, p3, body := getFactSet(t, owner, factsetBase+"?path=kb/u/&depth=2&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	id := (*p3.Cursor)[:strings.IndexByte(*p3.Cursor, '.')]
	code, _, body = getFactSet(t, owner, factsetBase+"?cursor="+id+".3.0")
	require.Equal(t, http.StatusBadRequest, code, "unit 3 of 2: past the extent: %s", body)
}

// One principal holds two factsets at once and pages them ALTERNATELY: each
// serves exactly the bytes it serves when paged alone (only the factset id in
// the cursor differs), page for page, to the end.
//
// Sabotage: put the owner-wide DELETE back in CreateFactSet, and the first
// factset's next page is a 404; key the cache by owner instead of id, and
// one factset's page shows the other's sections.
func TestFactSet_TwoFactSetsOfOneOwnerPageAlternately(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))
	searchA := factsetBase + "?path=kb/u/&depth=2&limit=1"
	searchB := factsetBase + "?path=kb/z/&depth=1&limit=1"
	get := func(u string) string {
		code, _, body := getFactSet(t, h, u)
		require.Equal(t, http.StatusOK, code, "%s: %s", u, body)
		return body
	}
	next := func(c string) string { return get(factsetBase + "?cursor=" + url.QueryEscape(c)) }
	alone := func(first string) []string {
		var pages []string
		for {
			var p materialize.FactSetPage
			require.NoError(t, json.Unmarshal([]byte(first), &p), first)
			norm := first
			if p.Cursor != nil {
				norm = strings.ReplaceAll(first, (*p.Cursor)[:strings.IndexByte(*p.Cursor, '.')], "ID")
			}
			pages = append(pages, norm)
			if !p.HasMore {
				return pages
			}
			first = next(*p.Cursor)
		}
	}
	aloneA := alone(get(searchA))
	aloneB := alone(get(searchB))
	require.Greater(t, len(aloneA), 3)
	require.Greater(t, len(aloneB), 2)
	require.NotEqual(t, aloneA[0], aloneB[0], "the two searches differ")

	// Both open before either is paged; then one page of each in turn.
	firstA, firstB := get(searchA), get(searchB)
	var gotA, gotB []string
	curA, curB := firstA, firstB
	moreA, moreB := true, true
	step := func(raw string, got *[]string) (string, bool) {
		var p materialize.FactSetPage
		require.NoError(t, json.Unmarshal([]byte(raw), &p), raw)
		norm := raw
		if p.Cursor != nil {
			norm = strings.ReplaceAll(raw, (*p.Cursor)[:strings.IndexByte(*p.Cursor, '.')], "ID")
		}
		*got = append(*got, norm)
		if !p.HasMore {
			return "", false
		}
		return next(*p.Cursor), true
	}
	for moreA || moreB {
		if moreA {
			curA, moreA = step(curA, &gotA)
		}
		if moreB {
			curB, moreB = step(curB, &gotB)
		}
	}
	require.Equal(t, aloneA, gotA, "factset A paged alternately with B is A paged alone")
	require.Equal(t, aloneB, gotB, "factset B paged alternately with A is B paged alone")
}

// Paging past the UI's 50: the factset holds every result up to Facts (the
// default is all, capped at 500), and its first 50 units are the /search
// list's DEFAULT page, in order (F23: the list's length is written once, as
// materialize.UIListLimit, the route's default; the UI sends no limit), and
// likewise for sort=recent and the /facts collection. facts=N keeps the best
// N.
//
// Sabotage: run the search with the UI's limit (50), and unit 51.. is
// missing; give the routes another default, and the list is not the prefix.
func TestFactSet_PagesPastFiftyResults(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	alpha := m.Get("alpha")
	for i := range 60 {
		writeMatFact(t, alpha, "agent/test", fmt.Sprintf("kb/p/f%02d.md", i), fmt.Sprintf("F%02d", i), "body.")
	}
	h := asPrincipal("u1", newTestRouter(m))
	ui := searchPaths(t, h, "path=kb/p/")
	require.Len(t, ui, materialize.UIListLimit, "the /search route's default page is the UI list")
	require.Equal(t, 50, materialize.UIListLimit)

	nodes, pages := readFactSet(t, h, factsetBase+"?path=kb/p/&depth=0&limit=5")
	require.Equal(t, 60, pages[0].Units)
	var roots []string
	for _, n := range nodes {
		roots = append(roots, n.Path)
	}
	require.Len(t, roots, 60)
	require.Equal(t, ui, roots[:50], "the first 50 units are the list the UI shows")

	recentList := recentPaths(t, h, "path=kb/p/&sort=recent")
	require.Len(t, recentList, materialize.UIListLimit, "the /facts collection's default page is the UI list")
	rnodes, _ := readFactSet(t, h, factsetBase+"?path=kb/p/&sort=recent&depth=0&limit=5")
	var rroots []string
	for _, n := range rnodes {
		rroots = append(rroots, n.Path)
	}
	require.Equal(t, recentList, rroots[:50], "sort=recent: the first 50 units are the recent list")

	_, few := readFactSet(t, h, factsetBase+"?path=kb/p/&depth=0&limit=5&facts=7")
	require.Equal(t, 7, few[0].Units)
	require.Equal(t, 500, materialize.DefaultFactSetFacts)
	code, _, _ := getFactSet(t, h, factsetBase+"?path=kb/p/&facts=501")
	require.Equal(t, http.StatusBadRequest, code)
}

// The title and the full-search line are computed on the server.
func TestFactSet_TitleAndSearchLine(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))
	code, p, body := getFactSet(t, h, factsetBase+"?path=kb/u/&type=observation&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "Observations in U", p.Title, "no text, entities or domains: the fallback title from the other filters")
	require.Equal(t, "path kb/u/ · type observation · sorted by relevance", p.SearchLine)
	require.Equal(t, materialize.FactSetMode, p.Mode)
	require.Equal(t, materialize.FactSetInstructions, p.Instructions)
}

// Overload bounds on the REST edge: over the concurrent-build cap is a 503
// with Retry-After and writes nothing.
//
// Sabotage: block on the slot instead of refusing, and the call hangs.
func TestFactSet_ConcurrentBuildCapIs503(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))
	release := materialize.ReserveFactSetBuildsForTest(materialize.MaxConcurrentFactSetBuilds)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, factsetBase+"?path=kb/u/", nil))
	release()
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Equal(t, materializeBusyRetryAfter, rec.Header().Get("Retry-After"))
	code, _, body := getFactSet(t, h, factsetBase+"?path=kb/u/")
	require.Equal(t, http.StatusOK, code, "a slot freed is a slot: %s", body)
}

// Export: the whole factset as one agent document, one "##" per unit, every
// section once, links as in-document anchors that resolve; an attachment
// named like the single export; the cursor's position is left alone.
//
// Sabotage: write a link's target as its own appearance anchor, and the
// "(see …)" anchor resolves to nothing.
func TestFactSet_ExportIsOneDocumentWithAnchors(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))
	code, p1, body := getFactSet(t, h, factsetBase+"?path=kb/u/&depth=2&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	c := url.QueryEscape(*p1.Cursor)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, factsetBase+"/export?cursor="+c, nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, `attachment; filename="`+materialize.FactSetExportFileName(p1.FactSetEnv, *p1.Cursor)+`"`, rec.Header().Get("Content-Disposition"))
	doc := rec.Body.String()
	require.True(t, strings.HasPrefix(doc, "# Facts in U\n"), doc)
	require.Equal(t, 2, strings.Count(doc, "\n## "), "one ## per unit:\n%s", doc)
	for _, b := range []string{"r1 body.", "r2 body.", "s body.", "d body.", "m body."} {
		require.Equal(t, 1, strings.Count(doc, b), "%q written once", b)
	}
	// Every in-document link target exists, once.
	for _, part := range strings.Split(doc, "](#")[1:] {
		id := part[:strings.IndexByte(part, ')')]
		require.Equal(t, 1, strings.Count(doc, `<a id="`+id+`"></a>`), "anchor %s resolves to one target", id)
	}
	require.Contains(t, doc, "### 1.2 R2 (see [2](#")
	require.Contains(t, doc, "### 2.1.1 S (see [1.1](#", "a back link resolved to the number written")

	code, p2, body := getFactSet(t, h, factsetBase+"?cursor="+c)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "1.1", p2.Facts[0].Number, "the export left the cursor's position alone")

	code, _, _ = getMaterialize(t, asPrincipal("u2", newTestRouter(m)), factsetBase+"/export?cursor="+c)
	require.Equal(t, http.StatusNotFound, code, "the export is owned like the pages")
}

// deadlineRecorder is a ResponseWriter that, like a real connection, takes
// a write deadline (http.ResponseController finds SetWriteDeadline on it),
// and records whether every Write was preceded by one.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	armed     bool
	unarmed   int
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.deadlines = append(d.deadlines, t)
	d.armed = true
	return nil
}

func (d *deadlineRecorder) Write(p []byte) (int, error) {
	if !d.armed {
		d.unarmed++
	}
	d.armed = false
	return d.ResponseRecorder.Write(p)
}

// Q5 (review of PR #692, round 2): every write of an export's stream sets
// the connection's write deadline about a minute ahead. The listeners run
// with WriteTimeout 0, so this deadline is the only thing that stops a
// client that stops reading from holding its export slot forever.
//
// Sabotage: drop the SetWriteDeadline call, or stream to the raw
// ResponseWriter, and a write goes out with no deadline.
func TestFactSet_ExportSetsAWriteDeadlineOnEveryWrite(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	alpha := m.Get("alpha")
	for i := range 60 {
		writeMatFact(t, alpha, "agent/test", fmt.Sprintf("kb/e/f%02d.md", i), fmt.Sprintf("F%02d", i), "body.")
	}
	h := asPrincipal("u1", newTestRouter(m))
	code, p1, body := getFactSet(t, h, factsetBase+"?path=kb/e/&depth=0&limit=1")
	require.Equal(t, http.StatusOK, code, body)

	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	start := time.Now()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, factsetBase+"/export?cursor="+*p1.Cursor, nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 60, strings.Count(rec.Body.String(), "\n## "))
	require.GreaterOrEqual(t, len(rec.deadlines), 2, "two chunks, at least two writes")
	require.Zero(t, rec.unarmed, "no write went out without a deadline")
	for _, d := range rec.deadlines {
		require.WithinDuration(t, start.Add(time.Minute), d, 30*time.Second, "about a minute ahead")
	}
}

// Over the export cap an export is refused at once: a 503 with
// Retry-After, nothing streamed.
//
// Sabotage: map ErrFactSetExportsBusy to nothing special, and it is a 500.
func TestFactSet_ExportCapIs503(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))
	code, p1, body := getFactSet(t, h, factsetBase+"?path=kb/u/&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	release := materialize.ReserveFactSetExportsForTest(materialize.MaxConcurrentFactSetExports)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, factsetBase+"/export?cursor="+*p1.Cursor, nil))
	release()
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Equal(t, materializeBusyRetryAfter, rec.Header().Get("Retry-After"))
}

// F11. A repo entered cross-KB is read at ONE tip for the whole FactSet,
// frozen at CREATION (the tips of every repo the caller can reach), even when
// later units are planned by later calls after that repo moved on, and after
// the cache is evicted (the replay reads the frozen tip too). Unit 1 (r1)
// reads beta's x; beta then rewrites x and adds y; unit 2 (r2 -> x, y),
// planned by the next calls, still reads beta at the frozen tip: x at the
// version unit 1 wrote (no different_version), and y not there yet.
//
// Sabotage: read each call's cross-KB tip afresh (drop the frozen tips in
// reader.foreign), and x's link is flagged and y is written.
func TestFactSet_CrossKBTipIsFrozenAcrossUnits(t *testing.T) {
	m := newMatStoreManager(t, "alpha", "beta")
	alpha, beta := m.Get("alpha"), m.Get("beta")
	const br = "agent/test"
	b12 := fact.ID12(beta.ID())
	x, y := "kb://"+b12+"/kb/b/x.md", "kb://"+b12+"/kb/b/y.md"
	writeMatFact(t, beta, br, "kb/b/x.md", "X", "x first.")
	writeMatFact(t, alpha, br, "kb/u/r2.md", "R2", "r2 body.", x, y)
	writeMatFact(t, alpha, br, "kb/u/r1.md", "R1", "r1 body.", x)
	h := asPrincipal("u1", newTestRouter(m))

	code, p, body := getFactSet(t, h, factsetBase+"?path=kb/u/&cross_kb=true&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 1, p.UnitsWalked, "only unit 1 is walked by the first call")
	writeMatFact(t, beta, br, "kb/b/x.md", "X", "x second.")
	writeMatFact(t, beta, br, "kb/b/y.md", "Y", "y body.")

	nodes := p.Facts
	materialize.DropFactSetCacheForTest()
	for p.HasMore {
		code, p, body = getFactSet(t, h, factsetBase+"?cursor="+url.QueryEscape(*p.Cursor))
		require.Equal(t, http.StatusOK, code, body)
		nodes = append(nodes, p.Facts...)
	}
	at := byUnitPath(nodes)
	x1, x2, r2 := at["1 "+x], at["2 "+x], at["2 kb/u/r2.md"]
	require.True(t, x1.Written)
	require.Equal(t, "x first.", x1.Body)
	require.NotNil(t, x2.Link, "x was written in unit 1: a back link")
	require.Equal(t, x1.Commit, x2.Commit, "unit 2 read x at the tip unit 1 froze")
	require.False(t, x2.Link.DifferentVersion)
	_, written := at["2 "+y]
	require.False(t, written, "y is not at the frozen tip")
	var reasons []string
	for _, o := range r2.NotIncluded {
		reasons = append(reasons, o.Path+" "+o.Reason)
	}
	require.Contains(t, reasons, y+" "+materialize.ReasonNotInThatKB)
	require.Contains(t, x2.Marks, materialize.MarkChangedSinceCited, "RULE 1: beta's read branch holds another x now")
}

// F11 (second half): a registered repo the caller does not reach reads
// exactly like one that does not exist (kb_unavailable, 89b26b07), and its
// tip is not frozen; a cursor whose frozen tips include a repo the resuming
// caller does not reach is NOT refused (no resume-time reach check): the
// content read through the narrow credential is served as unavailable.
//
// Sabotage: drop the reach check in reader.target, and the narrow caller is
// served the wide caller's cited body.
func TestFactSet_F11_UnreachableRepoIsUnavailable(t *testing.T) {
	m := newMatStoreManager(t, "alpha", "beta")
	alpha, beta := m.Get("alpha"), m.Get("beta")
	const br = "agent/test"
	x := "kb://" + fact.ID12(beta.ID()) + "/kb/b/x.md"
	writeMatFact(t, beta, br, "kb/b/x.md", "X", "x body.")
	writeMatFact(t, alpha, br, "kb/u/r1.md", "R1", "r1 body.", x)
	router := newTestRouter(m)
	narrow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithPrincipal(r.Context(), auth.Principal{Kind: auth.KindHost, ID: "u1", Via: auth.ViaSocket})
		router.ServeHTTP(w, r.WithContext(auth.WithRepoReach(ctx, func(repo string) bool { return repo == "alpha" })))
	})
	code, p, body := getFactSet(t, narrow, factsetBase+"?path=kb/u/&cross_kb=true&limit=5")
	require.Equal(t, http.StatusOK, code, body)
	require.Len(t, p.Facts, 1)
	require.Equal(t, []materialize.Omission{{Path: x, Reason: materialize.ReasonKBUnavail}}, p.Facts[0].NotIncluded)

	wide := asPrincipal("u1", router)
	code, pw, body := getFactSet(t, wide, factsetBase+"?path=kb/u/&cross_kb=true&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	code, p2, body := getFactSet(t, narrow, factsetBase+"?cursor="+url.QueryEscape(*pw.Cursor))
	require.Equal(t, http.StatusOK, code, "a narrower credential resuming a wider caller's cursor is served: %s", body)
	require.NotContains(t, body, "x body.")
	require.True(t, byUnitPath(p2.Facts)["1 "+x].Unavailable, body)
}

// factsetGone drives the resume reach semantics: the FactSet is frozen with
// tips for beta and gamma (cross_kb), cites only beta. Unit 1 (r1) writes
// beta's x; unit 2 (r2) writes beta's y, unplanned until the second page.
func factsetGoneFixture(t *testing.T) (m *repos.Manager, h http.Handler, x, y string, reach *map[string]bool) {
	t.Helper()
	m = newMatStoreManager(t, "alpha", "beta", "gamma")
	alpha, beta := m.Get("alpha"), m.Get("beta")
	const br = "agent/test"
	b12 := fact.ID12(beta.ID())
	x, y = "kb://"+b12+"/kb/b/x.md", "kb://"+b12+"/kb/b/y.md"
	writeMatFact(t, beta, br, "kb/b/x.md", "X", "x body.")
	writeMatFact(t, beta, br, "kb/b/y.md", "Y", "y body.")
	writeMatFact(t, m.Get("gamma"), br, "kb/g/g.md", "G", "g body.")
	writeMatFact(t, alpha, br, "kb/u/r1.md", "R1", "r1 body.", x)
	writeMatFact(t, alpha, br, "kb/u/r2.md", "R2", "r2 body.", y)
	blocked := map[string]bool{}
	router := newTestRouter(m)
	h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithPrincipal(r.Context(), auth.Principal{Kind: auth.KindHost, ID: "u1", Via: auth.ViaSocket})
		router.ServeHTTP(w, r.WithContext(auth.WithRepoReach(ctx, func(repo string) bool { return !blocked[repo] })))
	})
	return m, h, x, y, &blocked
}

func factsOf(t *testing.T, h http.Handler, u string) (int, string) {
	t.Helper()
	code, p, body := getFactSet(t, h, u)
	if code != http.StatusOK {
		return code, body
	}
	raw, err := json.Marshal(p.Facts)
	require.NoError(t, err)
	return code, string(raw)
}

// An unrelated repo (gamma: tip frozen, never cited) becoming unreachable
// does not touch the FactSet: every page's facts are byte-identical to an
// undisturbed read, with a warm cache and after the cache is dropped.
//
// Sabotage: restore the resume-time loop over e.fr.Tips (target() per frozen
// repo) in openFactSet, and the resume answers 404.
func TestFactSet_ResumeIgnoresAnUnrelatedUnreachableRepo(t *testing.T) {
	_, h, _, _, blocked := factsetGoneFixture(t)
	const first = factsetBase + "?path=kb/u/&cross_kb=true&limit=1"
	read := func(drop bool, block func()) []string {
		code, p, body := getFactSet(t, h, first)
		require.Equal(t, http.StatusOK, code, body)
		raw, _ := json.Marshal(p.Facts)
		out := []string{string(raw)}
		block()
		if drop {
			materialize.DropFactSetCacheForTest()
		}
		for p.HasMore {
			code, p, body = getFactSet(t, h, factsetBase+"?cursor="+url.QueryEscape(*p.Cursor))
			require.Equal(t, http.StatusOK, code, "resume after the unrelated repo became unreachable: %s", body)
			raw, _ = json.Marshal(p.Facts)
			out = append(out, string(raw))
		}
		return out
	}
	base := read(false, func() {})
	for _, drop := range []bool{false, true} {
		*blocked = map[string]bool{}
		got := read(drop, func() { (*blocked)["gamma"] = true })
		require.Equal(t, base, got, "drop=%v", drop)
	}
}

// A repo the FactSet cites becoming unreachable does not 404 the resume:
// content already planned (unit 1's x, planned by the first call) and read
// again now shows as unavailable (a heading alone, numbering kept); content
// not yet planned (unit 2's y) is planned now, and the unreachable repo reads
// as kb_unavailable in r2's not_included. Same with a warm cache and after
// the cache is dropped.
//
// Sabotage: drop the unavailable marking in fsCall.inflate and the page is a
// 500 (repo not available); restore the resume-time Tips loop and it is 404.
func TestFactSet_ResumeAfterACitedRepoIsGone(t *testing.T) {
	_, h, x, y, blocked := factsetGoneFixture(t)
	for _, drop := range []bool{false, true} {
		*blocked = map[string]bool{}
		code, p1, body := getFactSet(t, h, factsetBase+"?path=kb/u/&cross_kb=true&limit=1")
		require.Equal(t, http.StatusOK, code, body)
		require.Equal(t, "r1 body.", byUnitPath(p1.Facts)["1 kb/u/r1.md"].Body)
		(*blocked)["beta"] = true
		if drop {
			materialize.DropFactSetCacheForTest()
		}
		// Seek each unit (after a replay the first call's position can lie
		// past a re-planned unit's end: re-reads may differ).
		var nodes []materialize.FactSetNode
		for u := 1; u <= 2; u++ {
			p := p1
			code, p, body = getFactSet(t, h, factsetBase+"?cursor="+url.QueryEscape(*p1.Cursor)+"&unit="+fmt.Sprint(u))
			for {
				require.Equal(t, http.StatusOK, code, "drop=%v unit=%d: %s", drop, u, body)
				require.NotContains(t, body, "x body.")
				require.NotContains(t, body, "y body.")
				nodes = append(nodes, p.Facts...)
				if !p.HasMore || p.UnitFrom > u {
					break
				}
				code, p, body = getFactSet(t, h, factsetBase+"?cursor="+url.QueryEscape(*p.Cursor))
			}
		}
		at := byUnitPath(nodes)
		if !drop {
			nx := at["1 "+x]
			require.True(t, nx.Unavailable, "planned before, read now through a gone repo")
			require.False(t, nx.Written)
		}
		r2 := at["2 kb/u/r2.md"]
		require.Equal(t, "r2 body.", r2.Body)
		require.NotContains(t, at, "2 "+y, "y is not written: its repo is gone")
		require.Contains(t, r2.NotIncluded, materialize.Omission{Path: y, Reason: materialize.ReasonKBUnavail})
	}
}

// A cursor issued before a cited repo went away never 400s because its unit
// shrank: with beta gone and the cache dropped, unit 1 is re-planned as just
// r1 (x is unavailable-less: not written), so position 1.1 lies past its end
// and the resume continues at unit 2's first section. Past the last unit is
// still the end-of-FactSet 400.
//
// Sabotage: restore the ErrInvalid "past unit N's end" answer in fsCall.page
// and the resume is a 400.
func TestFactSet_ResumeInsideAShrunkUnitContinuesAtTheNextUnit(t *testing.T) {
	_, h, _, _, blocked := factsetGoneFixture(t)
	code, p1, body := getFactSet(t, h, factsetBase+"?path=kb/u/&cross_kb=true&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 1, p1.Facts[0].Unit)
	require.True(t, strings.HasSuffix(*p1.Cursor, ".1.1"), *p1.Cursor)
	(*blocked)["beta"] = true
	materialize.DropFactSetCacheForTest()
	code, p2, body := getFactSet(t, h, factsetBase+"?cursor="+url.QueryEscape(*p1.Cursor))
	require.Equal(t, http.StatusOK, code, "a valid cursor must not 400 when its unit shrank: %s", body)
	require.Equal(t, 2, p2.UnitFrom, body)
	require.Equal(t, "kb/u/r2.md", p2.Facts[0].Path, "continues at unit 2's first section")
	require.Equal(t, 2, p2.Facts[0].Unit)
	// a position past the last unit is still the end answer
	last := strings.TrimSuffix(*p1.Cursor, ".1.1") + ".3.0"
	code, _, body = getFactSet(t, h, factsetBase+"?cursor="+url.QueryEscape(last))
	require.Equal(t, http.StatusBadRequest, code, body)
}

// Unknown id, another principal and a wrong pin stay the identical 404,
// whatever the state of the cited repos (no existence oracle).
//
// Sabotage: answer another owner's cursor with the page, and it is a 200.
func TestFactSet_ResumeUnknownAndForeignCursorsStay404(t *testing.T) {
	m, h, _, _, blocked := factsetGoneFixture(t)
	code, p1, body := getFactSet(t, h, factsetBase+"?path=kb/u/&cross_kb=true&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	(*blocked)["beta"] = true
	other := asPrincipal("u2", newTestRouter(m))
	cur := url.QueryEscape(*p1.Cursor)
	c1, _, b1 := getMaterialize(t, other, factsetBase+"?cursor="+cur)
	unknown := "?cursor=" + url.QueryEscape(strings.Replace(*p1.Cursor, mustCursorID(t, *p1.Cursor), "00000000-0000-0000-0000-000000000000", 1))
	c2, _, b2 := getMaterialize(t, h, factsetBase+unknown)
	require.Equal(t, http.StatusNotFound, c1, b1)
	require.Equal(t, http.StatusNotFound, c2, b2)
	require.Equal(t, b2, b1)
}

func mustCursorID(t *testing.T, cursor string) string {
	t.Helper()
	i := strings.IndexByte(cursor, '.')
	require.Positive(t, i, cursor)
	return cursor[:i]
}

// newEmbFactSet is a store with the cos test embedder (its vectors searched
// for real) behind the kb routes, and the repo to write into.
func newEmbFactSet(t *testing.T) (http.Handler, *repos.RepoInstance) {
	t.Helper()
	emb := &cosEmbedder{}
	m := repos.New(context.Background(), repos.Deps{})
	svc, err := store.Open(filepath.Join(t.TempDir(), "alpha.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", UID: "uid-alpha", AgentBranch: "agent/test", Svc: svc, OntologyRoot: "kb", Embedder: emb,
	})
	m.Set("alpha", ri)
	r := chi.NewRouter()
	repo := r.Route("/repos/{repo}", func(chi.Router) {})
	repo.Use(repoFromURL(m))
	branch := repo.Route("/branches/{branch}", func(chi.Router) {})
	branch.Use(httpx.BranchMiddleware)
	lens := r.Route("/lenses/{lens}", func(chi.Router) {})
	lens.Use(lensFromURL(m))
	Mount(branch, lens, "kb", emb, m)
	return asPrincipal("u1", r), ri
}

// cosFacts writes path -> cosine facts in one commit; mod sets the type.
func cosFacts(t *testing.T, ri *repos.RepoInstance, files map[string]float64, typ fact.Type) {
	t.Helper()
	out := map[string]string{}
	for p, c := range files {
		out[p] = factText(t, p, strings.TrimSuffix(p[strings.LastIndexByte(p, '/')+1:], ".md"), c, func(f *fact.Fact) { f.Type = typ })
	}
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, _, err := svc.Facts().BatchWriteFacts(context.Background(), ri.AgentBranch(), out, nil, "add facts", "learn")
		require.NoError(t, err)
	}))
}

// N2 (review of PR #692): for a FILTERED TEXT search the factset's first 50
// units are exactly the UI's list (the /search call at limit 50), in its
// order, though the store's nearest-neighbour window grows with the limit
// and the filters apply after it:
//   - 300 observations at cosine 0.9 fill the UI's window (5 x 50), so the
//     type=hypothesis list holds only the 5 hypotheses at 0.95. The 25 at
//     0.6 are inside a limit-500 window, so a factset searching at limit 500
//     had 30 units; it has the UI's 5.
//   - 60 tied hypotheses at 0.9 (all inside the window): the UI lists the
//     first 50 by path; the factset's first 50 are those, then the other 10.
//
// Sabotage: search once at limit Facts (the old runFactSetSearch), and the
// factset holds 30 units where the list shows 5.
func TestFactSet_FirstFiftyAreTheUIListForFilteredText(t *testing.T) {
	h, ri := newEmbFactSet(t)
	files := map[string]float64{}
	for i := range 300 {
		files[fmt.Sprintf("kb/n/o%03d.md", i)] = 0.9
	}
	cosFacts(t, ri, files, fact.Observation)
	hyps := map[string]float64{}
	for i := range 5 {
		hyps[fmt.Sprintf("kb/n/h0%d.md", i)] = 0.95
	}
	for i := range 25 {
		hyps[fmt.Sprintf("kb/n/h1%02d.md", i)] = 0.6
	}
	cosFacts(t, ri, hyps, fact.Hypothesis)

	ui := searchPaths(t, h, "q=x&type=hypothesis&path=kb/n/&limit=50")
	require.Equal(t, []string{"kb/n/h00.md", "kb/n/h01.md", "kb/n/h02.md", "kb/n/h03.md", "kb/n/h04.md"}, ui,
		"the UI's window holds only the 5 hypotheses at 0.95")
	nodes, pages := readFactSet(t, h, factsetBase+"?q=x&type=hypothesis&path=kb/n/&depth=0&limit=5")
	require.Equal(t, 5, pages[0].Units, "the factset is the list the UI shows")
	var roots []string
	for _, n := range nodes {
		roots = append(roots, n.Path)
	}
	require.Equal(t, ui, roots)

	tied := map[string]float64{}
	for i := range 60 {
		tied[fmt.Sprintf("kb/m/t%02d.md", 59-i)] = 0.9
	}
	cosFacts(t, ri, tied, fact.Hypothesis)
	ui = searchPaths(t, h, "q=x&type=hypothesis&path=kb/m/&limit=50")
	require.Len(t, ui, 50)
	for i, p := range ui {
		require.Equal(t, fmt.Sprintf("kb/m/t%02d.md", i), p, "ties are listed by path")
	}
	nodes, pages = readFactSet(t, h, factsetBase+"?q=x&type=hypothesis&path=kb/m/&depth=0&limit=5")
	require.Equal(t, 60, pages[0].Units, "a full list extends past 50")
	roots = roots[:0]
	for _, n := range nodes {
		roots = append(roots, n.Path)
	}
	require.Equal(t, ui, roots[:50], "the first 50 are the UI's list")
	for i, p := range roots[50:] {
		require.Equal(t, fmt.Sprintf("kb/m/t%02d.md", 50+i), p, "then the rest, deterministically")
	}
}

func deleteFactSet(t *testing.T, h http.Handler, u string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, u, nil))
	return rec.Code, rec.Body.String()
}

// DELETE …/search/factset?cursor= closes the caller's factset: 204, and every
// cursor of it then reads like one that never existed (page, seek, export, a
// second close). Another principal's close, and the close of an unknown id,
// are the same 404 and close nothing. Closing one factset leaves the owner's
// other one open.
//
// Sabotage: drop the owner from CloseFactSet's DELETE (u2's close is a 204
// and u1's factset is gone), or skip the delete (the closed cursor still
// pages).
func TestFactSet_DeleteClosesOnlyTheOwnersFactSet(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	router := newTestRouter(m)
	owner, other := asPrincipal("u1", router), asPrincipal("u2", router)

	code, p, body := getFactSet(t, owner, factsetBase+"?path=kb/u/&depth=2&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	c := url.QueryEscape(*p.Cursor)
	code, keep, body := getFactSet(t, owner, factsetBase+"?path=kb/z/&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	const ghost = "00000000-0000-4000-8000-000000000000.1.0"

	code, ghostClose := deleteFactSet(t, other, factsetBase+"?cursor="+ghost)
	require.Equal(t, http.StatusNotFound, code)
	code, foreignClose := deleteFactSet(t, other, factsetBase+"?cursor="+c)
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, ghostClose, foreignClose, "closing another principal's factset reads exactly like closing none")
	code, p2, body := getFactSet(t, owner, factsetBase+"?cursor="+c)
	require.Equal(t, http.StatusOK, code, "u2's close closed nothing: %s", body)
	require.Equal(t, "1.1", p2.Facts[0].Number)

	for _, bad := range []string{"", "?cursor=" + c + "&unit=1", "?cursor=not-a-factset-cursor"} {
		code, body := deleteFactSet(t, owner, factsetBase+bad)
		require.Equal(t, http.StatusBadRequest, code, "%q: %s", bad, body)
	}

	code, body = deleteFactSet(t, owner, factsetBase+"?cursor="+url.QueryEscape(*p2.Cursor))
	require.Equal(t, http.StatusNoContent, code, "any cursor of the factset closes it: %s", body)
	require.Empty(t, body)

	_, _, ghostPage := getFactSet(t, owner, factsetBase+"?cursor="+ghost)
	code, _, gone := getFactSet(t, owner, factsetBase+"?cursor="+c)
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, ghostPage, gone, "a closed factset's cursor reads like none")
	code, _, _ = getFactSet(t, owner, factsetBase+"?cursor="+c+"&unit=2")
	require.Equal(t, http.StatusNotFound, code)
	code, _, _ = getMaterialize(t, owner, factsetBase+"/export?cursor="+c)
	require.Equal(t, http.StatusNotFound, code)
	code, again := deleteFactSet(t, owner, factsetBase+"?cursor="+c)
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, ghostClose, again, "a second close reads like closing none")

	code, _, body = getFactSet(t, owner, factsetBase+"?cursor="+url.QueryEscape(*keep.Cursor))
	require.Equal(t, http.StatusOK, code, "the owner's other factset stays open: %s", body)
}
