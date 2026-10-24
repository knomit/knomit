package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store/index"
)

// A READ of a repo whose store cannot be reached answers the same 503 the
// REST writes answer (user ruling 2026-10-24): title "Repo not ready", the
// fixed detail, never the error text. RepoInstance.WithRead does not run its
// closure when Acquire refuses, so a read helper that dropped WithRead's error
// used to answer a zero-valued success: an empty 200 list, zero stats, a 200
// with an EMPTY fact, or a false 404 ("Commit not found", "Unknown motif").
//
// The states are reached for real, with no fault hook: a store-less instance
// with an identity on a never-started manager (newTestManagerWithRepos), and a
// repo whose lifecycle tore its store down (ErrRepoClosed) on a running one.

// rawRefusalTexts are the error strings Acquire's and the lifecycle machine's
// refusals carry. None of them may reach a client: the 503's detail is fixed.
var rawRefusalTexts = []string{
	repos.ErrStoreUnavailable.Error(),
	repos.ErrRepoClosed.Error(),
	repos.ErrNotOpen.Error(),
	"populating",
	"no store available",
}

// requireUnavailableProblem asserts the one store-unavailable answer: 503,
// application/problem+json, the title, the fixed detail, the status member,
// the instance, and no refusal text anywhere in the body.
func requireUnavailableProblem(t *testing.T, rec *httptest.ResponseRecorder, instance string) {
	t.Helper()
	requireStoreUnavailable(t, rec)
	require.Equal(t, instance, writeProblem(t, rec).Instance)
	for _, raw := range rawRefusalTexts {
		require.NotContains(t, rec.Body.String(), raw, "the refusal's own text leaked")
	}
}

func serveRead(t *testing.T, s *Server, method, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, fromLoopback(httptest.NewRequest(method, url, nil)))
	return rec
}

// GET .../changes is not in the table: it needs the repo's wire id before it
// reads the store, and the id comes from the store (its root commit). A
// store-less instance never has one, and a closed repo's Identify exit clears
// it before Open detaches the store, so both answer 409 "Repo identity
// unresolved" first. Its 503 is reachable only when the store goes away after
// the id check (a race), which nothing here can hold open without a fault hook.

// unavailableReads are the repo routes whose store read answered a
// zero-valued success (or a false 404, or a 500) before the ruling, one per
// helper or handler that reads the store. path is the URL path (the problem's
// instance); query is appended to the request.
func unavailableReads() []struct{ name, method, path, query string } {
	const br = "/repos/alpha/branches/agent:test"
	const sha = "0123456789abcdef0123456789abcdef01234567"
	return []struct{ name, method, path, query string }{
		{"branches (listRepoBranches)", http.MethodGet, "/repos/alpha/branches", ""},
		{"pushed-branches (listRepoBranches)", http.MethodGet, "/repos/alpha/pushed-branches", ""},
		{"branch root (readBranchRoot)", http.MethodGet, br, ""},
		{"facts (recentFacts)", http.MethodGet, br + "/facts", ""},
		{"facts since_fork (experiment diff)", http.MethodGet, "/repos/alpha/branches/exp:probe/facts", "?since_fork=true"},
		{"fact (readFact)", http.MethodGet, br + "/facts/kb/ai/test.md", ""},
		{"fact commits (logPaginated)", http.MethodGet, br + "/facts/kb/ai/test.md/commits", ""},
		{"fact incoming (explainFact)", http.MethodGet, br + "/facts/kb/ai/test.md/incoming", ""},
		{"fact outgoing (explainFact)", http.MethodGet, br + "/facts/kb/ai/test.md/outgoing", ""},
		{"system file (writeSystemFile)", http.MethodGet, br + "/facts/.knomit/ontology.yaml", ""},
		{"topics (listTopicDir)", http.MethodGet, br + "/topics", ""},
		{"topic node (listTopicDir)", http.MethodGet, br + "/topics/ai", ""},
		{"topic stats (loadStats)", http.MethodGet, br + "/topics/ai/stats", ""},
		{"commits (logPaginated)", http.MethodGet, br + "/commits", ""},
		{"commit detail (commitDetail)", http.MethodGet, br + "/commits/" + sha, ""},
		{"commit-anchored fact (readFact)", http.MethodGet, br + "/commits/" + sha + "/facts/kb/ai/test.md", ""},
		{"commit-anchored incoming (factLiveAtCommit)", http.MethodGet, br + "/commits/" + sha + "/facts/kb/ai/test.md/incoming", ""},
		{"commit-anchored outgoing fallback (factExistsAt)", http.MethodGet, br + "/commits/" + sha + "/facts/kb/ai/test.md/outgoing", "?fallback=before"},
		{"search (searchRepo)", http.MethodGet, br + "/search", "?domain=ai"},
		{"activity (loadActivity)", http.MethodGet, br + "/activity", ""},
		{"completions (readCompletions)", http.MethodGet, br + "/completions", "?category=domain"},
		{"domains (domainStats)", http.MethodGet, br + "/domains", ""},
		{"domain facts (domainFacts)", http.MethodGet, br + "/domains/ai", ""},
		{"motifs (motifClusters)", http.MethodGet, br + "/motifs", ""},
		{"motif cluster (motifClusters)", http.MethodGet, br + "/motifs/some-motif", ""},
		{"stats (loadStats)", http.MethodGet, br + "/stats", ""},
		{"experiments (list)", http.MethodGet, "/repos/alpha/experiments", ""},
		{"origin GET (getOrigin)", http.MethodGet, "/repos/alpha/origin", ""},
		{"origin DELETE (ErrNotOpen)", http.MethodDelete, "/repos/alpha/origin", ""},
	}
}

// A store-less instance with an identity, on a never-started manager: every
// read route answers the one 503.
func TestReads_NoStore_Answer503(t *testing.T) {
	m := newTestManagerWithRepos(t, "alpha")
	_, _, aerr := m.Get("alpha").Acquire()
	require.ErrorIs(t, aerr, repos.ErrStoreUnavailable, "the precondition: this instance has no store")
	s := &Server{Manager: m, OntologyRoot: "kb"}

	for _, tc := range unavailableReads() {
		t.Run(tc.name, func(t *testing.T) {
			requireUnavailableProblem(t, serveRead(t, s, tc.method, tc.path+tc.query), tc.path)
		})
	}
}

// A repo whose lifecycle tore its store down (Acquire: ErrRepoClosed), still
// where the repo middleware finds it: the reads answer the same 503. Unlike a
// write, a read has no eligibility gate in front of it, so the HTTP answer is
// the helper's own. DELETE origin is left out: a closed machine answers
// ErrClosed ("Repo unavailable"), a different lifecycle state.
func TestReads_ClosedRepo_Answer503(t *testing.T) {
	m := newRealManagerInHome(t, t.TempDir())
	s := &Server{Manager: m, OntologyRoot: "kb"}
	createViaAPI(t, s.NewAPIRouter(), "alpha")
	ri := m.Get("alpha")
	require.NotNil(t, ri)
	m.Remove("alpha")
	_, _, aerr := ri.Acquire()
	require.ErrorIs(t, aerr, repos.ErrRepoClosed, "the precondition: this instance is closed")
	m.Set("alpha", ri)

	for _, tc := range unavailableReads() {
		if tc.method == http.MethodDelete {
			continue
		}
		path := strings.Replace(tc.path, "/branches/agent:test", "/branches/machine:test", 1)
		t.Run(tc.name, func(t *testing.T) {
			requireUnavailableProblem(t, serveRead(t, s, tc.method, path+tc.query), path)
		})
	}
}

// Each read helper returns WithRead's refusal itself, not a zero-valued
// success. The HTTP tables above reach most of them; this pins every one,
// including those a route reaches only after another helper has already
// failed (motifClusterKey, motifDefinitions, motifAliasRows,
// motifVocabularyHealth, loadHighlights, topicFactByPath, incomingAtCommit,
// outgoingAtCommit, factExistsAt on the fact view's ref resolver).
func TestReadHelpers_ReturnAcquireRefusal(t *testing.T) {
	ri := newTestManagerWithRepos(t, "alpha").Get("alpha")
	ctx := context.Background()
	const branch, path, sha = "agent/test", "kb/ai/test.md", "0123456789abcdef0123456789abcdef01234567"

	errs := map[string]error{}
	_, errs["loadActivity"] = loadActivity(ctx, ri, branch, "")
	_, errs["readCompletions"] = readCompletions(ctx, ri, branch, "domain", "", 20)
	_, errs["domainStats"] = domainStats(ctx, ri, branch)
	_, errs["domainFacts"] = domainFacts(ctx, ri, branch, "ai")
	_, errs["loadStats"] = loadStats(ctx, ri, branch, "", "")
	_, _, errs["loadHighlights"] = loadHighlights(ctx, ri, branch, "", "")
	_, errs["motifClusters"] = motifClusters(ctx, ri, branch, "")
	_, errs["motifDefinitions"] = motifDefinitions(ctx, ri, branch, []string{"k"})
	_, errs["motifVocabularyHealth"] = motifVocabularyHealth(ctx, ri, branch, "")
	_, errs["motifAliasRows"] = motifAliasRows(ctx, ri, branch)
	_, errs["motifClusterKey"] = motifClusterKey(ctx, ri, branch, "k")
	_, _, errs["readFact"] = readFact(ctx, ri, hal.Anchor{Branch: branch}, path, false)
	_, errs["explainFact"] = explainFact(ctx, ri, branch, path)
	_, errs["incomingAtCommit"] = incomingAtCommit(ctx, ri, branch, path, sha)
	_, errs["outgoingAtCommit"] = outgoingAtCommit(ctx, ri, branch, path, sha)
	_, errs["factLiveAtCommit"] = factLiveAtCommit(ctx, ri, branch, path, sha)
	_, errs["factExistsAt"] = factExistsAt(ctx, ri, branch, path, sha)
	_, _, errs["recentFacts"] = recentFacts(ctx, ri, branch, index.SearchOptions{})
	_, _, _, errs["logPaginated"] = logPaginated(ctx, ri, branch, "", 10, "", "", "")
	_, _, errs["commitDetail"] = commitDetail(ctx, ri, branch, sha, "kb")
	_, errs["searchRepo"] = searchRepo(ctx, ri, nil, branch, index.SearchOptions{})
	_, errs["listTopicDir"] = listTopicDir(ctx, ri, branch, "kb")
	_, errs["topicFactByPath"] = topicFactByPath(ctx, ri, branch, path)
	_, errs["listRepoBranches"] = listRepoBranches(ctx, ri)
	_, errs["readBranchRoot"] = readBranchRoot(ctx, ri, branch)
	_, errs["getOrigin"] = getOrigin(ctx, ri)

	for name, err := range errs {
		require.ErrorIs(t, err, repos.ErrStoreUnavailable, name)
	}
	require.NotErrorIs(t, errs["readFact"], errFactNotFound, "readFact: nothing was looked up, so not a 404")
	require.Len(t, errs, 26, "every helper is in the table")
}

// The lifecycle refusal for a store that is not attached (ErrNotOpen) gets the
// same answer as Acquire's, on every route that maps a machine's refusal; the
// other lifecycle states keep theirs. Distinct inputs, distinct answers: a
// mapping that sent everything to one case fails here.
func TestWriteLifecycleProblem_NotOpenIsTheStoreUnavailableAnswer(t *testing.T) {
	serve := func(err error) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		writeLifecycleProblem(rec, httptest.NewRequest(http.MethodDelete, "/repos/alpha/origin", nil),
			hal.URLBuilder{}, "alpha", err)
		return rec
	}
	requireUnavailableProblem(t, serve(repos.ErrNotOpen), "/repos/alpha/origin")

	for _, tc := range []struct {
		err    error
		status int
		title  string
	}{
		{repos.ErrClosed, http.StatusServiceUnavailable, "Repo unavailable"},
		{repos.ErrUnavailable, http.StatusServiceUnavailable, "Repo unavailable"},
		{repos.ErrIndexing, http.StatusConflict, "Repo is indexing"},
		{context.Canceled, http.StatusServiceUnavailable, "Request ended"},
	} {
		rec := serve(tc.err)
		require.Equal(t, tc.status, rec.Code, tc.err.Error())
		require.Equal(t, tc.title, writeProblem(t, rec).Title, tc.err.Error())
	}
}

// The routes that already answered 503 for this state, but as "Store
// unavailable" with the refusal's text as detail (or, for a commit with
// resolutions, a 500 carrying it), and the lifecycle routes whose machine
// answers ErrNotOpen: all now give the one store-unavailable answer. Reached
// on a store-less instance with an identity, so every gate before the store
// call passes (the agent branch is writable, the ontology error is nil).
func TestStoreUnavailableAnswers_AreOne(t *testing.T) {
	m := newTestManagerWithRepos(t)
	m.Set("alpha", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "alpha", AgentBranch: "agent/test", Hub: repos.NewTaskHub(context.Background()),
	}))
	s := &Server{Manager: m, OntologyRoot: "kb", LLMAdapter: &stubLLMAdapter{}}
	const tip = "0123456789abcdef0123456789abcdef01234567"

	for _, tc := range []struct{ name, method, url, body string }{
		{"experiment open", http.MethodPost, "/repos/alpha/experiments", `{"name":"probe"}`},
		{"experiment sync", http.MethodPost, "/repos/alpha/experiments/probe/sync", ""},
		{"experiment rollback", http.MethodDelete, "/repos/alpha/experiments/probe", ""},
		{"experiment commit with resolutions", http.MethodPost, "/repos/alpha/experiments/probe/commit", `{"resolutions":{"kb/ai/x.md":"ours"}}`},
		{"pushed-branch merge", http.MethodPost, "/repos/alpha/branches/agent:test/merge", `{"expected_tip":"` + tip + `"}`},
		{"synthesis run (Pin)", http.MethodPost, "/repos/alpha/branches/agent:test/synthesis-runs", ""},
		{"index rebuild (ErrNotOpen)", http.MethodPost, "/repos/alpha/branches/agent:test/index-rebuilds", ""},
		{"origin PUT (getOrigin)", http.MethodPut, "/repos/alpha/origin", `{"url":"https://x.test/kb.git","auth_method":"none"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireUnavailableProblem(t, serveWrite(t, s, tc.method, tc.url, tc.body), tc.url)
		})
	}
}
