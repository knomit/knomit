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
	const sid = "one-shared-connection"

	// One connection, one session id, two independent jobs.
	handleA := bindHandle(t, h, sid, "jobA-repo")
	handleB := bindHandle(t, h, sid, "jobB-repo")

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

	env, text, isErr := callExperiment(t, h, mount, "",
		`{"action":"open","name":"from-a-url"}`)
	require.False(t, isErr, "open on a URL-scoped mount: %s", text)

	require.Equal(t, "/api/v1/repos/jobA-repo/branches/exp:from-a-url/mcp", env.ReconnectURL)
	require.Empty(t, env.Active, "this session is NOT inside it")
	require.Contains(t, env.Summary, "NOT INSIDE IT")
	require.Contains(t, env.Summary, env.ReconnectURL)

	// The URL it names must actually be a mount, not a plausible-looking
	// string. This is the half that makes the hardcoded /api/v1 prefix in
	// internal/mcp safe.
	text2, isErr2 := callToolAt(t, h, apiPath(env.ReconnectURL), "", "knomit_experiment", `{"action":"list"}`)
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
	const sid = "resume-session"

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
	const sid = "healing-session"

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
	reposText, isErr := callToolAt(t, h, unscopedMount, sid, "knomit_repos",
		fmt.Sprintf(`{"binding":%q}`, handle))
	require.False(t, isErr, "%s", reposText)
	require.Contains(t, reposText, "lapsed_experiment")
	require.Contains(t, reposText, "doomed")
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
	reposText, isErr := callToolAt(t, h, mount, "", "knomit_repos", `{}`)
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

	const sid = "sub-session"
	handle := bindHandle(t, h, sid, "followed")
	_, text, isErr := callExperiment(t, h, unscopedMount, sid,
		fmt.Sprintf(`{"binding":%q,"action":"open","name":"wishful"}`, handle))
	require.True(t, isErr, "a subscription must refuse open, got: %s", text)
	require.Contains(t, text, "subscription")
	require.Contains(t, text, "no agent branch")
}
