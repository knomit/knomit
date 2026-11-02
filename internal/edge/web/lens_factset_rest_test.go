package web

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/oauth"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/auth"
)

// The lens FactSet REST routes (F4) against knomit_materialize on the lens's
// MCP endpoint and against the engine, through the real router and the OAuth
// listener.

const (
	engFactSet = "/api/v1/lenses/eng/search/factset"
	engMCP     = "/api/v1/lenses/eng/mcp"
)

// lensRESTEnv is lens "eng" over real mounts embedding with emb (write first,
// then reads), served by the OAuth listener of a server whose embedder is emb.
type lensRESTEnv struct {
	f   *oauthWebFixture
	m   *repos.Manager
	emb *cosEmbedder
}

func newLensRESTEnv(t *testing.T, write string, reads ...string) lensRESTEnv {
	t.Helper()
	emb := &cosEmbedder{}
	m, _ := newTestLensManagerWith(t, emb, append([]string{write}, reads...)...)
	rs := make([]string, 0, len(reads))
	for _, n := range reads {
		rs = append(rs, `{"repo":"`+n+`"}`)
	}
	createLens(t, m, (&Server{Manager: m}).NewAPIRouter(), `{"name":"eng","write":"`+write+`","reads":[`+strings.Join(rs, ",")+`]}`)
	f := newOAuthWebFixture(t, testIssuer)
	f.s.Manager, f.s.Embedder = m, emb
	return lensRESTEnv{f: f, m: m, emb: emb}
}

// as is the OAuth listener for a read-only token of subject reaching every
// repo.
func (e lensRESTEnv) as(t *testing.T, subject string) http.Handler {
	t.Helper()
	tok := e.f.mint(t, subject, testIssuer+"/api/v1", []string{"read"}, auth.Read)
	return withAuthorization(e.f.s.OAuthHandler(), "Bearer "+tok)
}

// seedThreeMembers writes 8 text-searchable facts over alpha (write), beta and
// gamma, ranked alpha, beta, gamma, alpha, ... by cosine, with local refs in
// every member and a0's ref into beta (another lens repo: not followed).
func seedThreeMembers(t *testing.T, m *repos.Manager) {
	t.Helper()
	beta12 := fact.ID12(m.Get("beta").ID())
	refs := func(rs ...string) func(*fact.Fact) { return func(f *fact.Fact) { f.Refs = rs } }
	writeCosFact(t, m.Get("alpha"), "kb/a/a1.md", "A1", 0.80, nil)
	writeCosFact(t, m.Get("alpha"), "kb/a/a2.md", "A2", 0.70, nil)
	writeCosFact(t, m.Get("alpha"), "kb/a/a0.md", "A0", 0.95, refs("kb/a/a1.md", "kb://"+beta12+"/kb/b/b1.md"))
	writeCosFact(t, m.Get("beta"), "kb/b/b1.md", "B1", 0.79, nil)
	writeCosFact(t, m.Get("beta"), "kb/b/b2.md", "B2", 0.69, refs("kb/b/b1.md"))
	writeCosFact(t, m.Get("beta"), "kb/b/b0.md", "B0", 0.94, refs("kb/b/b1.md"))
	writeCosFact(t, m.Get("gamma"), "kb/g/g0.md", "G0", 0.93, nil)
	writeCosFact(t, m.Get("gamma"), "kb/g/g1.md", "G1", 0.78, refs("kb/g/g0.md"))
}

// lensEngineCtx is the context the engine sees for subject's token on lens
// "eng": its principal, every repo reachable, and the lens binding.
func lensEngineCtx(t *testing.T, m *repos.Manager, subject string) (context.Context, *repos.Binding) {
	t.Helper()
	ctx, err := repos.ResolveLensBinding(auth.WithRepoReach(auth.WithPrincipal(context.Background(), oauth.TokenPrincipal(subject)), auth.AllRepos), m, "eng")
	require.NoError(t, err)
	return ctx, repos.BindingFromContext(ctx)
}

// REST == MCP, byte for byte, on a 3-member lens: the REST lens route and
// knomit_materialize on the lens's MCP endpoint return the same first page,
// cursor pages and seek page for the same search (only the FactSet id in the
// cursor differs: each first call mints one), and the REST export is byte
// for byte the engine's FactSetExport of the same cursor. The pages carry the
// lens-only fields (anchors of all three members, repo_name on unit roots,
// the lens_repo leaf for a0's ref into beta, kb://<member>/ paths). The
// REST first call embeds the query ONCE for the fan-out, as the lens list
// does (positive control: GET /lenses/eng/search makes the same one call).
//
// Sabotage: resolve the lens route's binding as its write repo only (pages
// differ: alpha's 3 units); drop req.Embed from the lens wiring (3 query
// inferences, one per mount, where the lens list makes 1).
func TestLensFactSet_RESTIsTheMCPFactSetAndTheEngineExport(t *testing.T) {
	e := newLensRESTEnv(t, "alpha", "beta", "gamma")
	seedThreeMembers(t, e.m)
	h := e.as(t, "laptop")
	mcp := newMCPSession(t, h, engMCP)
	restOK := func(u string) string {
		t.Helper()
		code, body := restGet(t, h, u)
		require.Equal(t, http.StatusOK, code, "%s: %s", u, body)
		return body
	}

	// Pages.
	mcpFirst := mcp.ok(`{"search":{"text":"x"},"depth":2,"limit":2}`)
	mcpPages := factsetPages(t, mcpFirst, func(c string) string { return mcp.ok(`{"cursor":"` + c + `"}`) })
	e.emb.queries.Store(0)
	restFirst := restOK(engFactSet + "?q=x&depth=2&limit=2")
	require.EqualValues(t, 1, e.emb.queries.Load(), "the REST lens FactSet embeds its query once for the fan-out")
	e.emb.queries.Store(0)
	restOK("/api/v1/lenses/eng/search?q=x")
	require.EqualValues(t, 1, e.emb.queries.Load(), "control: as the lens list does")
	restPages := factsetPages(t, restFirst, func(c string) string { return restOK(engFactSet + "?cursor=" + url.QueryEscape(c)) })
	require.Greater(t, len(restPages), 3)
	require.Equal(t, mcpPages, restPages, "page for page, byte for byte")

	all := strings.Join(restPages, "")
	beta12, gamma12 := fact.ID12(e.m.Get("beta").ID()), fact.ID12(e.m.Get("gamma").ID())
	require.Contains(t, restPages[0], `"units":8`)
	require.Contains(t, restPages[0], `"anchors":{`)
	for _, id := range []string{fact.ID12(e.m.Get("alpha").ID()), beta12, gamma12} {
		require.Contains(t, restPages[0], `"`+id+`":"`, "an anchor for every member")
	}
	require.Contains(t, all, `"reason":"lens_repo"`)
	require.Contains(t, all, `"repo_name":"beta"`)
	require.Contains(t, all, `"repo_name":"gamma"`)
	require.Contains(t, all, `"kb://`+gamma12+`/kb/g/g0.md"`)

	// A seek page.
	mcpFirst = mcp.ok(`{"search":{"text":"x"},"depth":2,"limit":2}`)
	restFirst = restOK(engFactSet + "?q=x&depth=2&limit=2")
	mc, rc := pageCursor(t, mcpFirst), pageCursor(t, restFirst)
	ms := strings.ReplaceAll(mcp.ok(`{"cursor":"`+mc+`","unit":6}`), factsetID(mc), "ID")
	rs := strings.ReplaceAll(restOK(engFactSet+"?cursor="+url.QueryEscape(rc)+"&unit=6"), factsetID(rc), "ID")
	require.Equal(t, ms, rs)
	require.Contains(t, rs, `"unit_from":6`)

	// The export: REST against the engine, for the same cursor.
	code, export := restGet(t, h, engFactSet+"/export?cursor="+url.QueryEscape(rc))
	require.Equal(t, http.StatusOK, code, export)
	require.Equal(t, 8, strings.Count(export, "\n## "), "every unit: %s", export)
	ctx, b := lensEngineCtx(t, e.m, "laptop")
	var engine bytes.Buffer
	require.NoError(t, b.Write().WithRead(func(svc *store.Service) {
		sc := materialize.CursorScope{Sessions: svc.ToolSession(), Branch: b.WriteMountBranch(), Pin: b.PinID(),
			ReadSet: federate.ReadSetFingerprint(b), Owner: materialize.FactSetOwner(ctx)}
		require.NoError(t, materialize.FactSetExport(ctx, sc, b, e.m, nil, rc, func(materialize.FactSetEnv) io.Writer { return &engine }))
	}))
	require.Equal(t, engine.String(), export, "the REST export is the engine's")
	// And the export of the MCP FactSet over REST is the same document.
	code, mexport := restGet(t, h, engFactSet+"/export?cursor="+url.QueryEscape(mc))
	require.Equal(t, http.StatusOK, code, mexport)
	require.Equal(t, export, mexport)
}

// Motif widening: the REST lens FactSet widens a motif term to its merged
// cluster over the lens's read set, exactly as the lens list (and MCP) do.
// beta spells the shape `config-drifts`, which shares alpha's cluster key;
// beta's own exact tier knows nothing of `config-drift`. The FactSet has the
// lens list's 3 results (1 from alpha, 2 from beta), and MCP's 3.
//
// Sabotage: drop req.Clusters from the lens wiring (beta is asked for
// `config-drift` alone: 1 unit).
func TestLensFactSet_RESTWidensMotifsAsTheLensList(t *testing.T) {
	e := newLensRESTEnv(t, "alpha", "beta")
	writeCosFact(t, e.m.Get("alpha"), "kb/a0.md", "alpha 0", 0.9, withMotifs("config-drift"))
	writeCosFact(t, e.m.Get("beta"), "kb/b0.md", "beta 0", 0.8, withMotifs("config-drifts"))
	writeCosFact(t, e.m.Get("beta"), "kb/b1.md", "beta 1", 0.7, withMotifs("config-drifts"))
	h := e.as(t, "laptop")
	const q = "q=drift&motifs=config-drift&motif_match=exact"
	code, body := restGet(t, h, "/api/v1/lenses/eng/search?"+q)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, 3, strings.Count(body, `"path":"`), "the lens list: %s", body)

	code, body = restGet(t, h, engFactSet+"?"+q+"&depth=0")
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, body, `"units":3`, "the lens list's 3 results")
	mcp := newMCPSession(t, h, engMCP)
	mtext := mcp.ok(`{"search":{"text":"drift","motifs":["config-drift"],"motif_match":"exact"},"depth":0}`)
	require.Equal(t, stripCursorID(t, mtext), stripCursorID(t, body), "and MCP's page")
}

// Cross-surface ownership of a LENS FactSet (owner = the principal alone,
// #701): one made on the lens's MCP endpoint pages, seeks and closes over the
// REST lens route for the same principal, page for page; one made over REST
// pages over MCP; another principal gets exactly the unknown cursor's answer
// on both surfaces and closes nothing. The REST close needs only read (the
// tokens hold read alone; DELETE is otherwise behind the write gate). A lens
// cursor on the write repo's REST route, and the repo endpoint's cursor on
// the lens REST route, are the unknown cursor's 404 (the pins differ).
//
// Sabotage: put the endpoint or route back into the FactSet owner (every
// cross-surface call is 404 / expired); drop the lens alternative from
// factsetClosePattern (the read token's DELETE is 403).
func TestLensFactSet_OneFactSetAcrossRESTAndMCP(t *testing.T) {
	e := newLensRESTEnv(t, "alpha", "beta", "gamma")
	seedThreeMembers(t, e.m)
	h, hOther := e.as(t, "laptop"), e.as(t, "desktop")
	mine, theirs := newMCPSession(t, h, engMCP), newMCPSession(t, hOther, engMCP)
	restOK := func(u string) string {
		t.Helper()
		code, body := restGet(t, h, u)
		require.Equal(t, http.StatusOK, code, "%s: %s", u, body)
		return body
	}
	ref := factsetPages(t, restOK(engFactSet+"?q=x&depth=1&limit=3"), func(c string) string {
		return restOK(engFactSet + "?cursor=" + url.QueryEscape(c))
	})
	require.Greater(t, len(ref), 2)

	// MCP-made, REST-paged.
	first := mine.ok(`{"search":{"text":"x"},"depth":1,"limit":3}`)
	got := factsetPages(t, first, func(c string) string { return restOK(engFactSet + "?cursor=" + url.QueryEscape(c)) })
	require.Equal(t, ref, got, "an MCP lens FactSet pages over REST")
	mc := pageCursor(t, first)
	require.Contains(t, restOK(engFactSet+"?cursor="+url.QueryEscape(mc)+"&unit=5"), `"unit_from":5`)

	// REST-made, MCP-paged.
	rfirst := restOK(engFactSet + "?q=x&depth=1&limit=3")
	got = factsetPages(t, rfirst, func(c string) string { return mine.ok(`{"cursor":"` + c + `"}`) })
	require.Equal(t, ref, got, "a REST lens FactSet pages over MCP")
	rc := pageCursor(t, rfirst)

	// Another principal: the unknown cursor's answer, nothing closed.
	const ghost = "00000000-0000-4000-8000-000000000000.1.0"
	code, ghostREST := restGet(t, hOther, engFactSet+"?cursor="+ghost)
	require.Equal(t, http.StatusNotFound, code)
	code, foreign := restGet(t, hOther, engFactSet+"?cursor="+url.QueryEscape(mc))
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, ghostREST, foreign)
	code, ghostDel := restDelete(t, hOther, engFactSet+"?cursor="+ghost)
	require.Equal(t, http.StatusNotFound, code, ghostDel)
	code, foreignDel := restDelete(t, hOther, engFactSet+"?cursor="+url.QueryEscape(mc))
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, ghostDel, foreignDel)
	ghostMCP, gErr := theirs.call(`{"cursor":"` + ghost + `"}`)
	foreignMCP, fErr := theirs.call(`{"cursor":"` + rc + `"}`)
	require.True(t, gErr, ghostMCP)
	require.True(t, fErr, foreignMCP)
	require.Equal(t, ghostMCP, foreignMCP)
	restOK(engFactSet + "?cursor=" + url.QueryEscape(mc) + "&unit=1")

	// Across the lens and its write repo: the unknown cursor's 404.
	alphaFS := "/api/v1/repos/alpha/branches/" + urlBranch(e.m.Get("alpha").AgentBranch()) + "/search/factset"
	alphaEP := "/api/v1/repos/alpha/branches/" + urlBranch(e.m.Get("alpha").AgentBranch()) + "/mcp"
	_, repoGhost := restGet(t, h, alphaFS+"?cursor="+ghost)
	code, body := restGet(t, h, alphaFS+"?cursor="+url.QueryEscape(mc))
	require.Equal(t, http.StatusNotFound, code, body)
	require.Equal(t, repoGhost, body, "the lens cursor on the repo route reads like none")
	repoFirst := newMCPSession(t, h, alphaEP).ok(`{"search":{"text":"x"},"depth":0,"limit":1}`)
	_, lensGhost := restGet(t, h, engFactSet+"?cursor="+ghost)
	code, body = restGet(t, h, engFactSet+"?cursor="+url.QueryEscape(pageCursor(t, repoFirst)))
	require.Equal(t, http.StatusNotFound, code, body)
	require.Equal(t, lensGhost, body, "the repo cursor on the lens route reads like none")

	// The owner closes the MCP-made FactSet over REST with a read token.
	code, body = restDelete(t, h, engFactSet+"?cursor="+url.QueryEscape(mc))
	require.Equal(t, http.StatusNoContent, code, body)
	page, isErr := mine.call(`{"cursor":"` + mc + `"}`)
	require.True(t, isErr, page)
	ownGhost, _ := mine.call(`{"cursor":"` + ghost + `"}`)
	require.Equal(t, ownGhost, page, "closed over REST: gone over MCP")
	code, again := restDelete(t, h, engFactSet+"?cursor="+url.QueryEscape(mc))
	require.Equal(t, http.StatusNotFound, code)
	_, ownGhostDel := restDelete(t, h, engFactSet+"?cursor="+ghost)
	require.Equal(t, ownGhostDel, again)
}

// The lens close is exempt from the read-only gate exactly as the repo close
// is: on a read-only server both DELETEs answer 204 (and the closed cursors
// read as none), while deleting the lens itself is still 403.
//
// Sabotage: drop the lens alternative from factsetClosePattern (the lens
// close is 403 "Read-only instance").
func TestLensFactSet_CloseOnAReadOnlyServer(t *testing.T) {
	emb := &cosEmbedder{}
	m, _ := newTestLensManagerWith(t, emb, "alpha", "beta")
	createLens(t, m, (&Server{Manager: m}).NewAPIRouter(), `{"name":"eng","write":"alpha","reads":[{"repo":"beta"}]}`)
	writeCosFact(t, m.Get("alpha"), "kb/a.md", "A", 0.9, nil)
	writeCosFact(t, m.Get("beta"), "kb/b.md", "B", 0.8, nil)
	h := (&Server{ReadOnly: true, Manager: m, Embedder: emb}).Handler()
	send := func(method, u string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, fromLoopback(httptest.NewRequest(method, u, nil)))
		return rec
	}
	alphaFS := "/api/v1/repos/alpha/branches/" + urlBranch(m.Get("alpha").AgentBranch()) + "/search/factset"
	for _, base := range []string{engFactSet, alphaFS} {
		rec := send(http.MethodGet, base+"?q=x&depth=0&limit=1")
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", base, rec.Body.String())
		cur := url.QueryEscape(pageCursor(t, rec.Body.String()))
		rec = send(http.MethodDelete, base+"?cursor="+cur)
		require.Equal(t, http.StatusNoContent, rec.Code, "%s close on a read-only server: %s", base, rec.Body.String())
		rec = send(http.MethodGet, base+"?cursor="+cur)
		require.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", base, rec.Body.String())
	}
	rec := send(http.MethodDelete, "/api/v1/lenses/eng")
	require.Equal(t, http.StatusForbidden, rec.Code, "control: the gate is on: %s", rec.Body.String())
	rec = send(http.MethodDelete, engFactSet+"/export?cursor=x")
	require.Equal(t, http.StatusForbidden, rec.Code, "only the factset resource itself is exempt: %s", rec.Body.String())
}
