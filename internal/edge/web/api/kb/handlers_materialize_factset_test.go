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
// nothing; the cursor belongs to the route and the principal; a new first
// call by the same owner replaces the old factset (one active factset per
// endpoint + principal).
//
// Sabotage: drop the owner comparison in openFactSet (u2 is served), or the
// DELETE in CreateFactSet (the old cursor still pages).
//
// A position past the extent is a 400, not a 404: the cursor is the
// caller's, the position is wrong.
func TestFactSet_CursorOwnershipRefusalAndReplacement(t *testing.T) {
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
	// The single-fact pages route is another endpoint.
	code, _, _ = getFactSet(t, owner, "/repos/alpha/branches/agent:test/facts/kb/u/r1.md/materialize/pages?cursor="+c)
	require.Equal(t, http.StatusNotFound, code)

	code, p2, body := getFactSet(t, owner, factsetBase+"?cursor="+c)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "1.1", p2.Facts[0].Number, "the refused calls changed nothing")

	// Another principal's factset does not replace this one; the owner's does.
	code, _, body = getFactSet(t, other, factsetBase+"?path=kb/u/&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	code, _, _ = getFactSet(t, owner, factsetBase+"?cursor="+c)
	require.Equal(t, http.StatusOK, code, "u2's factset left u1's alone")
	code, _, body = getFactSet(t, owner, factsetBase+"?path=kb/z/&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	code, _, gone := getFactSet(t, owner, factsetBase+"?cursor="+c)
	require.Equal(t, http.StatusNotFound, code, "a new first call replaced the owner's old factset")
	require.Equal(t, ghost, gone)

	code, p3, body := getFactSet(t, owner, factsetBase+"?path=kb/u/&depth=2&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	id := (*p3.Cursor)[:strings.IndexByte(*p3.Cursor, '.')]
	code, _, body = getFactSet(t, owner, factsetBase+"?cursor="+id+".3.0")
	require.Equal(t, http.StatusBadRequest, code, "unit 3 of 2: past the extent: %s", body)
}

// Paging past the UI's 50: the factset holds every result up to Facts (the
// default is all, capped at 500), and its first 50 units are the /search
// list's 50, in order. facts=N keeps the best N.
//
// Sabotage: run the search with the UI's limit (50), and unit 51.. is
// missing.
func TestFactSet_PagesPastFiftyResults(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	alpha := m.Get("alpha")
	for i := range 60 {
		writeMatFact(t, alpha, "agent/test", fmt.Sprintf("kb/p/f%02d.md", i), fmt.Sprintf("F%02d", i), "body.")
	}
	h := asPrincipal("u1", newTestRouter(m))
	ui := searchPaths(t, h, "path=kb/p/&limit=50")
	require.Len(t, ui, 50)

	nodes, pages := readFactSet(t, h, factsetBase+"?path=kb/p/&depth=0&limit=5")
	require.Equal(t, 60, pages[0].Units)
	var roots []string
	for _, n := range nodes {
		roots = append(roots, n.Path)
	}
	require.Len(t, roots, 60)
	require.Equal(t, ui, roots[:50], "the first 50 units are the list the UI shows")

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
// caller does not reach is the expiry 404.
//
// Sabotage: freeze tips without asking mayRead, and the narrow caller's ref
// into beta is followed.
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
	code, _, body = getFactSet(t, narrow, factsetBase+"?cursor="+url.QueryEscape(*pw.Cursor))
	require.Equal(t, http.StatusNotFound, code, "a narrower credential resuming a wider caller's cursor: %s", body)
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
