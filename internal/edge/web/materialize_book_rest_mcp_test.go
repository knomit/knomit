package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

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

	// A seek page is the same on both surfaces too.
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
}
