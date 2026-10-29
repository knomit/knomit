package web

// #685: on a URL-scoped endpoint (a `kb --repo` or `kb --lens` bridge) the
// connection's DEFAULT binding never moves. `knomit_experiment open` without a
// handle mints one inside the experiment and returns it; calls without
// `binding` stay on the agent branch, whatever anyone on the connection opened.
// Subagents share their parent's MCP session, so a default that moved would move
// all of them: stale reads, and writes landing in someone else's experiment.
//
// These drive the real router. Every assertion is about WHERE A WRITE LANDED or
// WHAT A READ SAW, never about a summary string: a misrouted write SUCCEEDS —
// the experiment and its parent share an ontology by construction — so a test
// that checked the result text would pass on exactly the build that misroutes.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/engine/store"
)

// repoMount is the URL-scoped mount a `kb --repo <name>` bridge connects to.
func repoMount(t *testing.T, m *repos.Manager, repo string) string {
	t.Helper()
	ri := m.Get(repo)
	require.NotNil(t, ri, "no repo %q", repo)
	return "/repos/" + repo + "/branches/" + strings.ReplaceAll(ri.AgentBranch(), "/", ":") + "/mcp"
}

// learnOn writes a fact through the MCP mount with NO binding key: the
// connection default.
func learnOn(t *testing.T, h http.Handler, mount, sid, title string) {
	t.Helper()
	text, isErr := callToolAt(t, h, mount, sid, "knomit_learn", urlScopedLearnArgs(title))
	require.False(t, isErr, "learn failed on %s: %s", mount, text)
}

// urlScopedLearnArgs is learnArgs WITHOUT a `binding` key.
func urlScopedLearnArgs(title string) string {
	return fmt.Sprintf(
		`{"moment_name":"scoped-default","facts":[{"topic":"architecture","category":"demo","title":%q,"body":"written by the scoped-default tests"}]}`,
		title)
}

// learnPathVia writes one fact (through handle, or the default when it is "")
// and returns the path it was written at.
func learnPathVia(t *testing.T, h http.Handler, mount, sid, handle, title string) string {
	t.Helper()
	args := urlScopedLearnArgs(title)
	if handle != "" {
		args = learnArgs(handle, "demo", title)
	}
	text, isErr := callToolAt(t, h, mount, sid, "knomit_learn", args)
	require.False(t, isErr, "learn %q: %s", title, text)
	var res struct {
		Commits []struct {
			File string `json:"file"`
		} `json:"commits"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &res), text)
	require.Len(t, res.Commits, 1, text)
	return res.Commits[0].File
}

// readVia is a READ through the tool surface: knomit_changes over the whole
// write repo at the branch the call is bound to (handle, or the default when
// it is ""). Its text names every fact path that branch holds.
func readVia(t *testing.T, h http.Handler, mount, sid, handle string) string {
	t.Helper()
	args := `{"prefix":""}`
	if handle != "" {
		args = withBinding(handle, args)
	}
	text, isErr := callToolAt(t, h, mount, sid, "knomit_changes", args)
	require.False(t, isErr, "read on %s: %s", mount, text)
	return text
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

// openNoHandle opens an experiment with NO `binding` and returns the handle
// the result hands back.
func openNoHandle(t *testing.T, h http.Handler, mount, sid, name string) string {
	t.Helper()
	env, text, isErr := callExperiment(t, h, mount, sid, fmt.Sprintf(`{"action":"open","name":%q}`, name))
	require.False(t, isErr, "open %s: %s", name, text)
	require.NotEmpty(t, env.Binding, "open without a handle must RETURN one: %s", text)
	require.Equal(t, name, env.Active, "the returned handle is inside the experiment: %s", text)
	require.Equal(t, "exp/"+name, env.Branch, text)
	return env.Binding
}

// After open, a default-binding write and read are on the agent branch; the
// same calls through the returned handle are on exp/<name>. On a repo mount and
// on a lens mount (where only the WRITE member moves, and only for the handle).
func TestScopedDefault_OpenLeavesTheDefaultOnTheAgentBranch(t *testing.T) {
	for _, c := range []struct{ name string }{{"repo"}, {"lens"}} {
		t.Run(c.name, func(t *testing.T) {
			h, m := experimentServer(t)
			mount := repoMount(t, m, "jobA-repo")
			if c.name == "lens" {
				_, err := m.LensRegistry().Create(control.Lens{
					Name: "demo-lens", WriteUID: m.Get("jobA-repo").UID(),
					Reads: []control.LensRead{{RepoUID: m.Get("jobA-repo").UID()}, {RepoUID: m.Get("jobB-repo").UID()}},
				})
				require.NoError(t, err)
				mount = "/lenses/demo-lens/mcp"
			}
			sid := initAt(t, h, mount)

			handle := openNoHandle(t, h, mount, sid, "work")

			defPath := learnPathVia(t, h, mount, sid, "", "default after open")
			expPath := learnPathVia(t, h, mount, sid, handle, "handle after open")

			require.True(t, branchHolding(t, m, "jobA-repo", "agent/test", "default after open"),
				"a write without `binding` must land on the agent branch")
			require.False(t, branchHolding(t, m, "jobA-repo", "exp/work", "default after open"),
				"a write without `binding` reached the experiment: the default moved")
			require.True(t, branchHolding(t, m, "jobA-repo", "exp/work", "handle after open"))
			require.False(t, branchHolding(t, m, "jobA-repo", "agent/test", "handle after open"))
			require.False(t, branchHolding(t, m, "jobB-repo", "agent/test", "handle after open"),
				"a lens read member is untouched")

			def := readVia(t, h, mount, sid, "")
			require.Contains(t, def, defPath, "the default READ is the agent branch")
			require.NotContains(t, def, expPath, "the default read saw the experiment")
			viaHandle := readVia(t, h, mount, sid, handle)
			require.Contains(t, viaHandle, expPath, "the handle reads its experiment")
			require.NotContains(t, viaHandle, defPath, "the experiment forked before the default's write")

			// list without a handle marks no experiment as the caller's.
			env, text, isErr := callExperiment(t, h, mount, sid, `{"action":"list"}`)
			require.False(t, isErr, text)
			require.Empty(t, env.Active, text)
			require.Equal(t, "agent/test", env.Branch, text)
			for _, e := range env.Experiments {
				require.False(t, e.Current, "no experiment is current for the default: %s", text)
			}
		})
	}
}

// Two handles opened on ONE session (subagents sharing their parent's MCP
// connection) in two different experiments do not interfere: each write goes
// to its own experiment, the default to neither, and one committing leaves the
// other inside its own.
func TestScopedDefault_TwoOpenedHandlesAreIndependent(t *testing.T) {
	h, m := experimentServer(t)
	mount := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, mount)

	h1 := openNoHandle(t, h, mount, sid, "e-one")
	h2 := openNoHandle(t, h, mount, sid, "e-two")
	require.NotEqual(t, h1, h2, "each open mints its own handle")

	learnPathVia(t, h, mount, sid, h1, "one writes")
	learnPathVia(t, h, mount, sid, h2, "two writes")
	learnOn(t, h, mount, sid, "default writes")

	require.True(t, branchHolding(t, m, "jobA-repo", "exp/e-one", "one writes"))
	require.False(t, branchHolding(t, m, "jobA-repo", "exp/e-two", "one writes"))
	require.True(t, branchHolding(t, m, "jobA-repo", "exp/e-two", "two writes"))
	require.False(t, branchHolding(t, m, "jobA-repo", "exp/e-one", "two writes"),
		"the second open moved the first handle")
	require.True(t, branchHolding(t, m, "jobA-repo", "agent/test", "default writes"))

	env, text, isErr := callExperiment(t, h, mount, sid, withBinding(h1, `{"action":"commit"}`))
	require.False(t, isErr, text)
	require.Empty(t, env.Active, text)

	learnPathVia(t, h, mount, sid, h2, "two after one commits")
	require.True(t, branchHolding(t, m, "jobA-repo", "exp/e-two", "two after one commits"),
		"h1's commit moved h2")
	learnOn(t, h, mount, sid, "default after one commits")
	require.True(t, branchHolding(t, m, "jobA-repo", "agent/test", "default after one commits"))
	require.False(t, branchHolding(t, m, "jobA-repo", "exp/e-two", "default after one commits"))
}

// commit, rollback and sync with neither a `name` nor a handle are REFUSED —
// the default is never inside an experiment, so there is nothing to act on —
// and the refusal changes nothing.
func TestScopedDefault_CommitRollbackSyncNeedANameOrAHandle(t *testing.T) {
	h, m := experimentServer(t)
	mount := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, mount)
	handle := openNoHandle(t, h, mount, sid, "kept")
	learnPathVia(t, h, mount, sid, handle, "still in kept")

	for _, action := range []string{"commit", "rollback", "sync"} {
		text, isErr := callToolAt(t, h, mount, sid, "knomit_experiment", fmt.Sprintf(`{"action":%q}`, action))
		require.True(t, isErr, "%s without a name or a handle must be refused, got: %s", action, text)
		require.Equal(t, fmt.Sprintf("knomit_experiment %s needs a `name`, or the `binding` handle knomit_experiment open returned. "+
			"A call without a handle is on the default branch, which is never inside an experiment", action), text)
	}
	require.True(t, branchHolding(t, m, "jobA-repo", "exp/kept", "still in kept"), "a refusal must change nothing")
	require.False(t, branchHolding(t, m, "jobA-repo", "agent/test", "still in kept"), "a refused commit merged anyway")

	// The same on the unscoped endpoint, for a handle that is in no experiment.
	sidU := initExperimentSession(t, h)
	plain := bindAt(t, h, unscopedMount, sidU, `{"repo":"jobA-repo"}`)
	text, isErr := callToolAt(t, h, unscopedMount, sidU, "knomit_experiment", withBinding(plain, `{"action":"commit"}`))
	require.True(t, isErr, text)
	require.Contains(t, text, "needs a `name`")

	// With the handle, or the name, they act.
	_, text, isErr = callExperiment(t, h, mount, sid, withBinding(handle, `{"action":"sync"}`))
	require.False(t, isErr, text)
	_, text, isErr = callExperiment(t, h, mount, sid, `{"action":"commit","name":"kept"}`)
	require.False(t, isErr, text)
	require.True(t, branchHolding(t, m, "jobA-repo", "agent/test", "still in kept"))
}

// THE #685 REPRO. One agent opens an experiment on a shared --repo connection;
// another commits its own experiment to the agent branch. A default-binding
// read must SEE the newly committed fact — before the fix it read the first
// agent's experiment, forked before that commit, and found nothing.
func TestScopedDefault_AgentBranchCommitAfterOpenIsVisibleOnTheDefault(t *testing.T) {
	h, m := experimentServer(t)
	mount := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, mount)

	// Agent X opens without a handle, as the issue's first agent did.
	hx := openNoHandle(t, h, mount, sid, "feat-ui-view")

	// Agent Y, on the same session, works in its own experiment and commits it.
	hy := openNoHandle(t, h, mount, sid, "feat-rest-cursor")
	landed := learnPathVia(t, h, mount, sid, hy, "committed by Y")
	_, text, isErr := callExperiment(t, h, mount, sid, withBinding(hy, `{"action":"commit"}`))
	require.False(t, isErr, text)

	require.Contains(t, readVia(t, h, mount, sid, ""), landed,
		"a default-binding read must see a fact committed to the agent branch after another agent's open")
	require.NotContains(t, readVia(t, h, mount, sid, hx), landed,
		"precondition: X's experiment forked before Y's commit, which is what made the old default read stale")

	env, text, isErr := callExperiment(t, h, mount, sid, `{"action":"list"}`)
	require.False(t, isErr, text)
	require.Equal(t, "agent/test", env.Branch, "the default is not inside X's experiment: %s", text)
}

// The unscoped endpoint is unchanged: `open` moves the handle it was called
// with (and names it back), and a second handle stays where it was.
func TestScopedDefault_UnscopedOpenMovesOnlyThePassedHandle(t *testing.T) {
	h, m := experimentServer(t)
	sid := initExperimentSession(t, h)
	a := bindAt(t, h, unscopedMount, sid, `{"repo":"jobA-repo"}`)
	b := bindAt(t, h, unscopedMount, sid, `{"repo":"jobA-repo"}`)

	env, text, isErr := callExperiment(t, h, unscopedMount, sid, withBinding(a, `{"action":"open","name":"u-exp"}`))
	require.False(t, isErr, text)
	require.Equal(t, a, env.Binding, "on the unscoped endpoint open moves the caller's own handle")
	require.Equal(t, "u-exp", env.Active)

	learnPathVia(t, h, unscopedMount, sid, a, "A unscoped")
	learnPathVia(t, h, unscopedMount, sid, b, "B unscoped")
	require.True(t, branchHolding(t, m, "jobA-repo", "exp/u-exp", "A unscoped"))
	require.True(t, branchHolding(t, m, "jobA-repo", "agent/test", "B unscoped"))
	require.False(t, branchHolding(t, m, "jobA-repo", "exp/u-exp", "B unscoped"))
}

// An experiment that vanishes out of band (the REST rollback, the UI, the
// sweeper) under the handle open returned falls back to the agent branch and
// SAYS SO on that handle; the default, which was never inside it, is not told
// it was moved.
func TestScopedDefault_OutOfBandRollbackHealsTheHandle(t *testing.T) {
	h, m := experimentServer(t)
	mount := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, mount)
	handle := openNoHandle(t, h, mount, sid, "vanishing")

	ri := m.Get("jobA-repo")
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Merge().RollbackExperiment(context.Background(), "vanishing"))
	}))

	learnPathVia(t, h, mount, sid, handle, "after it vanished")
	require.True(t, branchHolding(t, m, "jobA-repo", "agent/test", "after it vanished"),
		"a vanished experiment must fall back to the agent branch, not fail the call")

	viaHandle := reposAt(t, h, mount, sid, withBinding(handle, `{}`))
	require.Equal(t, "vanishing", viaHandle.Bound.LapsedExperiment, "the handle is told it was moved")
	plain := reposAt(t, h, mount, sid, `{}`)
	require.Empty(t, plain.Bound.LapsedExperiment, "the default was never inside it")
}

// A mount whose URL NAMES an experiment serves that experiment, whatever any
// handle on the same session is inside.
func TestScopedDefault_ExperimentURLMountServesItsOwnExperiment(t *testing.T) {
	h, m := experimentServer(t)
	plain := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, plain)
	openNoHandle(t, h, plain, sid, "expa")
	openNoHandle(t, h, plain, sid, "expb")

	expMount := "/repos/jobA-repo/branches/exp:expa/mcp"
	learnOn(t, h, expMount, sid, "addressed to expa")
	require.True(t, branchHolding(t, m, "jobA-repo", "exp/expa", "addressed to expa"))
	require.False(t, branchHolding(t, m, "jobA-repo", "exp/expb", "addressed to expa"))
}

// A mount whose URL names a branch this instance cannot write (main) refuses
// the write: nothing re-pins it anywhere.
func TestScopedDefault_NonWritableURLBranchStaysRefused(t *testing.T) {
	h, m := experimentServer(t)
	plain := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, plain)
	openNoHandle(t, h, plain, sid, "notformain")

	mainMount := "/repos/jobA-repo/branches/main/mcp"
	out, isErr := callToolAt(t, h, mainMount, sid, "knomit_learn", urlScopedLearnArgs("aimed at main"))
	require.True(t, isErr, "a write on a non-writable mount must be REFUSED, got: %s", out)
	for _, br := range []string{"main", "exp/notformain", m.Get("jobA-repo").AgentBranch()} {
		require.False(t, branchHolding(t, m, "jobA-repo", br, "aimed at main"),
			"the refused write must land on NO branch, and it reached %s", br)
	}
}

// markScopedMount never changes the binding: it only marks the mount as one a
// handle may steer, on the repo's own agent-branch route, and leaves every
// other route unmarked.
func TestMarkScopedMount_MarksWithoutRebinding(t *testing.T) {
	_, m := experimentServer(t)
	ri := m.Get("jobA-repo")

	agentCtx := repos.WithRepoInstance(repos.WithBranch(context.Background(), ri.AgentBranch()), ri)
	got := markScopedMount(agentCtx, m, "sid")
	_, uid, plain := repos.MountSessionFromContext(got)
	require.True(t, plain, "the agent-branch route takes a handle")
	require.Equal(t, repos.PinForRepo(ri), uid)
	_, rebound := repos.BindingFromContextOpt(got)
	require.False(t, rebound, "the marker must not install a binding of its own")

	mainCtx := repos.WithRepoInstance(repos.WithBranch(context.Background(), "main"), ri)
	_, _, plain = repos.MountSessionFromContext(markScopedMount(mainCtx, m, "sid"))
	require.False(t, plain, "a URL naming main is an address")
}
