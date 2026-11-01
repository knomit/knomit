package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/materialize"
	"knomit/internal/domain/fact"
	"knomit/internal/foundation/auth"
)

const alphaFactSet = "/api/v1/repos/alpha/branches/agent:test/search/factset"

// factsetPages pages a factset through next (a cursor -> the next raw page) from
// the first raw page, with every cursor id replaced by CURSOR.
func factsetPages(t *testing.T, first string, next func(cursor string) string) []string {
	t.Helper()
	var out []string
	raw := first
	for {
		var p struct {
			Cursor  *string `json:"cursor"`
			HasMore bool    `json:"has_more"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &p), raw)
		cur := ""
		if p.Cursor != nil {
			cur = *p.Cursor
		}
		out = append(out, strings.ReplaceAll(raw, `"`+cur+`"`, `"CURSOR"`))
		if !p.HasMore {
			return out
		}
		raw = next(cur)
		require.Less(t, len(out), 200)
	}
}

// factsetID is the FactSet id of a cursor token.
func factsetID(cursor string) string {
	if i := strings.IndexByte(cursor, '.'); i > 0 {
		return cursor[:i]
	}
	return cursor
}

// The REST factset route and knomit_materialize's factset mode return the SAME
// bytes for the same search, page for page, a seek page included; only the
// cursor id differs (each first call mints a new factset; the cross-surface
// paging of ONE factset is TestFactSet_OneFactSetAcrossRESTAndMCP).
//
// Sabotage: build the MCP search line from the raw arguments instead of
// FactSetSearch, or give MCP a different default page size, and a page differs.
func TestFactSet_RESTIsTheMCPFactSetPage(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	m, alpha, beta := newMaterializeRepos(t)
	f.s.Manager = m
	beta12 := fact.ID12(beta.ID())
	writeObs(t, beta, "kb/b/x.md", "X")
	writeObs(t, alpha, "kb/z/s.md", "S")
	writeObs(t, alpha, "kb/z/t.md", "T", "kb/z/s.md")
	writeObs(t, alpha, "kb/u/r1.md", "R1", "kb/z/s.md", "kb/z/t.md", "kb://"+beta12+"/kb/b/x.md")
	writeObs(t, alpha, "kb/u/r2.md", "R2", "kb/z/t.md", "kb/u/r1.md")
	writeObs(t, alpha, "kb/u/r3.md", "R3", "kb/z/s.md")

	tok := f.mint(t, "laptop", testIssuer+"/api/v1", []string{"read"}, auth.Read)
	h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)

	_, sid := rpcAt(t, h, alphaMCP, "", initBody)
	mcpCall := func(args string) string {
		text, isErr := callToolAt(t, h, alphaMCP, sid, "knomit_materialize", args)
		require.False(t, isErr, text)
		return text
	}
	mcpFirst := mcpCall(`{"search":{"path":"kb/u/","type":["observation"]},"cross_kb":true,"depth":2,"limit":2}`)
	mcpPages := factsetPages(t, mcpFirst, func(c string) string { return mcpCall(`{"cursor":"` + c + `"}`) })

	restCall := func(u string) string {
		code, body := restGet(t, h, u)
		require.Equal(t, http.StatusOK, code, body)
		return body
	}
	restFirst := restCall(alphaFactSet + "?path=kb/u/&type=observation&cross_kb=true&depth=2&limit=2")
	restPages := factsetPages(t, restFirst, func(c string) string { return restCall(alphaFactSet + "?cursor=" + url.QueryEscape(c)) })

	require.Greater(t, len(restPages), 2)
	require.Equal(t, mcpPages, restPages)
	require.Contains(t, restPages[0], `"search_line":"path kb/u/ · type observation · sorted by relevance"`)
	require.Contains(t, strings.Join(restPages, ""), `"latest_other_kb"`, "cross-KB is followed in a unit")

	// The defaults (no limit, no depth, every result) are the same too.
	md := mcpCall(`{"search":{"path":"kb/u/"}}`)
	rd := restCall(alphaFactSet + "?path=kb/u/")
	require.Equal(t, strings.ReplaceAll(md, factsetID(pageCursor(t, md)), "ID"), strings.ReplaceAll(rd, factsetID(pageCursor(t, rd)), "ID"))

	// A seek page is the same on both surfaces too.
	mcpFirst = mcpCall(`{"search":{"path":"kb/u/","type":["observation"]},"cross_kb":true,"depth":2,"limit":2}`)
	restFirst = restCall(alphaFactSet + "?path=kb/u/&type=observation&cross_kb=true&depth=2&limit=2")
	mc := pageCursor(t, mcpFirst)
	rc := pageCursor(t, restFirst)
	// A cursor is "<id>.<unit>.<section>": the ids differ, the positions not.
	ms := strings.ReplaceAll(mcpCall(`{"cursor":"`+mc+`","unit":3}`), factsetID(mc), "ID")
	rs := strings.ReplaceAll(restCall(alphaFactSet+"?cursor="+url.QueryEscape(rc)+"&unit=3"), factsetID(rc), "ID")
	require.Equal(t, ms, rs)
	require.Contains(t, rs, `"unit_from":3`)

	// A factset cursor refuses every other argument on MCP as on REST.
	text, isErr := callToolAt(t, h, alphaMCP, sid, "knomit_materialize", `{"cursor":"`+mc+`","limit":5}`)
	require.True(t, isErr, text)
}

// stripCursorID replaces the factset id of raw's cursor with ID.
func stripCursorID(t *testing.T, raw string) string {
	t.Helper()
	if c := pageCursor(t, raw); c != "" {
		return strings.ReplaceAll(raw, factsetID(c), "ID")
	}
	return raw
}

// mcpSession is one principal's MCP session on endpoint.
type mcpSession struct {
	t        *testing.T
	h        http.Handler
	endpoint string
	sid      string
}

func newMCPSession(t *testing.T, h http.Handler, endpoint string) mcpSession {
	t.Helper()
	_, sid := rpcAt(t, h, endpoint, "", initBody)
	return mcpSession{t: t, h: h, endpoint: endpoint, sid: sid}
}

func (s mcpSession) call(args string) (string, bool) {
	s.t.Helper()
	return callToolAt(s.t, s.h, s.endpoint, s.sid, "knomit_materialize", args)
}

func (s mcpSession) ok(args string) string {
	s.t.Helper()
	text, isErr := s.call(args)
	require.False(s.t, isErr, text)
	return text
}

// ONE factset belongs to its principal, not to the surface or endpoint that
// made it (user ruling 2026-11-01): a factset made over MCP pages, seeks and
// exports over REST, one made over REST pages over MCP, and one made on
// alpha's MCP endpoint pages on the unscoped /api/v1/mcp endpoint bound to
// the same repo and branch. Each continues exactly where the other surface
// left it. Another principal, on either surface, gets the answer an unknown
// id gets, byte for byte.
//
// Sabotage: put the endpoint or route back into the factset owner
// (callerOwner in factsetScope, or the URL path in the REST factsetScope), and
// every cross-surface call is the 404 / expired text.
func TestFactSet_OneFactSetAcrossRESTAndMCP(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	m, alpha, _ := newMaterializeRepos(t)
	f.s.Manager = m
	cs := newClientSessionsStore(t) // knomit_bind mints its handle there
	m.SetClientSessions(cs)
	f.s.ClientSessions = cs
	writeObs(t, alpha, "kb/z/s.md", "S")
	writeObs(t, alpha, "kb/u/r1.md", "R1", "kb/z/s.md")
	writeObs(t, alpha, "kb/u/r2.md", "R2", "kb/u/r1.md")
	writeObs(t, alpha, "kb/u/r3.md", "R3", "kb/z/s.md")

	handler := func(subject string) http.Handler {
		tok := f.mint(t, subject, testIssuer+"/api/v1", []string{"read"}, auth.Read)
		return withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
	}
	h, hOther := handler("laptop"), handler("desktop")
	mine := newMCPSession(t, h, alphaMCP)
	theirs := newMCPSession(t, hOther, alphaMCP)
	restOK := func(h http.Handler, u string) string {
		code, body := restGet(t, h, u)
		require.Equal(t, http.StatusOK, code, "%s: %s", u, body)
		return body
	}

	// The reference: a factset paged on REST alone.
	const search = `?path=kb/u/&depth=1&limit=1`
	ref := factsetPages(t, restOK(h, alphaFactSet+search), func(c string) string {
		return restOK(h, alphaFactSet+"?cursor="+url.QueryEscape(c))
	})
	require.Greater(t, len(ref), 3)

	// Made over MCP, paged over REST from page 2 on.
	first := mine.ok(`{"search":{"path":"kb/u/"},"depth":1,"limit":1}`)
	got := factsetPages(t, first, func(c string) string { return restOK(h, alphaFactSet+"?cursor="+url.QueryEscape(c)) })
	require.Equal(t, ref, got, "an MCP factset pages over REST, page for page")
	mc := pageCursor(t, first)
	seek := restOK(h, alphaFactSet+"?cursor="+url.QueryEscape(mc)+"&unit=2")
	require.Contains(t, seek, `"unit_from":2`, "and seeks over REST")
	code, export := restGet(t, h, alphaFactSet+"/export?cursor="+url.QueryEscape(mc))
	require.Equal(t, http.StatusOK, code, export)
	require.Equal(t, 3, strings.Count(export, "\n## "), "the export of an MCP factset over REST holds every unit: %s", export)

	// Made over REST, paged over MCP.
	rfirst := restOK(h, alphaFactSet+search)
	got = factsetPages(t, rfirst, func(c string) string { return mine.ok(`{"cursor":"` + c + `"}`) })
	require.Equal(t, ref, got, "a REST factset pages over MCP, page for page")

	// Made on alpha's endpoint, paged on the unscoped endpoint bound to alpha.
	unscoped := newMCPSession(t, h, "/api/v1/mcp")
	bound, isErr := callToolAt(t, h, "/api/v1/mcp", unscoped.sid, "knomit_bind", `{"repo":"alpha"}`)
	require.False(t, isErr, bound)
	var bind struct {
		Binding string `json:"binding"`
	}
	require.NoError(t, json.NewDecoder(strings.NewReader(bound)).Decode(&bind), bound)
	require.NotEmpty(t, bind.Binding)
	first = mine.ok(`{"search":{"path":"kb/u/"},"depth":1,"limit":1}`)
	got = factsetPages(t, first, func(c string) string {
		return unscoped.ok(`{"cursor":"` + c + `","binding":"` + bind.Binding + `"}`)
	})
	require.Equal(t, ref, got, "a factset made on one MCP endpoint pages on another with the same binding")

	// Another principal: the unknown id's answer, on both surfaces.
	const ghost = "00000000-0000-4000-8000-000000000000.1.0"
	code, ghostREST := restGet(t, hOther, alphaFactSet+"?cursor="+ghost)
	require.Equal(t, http.StatusNotFound, code)
	code, foreignREST := restGet(t, hOther, alphaFactSet+"?cursor="+url.QueryEscape(mc))
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, ghostREST, foreignREST, "another principal's MCP factset reads like none over REST")
	code, foreignExport := restGet(t, hOther, alphaFactSet+"/export?cursor="+url.QueryEscape(mc))
	require.Equal(t, http.StatusNotFound, code, foreignExport)
	ghostMCP, gErr := theirs.call(`{"cursor":"` + ghost + `"}`)
	foreignMCP, fErr := theirs.call(`{"cursor":"` + pageCursor(t, rfirst) + `"}`)
	require.True(t, gErr, ghostMCP)
	require.True(t, fErr, foreignMCP)
	require.Equal(t, ghostMCP, foreignMCP, "another principal's REST factset reads like none over MCP")

	// The owner is still served on both surfaces: nothing was consumed.
	require.Equal(t, stripCursorID(t, restOK(h, alphaFactSet+"?cursor="+url.QueryEscape(mc))),
		stripCursorID(t, mine.ok(`{"cursor":"`+mc+`"}`)))
}

// restDelete sends DELETE u through h.
func restDelete(t *testing.T, h http.Handler, u string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, u, nil))
	b, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	return rec.Code, string(b)
}

// Closing: knomit_materialize's `close: true` with a cursor closes its
// factset, and so does REST DELETE …/search/factset?cursor=, each for the
// other surface too (the factset is the principal's). A closed factset reads
// like none on both. Another principal's close, on either surface, answers
// exactly what closing an unknown id answers, and the factset still pages.
// The close needs only read: the tokens here hold read alone, and REST DELETE
// is otherwise a write behind the write gate.
//
// Sabotage: drop the owner from the close (another principal closes it), or
// gate the factset DELETE as a write (the read token's DELETE is a 403).
func TestFactSet_CloseOverMCPAndREST(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	m, alpha, _ := newMaterializeRepos(t)
	f.s.Manager = m
	writeObs(t, alpha, "kb/u/r1.md", "R1")
	writeObs(t, alpha, "kb/u/r2.md", "R2")
	handler := func(subject string) http.Handler {
		tok := f.mint(t, subject, testIssuer+"/api/v1", []string{"read"}, auth.Read)
		return withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
	}
	h, hOther := handler("laptop"), handler("desktop")
	mine, theirs := newMCPSession(t, h, alphaMCP), newMCPSession(t, hOther, alphaMCP)
	const ghost = "00000000-0000-4000-8000-000000000000.1.0"
	open := func() string { return pageCursor(t, mine.ok(`{"search":{"path":"kb/u/"},"limit":1}`)) }

	// Another principal's close: the unknown id's answer, and nothing closed.
	a := open()
	ghostClose, gErr := theirs.call(`{"close":true,"cursor":"` + ghost + `"}`)
	foreignClose, fErr := theirs.call(`{"close":true,"cursor":"` + a + `"}`)
	require.True(t, gErr, ghostClose)
	require.True(t, fErr, foreignClose)
	require.Equal(t, ghostClose, foreignClose)
	code, ghostDel := restDelete(t, hOther, alphaFactSet+"?cursor="+ghost)
	require.Equal(t, http.StatusNotFound, code, ghostDel)
	code, foreignDel := restDelete(t, hOther, alphaFactSet+"?cursor="+url.QueryEscape(a))
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, ghostDel, foreignDel)
	mine.ok(`{"cursor":"` + a + `"}`)

	// close takes only cursor and binding, and close must be true.
	for _, bad := range []string{`{"close":true}`, `{"close":false,"cursor":"` + a + `"}`, `{"close":true,"cursor":"` + a + `","unit":1}`} {
		text, isErr := mine.call(bad)
		require.True(t, isErr, "%s: %s", bad, text)
	}
	mine.ok(`{"cursor":"` + a + `"}`)

	// Closed over MCP: gone on MCP and on REST.
	ghostPage, _ := mine.call(`{"cursor":"` + ghost + `"}`)
	closed := mine.ok(`{"close":true,"cursor":"` + a + `"}`)
	require.JSONEq(t, `{"closed":true}`, closed)
	page, isErr := mine.call(`{"cursor":"` + a + `"}`)
	require.True(t, isErr, page)
	require.Equal(t, ghostPage, page, "a closed factset reads like none over MCP")
	code, body := restGet(t, h, alphaFactSet+"?cursor="+url.QueryEscape(a))
	require.Equal(t, http.StatusNotFound, code, body)
	again, isErr := mine.call(`{"close":true,"cursor":"` + a + `"}`)
	require.True(t, isErr, again)
	require.Equal(t, ghostClose, again, "a second close reads like closing none")

	// Closed over REST (with a read-only token): gone on REST and on MCP.
	b := open()
	code, body = restDelete(t, h, alphaFactSet+"?cursor="+url.QueryEscape(b))
	require.Equal(t, http.StatusNoContent, code, body)
	code, _ = restGet(t, h, alphaFactSet+"?cursor="+url.QueryEscape(b))
	require.Equal(t, http.StatusNotFound, code)
	page, isErr = mine.call(`{"cursor":"` + b + `"}`)
	require.True(t, isErr, page)
	require.Equal(t, ghostPage, page, "a factset closed over REST reads like none over MCP")
}

// N1 (review of PR #692): on MCP, another owner's live factset cursor and a
// cursor that does not exist are refused with the SAME text, whatever
// argument is added (fact d870cc94: an unauthorized cursor reads as
// expired), so the refusal is no existence oracle.
//
// Sabotage: route on IsFactSetCursor ignoring the owner, and `unit` or
// `limit` with the other owner's cursor answers a factset-only text.
func TestFactSet_MCPRefusalsDoNotTellAnotherOwnersCursorFromNone(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	m, alpha, _ := newMaterializeRepos(t)
	f.s.Manager = m
	writeObs(t, alpha, "kb/u/r1.md", "R1")
	writeObs(t, alpha, "kb/u/r2.md", "R2")
	handler := func(subject string) (http.Handler, string) {
		tok := f.mint(t, subject, testIssuer+"/api/v1", []string{"read"}, auth.Read)
		h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
		_, sid := rpcAt(t, h, alphaMCP, "", initBody)
		return h, sid
	}
	hA, sidA := handler("laptop")
	hB, sidB := handler("desktop")
	first, isErr := callToolAt(t, hA, alphaMCP, sidA, "knomit_materialize", `{"search":{"path":"kb/u/"},"limit":1}`)
	require.False(t, isErr, first)
	mine := pageCursor(t, first)
	const ghost = "00000000-0000-4000-8000-000000000000"
	for _, extra := range []string{``, `,"unit":1`, `,"limit":2`, `,"depth":1`, `,"unit":1,"limit":2`} {
		other, otherErr := callToolAt(t, hB, alphaMCP, sidB, "knomit_materialize", `{"cursor":"`+mine+`"`+extra+`}`)
		none, noneErr := callToolAt(t, hB, alphaMCP, sidB, "knomit_materialize", `{"cursor":"`+ghost+`"`+extra+`}`)
		require.True(t, otherErr, other)
		require.True(t, noneErr, none)
		require.Equal(t, none, other, "another owner's cursor reads like none, with %q", extra)
	}
	// The owner is still served (nothing was consumed).
	text, isErr := callToolAt(t, hA, alphaMCP, sidA, "knomit_materialize", `{"cursor":"`+mine+`"}`)
	require.False(t, isErr, text)
}

// R10 (review of PR #692): an export that fails after its first bytes
// aborts the connection through the real router (chi's Recoverer and
// reportPanic pass http.ErrAbortHandler through), so the client's read
// fails: never a short file that ends cleanly.
//
// Sabotage: return instead of panic(http.ErrAbortHandler) on a mid-stream
// failure, and the client reads a short body with a clean EOF.
func TestFactSet_ExportFailingMidStreamAbortsTheDownload(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	m, alpha, _ := newMaterializeRepos(t)
	f.s.Manager = m
	for i := range 60 {
		writeObs(t, alpha, fmt.Sprintf("kb/e/f%02d.md", i), fmt.Sprintf("F%02d", i))
	}
	tok := f.mint(t, "laptop", testIssuer+"/api/v1", []string{"read"}, auth.Read)
	h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
	code, body := restGet(t, h, alphaFactSet+"?path=kb/e/&depth=0&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	cur := pageCursor(t, body)

	srv := httptest.NewServer(h)
	defer srv.Close()
	// get is the download: its status and body, or the error that ended it
	// (the request itself, when the abort comes before the buffered headers
	// are flushed; the body read, after).
	get := func() (int, []byte, error) {
		resp, err := srv.Client().Get(srv.URL + alphaFactSet + "/export?cursor=" + url.QueryEscape(cur))
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		return resp.StatusCode, b, err
	}
	st, whole, err := get()
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, st)
	require.Equal(t, 60, strings.Count(string(whole), "\n## "), "the whole export, before the fault")

	defer materialize.SetFactSetExportFaultForTest(func(int) error { return errors.New("injected mid-stream failure") })()
	st, part, err := get()
	require.Error(t, err, "the download fails (status %d, %d of %d bytes read): never a short file that ends cleanly", st, len(part), len(whole))
}

// Reach: a factset cannot be built on a repo the token does not reach (the
// answer is exactly a missing repo's); a cross-KB ref outside the token's
// reach is kb_unavailable in the unit; and a cursor built by a wide token
// cannot be paged by a narrower token of the same principal when a page
// holds a row from a repo the narrower one does not reach.
//
// Sabotage: drop the per-row reach check in inflateRows, and the confined
// token is served X's page.
func TestFactSet_ReachIsEnforced(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	m, alpha, beta := newMaterializeRepos(t)
	f.s.Manager = m
	beta12 := fact.ID12(beta.ID())
	xWire := "kb://" + beta12 + "/kb/b/x.md"
	writeObs(t, beta, "kb/b/x.md", "X")
	writeObs(t, alpha, "kb/u/r.md", "R", xWire)

	handler := func(resource string) http.Handler {
		tok := f.mint(t, "laptop", resource, []string{"read"}, auth.Read)
		return withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
	}
	confined := handler(testIssuer + "/api/v1/repos/alpha")
	wide := handler(testIssuer + "/api/v1")

	bc, bb := restGet(t, confined, "/api/v1/repos/beta/branches/agent:test/search/factset?path=kb/")
	gc, gb := restGet(t, confined, "/api/v1/repos/ghost/branches/agent:test/search/factset?path=kb/")
	require.NotEqual(t, http.StatusOK, bc, bb)
	require.Equal(t, gc, bc)
	require.Equal(t, strings.ReplaceAll(gb, "/ghost/", "/beta/"), bb, "an unreachable repo answers exactly like a missing one")

	code, body := restGet(t, confined, alphaFactSet+"?path=kb/u/&cross_kb=true&limit=5")
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, body, xWire+`","reason":"kb_unavailable"`)
	require.NotContains(t, body, `"X body."`)

	code, body = restGet(t, wide, alphaFactSet+"?path=kb/u/&cross_kb=true&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	cur := pageCursor(t, body)
	code, body = restGet(t, confined, alphaFactSet+"?cursor="+url.QueryEscape(cur))
	require.Equal(t, http.StatusOK, code, "no resume-time reach check: %s", body)
	require.NotContains(t, body, `"X body."`, "the narrower credential is not served X")
	code, body = restGet(t, wide, alphaFactSet+"?cursor="+url.QueryEscape(cur))
	require.Equal(t, http.StatusOK, code, "and the refusal consumed nothing: %s", body)
	require.Contains(t, body, `"X body."`)

	// A page whose only foreign row is a LINK (no body to read) is refused
	// too: the link's title and anchor come from the unreachable repo.
	writeObs(t, alpha, "kb/v/r1.md", "V1", xWire)
	writeObs(t, alpha, "kb/v/r2.md", "V2", xWire)
	code, body = restGet(t, wide, alphaFactSet+"?path=kb/v/&cross_kb=true&limit=2")
	require.Equal(t, http.StatusOK, code, body)
	cur = pageCursor(t, body)
	code, body = restGet(t, wide, alphaFactSet+"?cursor="+url.QueryEscape(cur)+"&unit=2")
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, body, `"link":`, "unit 2 holds X as a link")
	code, body = restGet(t, confined, alphaFactSet+"?cursor="+url.QueryEscape(cur)+"&unit=2")
	require.Equal(t, http.StatusOK, code, "no resume-time reach check: %s", body)
	require.NotContains(t, body, `"link":`, "X's link is read through the narrower credential: unavailable, not a link")

	// A unit the NARROWER credential walks reads only what it reaches: X is
	// listed as kb_unavailable there (units are walked with the reach of the
	// call that walks them).
	writeObs(t, alpha, "kb/w/r1.md", "W1")
	writeObs(t, alpha, "kb/w/r2.md", "W2", xWire)
	code, body = restGet(t, confined, alphaFactSet+"?path=kb/w/&cross_kb=true&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	cur = pageCursor(t, body)
	var all strings.Builder
	for u := 1; u <= 2; u++ {
		code, body = restGet(t, confined, alphaFactSet+"?cursor="+url.QueryEscape(cur)+"&unit="+strconv.Itoa(u))
		require.Equal(t, http.StatusOK, code, body)
		all.WriteString(body)
	}
	require.Contains(t, all.String(), xWire+`","reason":"kb_unavailable"`)
	require.NotContains(t, all.String(), `"X body."`)
}
