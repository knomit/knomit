package web

// #349 extended to knomit_experiment (rehearsal finding F10): the agent's
// `trace` on open/commit/sync/rollback, end to end over the real router and
// MCP transport.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
	"knomit/internal/store"
)

func headIn(t *testing.T, m *repos.Manager, repo, branch string) string {
	t.Helper()
	var h string
	require.NoError(t, m.Get(repo).WithRead(func(svc *store.Service) {
		got, err := svc.Branches().HeadCommit(context.Background(), branch)
		if err == nil {
			h = got
		}
	}))
	return h
}

func writeAgentSide(t *testing.T, m *repos.Manager, repo, path string) {
	t.Helper()
	ri := m.Get(repo)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(context.Background(), ri.AgentBranch(), path,
			"---\ntype: observation\nconfidence: 0.8\nsources: 1\n---\n# "+path+"\n\nbody\n", "add "+path, "learn")
		require.NoError(t, err)
	}))
}

// TestExperimentTrace_E2E_StampsItsMergesOnly: the trace passed to
// knomit_experiment is stamped on the merge commit `sync` writes on the
// experiment and the one `commit` writes on the agent branch, as the last
// trailer paragraph; `open` makes no commit; a fast-forward `commit` makes no
// new commit (the fact commits keep the trace their writes carried); the same
// actions WITHOUT a trace stamp nothing (D-mint). Sabotage: drop applyTrace
// from ExperimentHandler, or drop mergeOpts.trace from mergeExperiment — red.
func TestExperimentTrace_E2E_StampsItsMergesOnly(t *testing.T) {
	h, m := experimentServer(t)
	sid := initExperimentSession(t, h)
	handle := bindHandle(t, h, sid, "jobA-repo")
	call := func(name, args string) (string, bool) { return callToolAt(t, h, unscopedMount, sid, name, args) }
	agent := m.Get("jobA-repo").AgentBranch()
	tr := map[string]string{"Knomit-Trace": "task-9", "Knomit-Run": e2eRun}

	// Traced: open (no commit), a traced write, sync (merge), commit (merge).
	before := headIn(t, m, "jobA-repo", agent)
	_, text, isErr := callExperiment(t, h, unscopedMount, sid, fmt.Sprintf(`{"binding":%q,"action":"open","name":"traced"%s}`, handle, traceJSON(tr)))
	require.False(t, isErr, text)
	require.Equal(t, before, headIn(t, m, "jobA-repo", "exp/traced"), "open makes no commit")
	_, factHash := learnTraced(t, call, handle, "Experiment fact", tr, "")
	require.Equal(t, "task-9", store.TrailerValue(commitMessageIn(t, m, "jobA-repo", factHash), store.TrailerTrace),
		"fixture: the fact commit carries its own write's trace")

	writeAgentSide(t, m, "jobA-repo", "kb/architecture/side/one.md")
	_, text, isErr = callExperiment(t, h, unscopedMount, sid, fmt.Sprintf(`{"binding":%q,"action":"sync"%s}`, handle, traceJSON(tr)))
	require.False(t, isErr, text)
	syncMsg := commitMessageIn(t, m, "jobA-repo", headIn(t, m, "jobA-repo", "exp/traced"))
	require.True(t, strings.HasPrefix(syncMsg, "merge: "+agent+" into exp/traced"), "fixture: sync wrote a merge commit: %q", syncMsg)
	require.True(t, strings.HasSuffix(syncMsg, "\n\nKnomit-Trace: task-9\nKnomit-Run: "+e2eRun+"\n"), "sync's merge carries the trace: %q", syncMsg)

	writeAgentSide(t, m, "jobA-repo", "kb/architecture/side/two.md")
	_, text, isErr = callExperiment(t, h, unscopedMount, sid, fmt.Sprintf(`{"binding":%q,"action":"commit","name":"traced"%s}`, handle, traceJSON(tr)))
	require.False(t, isErr, text)
	commitMsg := commitMessageIn(t, m, "jobA-repo", headIn(t, m, "jobA-repo", agent))
	require.True(t, strings.HasPrefix(commitMsg, "merge: exp/traced into "+agent), "fixture: commit wrote a merge commit: %q", commitMsg)
	require.Equal(t, "task-9", store.TrailerValue(commitMsg, store.TrailerTrace))
	require.Equal(t, e2eRun, store.TrailerValue(commitMsg, store.TrailerRun))

	// Fast-forward: no new commit, so nothing new to stamp.
	_, text, isErr = callExperiment(t, h, unscopedMount, sid, fmt.Sprintf(`{"binding":%q,"action":"open","name":"ff"%s}`, handle, traceJSON(tr)))
	require.False(t, isErr, text)
	_, ffHash := learnTraced(t, call, handle, "Fast fact", tr, "")
	_, text, isErr = callExperiment(t, h, unscopedMount, sid, fmt.Sprintf(`{"binding":%q,"action":"commit","name":"ff"%s}`, handle, traceJSON(tr)))
	require.False(t, isErr, text)
	require.Equal(t, ffHash, headIn(t, m, "jobA-repo", agent), "a fast-forward lands the fact commit itself, no merge commit")

	// Untraced: the same merge carries no paragraph of any kind (D-mint).
	_, text, isErr = callExperiment(t, h, unscopedMount, sid, fmt.Sprintf(`{"binding":%q,"action":"open","name":"plain"}`, handle))
	require.False(t, isErr, text)
	learnTraced(t, call, handle, "Plain fact", nil, "")
	writeAgentSide(t, m, "jobA-repo", "kb/architecture/side/three.md")
	_, text, isErr = callExperiment(t, h, unscopedMount, sid, fmt.Sprintf(`{"binding":%q,"action":"commit","name":"plain"}`, handle))
	require.False(t, isErr, text)
	plainMsg := commitMessageIn(t, m, "jobA-repo", headIn(t, m, "jobA-repo", agent))
	require.True(t, strings.HasPrefix(plainMsg, "merge: exp/plain into "), "fixture: a merge commit: %q", plainMsg)
	require.NotContains(t, plainMsg, "\n\n", "no trace passed: no paragraph (D-mint): %q", plainMsg)
}

// TestExperimentTrace_E2E_BadTraceRefusesTheWholeCall: an entry the write
// tools refuse is refused on every knomit_experiment action, with the same
// message, and NOTHING happens — open creates no branch, commit leaves both
// tips and the experiment, rollback leaves it in place. Sabotage: validate
// after the action (red: the branch exists / the experiment is gone).
func TestExperimentTrace_E2E_BadTraceRefusesTheWholeCall(t *testing.T) {
	h, m := experimentServer(t)
	sid := initExperimentSession(t, h)
	handle := bindHandle(t, h, sid, "jobA-repo")
	call := func(name, args string) (string, bool) { return callToolAt(t, h, unscopedMount, sid, name, args) }
	agent := m.Get("jobA-repo").AgentBranch()
	bad := `,"trace":{"Knomit-Trigger":"wake"}`

	_, text, isErr := callExperiment(t, h, unscopedMount, sid, fmt.Sprintf(`{"binding":%q,"action":"open","name":"refused"%s}`, handle, bad))
	require.True(t, isErr, text)
	require.Contains(t, text, "Knomit-Trigger is reserved")
	require.Empty(t, headIn(t, m, "jobA-repo", "exp/refused"), "a refused open creates no branch")

	_, text, isErr = callExperiment(t, h, unscopedMount, sid, fmt.Sprintf(`{"binding":%q,"action":"open","name":"kept"}`, handle))
	require.False(t, isErr, text)
	learnTraced(t, call, handle, "Kept fact", nil, "")
	writeAgentSide(t, m, "jobA-repo", "kb/architecture/side/k.md")
	agentBefore, expBefore := headIn(t, m, "jobA-repo", agent), headIn(t, m, "jobA-repo", "exp/kept")
	for _, action := range []string{"commit", "sync", "rollback"} {
		_, text, isErr = callExperiment(t, h, unscopedMount, sid, fmt.Sprintf(`{"binding":%q,"action":%q,"name":"kept"%s}`, handle, action, bad))
		require.True(t, isErr, "%s: %s", action, text)
		require.Contains(t, text, "Knomit-Trigger is reserved", action)
		require.Equal(t, agentBefore, headIn(t, m, "jobA-repo", agent), "%s: the agent branch did not move", action)
		require.Equal(t, expBefore, headIn(t, m, "jobA-repo", "exp/kept"), "%s: the experiment is untouched and still there", action)
	}
}
