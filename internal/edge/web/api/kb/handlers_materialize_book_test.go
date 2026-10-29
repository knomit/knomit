package kb

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
)

const bookBase = "/repos/alpha/branches/agent:test/search/book"

func getBook(t *testing.T, h http.Handler, u string) (int, materialize.BookPage, string) {
	t.Helper()
	code, ctype, body := getMaterialize(t, h, u)
	var p materialize.BookPage
	if code == http.StatusOK {
		require.Equal(t, "application/json", ctype)
		require.NoError(t, json.Unmarshal([]byte(body), &p), body)
	}
	return code, p, body
}

// readBook pages a book from its first call to the end: every node in book
// order, and the pages.
func readBook(t *testing.T, h http.Handler, first string) ([]materialize.BookNode, []materialize.BookPage) {
	t.Helper()
	var nodes []materialize.BookNode
	var pages []materialize.BookPage
	u := first
	for {
		code, p, body := getBook(t, h, u)
		require.Equal(t, http.StatusOK, code, body)
		pages = append(pages, p)
		nodes = append(nodes, p.Facts...)
		if !p.HasMore {
			return nodes, pages
		}
		require.NotNil(t, p.Cursor)
		u = bookBase + "?cursor=" + url.QueryEscape(*p.Cursor)
		require.Less(t, len(pages), 2000, "paging does not end")
	}
}

// searchPaths is what the repo /search route lists for query: the list the
// book must materialize.
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

// seedShared writes the write-once fixture under kb/u/ (the results) and
// kb/z/ (what they build on):
//
//	u/r1 -> z/s, u/r2        z/s -> z/d
//	u/r2 -> z/m              z/m -> z/s
//
// At depth 2, s has room 2 in r1's unit (s, d) and room 1 in r2's (d is at
// distance 3 there). r2 is a result, so in r1's unit it is a link to unit r2.
func seedShared(t *testing.T, alpha *repos.RepoInstance) {
	t.Helper()
	const br = "agent/test"
	writeMatFact(t, alpha, br, "kb/z/d.md", "D", "d body.")
	writeMatFact(t, alpha, br, "kb/z/s.md", "S", "s body.", "kb/z/d.md")
	writeMatFact(t, alpha, br, "kb/z/m.md", "M", "m body.", "kb/z/s.md")
	writeMatFact(t, alpha, br, "kb/u/r2.md", "R2", "r2 body.", "kb/z/m.md")
	writeMatFact(t, alpha, br, "kb/u/r1.md", "R1", "r1 body.", "kb/z/s.md", "kb/u/r2.md")
}

func byUnitPath(nodes []materialize.BookNode) map[string]materialize.BookNode {
	out := map[string]materialize.BookNode{}
	for _, n := range nodes {
		out[fmt.Sprintf("%d %s", n.Unit, n.Path)] = n
	}
	return out
}

func unitOf(nodes []materialize.BookNode, root string) int {
	for _, n := range nodes {
		if n.Path == root && (n.Role == materialize.RoleRoot || n.Role == materialize.RoleFocal) {
			return n.Unit
		}
	}
	return 0
}

// Every search result has its own unit, in search order, rooted at it; the
// book's sections are exactly each unit's own walk; every fact is written
// once and carries its body there; every other appearance is a heading-only
// link to it, forward or back; a result is written in its own unit.
//
// Sabotage: drop `result DESC` from FinishBook's placement order (s's most
// room loses to... r2's root is written in unit r1), or flip the direction
// comparison, or drop room from the ORDER BY (earliest wins).
func TestBook_WriteOnceAtMostRoomAndLinks(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))

	want := searchPaths(t, h, "path=kb/u/")
	require.ElementsMatch(t, []string{"kb/u/r1.md", "kb/u/r2.md"}, want)

	nodes, pages := readBook(t, h, bookBase+"?path=kb/u/&depth=2&limit=5")
	require.Equal(t, 2, pages[0].Units)
	require.Equal(t, len(nodes), pages[0].Total)
	for i, p := range want {
		require.Equal(t, i+1, unitOf(nodes, p), "unit %d is search result %d", i+1, i+1)
	}
	u1, u2 := unitOf(nodes, "kb/u/r1.md"), unitOf(nodes, "kb/u/r2.md")
	at := byUnitPath(nodes)

	// Unit r1: r1, s, d, r2 (link), m. Unit r2: r2, m, s (link).
	written := map[string]int{}
	for i, n := range nodes {
		require.Equal(t, i+1, n.Seq, "seq is book order")
		if n.Written {
			written[n.Path]++
			require.NotEmpty(t, n.Body, "a written section carries its body")
			require.Nil(t, n.Link)
		} else {
			require.Empty(t, n.Body, "a link carries no body")
			require.Nil(t, n.BuildsOn)
			require.NotNil(t, n.Link)
		}
	}
	for p, c := range written {
		require.Equal(t, 1, c, "%s is written once", p)
	}
	require.Len(t, written, 5, "r1, r2, s, d, m are each written")

	s1, s2 := at[fmt.Sprintf("%d kb/z/s.md", u1)], at[fmt.Sprintf("%d kb/z/s.md", u2)]
	require.True(t, s1.Written, "s is written where it has the most room (unit r1: s and d)")
	require.False(t, s2.Written)
	require.Equal(t, s1.Seq, s2.Link.Seq)
	require.Equal(t, s1.Number, s2.Link.Number)
	require.Equal(t, s1.AnchorID, s2.Link.AnchorID)
	require.Equal(t, s1.Unit, s2.Link.Unit)
	wantDir := materialize.LinkBack
	if s1.Seq > s2.Seq {
		wantDir = materialize.LinkForward
	}
	require.Equal(t, wantDir, s2.Link.Direction)

	// m: room 1 in r1's unit (s was already written above it there), room 2
	// in r2's (m, s): written in r2's unit whichever unit comes first.
	m1, m2 := at[fmt.Sprintf("%d kb/z/m.md", u1)], at[fmt.Sprintf("%d kb/z/m.md", u2)]
	require.True(t, m2.Written, "m is written where it has the most room, not where it appears first")
	require.False(t, m1.Written)
	require.Equal(t, m2.Seq, m1.Link.Seq)

	r2link := at[fmt.Sprintf("%d kb/u/r2.md", u1)]
	r2root := at[fmt.Sprintf("%d kb/u/r2.md", u2)]
	require.False(t, r2link.Written, "a result is written in its own unit, never where it is cited")
	require.True(t, r2root.Written)
	require.Equal(t, u2, r2link.Link.Unit)
	require.Equal(t, fmt.Sprint(u2), r2link.Link.Number, "a link to a unit root reads (see <unit>)")
	if u2 > u1 {
		require.Equal(t, materialize.LinkForward, r2link.Link.Direction)
	} else {
		require.Equal(t, materialize.LinkBack, r2link.Link.Direction)
	}

	// Builds-on anchors point at the written appearance, wherever it is.
	r1 := at[fmt.Sprintf("%d kb/u/r1.md", u1)]
	anchors := map[string]string{}
	for _, l := range r1.BuildsOn {
		anchors[l.Title] = l.AnchorID
	}
	require.Equal(t, s1.AnchorID, anchors["S"])
	require.Equal(t, r2root.AnchorID, anchors["R2"])
}

// Numbering restarts in each unit under the unit's number: the root is "u",
// its sections u.1, u.2, u.1.1, and the tree depth follows; it is fixed by
// the structure, so it is the same on every read.
//
// Sabotage: number children from the unit's first node instead of their
// tree parent, and d reads "u.2" instead of "u.1.1".
func TestBook_SectionNumbersPerUnit(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))
	nodes, _ := readBook(t, h, bookBase+"?path=kb/u/&depth=2&limit=5")
	u1, u2 := unitOf(nodes, "kb/u/r1.md"), unitOf(nodes, "kb/u/r2.md")
	at := byUnitPath(nodes)
	for _, c := range []struct {
		unit      int
		path, num string
		treeDepth int
	}{
		{u1, "kb/u/r1.md", fmt.Sprint(u1), 0},
		{u1, "kb/z/s.md", fmt.Sprintf("%d.1", u1), 1},
		{u1, "kb/z/d.md", fmt.Sprintf("%d.1.1", u1), 2},
		{u1, "kb/u/r2.md", fmt.Sprintf("%d.2", u1), 1},
		{u2, "kb/u/r2.md", fmt.Sprint(u2), 0},
		{u2, "kb/z/m.md", fmt.Sprintf("%d.1", u2), 1},
		{u2, "kb/z/s.md", fmt.Sprintf("%d.1.1", u2), 2},
	} {
		n, ok := at[fmt.Sprintf("%d %s", c.unit, c.path)]
		require.True(t, ok, "%s in unit %d", c.path, c.unit)
		require.Equal(t, c.num, n.Number, "%s in unit %d", c.path, c.unit)
		require.Equal(t, c.treeDepth, n.TreeDepth, "%s in unit %d", c.path, c.unit)
	}
	n, ok := at[fmt.Sprintf("%d kb/z/m.md", u1)]
	require.True(t, ok)
	require.Equal(t, fmt.Sprintf("%d.2.1", u1), n.Number, "a link's walk children stay in place under it")
	require.Len(t, nodes, 8)
}

// Seek: cursor + unit continues from that unit's first section, forward or
// back, and a later plain resume continues after the page seek served.
// Out-of-range units are a 400 that moves nothing.
//
// Sabotage: ignore unit (serve from the cursor's position), and the seek
// page starts elsewhere.
func TestBook_Seek(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))
	all, _ := readBook(t, h, bookBase+"?path=kb/u/&depth=2&limit=5")

	code, p1, body := getBook(t, h, bookBase+"?path=kb/u/&depth=2&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	c := *p1.Cursor
	start2 := 0
	for _, n := range all {
		if n.Unit == 2 {
			start2 = n.Seq
			break
		}
	}
	code, p, body := getBook(t, h, bookBase+"?cursor="+c+"&unit=2")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, start2, p.SeqFrom, "the seek page starts at unit 2's first section")
	require.Equal(t, 2, p.Facts[0].Unit)
	code, p, body = getBook(t, h, bookBase+"?cursor="+c)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, start2+1, p.SeqFrom, "a resume continues after the seek page")

	code, p, body = getBook(t, h, bookBase+"?cursor="+c+"&unit=1")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 1, p.SeqFrom, "seek goes back too")
	for _, u := range []string{"0", "3", "x", "-1"} {
		code, _, body := getBook(t, h, bookBase+"?cursor="+c+"&unit="+u)
		require.Equal(t, http.StatusBadRequest, code, "unit=%s: %s", u, body)
	}
	code, p, body = getBook(t, h, bookBase+"?cursor="+c)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 2, p.SeqFrom, "the refused seeks moved nothing")
}

// A cursor refuses every argument but unit, and the refusal consumes
// nothing; the cursor belongs to the route and the principal; a new first
// call by the same owner replaces the old book (one active book per
// endpoint + principal).
//
// Sabotage: drop the owner comparison in openBook (u2 is served), or the
// DELETE in CreateBook (the old cursor still pages).
func TestBook_CursorOwnershipRefusalAndReplacement(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	router := newTestRouter(m)
	owner, other := asPrincipal("u1", router), asPrincipal("u2", router)

	code, p1, body := getBook(t, owner, bookBase+"?path=kb/u/&depth=2&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	c := *p1.Cursor
	for _, extra := range []string{"depth=1", "facts=1", "q=x", "limit=2", "path=kb/", "x=1"} {
		code, _, body := getBook(t, owner, bookBase+"?cursor="+c+"&"+extra)
		require.Equal(t, http.StatusBadRequest, code, "%s: %s", extra, body)
	}
	_, _, ghost := getBook(t, owner, bookBase+"?cursor=00000000-0000-4000-8000-000000000000")
	code, _, foreign := getBook(t, other, bookBase+"?cursor="+c)
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, ghost, foreign, "another principal's cursor reads exactly like none")
	// The single-fact pages route is another endpoint.
	code, _, _ = getBook(t, owner, "/repos/alpha/branches/agent:test/facts/kb/u/r1.md/materialize/pages?cursor="+c)
	require.Equal(t, http.StatusNotFound, code)

	code, p2, body := getBook(t, owner, bookBase+"?cursor="+c)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, p1.SeqTo+1, p2.SeqFrom, "the refused calls consumed nothing")

	// Another principal's book does not replace this one; the owner's does.
	code, _, body = getBook(t, other, bookBase+"?path=kb/u/&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	code, _, _ = getBook(t, owner, bookBase+"?cursor="+c)
	require.Equal(t, http.StatusOK, code, "u2's book left u1's alone")
	code, _, body = getBook(t, owner, bookBase+"?path=kb/z/&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	code, _, gone := getBook(t, owner, bookBase+"?cursor="+c)
	require.Equal(t, http.StatusNotFound, code, "a new first call replaced the owner's old book")
	require.Equal(t, ghost, gone)
}

// Paging past the UI's 50: the book holds every result up to Facts (the
// default is all, capped at 500), and its first 50 units are the /search
// list's 50, in order. facts=N keeps the best N.
//
// Sabotage: run the search with the UI's limit (50), and unit 51.. is
// missing.
func TestBook_PagesPastFiftyResults(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	alpha := m.Get("alpha")
	for i := range 60 {
		writeMatFact(t, alpha, "agent/test", fmt.Sprintf("kb/p/f%02d.md", i), fmt.Sprintf("F%02d", i), "body.")
	}
	h := asPrincipal("u1", newTestRouter(m))
	ui := searchPaths(t, h, "path=kb/p/&limit=50")
	require.Len(t, ui, 50)

	nodes, pages := readBook(t, h, bookBase+"?path=kb/p/&depth=0&limit=5")
	require.Equal(t, 60, pages[0].Units)
	var roots []string
	for _, n := range nodes {
		roots = append(roots, n.Path)
	}
	require.Len(t, roots, 60)
	require.Equal(t, ui, roots[:50], "the first 50 units are the list the UI shows")

	_, few := readBook(t, h, bookBase+"?path=kb/p/&depth=0&limit=5&facts=7")
	require.Equal(t, 7, few[0].Units)
	require.Equal(t, 500, materialize.DefaultBookFacts)
	code, _, _ := getBook(t, h, bookBase+"?path=kb/p/&facts=501")
	require.Equal(t, http.StatusBadRequest, code)
}

// The title and the full-search line are computed on the server.
func TestBook_TitleAndSearchLine(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))
	code, p, body := getBook(t, h, bookBase+"?path=kb/u/&type=observation&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "U", p.Title, "path only: its segments under the ontology root")
	require.Equal(t, "path kb/u/ · type observation · sorted by relevance", p.SearchLine)
	require.Equal(t, materialize.BookModeBook, p.Mode)
	require.Equal(t, materialize.BookInstructions, p.Instructions)
}

// Overload bounds on the REST edge: over the concurrent-build cap is a 503
// with Retry-After and writes nothing.
//
// Sabotage: block on the slot instead of refusing, and the call hangs.
func TestBook_ConcurrentBuildCapIs503(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))
	release := materialize.ReserveBookBuildsForTest(materialize.MaxConcurrentBookBuilds)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, bookBase+"?path=kb/u/", nil))
	release()
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Equal(t, "2", rec.Header().Get("Retry-After"))
	code, _, body := getBook(t, h, bookBase+"?path=kb/u/")
	require.Equal(t, http.StatusOK, code, "a slot freed is a slot: %s", body)
}

// Export: the whole book as one agent document, one "##" per unit, every
// section once, links as in-document anchors that resolve; an attachment
// named like the single export; the cursor's position is left alone.
//
// Sabotage: write a link's target as its own appearance anchor, and the
// "(see …)" anchor resolves to nothing.
func TestBook_ExportIsOneDocumentWithAnchors(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedShared(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))
	code, p1, body := getBook(t, h, bookBase+"?path=kb/u/&depth=2&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	c := *p1.Cursor

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, bookBase+"/export?cursor="+c, nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, `attachment; filename="`+materialize.BookExportFileName(p1.BookEnv, c)+`"`, rec.Header().Get("Content-Disposition"))
	doc := rec.Body.String()
	require.True(t, strings.HasPrefix(doc, "# U\n"), doc)
	require.Equal(t, 2, strings.Count(doc, "\n## "), "one ## per unit:\n%s", doc)
	for _, b := range []string{"r1 body.", "r2 body.", "s body.", "d body.", "m body."} {
		require.Equal(t, 1, strings.Count(doc, b), "%q written once", b)
	}
	// Every in-document link target exists.
	for _, part := range strings.Split(doc, "](#")[1:] {
		id := part[:strings.IndexByte(part, ')')]
		require.Contains(t, doc, `<a id="`+id+`"></a>`, "anchor %s resolves", id)
	}
	require.Contains(t, doc, "(see [")

	code, p2, body := getBook(t, h, bookBase+"?cursor="+c)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 2, p2.SeqFrom, "the export left the reading position alone")

	code, _, _ = getMaterialize(t, asPrincipal("u2", newTestRouter(m)), bookBase+"/export?cursor="+c)
	require.Equal(t, http.StatusNotFound, code, "the export is owned like the pages")
}
