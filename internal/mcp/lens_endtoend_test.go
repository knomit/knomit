package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// This file is the ONE end-to-end federation test that goes through the REAL
// HTTP wiring: repos.LensMiddleware resolving a lens PERSISTED via
// Manager.CreateLens, building the Binding that the MCP handlers receive.
// Every other federation test (query_federation_test.go /
// explain_federation_test.go) injects a test-constructed Binding directly;
// here the Binding is minted by the middleware from control-plane state.
//
// Placement: internal/mcp (not internal/repos). The MCP tool handlers live in
// this package, and internal/repos cannot import internal/mcp (mcp already
// imports repos), so a test that drives the handlers through LensMiddleware can
// only live here. The Manager, LensMiddleware, and CreateLens are all exported,
// so the full control-plane path is exercised from this side.

// e2eHandler is the shape of every MCP tool handler.
type e2eHandler = func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)

// newLensE2E stands up a started Manager with two independent repos (alpha as
// the write repo, beta as a foreign read mount) and persists a lens over them
// via Manager.CreateLens — exercising replica/branch validation in the wiring.
// It returns the manager, the two repos, and seeding contexts bound to each.
// The repos are built with the same construction as the proven federation
// fixtures (CodeOntology, agent/test branch, no live embedder) and injected via
// Manager.Set, so seeding through LearnHandler behaves identically to the rest
// of the federation suite while the lens still resolves through real state.
func newLensE2E(t *testing.T) (m *repos.Manager, repoA, repoB *repos.RepoInstance, ctxA, ctxB context.Context, lens string) {
	t.Helper()

	dir := t.TempDir()
	m = repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: dir},
		AgentBranch: "agent/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	repoA = newE2ERepo(t, "alpha")
	repoB = newE2ERepo(t, "beta")
	// Distinct root-commit IDs are what make this a valid (non-replica) lens.
	require.NotEqual(t, repoA.ID(), repoB.ID(), "fresh repos must have distinct IDs")
	m.Set("alpha", repoA)
	m.Set("beta", repoB)

	stored, err := m.CreateLens(context.Background(), repos.Lens{
		Name:  "eng",
		Write: "alpha",
		Reads: []repos.LensRead{{Repo: "beta"}},
	})
	require.NoError(t, err)
	require.Equal(t, "eng", stored.Name)

	return m, repoA, repoB, repos.WithRepoInstance(context.Background(), repoA),
		repos.WithRepoInstance(context.Background(), repoB), "eng"
}

// newE2ERepo builds a RepoInstance backed by an on-disk store initialized on
// the agent/test branch, wired with the real CodeOntology. Mirrors
// newLearnTestRepo but takes a name so two distinct mounts can coexist.
func newE2ERepo(t *testing.T, name string) *repos.RepoInstance {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	return repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         name,
		AgentBranch:  "agent/test",
		Svc:          svc,
		Ontology:     fact.CodeOntology(),
		OntologyRoot: "kb",
	})
}

// viaLens drives one MCP tool handler through the REAL middleware stack: a chi
// router mounting repos.LensMiddleware(m) over /lenses/{lens}/mcp, an HTTP
// request that the middleware resolves into a Binding, and the handler invoked
// with that request's context. The Binding the handler sees is therefore the
// one LensMiddleware minted from the persisted lens — never a test construction.
func viaLens(t *testing.T, m *repos.Manager, lens string, handler e2eHandler, args map[string]any) (*mcpgo.CallToolResult, string) {
	t.Helper()

	var result *mcpgo.CallToolResult
	var herr error
	probe := func(w http.ResponseWriter, r *http.Request) {
		// Prove the binding really came from the middleware before we call in.
		_, ok := repos.BindingFromContextOpt(r.Context())
		require.True(t, ok, "LensMiddleware must have set a Binding on the context")
		var req mcpgo.CallToolRequest
		req.Params.Arguments = args
		result, herr = handler(r.Context(), req)
		w.WriteHeader(http.StatusOK)
	}

	router := chi.NewRouter()
	router.With(repos.LensMiddleware(m)).Get("/lenses/{lens}/mcp", probe)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest("GET", "/lenses/"+lens+"/mcp", nil))
	require.Equal(t, http.StatusOK, rec.Code, "middleware rejected the lens: %s", rec.Body.String())
	require.NoError(t, herr)
	require.NotNil(t, result)
	return result, resultText(t, result)
}

// TestLensE2E_QueryFederatesAndPages: knomit_query through the lens endpoint
// returns rows from BOTH repos — the write repo (alpha) bare, the foreign read
// mount (beta) kb://<id12(beta)>/-qualified — and pages to exhaustion via the
// cursor, every row appearing exactly once with its mount-correct qualification.
func TestLensE2E_QueryFederatesAndPages(t *testing.T) {
	m, _, repoB, ctxA, ctxB, lens := newLensE2E(t)

	const perMount = 15 // 30 > defaultPageSize(20) → forces a resumed page
	seedFedMany(t, ctxA, perMount, "Alpha", "alpha body ", "store")
	seedFedMany(t, ctxB, perMount, "Bravo", "bravo body ", "ui")

	qualPrefix := qualifyPath(id12(repoB.ID()), "")

	seen := map[string]bool{}
	qualified, bare := 0, 0
	collect := func(facts []factOutput) {
		for _, f := range facts {
			require.Falsef(t, seen[f.File], "row %s returned twice across pages", f.File)
			seen[f.File] = true
			if strings.HasPrefix(f.File, kbScheme) {
				require.Truef(t, strings.HasPrefix(f.File, qualPrefix), "beta row must be qualified to beta: %s", f.File)
				require.Truef(t, strings.HasPrefix(f.Title, "Bravo"), "qualified row must carry beta's title: %s", f.Title)
				qualified++
			} else {
				require.Truef(t, strings.HasPrefix(f.Title, "Alpha"), "bare row must carry alpha's title: %s", f.Title)
				bare++
			}
		}
	}

	result, text := viaLens(t, m, lens, QueryHandler(), map[string]any{"type": []any{"policy"}})
	require.Falsef(t, result.IsError, "query failed: %s", text)
	var first queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &first))
	require.Len(t, first.Facts, defaultPageSize, "first page must be full")
	require.True(t, first.HasMore, "more results remain")
	require.NotNil(t, first.Cursor, "cursor must be returned while more remain")
	collect(first.Facts)

	cursor := *first.Cursor
	for {
		result, text := viaLens(t, m, lens, QueryHandler(), map[string]any{"cursor": cursor})
		require.Falsef(t, result.IsError, "resume failed: %s", text)
		var page queryResponse
		require.NoError(t, json.Unmarshal([]byte(text), &page))
		collect(page.Facts)
		if !page.HasMore {
			require.Nil(t, page.Cursor, "cursor must be nil once drained")
			break
		}
		require.NotNil(t, page.Cursor)
		cursor = *page.Cursor
	}

	require.Len(t, seen, 2*perMount, "every seeded fact must appear exactly once across pages")
	require.Equal(t, perMount, qualified, "every beta row must be kb://-qualified")
	require.Equal(t, perMount, bare, "every alpha row must be bare")
}

// TestLensE2E_ExplainCopyPastePath: the copy-paste invariant (RFC §6.2). A
// kb://-qualified path is taken VERBATIM from a knomit_query result row and fed
// straight back into knomit_explain through the same lens endpoint — it must
// resolve to beta's fact with no transformation by the caller.
func TestLensE2E_ExplainCopyPastePath(t *testing.T) {
	m, _, repoB, _, ctxB, lens := newLensE2E(t)

	seedFedFact(t, ctxB, "seed-b", "mission/ui", "Bravo", "ui", nil)

	// Query the lens and locate beta's row.
	_, text := viaLens(t, m, lens, QueryHandler(), map[string]any{"type": []any{"policy"}})
	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	rowB := factByTitle(t, resp, "Bravo")
	require.Truef(t, strings.HasPrefix(rowB.File, qualifyPath(id12(repoB.ID()), "")),
		"query row for beta must be kb://-qualified: %s", rowB.File)

	// Copy the path VERBATIM — no rewriting — into explain through the lens.
	copied := rowB.File
	result, exText := viaLens(t, m, lens, ExplainHandler(), map[string]any{"file": copied})
	require.Falsef(t, result.IsError, "explain of copy-pasted path failed: %s", exText)

	var exResp expResp
	require.NoError(t, json.Unmarshal([]byte(exText), &exResp))
	root := findExpFact(exResp.Facts, copied)
	require.NotNilf(t, root, "explain must return the fact under the exact copy-pasted path %q: %s", copied, exText)
	require.Equal(t, "Bravo", root.Title)
	require.Equal(t, 0, root.Depth)
}

// TestLensE2E_LearnLandsOnWriteRepo: knomit_learn through the lens writes to the
// write repo (alpha) — path returned UNQUALIFIED (bare) — and the fact is then
// visible via the lens as a bare (write-repo) row, proving it landed on alpha's
// agent branch rather than the foreign read mount.
func TestLensE2E_LearnLandsOnWriteRepo(t *testing.T) {
	m, _, _, _, _, lens := newLensE2E(t)

	learnArgs := map[string]any{
		"moment_name": "e2e-learn",
		"facts": []any{
			map[string]any{
				"topic":      "principles",
				"category":   "mission/store",
				"title":      "LensWrite",
				"body":       "designer authored LensWrite.",
				"kind":       "pragmatic",
				"type":       "policy",
				"domain":     []any{"store"},
				"confidence": 0.8,
				"sources":    1,
				"entities":   []any{"designer"},
				"refs":       []any{},
			},
		},
	}
	result, text := viaLens(t, m, lens, LearnHandler(), learnArgs)
	require.Falsef(t, result.IsError, "learn through lens failed: %s", text)

	var learned struct {
		Commits []struct {
			File string `json:"file"`
		} `json:"commits"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &learned))
	require.Len(t, learned.Commits, 1, "learn must commit exactly one fact: %s", text)
	writtenPath := learned.Commits[0].File
	require.NotContains(t, writtenPath, kbScheme, "write path must be returned bare (unqualified): %s", writtenPath)
	require.True(t, strings.HasPrefix(writtenPath, "kb/principles/mission/store/"),
		"write must land under the requested category: %s", writtenPath)

	// Read it back through the lens: it must surface as a bare (write-repo) row.
	_, qText := viaLens(t, m, lens, QueryHandler(), map[string]any{"type": []any{"policy"}})
	var qResp queryResponse
	require.NoError(t, json.Unmarshal([]byte(qText), &qResp))
	row := factByTitle(t, qResp, "LensWrite")
	require.Equal(t, writtenPath, row.File, "learned fact must appear at its bare write-repo path")
	require.NotContains(t, row.File, kbScheme, "write-repo row must be bare")
}

// TestLensE2E_ReposMountsMatchQualifiedIDs: knomit_repos through the lens lists
// every mount, and each mount's id12(mount.ID) is exactly the prefix that
// qualifies that mount's rows in a federated query — the discovery contract
// (RFC §6.1) that lets a caller interpret kb://<id>/ paths.
func TestLensE2E_ReposMountsMatchQualifiedIDs(t *testing.T) {
	m, repoA, repoB, ctxA, ctxB, lens := newLensE2E(t)

	seedFedFact(t, ctxA, "seed-a", "mission/store", "Alpha", "store", nil)
	seedFedFact(t, ctxB, "seed-b", "mission/ui", "Bravo", "ui", nil)

	// Discover mounts through the lens.
	result, text := viaLens(t, m, lens, ReposHandler(), map[string]any{})
	require.Falsef(t, result.IsError, "repos failed: %s", text)
	var mounts reposResponse
	require.NoError(t, json.Unmarshal([]byte(text), &mounts))
	require.Equal(t, "eng", mounts.Binding)
	require.Len(t, mounts.Mounts, 2, "both mounts must be listed: %s", text)

	byName := map[string]reposMount{}
	for _, mt := range mounts.Mounts {
		byName[mt.Name] = mt
	}
	require.Contains(t, byName, "alpha")
	require.Contains(t, byName, "beta")
	require.Equal(t, repoA.ID(), byName["alpha"].ID)
	require.Equal(t, repoB.ID(), byName["beta"].ID)
	require.Equal(t, "read+write", byName["alpha"].Role, "the write repo is read+write")
	require.Equal(t, "read", byName["beta"].Role, "a foreign mount is read-only")

	// The qualified prefix used in query rows must be id12 of the mount's ID.
	_, qText := viaLens(t, m, lens, QueryHandler(), map[string]any{"type": []any{"policy"}})
	var qResp queryResponse
	require.NoError(t, json.Unmarshal([]byte(qText), &qResp))
	rowB := factByTitle(t, qResp, "Bravo")
	require.True(t, strings.HasPrefix(rowB.File, kbScheme))
	gotID := strings.TrimPrefix(rowB.File, kbScheme)
	gotID = gotID[:strings.Index(gotID, "/")]
	require.Equal(t, id12(byName["beta"].ID), gotID,
		"the kb://<id>/ prefix on beta's rows must equal id12 of beta's knomit_repos ID")
	require.Len(t, gotID, 12, "wire IDs are the 12-hex prefix")
}
