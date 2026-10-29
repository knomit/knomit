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

const alphaBook = "/api/v1/repos/alpha/branches/agent:test/search/book"

// bookPages pages a book through next (a cursor -> the next raw page) from
// the first raw page, with every cursor id replaced by CURSOR.
func bookPages(t *testing.T, first string, next func(cursor string) string) []string {
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

// The REST book route and knomit_materialize's book mode return the SAME
// bytes for the same search, page for page, a seek page included; only the
// cursor id differs (each surface mints its own, owned by its own endpoint).
//
// Sabotage: build the MCP search line from the raw arguments instead of
// BookSearch, or give MCP a different default page size, and a page differs.
func TestBook_RESTIsTheMCPBookPage(t *testing.T) {
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
	mcpPages := bookPages(t, mcpFirst, func(c string) string { return mcpCall(`{"cursor":"` + c + `"}`) })

	restCall := func(u string) string {
		code, body := restGet(t, h, u)
		require.Equal(t, http.StatusOK, code, body)
		return body
	}
	restFirst := restCall(alphaBook + "?path=kb/u/&type=observation&cross_kb=true&depth=2&limit=2")
	restPages := bookPages(t, restFirst, func(c string) string { return restCall(alphaBook + "?cursor=" + url.QueryEscape(c)) })

	require.Greater(t, len(restPages), 2)
	require.Equal(t, mcpPages, restPages)
	require.Contains(t, restPages[0], `"search_line":"path kb/u/ · type observation · sorted by relevance"`)
	require.Contains(t, strings.Join(restPages, ""), `"latest_other_kb"`, "cross-KB is followed in a unit")

	// The defaults (no limit, no depth, every result) are the same too.
	md := mcpCall(`{"search":{"path":"kb/u/"}}`)
	rd := restCall(alphaBook + "?path=kb/u/")
	require.Equal(t, strings.ReplaceAll(md, pageCursor(t, md), "CURSOR"), strings.ReplaceAll(rd, pageCursor(t, rd), "CURSOR"))

	// A seek page is the same on both surfaces too.
	mcpFirst = mcpCall(`{"search":{"path":"kb/u/","type":["observation"]},"cross_kb":true,"depth":2,"limit":2}`)
	restFirst = restCall(alphaBook + "?path=kb/u/&type=observation&cross_kb=true&depth=2&limit=2")
	mc := pageCursor(t, mcpFirst)
	rc := pageCursor(t, restFirst)
	ms := strings.ReplaceAll(mcpCall(`{"cursor":"`+mc+`","unit":3}`), mc, "CURSOR")
	rs := strings.ReplaceAll(restCall(alphaBook+"?cursor="+url.QueryEscape(rc)+"&unit=3"), rc, "CURSOR")
	require.Equal(t, ms, rs)

	// The MCP cursor is not the REST route's, and the reverse.
	code, _ := restGet(t, h, alphaBook+"?cursor="+url.QueryEscape(mc))
	require.Equal(t, http.StatusNotFound, code)
	text, isErr := callToolAt(t, h, alphaMCP, sid, "knomit_materialize", `{"cursor":"`+rc+`"}`)
	require.True(t, isErr, text)
	// A book cursor refuses every other argument on MCP as on REST.
	text, isErr = callToolAt(t, h, alphaMCP, sid, "knomit_materialize", `{"cursor":"`+mc+`","limit":5}`)
	require.True(t, isErr, text)
}

// N1 (review of PR #692): on MCP, another owner's live book cursor and a
// cursor that does not exist are refused with the SAME text, whatever
// argument is added (fact d870cc94: an unauthorized cursor reads as
// expired), so the refusal is no existence oracle.
//
// Sabotage: route on IsBookCursor ignoring the owner, and `unit` or
// `limit` with the other owner's cursor answers a book-only text.
func TestBook_MCPRefusalsDoNotTellAnotherOwnersCursorFromNone(t *testing.T) {
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
func TestBook_ExportFailingMidStreamAbortsTheDownload(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	m, alpha, _ := newMaterializeRepos(t)
	f.s.Manager = m
	for i := range 60 {
		writeObs(t, alpha, fmt.Sprintf("kb/e/f%02d.md", i), fmt.Sprintf("F%02d", i))
	}
	tok := f.mint(t, "laptop", testIssuer+"/api/v1", []string{"read"}, auth.Read)
	h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
	code, body := restGet(t, h, alphaBook+"?path=kb/e/&depth=0&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	cur := pageCursor(t, body)

	srv := httptest.NewServer(h)
	defer srv.Close()
	// get is the download: its status and body, or the error that ended it
	// (the request itself, when the abort comes before the buffered headers
	// are flushed; the body read, after).
	get := func() (int, []byte, error) {
		resp, err := srv.Client().Get(srv.URL + alphaBook + "/export?cursor=" + url.QueryEscape(cur))
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

	defer materialize.SetBookExportFaultForTest(func(int) error { return errors.New("injected mid-stream failure") })()
	st, part, err := get()
	require.Error(t, err, "the download fails (status %d, %d of %d bytes read): never a short file that ends cleanly", st, len(part), len(whole))
}

// Reach: a book cannot be built on a repo the token does not reach (the
// answer is exactly a missing repo's); a cross-KB ref outside the token's
// reach is kb_unavailable in the unit; and a cursor built by a wide token
// cannot be paged by a narrower token of the same principal when a page
// holds a row from a repo the narrower one does not reach.
//
// Sabotage: drop the per-row reach check in inflateRows, and the confined
// token is served X's page.
func TestBook_ReachIsEnforced(t *testing.T) {
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

	bc, bb := restGet(t, confined, "/api/v1/repos/beta/branches/agent:test/search/book?path=kb/")
	gc, gb := restGet(t, confined, "/api/v1/repos/ghost/branches/agent:test/search/book?path=kb/")
	require.NotEqual(t, http.StatusOK, bc, bb)
	require.Equal(t, gc, bc)
	require.Equal(t, strings.ReplaceAll(gb, "/ghost/", "/beta/"), bb, "an unreachable repo answers exactly like a missing one")

	code, body := restGet(t, confined, alphaBook+"?path=kb/u/&cross_kb=true&limit=5")
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, body, xWire+`","reason":"kb_unavailable"`)
	require.NotContains(t, body, `"X body."`)

	code, body = restGet(t, wide, alphaBook+"?path=kb/u/&cross_kb=true&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	cur := pageCursor(t, body)
	code, body = restGet(t, confined, alphaBook+"?cursor="+url.QueryEscape(cur))
	require.Equal(t, http.StatusNotFound, code, "the narrower credential is not served X: %s", body)
	code, body = restGet(t, wide, alphaBook+"?cursor="+url.QueryEscape(cur))
	require.Equal(t, http.StatusOK, code, "and the refusal consumed nothing: %s", body)
	require.Contains(t, body, `"X body."`)

	// A page whose only foreign row is a LINK (no body to read) is refused
	// too: the link's title and anchor come from the unreachable repo.
	writeObs(t, alpha, "kb/v/r1.md", "V1", xWire)
	writeObs(t, alpha, "kb/v/r2.md", "V2", xWire)
	code, body = restGet(t, wide, alphaBook+"?path=kb/v/&cross_kb=true&limit=2")
	require.Equal(t, http.StatusOK, code, body)
	cur = pageCursor(t, body)
	code, body = restGet(t, wide, alphaBook+"?cursor="+url.QueryEscape(cur)+"&unit=2")
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, body, `"link":`, "unit 2 holds X as a link")
	code, body = restGet(t, confined, alphaBook+"?cursor="+url.QueryEscape(cur)+"&unit=2")
	require.Equal(t, http.StatusNotFound, code, "the narrower credential is not served X's link: %s", body)

	// A unit the NARROWER credential walks reads only what it reaches: X is
	// listed as kb_unavailable there (units are walked with the reach of the
	// call that walks them).
	writeObs(t, alpha, "kb/w/r1.md", "W1")
	writeObs(t, alpha, "kb/w/r2.md", "W2", xWire)
	code, body = restGet(t, confined, alphaBook+"?path=kb/w/&cross_kb=true&limit=1")
	require.Equal(t, http.StatusOK, code, body)
	cur = pageCursor(t, body)
	var all strings.Builder
	for u := 1; u <= 2; u++ {
		code, body = restGet(t, confined, alphaBook+"?cursor="+url.QueryEscape(cur)+"&unit="+strconv.Itoa(u))
		require.Equal(t, http.StatusOK, code, body)
		all.WriteString(body)
	}
	require.Contains(t, all.String(), xWire+`","reason":"kb_unavailable"`)
	require.NotContains(t, all.String(), `"X body."`)
}
