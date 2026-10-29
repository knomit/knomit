package kb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/foundation/auth"
)

// asPrincipal records what an auth edge records for an OS user: the
// principal, and a reach of every repo. The pages cursor is owned by the
// route URL plus this principal.
func asPrincipal(id string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := auth.WithPrincipal(r.Context(), auth.Principal{Kind: auth.KindHost, ID: id, Via: auth.ViaSocket})
		h.ServeHTTP(w, r.WithContext(auth.WithRepoReach(ctx, auth.AllRepos)))
	})
}

func getPage(t *testing.T, h http.Handler, u string) (int, materialize.Page, string) {
	t.Helper()
	code, ctype, body := getMaterialize(t, h, u)
	var p materialize.Page
	if code == http.StatusOK {
		require.Equal(t, "application/json", ctype)
		require.NoError(t, json.Unmarshal([]byte(body), &p), body)
	}
	return code, p, body
}

// pageAll pages from first to the end and returns every page.
func pageAll(t *testing.T, h http.Handler, first string) []materialize.Page {
	t.Helper()
	base, _, _ := strings.Cut(first, "?")
	var pages []materialize.Page
	u := first
	for {
		code, p, body := getPage(t, h, u)
		require.Equal(t, http.StatusOK, code, body)
		pages = append(pages, p)
		if !p.HasMore {
			require.Nil(t, p.Cursor, "the last page carries no cursor")
			return pages
		}
		require.NotNil(t, p.Cursor)
		u = base + "?cursor=" + url.QueryEscape(*p.Cursor)
		require.Less(t, len(pages), 100, "paging does not end")
	}
}

// seedChain writes r -> a1..a7 (each a leaf) on alpha, so the walk has eight
// facts in refs order, more than one 3-fact page.
func seedChain(t *testing.T, alpha *repos.RepoInstance) []string {
	t.Helper()
	const branch = "agent/test"
	var refs []string
	for _, n := range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7"} {
		p := "kb/a/" + n + ".md"
		writeMatFact(t, alpha, branch, p, strings.ToUpper(n), n+" body.")
		refs = append(refs, p)
	}
	writeMatFact(t, alpha, branch, "kb/a/r.md", "R", "R body.", refs...)
	return append([]string{"kb/a/r.md"}, refs...)
}

// Every fact comes back exactly once, in the planned (MCP) order, with its
// full body; each page's n_from..n_to is contiguous and its nodes are whole.
//
// Sabotage: drop the re-enqueue of nodes that did not fit, or start the
// snapshot at doc.Nodes[used+1:], and a fact goes missing.
func TestMaterializePages_EveryFactOnceInPlanOrder(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	alpha := m.Get("alpha")
	want := seedChain(t, alpha)
	h := asPrincipal("u1", newTestRouter(m))

	pages := pageAll(t, h, "/repos/alpha/branches/agent:test/facts/kb/a/r.md/materialize/pages")
	require.Greater(t, len(pages), 2, "eight facts at three a page are several pages")

	doc, err := materialize.Plan(context.Background(), materialize.Request{
		Binding: repos.NewBindingOfRepo(alpha, "agent/test"), Manager: m, File: "kb/a/r.md",
		Options: materialize.DefaultOptions(),
	})
	require.NoError(t, err)
	var got []string
	next := 1
	for i, p := range pages {
		require.Equal(t, materialize.AudienceHuman, p.Audience)
		require.Equal(t, len(want), p.Total, "total is exact on every page")
		require.Equal(t, i == 0, p.Instructions != "", "instructions on page 1 only")
		require.Equal(t, materialize.InstructionsVersion, p.InstructionsVersion)
		require.Equal(t, next, p.NFrom, "page %d starts where the last ended", i+1)
		for _, n := range p.Facts {
			require.Equal(t, next, n.N)
			require.Equal(t, doc.Nodes[n.N-1].Body, n.Body, "full body of %s", n.Path)
			got = append(got, n.Path)
			next++
		}
		require.Equal(t, next-1, p.NTo)
	}
	require.Equal(t, want, got, "every fact once, in refs order")
}

// A page never splits a fact and stays within the budget; a fact larger than
// the budget is sent alone, whole.
//
// Sabotage: break out of the measuring loop one node later (used = k before
// the size check), and the page over the cap carries two facts.
func TestMaterializePages_WholeFactsUnderTheBudget(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	alpha := m.Get("alpha")
	const branch = "agent/test"
	big := strings.Repeat("word ", 10<<10/5)  // ~10 KiB
	huge := strings.Repeat("huge ", 40<<10/5) // ~40 KiB, over the cap alone
	var refs []string
	for i, body := range []string{big, big, huge, big, big} {
		p := "kb/a/f" + string(rune('0'+i)) + ".md"
		writeMatFact(t, alpha, branch, p, "F"+string(rune('0'+i)), body)
		refs = append(refs, p)
	}
	writeMatFact(t, alpha, branch, "kb/a/r.md", "R", "R body.", refs...)
	h := asPrincipal("u1", newTestRouter(m))

	base := "/repos/alpha/branches/agent:test/facts/kb/a/r.md/materialize/pages"
	first := base
	var seen int
	for u := first; ; {
		code, p, raw := getPage(t, h, u)
		require.Equal(t, http.StatusOK, code, raw)
		for _, n := range p.Facts {
			if strings.HasPrefix(n.Body, huge[:100]) {
				require.Len(t, p.Facts, 1, "the oversize fact is sent alone")
				require.Equal(t, strings.TrimSpace(huge), n.Body, "and whole")
				require.Greater(t, len(raw), materialize.PageCap)
			}
		}
		if len(p.Facts) > 1 {
			require.LessOrEqual(t, len(raw), materialize.PageCap, "a multi-fact page stays within the budget")
		}
		seen += len(p.Facts)
		if !p.HasMore {
			break
		}
		u = base + "?cursor=" + *p.Cursor
	}
	require.Equal(t, 6, seen)
}

// A cursor refuses every other argument, and the refusal consumes nothing.
//
// Sabotage: drop the "any other parameter" loop, and depth=1 with the cursor
// is served (200).
func TestMaterializePages_CursorRefusesOtherArguments(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedChain(t, m.Get("alpha"))
	h := asPrincipal("u1", newTestRouter(m))
	base := "/repos/alpha/branches/agent:test/facts/kb/a/r.md/materialize/pages"

	code, p1, body := getPage(t, h, base)
	require.Equal(t, http.StatusOK, code, body)
	c := *p1.Cursor
	for _, extra := range []string{"depth=1", "max_nodes=5", "unpin=true", "cross_kb=true", "limit=2", "x=1"} {
		code, _, body := getPage(t, h, base+"?cursor="+c+"&"+extra)
		require.Equal(t, http.StatusBadRequest, code, "%s: %s", extra, body)
	}
	code, _, _ = getPage(t, h, base+"?cursor=")
	require.Equal(t, http.StatusBadRequest, code, "an empty cursor is not a first call")

	code, p2, body := getPage(t, h, base+"?cursor="+c)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, p1.NTo+1, p2.NFrom, "the refused calls consumed nothing")
}

// A cursor belongs to the route URL and the principal. Another principal, or
// the same principal on another fact's or another commit's URL, gets exactly
// the answer of a cursor that never existed, and consumes nothing.
//
// Sabotage: drop `session.Owner != sc.Owner` from NextPage, and u2 is served
// page 2.
func TestMaterializePages_CursorIsOwnedByURLAndPrincipal(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	seedChain(t, m.Get("alpha"))
	router := newTestRouter(m)
	owner, other := asPrincipal("u1", router), asPrincipal("u2", router)
	base := "/repos/alpha/branches/agent:test/facts/kb/a/r.md/materialize/pages"

	code, p1, body := getPage(t, owner, base)
	require.Equal(t, http.StatusOK, code, body)
	c := *p1.Cursor

	_, _, ghost := getPage(t, owner, base+"?cursor=00000000-0000-4000-8000-000000000000")
	code, _, foreign := getPage(t, other, base+"?cursor="+c)
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, ghost, foreign, "another principal's cursor reads exactly like none")

	otherFact := "/repos/alpha/branches/agent:test/facts/kb/a/a1.md/materialize/pages"
	_, _, ghostThere := getPage(t, owner, otherFact+"?cursor=00000000-0000-4000-8000-000000000000")
	code, _, elsewhere := getPage(t, owner, otherFact+"?cursor="+c)
	require.Equal(t, http.StatusNotFound, code)
	require.Equal(t, ghostThere, elsewhere, "another URL's cursor reads exactly like none")

	code, p2, body := getPage(t, owner, base+"?cursor="+c)
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, p1.NTo+1, p2.NFrom, "the foreign calls consumed nothing")
}

// cross_kb: the foreign target is read at its repo's tip as of the FIRST
// call. A commit to that repo between pages changes nothing in later pages.
//
// Sabotage: make ReadBodies read a foreign node at its repo's current tip
// instead of the frozen commit, and page 2 carries "X second.".
func TestMaterializePages_CrossKBIsFrozenAtTheFirstCall(t *testing.T) {
	m := newMatStoreManager(t, "alpha", "beta")
	alpha, beta := m.Get("alpha"), m.Get("beta")
	const branch = "agent/test"
	beta12 := fact.ID12(beta.ID())
	xWire := "kb://" + beta12 + "/kb/b/x.md"
	x1 := writeMatFact(t, beta, branch, "kb/b/x.md", "X", "X first.")
	var refs []string
	for _, n := range []string{"a1", "a2", "a3"} {
		p := "kb/a/" + n + ".md"
		writeMatFact(t, alpha, branch, p, strings.ToUpper(n), n+" body.")
		refs = append(refs, p)
	}
	writeMatFact(t, alpha, branch, "kb/a/r.md", "R", "R body.", append(refs, xWire)...)
	h := asPrincipal("u1", newTestRouter(m))
	base := "/repos/alpha/branches/agent:test/facts/kb/a/r.md/materialize/pages"

	code, p1, body := getPage(t, h, base+"?cross_kb=true")
	require.Equal(t, http.StatusOK, code, body)
	require.True(t, p1.CrossKB)
	require.NotContains(t, body, xWire+`","repo"`, "X is on a later page")

	writeMatFact(t, beta, branch, "kb/b/x.md", "X", "X second.")

	var x *materialize.Node
	for u := base + "?cursor=" + *p1.Cursor; ; {
		code, p, body := getPage(t, h, u)
		require.Equal(t, http.StatusOK, code, body)
		for i := range p.Facts {
			if p.Facts[i].Path == xWire {
				x = &p.Facts[i]
			}
		}
		if !p.HasMore {
			break
		}
		u = base + "?cursor=" + *p.Cursor
	}
	require.NotNil(t, x, "X was written")
	require.Equal(t, "X first.", x.Body, "the foreign version is the one frozen on the first call")
	require.Equal(t, x1, x.Commit)
	require.Contains(t, x.Marks, materialize.MarkLatestOtherKB)
}

// Export: the agent document is an attachment named
// <slug of the title, at most 48 characters>-<id8>.md, on every route.
//
// Sabotage: drop the Content-Disposition header, or use the fact path for
// the name.
func TestMaterializeRoute_ExportIsAnAttachment(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	alpha := m.Get("alpha")
	c := writeMatFact(t, alpha, "agent/test", "kb/decisions/materialize/rulings/16ccefe6.md",
		"materialize rulings (2026-10-28): pinned by default, unpin silently drops gone targets", "Body.")
	h := asLocalCaller(newTestRouter(m))
	const want = `attachment; filename="materialize-rulings-2026-10-28-pinned-by-default-16ccefe6.md"`
	for _, u := range []string{
		"/repos/alpha/branches/agent:test/facts/kb/decisions/materialize/rulings/16ccefe6.md/materialize",
		"/repos/alpha/branches/agent:test/commits/" + c + "/facts/kb/decisions/materialize/rulings/16ccefe6.md/materialize",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, u, nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, want, rec.Header().Get("Content-Disposition"), u)
		require.Equal(t, "text/markdown; charset=utf-8", rec.Header().Get("Content-Type"))
	}
	// The pages route is data, never a download.
	rec := httptest.NewRecorder()
	asPrincipal("u1", newTestRouter(m)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/repos/alpha/branches/agent:test/facts/kb/decisions/materialize/rulings/16ccefe6.md/materialize/pages", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Empty(t, rec.Header().Get("Content-Disposition"))
}

// The pages route walks at the commit of the commit-anchored URL and through
// a lens's binding, like the document route.
func TestMaterializePages_CommitAndLensRoutes(t *testing.T) {
	m := newMatStoreManager(t, "alpha")
	alpha := m.Get("alpha")
	c1 := writeMatFact(t, alpha, "agent/test", "kb/a/r.md", "R", "R first.")
	writeMatFact(t, alpha, "agent/test", "kb/a/r.md", "R", "R second.")
	h := asPrincipal("u1", newTestRouter(m))
	code, p, body := getPage(t, h, "/repos/alpha/branches/agent:test/commits/"+c1+"/facts/kb/a/r.md/materialize/pages")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, c1, p.Anchor)
	require.Equal(t, "R first.", p.Facts[0].Body)

	lm := newMatLensManager(t, "w", "r")
	r := lm.Get("r")
	createLens(t, lm, "both", "w", "r")
	r12 := fact.ID12(r.ID())
	writeMatFact(t, r, r.AgentBranch(), "kb/m/x.md", "X", "X body.")
	lh := asPrincipal("u1", newTestRouter(lm))
	code, p, body = getPage(t, lh, "/lenses/both/facts/kb:%2F%2F"+r12+"%2Fkb%2Fm%2Fx.md/materialize/pages")
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, "X body.", p.Facts[0].Body)
	_, _, unmounted := getMaterialize(t, lh, "/lenses/both/facts/kb:%2F%2F0123456789ab%2Fkb%2Fm%2Fq.md/materialize/pages")
	_, _, missing := getMaterialize(t, lh, "/lenses/both/facts/kb:%2F%2F"+r12+"%2Fkb%2Fm%2Fq.md/materialize/pages")
	require.Equal(t, strings.ReplaceAll(unmounted, "0123456789ab", r12), missing, "the lens 404 names nothing about the mounts")
}
