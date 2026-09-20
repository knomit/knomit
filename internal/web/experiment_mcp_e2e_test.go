package web

// End-to-end tests for the experiment MCP surface, through the real router.
//
// They reuse the 2026-09-17 incident harness (incidentServer, callToolAt,
// bindHandle, learnArgs) on purpose: the first test below is that incident's
// shape with an experiment in place of a repo, and sharing the fixture is what
// keeps the two from drifting into different notions of "two jobs on one
// connection".

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// experimentServer mirrors incidentServer but also hands back the Manager,
// which these tests need in order to act OUT OF BAND — rolling an experiment
// back the way the REST route and the sweeper do, with no handle involved.
//
// A separate constructor rather than a wider return on incidentServer: that
// one anchors a regression for a shipped incident, and widening its signature
// to serve a new feature's tests is how an anchor starts drifting.
func experimentServer(t *testing.T) (http.Handler, *repos.Manager) {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{
		Cfg:         config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentBranch: "agent/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })

	// Both repos share fact.CodeOntology(), so a write routed to the wrong
	// branch SUCCEEDS — which is what makes the routing assertions below
	// about routing rather than about validation.
	newE2EMount(t, m, "jobA-repo", false)
	newE2EMount(t, m, "jobB-repo", false)

	st := newClientSessionsStore(t)
	m.SetClientSessions(st)

	s := &Server{Manager: m, ClientSessions: st, OntologyRoot: "kb"}
	s.buildMCPHandler()
	return s.NewAPIRouter(), m
}

// apiPath strips the APIBase prefix from a client-facing URL.
//
// These tests drive s.NewAPIRouter() directly, and the real server mounts
// that router UNDER APIBase (server.go: r.Mount(APIBase, s.NewAPIRouter())).
// So a path a client would use — which is what knomit_experiment returns in
// reconnect_url — carries a /api/v1 prefix the bare router does not expect.
// Asserting the full client form and then calling the stripped one keeps both
// halves honest: the string the agent is handed, and that it resolves.
func apiPath(clientURL string) string {
	return strings.TrimPrefix(clientURL, APIBase)
}

// initExperimentSession performs the initialize handshake and returns the
// session id the SERVER minted. mcp-go validates the id, so an invented one
// is answered "Invalid session ID" — a test that made one up would fail
// before reaching anything it meant to assert.
func initExperimentSession(t *testing.T, h http.Handler) string {
	t.Helper()
	_, sid := rpcAt(t, h, unscopedMount, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"experiments","version":"1.0"}}}`)
	require.NotEmpty(t, sid, "server must mint a session id")
	return sid
}

// initAt is initExperimentSession for an arbitrary mount. Every mount needs
// its own handshake — a URL-scoped endpoint is still an MCP connection, and
// mcp-go answers a call with no established session id "Invalid session ID"
// before the handler is reached.
func initAt(t *testing.T, h http.Handler, mount string) string {
	t.Helper()
	_, sid := rpcAt(t, h, mount, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"experiments","version":"1.0"}}}`)
	require.NotEmpty(t, sid, "server must mint a session id at %s", mount)
	return sid
}

// experimentEnvelope is the part of a knomit_experiment result these tests
// read.
type experimentEnvelope struct {
	Action       string `json:"action"`
	Repo         string `json:"repo"`
	Branch       string `json:"branch"`
	Active       string `json:"active_experiment"`
	ReconnectURL string `json:"reconnect_url"`
	Summary      string `json:"summary"`
	Experiments  []struct {
		Name    string `json:"name"`
		Current bool   `json:"current"`
	} `json:"experiments"`
}

func callExperiment(t *testing.T, h http.Handler, mount, sid, args string) (experimentEnvelope, string, bool) {
	t.Helper()
	text, isErr := callToolAt(t, h, mount, sid, "knomit_experiment", args)
	var env experimentEnvelope
	if !isErr {
		require.NoError(t, json.Unmarshal([]byte(text), &env), "experiment result: %s", text)
	}
	return env, text, isErr
}

// factPathsOn lists the fact paths live on a branch of a repo.
func factPathsOn(t *testing.T, m *repos.Manager, repo, branch string) []string {
	t.Helper()
	var paths []string
	ri := m.Get(repo)
	require.NotNil(t, ri, "no repo %q", repo)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		got, err := svc.Facts().ListAll(context.Background(), branch)
		require.NoError(t, err)
		paths = got
	}))
	return paths
}

// TestExperiment_IsPerHandleNotPerSession is the 2026-09-17 incident's shape,
// with an experiment standing in for the repo.
//
// Two logical jobs share ONE MCP connection and therefore one Mcp-Session-Id —
// the arrangement Claude Desktop produces for concurrent Cowork sessions. Job A
// opens an experiment. Job B, which never asked for one, writes a fact.
//
// If the active experiment were keyed on the session id, job B's write would
// land on exp/job-a-work: the state would serve whichever job acted LAST. The
// assertion is therefore about WHERE job B's fact is, not that its call
// succeeded — a misrouted write here succeeds and looks correct, and it is
// WORSE than the original incident, where the two repos' ontologies differed
// and the misrouted writes at least failed. Here both branches share an
// ontology by construction, so nothing would catch it.
func TestExperiment_IsPerHandleNotPerSession(t *testing.T) {
	h, m := experimentServer(t)

	// ONE initialize, ONE session id, for both jobs — the shared Claude
	// Desktop connection. The id is the SERVER's; mcp-go rejects an invented
	// one, so this cannot be faked by passing a string.
	//
	// The complementary hostile case — the session id being a LIVE HANDLE's
	// own value, the most favourable input a session-keyed lookup could get —
	// is not reachable through the transport for that same reason, so it
	// lives one layer down in
	// TestSessionBindingMiddleware_ResolvesNoExperimentFromTheSessionID.
	sid := initExperimentSession(t, h)
	handleA := bindHandle(t, h, sid, "jobA-repo")
	handleB := bindHandle(t, h, sid, "jobB-repo")
	require.NotEqual(t, handleA, handleB, "each bind mints its own handle")

	// Job A moves itself into an experiment.
	envA, textA, isErr := callExperiment(t, h, unscopedMount, sid,
		fmt.Sprintf(`{"binding":%q,"action":"open","name":"job-a-work"}`, handleA))
	require.False(t, isErr, "open: %s", textA)
	require.Equal(t, "exp/job-a-work", envA.Branch)
	require.Equal(t, "job-a-work", envA.Active)

	// Job B — same session id, different handle — writes a fact. It never
	// mentioned an experiment.
	textB, isErr := callToolAt(t, h, unscopedMount, sid, "knomit_learn",
		learnArgs(handleB, "storage", "Job B Fact"))
	require.False(t, isErr, "job B learn: %s", textB)

	// THE ASSERTION, and it is about WHERE the bytes are. Job B's fact is on
	// jobB-repo's own agent branch.
	agentPaths := factPathsOn(t, m, "jobB-repo", "agent/test")
	require.NotEmpty(t, agentPaths, "job B's fact must be on its own agent branch")

	// Session-keyed state would have put it on job A's experiment instead, so
	// that branch must not have grown a fact — and its repo is not even job
	// B's, which is the second way the same mistake would show.
	expPaths := factPathsOn(t, m, "jobA-repo", "exp/job-a-work")
	for _, p := range expPaths {
		require.NotContains(t, strings.ToLower(p), "job-b",
			"job B's fact reached job A's experiment: the active experiment is being keyed on the session id")
	}

	// And jobB-repo has no experiment at all: job A opening one did not
	// conjure one for the other job on the same connection.
	ri := m.Get("jobB-repo")
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		list, err := svc.Experiments().ListExperiments(context.Background())
		require.NoError(t, err)
		require.Empty(t, list, "job B's repo must hold no experiment")
	}))
}

// TestExperiment_URLScopedOpenReturnsTheReconnectURL: a URL-scoped mount has
// no handle, so `open` can create the experiment but cannot move the caller.
// It must say so and name the endpoint to reconnect on — an agent told only
// "created" keeps writing to the agent branch believing otherwise.
func TestExperiment_URLScopedOpenReturnsTheReconnectURL(t *testing.T) {
	h, _ := experimentServer(t)
	mount := "/repos/jobA-repo/branches/agent:test/mcp"

	env, text, isErr := callExperiment(t, h, mount, initAt(t, h, mount),
		`{"action":"open","name":"from-a-url"}`)
	require.False(t, isErr, "open on a URL-scoped mount: %s", text)

	require.Equal(t, "/api/v1/repos/jobA-repo/branches/exp:from-a-url/mcp", env.ReconnectURL)
	require.Empty(t, env.Active, "this session is NOT inside it")
	require.Contains(t, env.Summary, "NOT INSIDE IT")
	require.Contains(t, env.Summary, env.ReconnectURL)

	// The URL it names must actually be a mount, not a plausible-looking
	// string. This is the half that makes the hardcoded /api/v1 prefix in
	// internal/mcp safe.
	back := apiPath(env.ReconnectURL)
	text2, isErr2 := callToolAt(t, h, back, initAt(t, h, back), "knomit_experiment", `{"action":"list"}`)
	require.False(t, isErr2, "the reconnect URL must resolve: %s", text2)
	var listed experimentEnvelope
	require.NoError(t, json.Unmarshal([]byte(text2), &listed))
	require.Equal(t, "exp/from-a-url", listed.Branch, "and it must land inside the experiment")
	require.Equal(t, "from-a-url", listed.Active)
}

// TestExperiment_BindResumesAnExistingExperiment: a handle dies with its
// session, so re-entry after a reconnect has to be one call.
func TestExperiment_BindResumesAnExistingExperiment(t *testing.T) {
	h, _ := experimentServer(t)
	sid := initExperimentSession(t, h)

	first := bindHandle(t, h, sid, "jobA-repo")
	_, text, isErr := callExperiment(t, h, unscopedMount, sid,
		fmt.Sprintf(`{"binding":%q,"action":"open","name":"resume-me"}`, first))
	require.False(t, isErr, "open: %s", text)

	// A fresh bind WITHOUT the experiment is on the agent branch — the state
	// a reconnect leaves behind, and the reason resume exists.
	plain := bindHandle(t, h, sid, "jobA-repo")
	env, _, _ := callExperiment(t, h, unscopedMount, sid,
		fmt.Sprintf(`{"binding":%q,"action":"list"}`, plain))
	require.Empty(t, env.Active, "a fresh handle starts outside any experiment")

	// With `experiment`, it starts inside.
	resumedText, isErr := callToolAt(t, h, unscopedMount, sid, "knomit_bind",
		`{"repo":"jobA-repo","experiment":"resume-me"}`)
	require.False(t, isErr, "bind with experiment: %s", resumedText)
	var envelope struct {
		Binding string `json:"binding"`
	}
	require.NoError(t, json.NewDecoder(strings.NewReader(resumedText)).Decode(&envelope))
	require.NotEmpty(t, envelope.Binding)

	back, _, _ := callExperiment(t, h, unscopedMount, sid,
		fmt.Sprintf(`{"binding":%q,"action":"list"}`, envelope.Binding))
	require.Equal(t, "resume-me", back.Active, "bind with `experiment` resumes it")
	require.Equal(t, "exp/resume-me", back.Branch)

	// RESUME ONLY: an unknown name is refused and names the tool that creates.
	unknown, isErr := callToolAt(t, h, unscopedMount, sid, "knomit_bind",
		`{"repo":"jobA-repo","experiment":"never-opened"}`)
	require.True(t, isErr, "an unknown experiment must be refused, not created")
	require.Contains(t, unknown, "knomit_experiment")
}

// TestExperiment_LazyHealAfterAnOutOfBandRollback: an experiment can vanish
// under a live caller — the expiry sweeper, the REST API, the UI, or another
// session committing a shared one. None of those hold the caller's handle, so
// eager clearing cannot reach it and the heal has to happen at resolution.
//
// The call must still RUN. Failing a query because the experiment behind it
// was rolled back elsewhere would be a worse answer than serving the agent
// branch and saying so.
func TestExperiment_LazyHealAfterAnOutOfBandRollback(t *testing.T) {
	h, m := experimentServer(t)
	sid := initExperimentSession(t, h)

	handle := bindHandle(t, h, sid, "jobA-repo")
	_, text, isErr := callExperiment(t, h, unscopedMount, sid,
		fmt.Sprintf(`{"binding":%q,"action":"open","name":"doomed"}`, handle))
	require.False(t, isErr, "open: %s", text)

	// Out of band: the same rollback the REST route and the sweeper perform,
	// with no handle in sight.
	ri := m.Get("jobA-repo")
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Experiments().RollbackExperiment(context.Background(), "doomed"))
	}))

	// The caller's next call still runs, on the agent branch.
	env, text, isErr := callExperiment(t, h, unscopedMount, sid,
		fmt.Sprintf(`{"binding":%q,"action":"list"}`, handle))
	require.False(t, isErr, "the call must still run after the experiment vanished: %s", text)
	require.Empty(t, env.Active, "the caller is no longer inside it")
	require.Equal(t, "agent/test", env.Branch, "and is back on the agent branch")

	// knomit_repos says what happened, rather than moving the caller silently.
	// Decoded, not substring-matched: "no error returned" is not the
	// assertion, and neither is the word appearing somewhere in the blob.
	require.Equal(t, "doomed", lapsedExperimentOf(t, h, sid, handle),
		"the caller must be TOLD its experiment is gone, not just moved off it")

	// And the fallback really is the agent branch, read off the mount table
	// rather than inferred from the list result.
	require.Equal(t, "agent/test", writeBranchOf(t, h, sid, handle))
}

// TestExperiment_LazyHealWhenEligibilityLapses is the OTHER half of the heal,
// and it is a different case from the one above: the experiments row still
// EXISTS. What changed is that the repo's agent branch moved out from under
// it — a takeover — so its recorded parent no longer matches and
// WritableBranch says false.
//
// Existence and eligibility are separate conditions, so a test that only
// deletes the row exercises one of them and leaves the other unguarded.
func TestExperiment_LazyHealWhenEligibilityLapses(t *testing.T) {
	h, m := experimentServer(t)

	sid := initExperimentSession(t, h)
	handle := bindHandle(t, h, sid, "jobA-repo")
	_, text, isErr := callExperiment(t, h, unscopedMount, sid,
		fmt.Sprintf(`{"binding":%q,"action":"open","name":"orphaned"}`, handle))
	require.False(t, isErr, "open: %s", text)

	// The repo is taken over by a different agent branch, keeping its uid and
	// its store — so the handle still resolves to this repo, and only the
	// eligibility answer changes. The experiments row is untouched.
	old := m.Get("jobA-repo")
	m.Set("jobA-repo", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: "jobA-repo", UID: old.UID(), Svc: serviceOf(t, old),
		AgentBranch: "agent/successor", Ontology: old.Ontology(), OntologyRoot: "kb",
	}))
	require.NoError(t, m.Get("jobA-repo").WithRead(func(svc *store.Service) {
		_, ok, err := svc.Experiments().GetExperiment(context.Background(), "orphaned")
		require.NoError(t, err)
		require.True(t, ok, "precondition: the row still EXISTS — only eligibility changed")
	}))

	env, text, isErr := callExperiment(t, h, unscopedMount, sid,
		fmt.Sprintf(`{"binding":%q,"action":"list"}`, handle))
	require.False(t, isErr, "the call must still run: %s", text)
	require.Empty(t, env.Active, "an experiment whose parent moved is no longer active")
	require.Equal(t, "agent/successor", env.Branch, "the caller falls back to the CURRENT agent branch")
	require.Equal(t, "orphaned", lapsedExperimentOf(t, h, sid, handle))
}

// TestExperiment_SyncDoesNotLeaveTheExperiment: commit and rollback clear the
// caller's handle; sync must not. Syncing is how you STAY in an experiment
// after a refused commit, so a sync that dropped you out would send the next
// write to the agent branch precisely when the two have diverged.
func TestExperiment_SyncDoesNotLeaveTheExperiment(t *testing.T) {
	h, _ := experimentServer(t)
	sid := initExperimentSession(t, h)
	handle := bindHandle(t, h, sid, "jobA-repo")

	_, text, isErr := callExperiment(t, h, unscopedMount, sid,
		fmt.Sprintf(`{"binding":%q,"action":"open","name":"staying"}`, handle))
	require.False(t, isErr, "open: %s", text)

	env, text, isErr := callExperiment(t, h, unscopedMount, sid,
		fmt.Sprintf(`{"binding":%q,"action":"sync"}`, handle))
	require.False(t, isErr, "sync: %s", text)
	require.Equal(t, "staying", env.Active, "sync reports you are still inside")

	// And the next call agrees — the handle itself was not cleared.
	after, _, _ := callExperiment(t, h, unscopedMount, sid,
		fmt.Sprintf(`{"binding":%q,"action":"list"}`, handle))
	require.Equal(t, "staying", after.Active, "sync must not clear the handle's experiment")
	require.Equal(t, "exp/staying", writeBranchOf(t, h, sid, handle))

	// Commit, by contrast, DOES clear it.
	_, text, isErr = callExperiment(t, h, unscopedMount, sid,
		fmt.Sprintf(`{"binding":%q,"action":"commit"}`, handle))
	require.False(t, isErr, "commit: %s", text)
	gone, _, _ := callExperiment(t, h, unscopedMount, sid,
		fmt.Sprintf(`{"binding":%q,"action":"list"}`, handle))
	require.Empty(t, gone.Active, "commit clears the caller's handle eagerly")
	require.Equal(t, "agent/test", writeBranchOf(t, h, sid, handle))
}

// TestLensExperimentMount_RefusesABindingArgument: the new route is
// URL-scoped, so a `binding` argument is refused ON PRESENCE — empty string
// included. Ignoring it would be worse than failing: the caller believes it
// selected a base while the URL serves another.
func TestLensExperimentMount_RefusesABindingArgument(t *testing.T) {
	h, m := experimentServer(t)
	write := m.Get("jobA-repo")
	require.NoError(t, write.WithRead(func(svc *store.Service) {
		_, err := svc.Experiments().OpenExperiment(context.Background(), "url-bound", "", write.AgentBranch())
		require.NoError(t, err)
	}))
	_, err := m.LensRegistry().Create(repos.Lens{
		Name: "urlbound", WriteUID: write.UID(),
		Reads: []repos.LensRead{{RepoUID: write.UID()}},
	})
	require.NoError(t, err)

	mount := "/lenses/urlbound/experiments/url-bound/mcp"
	sid := initAt(t, h, mount)
	for _, args := range []string{`{"binding":"anything","action":"list"}`, `{"binding":"","action":"list"}`} {
		text, isErr := callToolAt(t, h, mount, sid, "knomit_experiment", args)
		require.True(t, isErr, "a binding on a URL-scoped mount must be refused: %s", args)
		require.Contains(t, text, "bound by its URL", args)
	}
}

// TestLensExperimentMount_RePinsOnlyTheWriteMember: the lens twin of the
// branch segment. Only the write member moves; every co-mounted repo stays on
// its own branch, because an experiment belongs to exactly one repo.
func TestLensExperimentMount_RePinsOnlyTheWriteMember(t *testing.T) {
	h, m := experimentServer(t)

	write := m.Get("jobA-repo")
	read := m.Get("jobB-repo")
	require.NoError(t, write.WithRead(func(svc *store.Service) {
		_, err := svc.Experiments().OpenExperiment(context.Background(), "lens-side", "", write.AgentBranch())
		require.NoError(t, err)
	}))
	_, err := m.LensRegistry().Create(repos.Lens{
		Name: "pair", WriteUID: write.UID(),
		Reads: []repos.LensRead{{RepoUID: write.UID()}, {RepoUID: read.UID()}},
	})
	require.NoError(t, err)

	mount := "/lenses/pair/experiments/lens-side/mcp"
	reposText, isErr := callToolAt(t, h, mount, initAt(t, h, mount), "knomit_repos", `{}`)
	require.False(t, isErr, "%s", reposText)

	var resp struct {
		Bound struct {
			Mounts []struct {
				Name        string `json:"name"`
				Branch      string `json:"branch"`
				Role        string `json:"role"`
				WriteBranch string `json:"write_branch"`
				Experiment  string `json:"experiment"`
			} `json:"mounts"`
		} `json:"bound"`
	}
	require.NoError(t, json.Unmarshal([]byte(reposText), &resp))
	require.Len(t, resp.Bound.Mounts, 2)

	var sawWrite, sawRead bool
	for _, mt := range resp.Bound.Mounts {
		switch mt.Name {
		case "jobA-repo":
			sawWrite = true
			require.Equal(t, "read+write", mt.Role)
			require.Equal(t, "exp/lens-side", mt.Branch, "the write member READS the experiment")
			require.Equal(t, "exp/lens-side", mt.WriteBranch, "and WRITES there")
			require.Equal(t, "lens-side", mt.Experiment)
		case "jobB-repo":
			sawRead = true
			require.Equal(t, "read", mt.Role)
			require.Equal(t, "agent/test", mt.Branch,
				"a co-mounted repo keeps its own branch: an experiment belongs to ONE repo")
			require.Empty(t, mt.Experiment)
		}
	}
	require.True(t, sawWrite && sawRead, "both mounts must appear")
}

// TestLensExperimentMount_UnknownExperimentIs404: on a URL-scoped mount the
// experiment IS the endpoint, so an experiment that does not exist (or that
// this instance may not write) is a 404 rather than a quiet fall-back to the
// agent branch. The session-scoped mount heals instead, because there the
// caller is mid-session and its call must still run.
func TestLensExperimentMount_UnknownExperimentIs404(t *testing.T) {
	h, m := experimentServer(t)
	write := m.Get("jobA-repo")
	_, err := m.LensRegistry().Create(repos.Lens{
		Name: "solo", WriteUID: write.UID(),
		Reads: []repos.LensRead{{RepoUID: write.UID()}},
	})
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/lenses/solo/experiments/never-opened/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, "body: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "never-opened")
}

// TestExperiment_SubscriptionRefusesOpen: a subscription has no agent branch,
// so there is nothing to fork from. The refusal names that reason rather than
// letting the write gate refuse it later as an unexplained read-only view.
func TestExperiment_SubscriptionRefusesOpen(t *testing.T) {
	h, m := experimentServer(t)
	newE2EMount(t, m, "followed", true)

	sid := initExperimentSession(t, h)
	handle := bindHandle(t, h, sid, "followed")
	_, text, isErr := callExperiment(t, h, unscopedMount, sid,
		fmt.Sprintf(`{"binding":%q,"action":"open","name":"wishful"}`, handle))
	require.True(t, isErr, "a subscription must refuse open, got: %s", text)
	require.Contains(t, text, "subscription")
	require.Contains(t, text, "no agent branch")
}

// lapsedExperimentOf decodes bound.lapsed_experiment from knomit_repos.
func lapsedExperimentOf(t *testing.T, h http.Handler, sid, handle string) string {
	t.Helper()
	return boundOfHandle(t, h, sid, handle).LapsedExperiment
}

// writeBranchOf decodes the write mount's write_branch from knomit_repos —
// where a write would actually land, which is the claim the heal tests need.
func writeBranchOf(t *testing.T, h http.Handler, sid, handle string) string {
	t.Helper()
	for _, mt := range boundOfHandle(t, h, sid, handle).Mounts {
		if mt.Role == "read+write" {
			return mt.WriteBranch
		}
	}
	return ""
}

type boundView struct {
	LapsedExperiment string `json:"lapsed_experiment"`
	Mounts           []struct {
		Name        string `json:"name"`
		Branch      string `json:"branch"`
		Role        string `json:"role"`
		WriteBranch string `json:"write_branch"`
		Experiment  string `json:"experiment"`
	} `json:"mounts"`
}

func boundOfHandle(t *testing.T, h http.Handler, sid, handle string) boundView {
	t.Helper()
	text, isErr := callToolAt(t, h, unscopedMount, sid, "knomit_repos",
		fmt.Sprintf(`{"binding":%q}`, handle))
	require.False(t, isErr, "knomit_repos: %s", text)
	var resp struct {
		Bound boundView `json:"bound"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &resp), "repos result: %s", text)
	return resp.Bound
}

// serviceOf reaches an instance's live store so a fixture can re-register the
// same repo under a different agent branch — the takeover shape.
func serviceOf(t *testing.T, ri *repos.RepoInstance) *store.Service {
	t.Helper()
	svc, release, err := ri.Acquire()
	require.NoError(t, err)
	t.Cleanup(release)
	return svc
}
