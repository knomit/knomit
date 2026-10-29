package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
	"knomit/internal/foundation/auth"
)

const alphaPages = "/api/v1/repos/alpha/branches/agent:test/facts/kb/a/r.md/materialize/pages"

// restGet is one GET through h: status and body.
func restGet(t *testing.T, h http.Handler, u string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u, nil))
	b, err := io.ReadAll(rec.Body)
	require.NoError(t, err)
	return rec.Code, string(b)
}

// pageCursor reads a page's cursor ("" at the end).
func pageCursor(t *testing.T, raw string) string {
	t.Helper()
	var p struct {
		Cursor *string `json:"cursor"`
	}
	require.NoError(t, json.Unmarshal([]byte(raw), &p), raw)
	if p.Cursor == nil {
		return ""
	}
	return *p.Cursor
}

// The REST pages route and knomit_materialize with audience "human" return
// the SAME bytes for the same input, page for page; only the cursor id
// differs (each surface mints its own, owned by its own endpoint).
//
// Sabotage: rename a Page field's JSON tag on one surface only, give REST a
// different page limit, or let either surface forget the first call's limit
// on a resume, and a page differs.
func TestMaterializePages_RESTIsTheMCPHumanPage(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	m, alpha, beta := newMaterializeRepos(t)
	f.s.Manager = m
	beta12 := fact.ID12(beta.ID())
	writeObs(t, beta, "kb/b/x.md", "X")
	var refs []string
	for _, n := range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "a10"} {
		writeObs(t, alpha, "kb/a/"+n+".md", strings.ToUpper(n))
		refs = append(refs, "kb/a/"+n+".md")
	}
	writeObs(t, alpha, "kb/a/r.md", "R", append(refs, "kb://"+beta12+"/kb/b/x.md", "kb/a/a1.md")...)

	tok := f.mint(t, "laptop", testIssuer+"/api/v1", []string{"read"}, auth.Read)
	h := withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)

	for _, c := range []struct {
		name, mcpLimit, restLimit string
		facts                     []int
	}{
		{"default limit", "", "", []int{3, 3, 3, 3}},
		{"limit 5, frozen for the cursor", `,"limit":5`, "&limit=5", []int{5, 5, 2}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var mcpPages []string
			var sizes []int
			_, sid := rpcAt(t, h, alphaMCP, "", initBody)
			args := `{"file":"kb/a/r.md","audience":"human","cross_kb":true,"depth":3` + c.mcpLimit + `}`
			for {
				text, isErr := callToolAt(t, h, alphaMCP, sid, "knomit_materialize", args)
				require.False(t, isErr, text)
				cur := pageCursor(t, text)
				mcpPages = append(mcpPages, strings.ReplaceAll(text, `"`+cur+`"`, `"CURSOR"`))
				var p struct {
					Facts []json.RawMessage `json:"facts"`
				}
				require.NoError(t, json.Unmarshal([]byte(text), &p))
				sizes = append(sizes, len(p.Facts))
				if cur == "" {
					break
				}
				args = `{"cursor":"` + cur + `"}`
			}
			require.Equal(t, c.facts, sizes, "MCP keeps the first call's limit on every page")

			var restPages []string
			u := alphaPages + "?cross_kb=true&depth=3" + c.restLimit
			for {
				code, body := restGet(t, h, u)
				require.Equal(t, http.StatusOK, code, body)
				cur := pageCursor(t, body)
				restPages = append(restPages, strings.ReplaceAll(body, `"`+cur+`"`, `"CURSOR"`))
				if cur == "" {
					break
				}
				u = alphaPages + "?cursor=" + url.QueryEscape(cur)
			}
			require.Equal(t, mcpPages, restPages)
			require.Contains(t, restPages[len(restPages)-1], `"cursor":null`)
		})
	}
}

// Route level: a token that does not reach a repo gets exactly the answer of
// a repo that does not exist. Cross-KB: a ref into a repo outside the
// token's reach is "not included" exactly like a ref into a repo that does
// not exist (kb_unavailable); a token whose audience covers every repo
// follows it.
//
// Sabotage: pass a MayRead that always says yes in planMaterialize, and the
// confined token reads X.
func TestOAuthListener_MaterializePagesStayInsideTheTokensReach(t *testing.T) {
	f := newOAuthWebFixture(t, testIssuer)
	m, alpha, beta := newMaterializeRepos(t)
	f.s.Manager = m
	beta12 := fact.ID12(beta.ID())
	xWire := "kb://" + beta12 + "/kb/b/x.md"
	zWire := "kb://0123456789ab/kb/z.md"
	writeObs(t, beta, "kb/b/x.md", "X")
	writeObs(t, alpha, "kb/a/r.md", "R", xWire, zWire)

	handler := func(resource string) http.Handler {
		tok := f.mint(t, "laptop-"+resource, resource, []string{"read"}, auth.Read)
		return withAuthorization(f.s.OAuthHandler(), "Bearer "+tok)
	}
	confined := handler(testIssuer + "/api/v1/repos/alpha")

	betaURL := "/api/v1/repos/beta/branches/agent:test/facts/kb/b/x.md/materialize/pages"
	ghostURL := "/api/v1/repos/ghost/branches/agent:test/facts/kb/b/x.md/materialize/pages"
	bc, bb := restGet(t, confined, betaURL)
	gc, gb := restGet(t, confined, ghostURL)
	require.NotEqual(t, http.StatusOK, bc, bb)
	require.Equal(t, gc, bc)
	require.Equal(t, strings.ReplaceAll(gb, "/ghost/", "/beta/"), bb, "an unreachable repo answers exactly like a missing one")

	facts := func(h http.Handler) []map[string]any {
		t.Helper()
		code, body := restGet(t, h, alphaPages+"?cross_kb=true")
		require.Equal(t, http.StatusOK, code, body)
		var p struct {
			Facts []map[string]any `json:"facts"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &p), body)
		return p.Facts
	}
	omitted := func(n map[string]any) []string {
		var out []string
		list, _ := n["not_included"].([]any)
		for _, o := range list {
			om := o.(map[string]any)
			out = append(out, om["path"].(string)+"|"+om["reason"].(string))
		}
		return out
	}
	in := facts(confined)
	require.Len(t, in, 1, "beta is outside the token's reach: X is not read")
	require.Equal(t, []string{xWire + "|kb_unavailable", zWire + "|kb_unavailable"}, omitted(in[0]))

	open := facts(handler(testIssuer + "/api/v1"))
	require.Len(t, open, 2, "a token for the whole API reaches beta")
	require.Equal(t, "X", open[1]["title"])
}
