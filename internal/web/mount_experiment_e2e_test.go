package web

// An experiment must be enterable on a --repo or --lens bridge WITHOUT a
// reconnect and without a bridge flag. These drive the real router, because the
// whole mechanism is middleware plus mount identity and a handler-level test
// would assert past the part that can be wrong.
//
// Every assertion is about WHERE A WRITE LANDED, never about a summary string.
// A misrouted write here SUCCEEDS — the experiment and its parent share an
// ontology by construction — so a test that checked the result text would pass
// on exactly the build that writes to the wrong branch.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
	"knomit/internal/store"
)

// repoMount is the URL-scoped mount a `kb --repo <name>` bridge connects to.
func repoMount(t *testing.T, m *repos.Manager, repo string) string {
	t.Helper()
	ri := m.Get(repo)
	require.NotNil(t, ri, "no repo %q", repo)
	return "/repos/" + repo + "/branches/" + strings.ReplaceAll(ri.AgentBranch(), "/", ":") + "/mcp"
}

// learnOn writes a fact through the MCP mount and returns nothing: the test's
// question is always which branch holds it afterwards.
func learnOn(t *testing.T, h http.Handler, mount, sid, title string) {
	t.Helper()
	text, isErr := callToolAt(t, h, mount, sid, "knomit_learn", urlScopedLearnArgs(title))
	require.False(t, isErr, "learn failed on %s: %s", mount, text)
}

// urlScopedLearnArgs is learnArgs WITHOUT a `binding` key.
//
// Not a detail: a URL-scoped mount hard-fails any call that carries one, empty
// string included — the endpoint is the selection, so accepting a binding would
// mean two answers to "where does this write go"
// (kb/invariants/mcp/session-binding/handle-is-the-only-router). learnArgs
// always emits the key, so these tests cannot use it.
func urlScopedLearnArgs(title string) string {
	return fmt.Sprintf(
		`{"moment_name":"mount-experiment","facts":[{"topic":"architecture","category":"demo","title":%q,"body":"written by the mount-experiment tests"}]}`,
		title)
}

// branchHolding reports whether a branch holds a fact whose content mentions s.
func branchHolding(t *testing.T, m *repos.Manager, repo, branch, want string) bool {
	t.Helper()
	ri := m.Get(repo)
	require.NotNil(t, ri)
	found := false
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		paths, err := svc.Facts().ListAll(context.Background(), branch)
		if err != nil {
			return // branch may not exist; that is an answer of "no"
		}
		for _, p := range paths {
			res, rerr := svc.Facts().ReadFact(context.Background(), branch, p, nil)
			if rerr == nil && strings.Contains(res.Content, want) {
				found = true
				return
			}
		}
	}))
	return found
}

// TestMountExperiment_OpenThenLearnLandsOnTheExperiment is the whole feature:
// a --repo bridge opens an experiment and its very next write goes there, with
// no reconnect.
func TestMountExperiment_OpenThenLearnLandsOnTheExperiment(t *testing.T) {
	h, m := experimentServer(t)
	mount := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, mount)

	env, text, isErr := callExperiment(t, h, mount, sid,
		`{"action":"open","name":"on-the-fly","description":"no reconnect"}`)
	require.False(t, isErr, "open failed: %s", text)
	require.Equal(t, "on-the-fly", env.Active,
		"the session must report itself INSIDE the experiment, not merely that one was created")
	require.NotContains(t, text, "reconnect_url",
		"a plain-branch URL mount no longer hands back a URL to reconnect on")

	learnOn(t, h, mount, sid, "written after open")

	require.True(t, branchHolding(t, m, "jobA-repo", "exp/on-the-fly", "written after open"),
		"the write must land on the experiment")
	require.False(t, branchHolding(t, m, "jobA-repo", m.Get("jobA-repo").AgentBranch(), "written after open"),
		"and must NOT land on the agent branch the URL names")
}

// TestMountExperiment_CommitReturnsTheSessionToTheAgentBranch: the row is
// cleared, so the next write goes back to where the URL points.
func TestMountExperiment_CommitReturnsTheSessionToTheAgentBranch(t *testing.T) {
	h, m := experimentServer(t)
	mount := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, mount)
	agent := m.Get("jobA-repo").AgentBranch()

	_, _, isErr := callExperiment(t, h, mount, sid, `{"action":"open","name":"commitme"}`)
	require.False(t, isErr)
	learnOn(t, h, mount, sid, "inside the experiment")

	_, text, isErr := callExperiment(t, h, mount, sid, `{"action":"commit","name":"commitme"}`)
	require.False(t, isErr, "commit failed: %s", text)

	learnOn(t, h, mount, sid, "after the commit")
	require.True(t, branchHolding(t, m, "jobA-repo", agent, "after the commit"),
		"once committed, the session is back on the branch the URL names")
}

// TestMountExperiment_RollbackReturnsTheSessionToTheAgentBranch.
func TestMountExperiment_RollbackReturnsTheSessionToTheAgentBranch(t *testing.T) {
	h, m := experimentServer(t)
	mount := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, mount)
	agent := m.Get("jobA-repo").AgentBranch()

	_, _, isErr := callExperiment(t, h, mount, sid, `{"action":"open","name":"dropme"}`)
	require.False(t, isErr)
	_, text, isErr := callExperiment(t, h, mount, sid, `{"action":"rollback","name":"dropme"}`)
	require.False(t, isErr, "rollback failed: %s", text)

	learnOn(t, h, mount, sid, "after the rollback")
	require.True(t, branchHolding(t, m, "jobA-repo", agent, "after the rollback"))
}

// TestMountExperiment_OutOfBandRollbackHealsLazily: the REST route, the UI and
// the sweeper all remove experiments without touching this session's row. The
// next call must fall back to the URL's branch rather than failing, and must
// SAY it lapsed — a session moved without being told writes its next fact
// somewhere it did not choose.
func TestMountExperiment_OutOfBandRollbackHealsLazily(t *testing.T) {
	h, m := experimentServer(t)
	mount := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, mount)
	agent := m.Get("jobA-repo").AgentBranch()

	_, _, isErr := callExperiment(t, h, mount, sid, `{"action":"open","name":"vanishing"}`)
	require.False(t, isErr)

	// Out of band: exactly what the REST rollback and the sweeper do.
	ri := m.Get("jobA-repo")
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Experiments().RollbackExperiment(context.Background(), "vanishing"))
	}))

	learnOn(t, h, mount, sid, "after it vanished")
	require.True(t, branchHolding(t, m, "jobA-repo", agent, "after it vanished"),
		"a vanished experiment must fall back to the URL's branch, not fail the call")

	text, isErr := callToolAt(t, h, mount, sid, "knomit_repos", `{}`)
	require.False(t, isErr)
	require.Contains(t, text, "lapsed_experiment",
		"and the result must SAY the session was moved")
}

// TestMountExperiment_SharedConnectionSharesTheExperiment pins the hard half of
// the design, and the part knowingly accepted.
//
// Two logical jobs on one Desktop connection share one Mcp-Session-Id — the
// 2026-09-17 incident's exact shape. On a URL-scoped mount they therefore share
// the row: one opening an experiment moves the other. That is bounded by the
// URL (it can never reach another knowledge base) and it is the only sense of
// "session" the wire exposes, so it is pinned rather than left to chance.
func TestMountExperiment_SharedConnectionSharesTheExperiment(t *testing.T) {
	h, m := experimentServer(t)
	mount := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, mount)

	// Job A opens. Job B never asked for an experiment.
	_, _, isErr := callExperiment(t, h, mount, sid, `{"action":"open","name":"shared"}`)
	require.False(t, isErr)

	learnOn(t, h, mount, sid, "job B write")

	require.True(t, branchHolding(t, m, "jobA-repo", "exp/shared", "job B write"),
		"a job sharing the connection follows the experiment — accepted, bounded by the URL's repo")
}

// TestMountExperiment_DifferentSessionsAreIndependent: two connections to the
// same mount are two sessions, and one entering an experiment must not move the
// other.
func TestMountExperiment_DifferentSessionsAreIndependent(t *testing.T) {
	h, m := experimentServer(t)
	mount := repoMount(t, m, "jobA-repo")
	agent := m.Get("jobA-repo").AgentBranch()

	sidA := initAt(t, h, mount)
	sidB := initAt(t, h, mount)
	require.NotEqual(t, sidA, sidB, "precondition: two distinct sessions")

	_, _, isErr := callExperiment(t, h, mount, sidA, `{"action":"open","name":"only-a"}`)
	require.False(t, isErr)

	learnOn(t, h, mount, sidB, "session B write")

	require.True(t, branchHolding(t, m, "jobA-repo", agent, "session B write"),
		"session B never opened an experiment and must stay on the URL's branch")
	require.False(t, branchHolding(t, m, "jobA-repo", "exp/only-a", "session B write"))
}

// TestMountExperiment_ExperimentURLMountIgnoresSessionState: an endpoint that
// NAMES an experiment is an address of that one. Letting session state redirect
// it would make …/exp:a/mcp serve exp/b — the misrouting this design exists to
// prevent, arriving through the door built to prevent it.
func TestMountExperiment_ExperimentURLMountIgnoresSessionState(t *testing.T) {
	h, m := experimentServer(t)
	plain := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, plain)

	// Create two experiments, and leave the session inside "a".
	for _, n := range []string{"expa", "expb"} {
		_, _, isErr := callExperiment(t, h, plain, sid, fmt.Sprintf(`{"action":"open","name":%q}`, n))
		require.False(t, isErr)
	}

	// Now talk to the mount that names expa, on the SAME session — whose
	// stored experiment is expb.
	expMount := "/repos/jobA-repo/branches/exp:expa/mcp"
	expSid := initAt(t, h, expMount)
	learnOn(t, h, expMount, expSid, "addressed to expa")

	require.True(t, branchHolding(t, m, "jobA-repo", "exp/expa", "addressed to expa"),
		"a mount that names an experiment serves THAT experiment")
	require.False(t, branchHolding(t, m, "jobA-repo", "exp/expb", "addressed to expa"),
		"session state must not redirect an endpoint that names its experiment by URL")
}

// TestMountExperiment_SessionRowNamesTheBranchItWritesTo: attribution. The
// sessions endpoint must state the branch the session actually writes to,
// STORED, not the one its URL names — a table reading the URL would confidently
// show the wrong branch.
func TestMountExperiment_SessionRowNamesTheBranchItWritesTo(t *testing.T) {
	h, m := experimentServer(t)
	mount := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, mount)

	_, _, isErr := callExperiment(t, h, mount, sid, `{"action":"open","name":"attributed"}`)
	require.False(t, isErr)
	learnOn(t, h, mount, sid, "attributed write")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body struct {
		Embedded struct {
			Sessions []struct {
				ID     string `json:"id"`
				Mounts []struct {
					Experiment string `json:"experiment"`
					Branch     string `json:"branch"`
				} `json:"mounts"`
			} `json:"sessions"`
		} `json:"_embedded"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))

	var found bool
	for _, s := range body.Embedded.Sessions {
		if s.ID != sid {
			continue
		}
		for _, mo := range s.Mounts {
			if mo.Experiment == "attributed" {
				require.Equal(t, "exp/attributed", mo.Branch,
					"the row must name the branch written to, not the one the URL names")
				found = true
			}
		}
	}
	require.True(t, found, "the session row must carry its mount's experiment: %s", rec.Body.String())
}
