package web

// End-to-end tests for knomit_bind and `binding` on a URL-SCOPED endpoint (a
// `kb --repo` or `kb --lens` bridge), through the real router.
//
// The rule under test: a scoped endpoint behaves exactly like the unscoped one,
// except that every bind, and every handle it accepts, is confined to the
// connection's own repo or lens. Several agents sharing one connection must each
// be able to hold their own handle and their own experiment.

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

// bindAt calls knomit_bind on mount and returns the handle.
func bindAt(t *testing.T, h http.Handler, mount, sid, args string) string {
	t.Helper()
	text, isErr := callToolAt(t, h, mount, sid, "knomit_bind", args)
	require.False(t, isErr, "bind on %s %s: %s", mount, args, text)
	var envelope struct {
		Binding string `json:"binding"`
	}
	require.NoError(t, json.NewDecoder(strings.NewReader(text)).Decode(&envelope), "bind result: %s", text)
	require.NotEmpty(t, envelope.Binding, "bind result: %s", text)
	return envelope.Binding
}

// learnVia writes one fact on mount, through handle when it is non-empty and
// with NO binding key when it is "".
func learnVia(t *testing.T, h http.Handler, mount, sid, handle, title string) {
	t.Helper()
	args := urlScopedLearnArgs(title)
	if handle != "" {
		args = learnArgs(handle, "demo", title)
	}
	text, isErr := callToolAt(t, h, mount, sid, "knomit_learn", args)
	require.False(t, isErr, "learn %q on %s: %s", title, mount, text)
}

// withBinding adds a `binding` key to a JSON object argument string.
func withBinding(handle, args string) string {
	if args == "{}" {
		return fmt.Sprintf(`{"binding":%q}`, handle)
	}
	return fmt.Sprintf(`{"binding":%q,`, handle) + strings.TrimPrefix(args, "{")
}

// openOutOfBand opens an experiment the way the REST route does, with no
// handle or session involved.
func openOutOfBand(t *testing.T, m *repos.Manager, repo, name string) {
	t.Helper()
	ri := m.Get(repo)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, err := svc.Merge().OpenExperiment(context.Background(), name, "", ri.AgentBranch())
		require.NoError(t, err)
	}))
}

// A --repo bridge binds to another branch of its own repo: an existing
// experiment. Writes through the handle land there; writes without it stay on
// the branch the URL names.
func TestScopedBind_BindsAnotherBranchOfItsOwnRepo(t *testing.T) {
	h, m := experimentServer(t)
	mount := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, mount)
	openOutOfBand(t, m, "jobA-repo", "side")

	handle := bindAt(t, h, mount, sid, `{"experiment":"side"}`)
	learnVia(t, h, mount, sid, handle, "through the scoped handle")
	learnVia(t, h, mount, sid, "", "through the URL")

	require.True(t, branchHolding(t, m, "jobA-repo", "exp/side", "through the scoped handle"))
	require.False(t, branchHolding(t, m, "jobA-repo", "agent/test", "through the scoped handle"))
	require.True(t, branchHolding(t, m, "jobA-repo", "agent/test", "through the URL"))
	require.False(t, branchHolding(t, m, "jobA-repo", "exp/side", "through the URL"))

	// A plain bind (no arguments) is on the default branch.
	plain := bindAt(t, h, mount, sid, `{}`)
	learnVia(t, h, mount, sid, plain, "through the plain handle")
	require.True(t, branchHolding(t, m, "jobA-repo", "agent/test", "through the plain handle"))
}

// Naming another repo or lens is the one thing a scoped knomit_bind cannot do,
// and the refusal says who can fix it and that retrying will not.
func TestScopedBind_AnotherRepoOrLensIsRefused(t *testing.T) {
	h, m := experimentServer(t)
	_, err := m.LensRegistry().Create(control.Lens{
		Name: "pair", WriteUID: m.Get("jobA-repo").UID(),
		Reads: []control.LensRead{{RepoUID: m.Get("jobA-repo").UID()}, {RepoUID: m.Get("jobB-repo").UID()}},
	})
	require.NoError(t, err)

	repoM := repoMount(t, m, "jobA-repo")
	lensM := "/lenses/pair/mcp"
	cases := []struct {
		mount, args, want string
	}{
		{repoM, `{"repo":"jobB-repo"}`,
			`This connection is fixed to repo jobA-repo; knomit_bind here only changes the branch. To use another repo, the user must restart the bridge with --repo jobB-repo. Do not retry.`},
		{repoM, `{"lens":"pair"}`,
			`This connection is fixed to repo jobA-repo; knomit_bind here only changes the branch. To use another lens, the user must restart the bridge with --lens pair. Do not retry.`},
		// The lens's own write repo is still ANOTHER target than the lens.
		{lensM, `{"repo":"jobA-repo"}`,
			`This connection is fixed to lens pair; knomit_bind here only changes the branch. To use another repo, the user must restart the bridge with --repo jobA-repo. Do not retry.`},
	}
	for _, c := range cases {
		text, isErr := callToolAt(t, h, c.mount, initAt(t, h, c.mount), "knomit_bind", c.args)
		require.True(t, isErr, "%s %s: %s", c.mount, c.args, text)
		require.Equal(t, c.want, text)
	}

	// Naming the connection's OWN target is harmless and binds.
	bindAt(t, h, repoM, initAt(t, h, repoM), `{"repo":"jobA-repo"}`)
	bindAt(t, h, lensM, initAt(t, h, lensM), `{"lens":"pair"}`)
}

// A mount whose URL names a specific branch (here an experiment) is an
// address: neither knomit_bind nor a handle may move it.
func TestScopedBind_AddressedBranchTakesNoBind(t *testing.T) {
	h, m := experimentServer(t)
	openOutOfBand(t, m, "jobA-repo", "addressed")
	mount := "/repos/jobA-repo/branches/exp:addressed/mcp"
	sid := initAt(t, h, mount)

	text, isErr := callToolAt(t, h, mount, sid, "knomit_bind", `{}`)
	require.True(t, isErr, text)
	require.Contains(t, text, "Do not retry")

	text, isErr = callToolAt(t, h, mount, sid, "knomit_query", `{"binding":"anything","text":"x"}`)
	require.True(t, isErr, text)
	require.Contains(t, text, "by its URL")
}

// THE ISOLATION TEST. Two handles on ONE scoped connection — one session id,
// the shape of subagents sharing a bridge. Opening an experiment through A
// moves A alone: B's writes stay on B's branch, and so do the writes of a
// caller on the same connection that passes no handle and is inside its OWN
// session-keyed experiment. A committing must not pull that caller out.
func TestScopedBind_TwoHandlesOnOneConnectionAreIsolated(t *testing.T) {
	h, m := experimentServer(t)
	mount := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, mount)

	// The handle-less caller on this connection enters its own experiment.
	_, text, isErr := callExperiment(t, h, mount, sid, `{"action":"open","name":"plain-exp"}`)
	require.False(t, isErr, text)

	a := bindAt(t, h, mount, sid, `{}`)
	b := bindAt(t, h, mount, sid, `{}`)
	require.NotEqual(t, a, b)

	envA, text, isErr := callExperiment(t, h, mount, sid, withBinding(a, `{"action":"open","name":"a-work"}`))
	require.False(t, isErr, text)
	require.Equal(t, "a-work", envA.Active)

	learnVia(t, h, mount, sid, b, "B writes")
	learnVia(t, h, mount, sid, a, "A writes")
	learnVia(t, h, mount, sid, "", "plain writes")

	require.True(t, branchHolding(t, m, "jobA-repo", "agent/test", "B writes"), "B's write must land on B's branch")
	require.False(t, branchHolding(t, m, "jobA-repo", "exp/a-work", "B writes"), "B's write reached A's experiment")
	require.True(t, branchHolding(t, m, "jobA-repo", "exp/a-work", "A writes"))
	require.True(t, branchHolding(t, m, "jobA-repo", "exp/plain-exp", "plain writes"),
		"a handle-less call keeps the connection's own experiment")
	require.False(t, branchHolding(t, m, "jobA-repo", "exp/a-work", "plain writes"))

	// A commits. The connection's own experiment must survive it.
	_, text, isErr = callExperiment(t, h, mount, sid, withBinding(a, `{"action":"commit"}`))
	require.False(t, isErr, text)
	learnVia(t, h, mount, sid, "", "plain after A commits")
	require.True(t, branchHolding(t, m, "jobA-repo", "exp/plain-exp", "plain after A commits"),
		"A's commit cleared the connection's session-keyed experiment")
}

// A handle works only on the endpoint that minted it.
func TestScopedBind_ForeignHandlesAreRefused(t *testing.T) {
	h, m := experimentServer(t)
	_, err := m.LensRegistry().Create(control.Lens{
		Name: "pair", WriteUID: m.Get("jobA-repo").UID(),
		Reads: []control.LensRead{{RepoUID: m.Get("jobA-repo").UID()}},
	})
	require.NoError(t, err)
	mountA := repoMount(t, m, "jobA-repo")
	mountB := repoMount(t, m, "jobB-repo")
	lensM := "/lenses/pair/mcp"
	sidA, sidB, sidL := initAt(t, h, mountA), initAt(t, h, mountB), initAt(t, h, lensM)
	sidU := initExperimentSession(t, h)

	unscopedA := bindAt(t, h, unscopedMount, sidU, `{"repo":"jobA-repo"}`)
	scopedA := bindAt(t, h, mountA, sidA, `{}`)
	scopedL := bindAt(t, h, lensM, sidL, `{}`)

	wantOther := func(kind, name string) string {
		return fmt.Sprintf("this binding handle belongs to another endpoint — omit `binding` to use this connection's %s %q, "+
			"or call knomit_bind on this connection for a handle", kind, name)
	}
	for _, c := range []struct {
		why, mount, sid, handle, want string
	}{
		{"unscoped handle for the SAME repo, on its scoped mount", mountA, sidA, unscopedA, wantOther("repo", "jobA-repo")},
		{"another repo's scoped handle", mountB, sidB, scopedA, wantOther("repo", "jobB-repo")},
		{"the lens's handle on its write repo's mount", mountA, sidA, scopedL, wantOther("repo", "jobA-repo")},
		{"a repo mount's handle on the lens", lensM, sidL, scopedA, wantOther("lens", "pair")},
		{"a scoped handle on the unscoped endpoint", unscopedMount, sidU, scopedA,
			"this binding handle was minted on a URL-scoped endpoint (a --repo or --lens bridge) and works only there — call knomit_bind on this endpoint"},
		{"a handle nobody minted", mountA, sidA, "Zm9yZ2VkLWhhbmRsZS0yNC1ieXRlcyEh", wantUnknownHandle},
	} {
		text, isErr := callToolAt(t, h, c.mount, c.sid, "knomit_learn", learnArgs(c.handle, "demo", "foreign "+c.why))
		require.True(t, isErr, "%s: must be refused, got %s", c.why, text)
		require.Equal(t, c.want, text, c.why)
	}
	for _, br := range []string{"agent/test"} {
		for _, repo := range []string{"jobA-repo", "jobB-repo"} {
			require.False(t, branchHolding(t, m, repo, br, "foreign "), "a refused write landed on %s %s", repo, br)
		}
	}
}

// Calls without `binding` behave as before on a scoped endpoint, and "" counts
// as absent on BOTH endpoint kinds.
func TestScopedBind_AbsentAndEmptyBinding(t *testing.T) {
	h, m := experimentServer(t)
	mount := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, mount)

	learnVia(t, h, mount, sid, "", "no binding key")
	text, isErr := callToolAt(t, h, mount, sid, "knomit_learn", learnArgs("", "demo", "empty binding"))
	require.False(t, isErr, "binding \"\" on a scoped endpoint is absent: %s", text)
	require.True(t, branchHolding(t, m, "jobA-repo", "agent/test", "no binding key"))
	require.True(t, branchHolding(t, m, "jobA-repo", "agent/test", "empty binding"))

	sidU := initExperimentSession(t, h)
	text, isErr = callToolAt(t, h, unscopedMount, sidU, "knomit_learn", learnArgs("", "demo", "unscoped empty"))
	require.True(t, isErr, text)
	require.Equal(t, wantMissingHandle, text, "\"\" on the unscoped endpoint is a missing handle")

	text, isErr = callToolAt(t, h, unscopedMount, sidU, "knomit_repos", `{"binding":""}`)
	require.False(t, isErr, text)
	require.NotContains(t, text, `"bound"`, "\"\" on the unscoped knomit_repos is no handle")
}

// reposView is the part of a knomit_repos result these tests read.
type reposView struct {
	Repos []struct {
		Name string `json:"name"`
	} `json:"repos"`
	Bound *struct {
		Name   string `json:"name"`
		Status string `json:"status"`
		Mounts []struct {
			Name        string `json:"name"`
			Branch      string `json:"branch"`
			WriteBranch string `json:"write_branch"`
			Experiment  string `json:"experiment"`
		} `json:"mounts"`
	} `json:"bound"`
	Endpoint *struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"endpoint"`
}

func reposAt(t *testing.T, h http.Handler, mount, sid, args string) reposView {
	t.Helper()
	text, isErr := callToolAt(t, h, mount, sid, "knomit_repos", args)
	require.False(t, isErr, "knomit_repos on %s must answer: %s", mount, text)
	var v reposView
	require.NoError(t, json.Unmarshal([]byte(text), &v), text)
	return v
}

// knomit_repos always answers on a scoped endpoint: the catalogue, the fixed
// target, and what a handle names — or why it does not resolve.
func TestScopedBind_ReposAnswers(t *testing.T) {
	h, m := experimentServer(t)
	mount := repoMount(t, m, "jobA-repo")
	sid := initAt(t, h, mount)

	v := reposAt(t, h, mount, sid, `{}`)
	require.Len(t, v.Repos, 2)
	require.NotNil(t, v.Endpoint)
	require.Equal(t, "repo", v.Endpoint.Kind)
	require.Equal(t, "jobA-repo", v.Endpoint.Name)
	require.NotNil(t, v.Bound)
	require.Equal(t, "jobA-repo", v.Bound.Name)
	require.Equal(t, "agent/test", v.Bound.Mounts[0].WriteBranch)

	handle := bindAt(t, h, mount, sid, `{}`)
	_, text, isErr := callExperiment(t, h, mount, sid, withBinding(handle, `{"action":"open","name":"seen"}`))
	require.False(t, isErr, text)
	v = reposAt(t, h, mount, sid, withBinding(handle, `{}`))
	require.Equal(t, "exp/seen", v.Bound.Mounts[0].WriteBranch, "bound describes the HANDLE")
	require.Equal(t, "seen", v.Bound.Mounts[0].Experiment)
	require.Equal(t, "jobA-repo", v.Endpoint.Name)

	v = reposAt(t, h, mount, sid, `{"binding":"never-minted"}`)
	require.Equal(t, "unresolvable", v.Bound.Status, "a bad handle is reported, not raised")
	require.Len(t, v.Repos, 2)
	require.Equal(t, "jobA-repo", v.Endpoint.Name)

	// The unscoped endpoint has no fixed target.
	v = reposAt(t, h, unscopedMount, initExperimentSession(t, h), `{}`)
	require.Nil(t, v.Endpoint)
}

// On a lens, a handle's experiment moves ONLY the write repo; the read mounts
// stay where they are.
func TestScopedBind_LensHandleMovesOnlyTheWriteRepo(t *testing.T) {
	h, m := experimentServer(t)
	_, err := m.LensRegistry().Create(control.Lens{
		Name: "pair", WriteUID: m.Get("jobA-repo").UID(),
		Reads: []control.LensRead{{RepoUID: m.Get("jobA-repo").UID()}, {RepoUID: m.Get("jobB-repo").UID()}},
	})
	require.NoError(t, err)
	mount := "/lenses/pair/mcp"
	sid := initAt(t, h, mount)

	handle := bindAt(t, h, mount, sid, `{}`)
	_, text, isErr := callExperiment(t, h, mount, sid, withBinding(handle, `{"action":"open","name":"lx"}`))
	require.False(t, isErr, text)

	v := reposAt(t, h, mount, sid, withBinding(handle, `{}`))
	require.Equal(t, "pair", v.Bound.Name)
	branches := map[string]string{}
	for _, mt := range v.Bound.Mounts {
		branches[mt.Name] = mt.Branch
	}
	require.Equal(t, map[string]string{"jobA-repo": "exp/lx", "jobB-repo": "agent/test"}, branches,
		"only the write repo moves onto the experiment")

	learnVia(t, h, mount, sid, handle, "lens handle write")
	learnVia(t, h, mount, sid, "", "lens plain write")
	require.True(t, branchHolding(t, m, "jobA-repo", "exp/lx", "lens handle write"))
	require.True(t, branchHolding(t, m, "jobA-repo", "agent/test", "lens plain write"))
	require.False(t, branchHolding(t, m, "jobB-repo", "agent/test", "lens handle write"))
}

// The experiment lifecycle through a handle is the SAME on both endpoint kinds:
// one script, run against each, and the observable results compared step by
// step.
func TestExperimentThroughHandle_SameOnBothEndpointKinds(t *testing.T) {
	type step struct {
		Action, Branch, Active, Summary string
		Err                             bool
	}
	run := func(t *testing.T, scoped bool) []step {
		h, m := experimentServer(t)
		mount, bindArgs := unscopedMount, `{"repo":"jobA-repo"}`
		if scoped {
			mount, bindArgs = repoMount(t, m, "jobA-repo"), `{}`
		}
		sid := initAt(t, h, mount)
		a := bindAt(t, h, mount, sid, bindArgs)
		b := bindAt(t, h, mount, sid, bindArgs)

		var out []step
		exp := func(handle, args string) {
			env, text, isErr := callExperiment(t, h, mount, sid, withBinding(handle, args))
			s := step{Action: env.Action, Branch: env.Branch, Active: env.Active, Summary: env.Summary, Err: isErr}
			if isErr {
				s.Summary = text
			}
			out = append(out, s)
		}
		where := func(title string) {
			for _, br := range []string{"agent/test", "exp/e1", "exp/e2"} {
				if branchHolding(t, m, "jobA-repo", br, title) {
					out = append(out, step{Action: "fact", Summary: title + "@" + br})
				}
			}
		}

		exp(a, `{"action":"open","name":"e1"}`)
		learnVia(t, h, mount, sid, a, "A in e1")
		learnVia(t, h, mount, sid, b, "B beside e1")
		where("A in e1")
		where("B beside e1")
		exp(a, `{"action":"list"}`)
		exp(b, `{"action":"list"}`)
		exp(a, `{"action":"sync"}`)
		exp(a, `{"action":"commit"}`)
		learnVia(t, h, mount, sid, a, "A after commit")
		where("A after commit")
		where("A in e1")
		exp(a, `{"action":"open","name":"e2"}`)
		exp(b, `{"action":"rollback","name":"e2"}`)
		exp(a, `{"action":"list"}`)
		learnVia(t, h, mount, sid, a, "A after rollback")
		where("A after rollback")
		return out
	}
	unscoped := run(t, false)
	scoped := run(t, true)
	require.NotEmpty(t, unscoped)
	require.Equal(t, unscoped, scoped)
}

// Each endpoint kind is SERVED its own schemas and instructions through the
// transport: the filter and the initialize hook read the endpoint marker the
// router put on the request, which a unit test of the filter cannot show.
func TestScopedBind_SchemasAndInstructionsPerEndpoint(t *testing.T) {
	h, m := experimentServer(t)
	openOutOfBand(t, m, "jobA-repo", "addressed")
	const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"schemas","version":"1.0"}}}`

	toolsAt := func(mount string) map[string]map[string]any {
		_, sid := rpcAt(t, h, mount, "", initialize)
		resp, _ := rpcAt(t, h, mount, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
		result := resp["result"].(map[string]any)
		out := map[string]map[string]any{}
		for _, raw := range result["tools"].([]any) {
			tool := raw.(map[string]any)
			out[tool["name"].(string)] = tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
		}
		return out
	}
	instructionsAt := func(mount string) string {
		resp, _ := rpcAt(t, h, mount, "", initialize)
		s, _ := resp["result"].(map[string]any)["instructions"].(string)
		return s
	}
	descOf := func(props map[string]any) string {
		d, _ := props["binding"].(map[string]any)["description"].(string)
		return d
	}

	unscoped := toolsAt(unscopedMount)
	scoped := toolsAt(repoMount(t, m, "jobA-repo"))

	require.Contains(t, unscoped["knomit_bind"], "repo")
	require.Contains(t, unscoped["knomit_bind"], "lens")
	require.Contains(t, unscoped["knomit_bind"], "experiment")
	require.NotContains(t, scoped["knomit_bind"], "repo")
	require.NotContains(t, scoped["knomit_bind"], "lens")
	require.Contains(t, scoped["knomit_bind"], "experiment")

	require.Contains(t, descOf(unscoped["knomit_learn"]), "REQUIRED")
	require.True(t, strings.HasPrefix(descOf(scoped["knomit_learn"]), "Optional on this endpoint"), descOf(scoped["knomit_learn"]))
	require.Contains(t, descOf(scoped["knomit_repos"]), "this connection's own repo or lens")

	require.Contains(t, instructionsAt(repoMount(t, m, "jobA-repo")), "## Binding on this connection")
	require.NotContains(t, instructionsAt("/repos/jobA-repo/branches/exp:addressed/mcp"), "## Binding on this connection",
		"a mount whose URL names a branch cannot be steered, so it must not be told it can")
	require.NotContains(t, instructionsAt(unscopedMount), "## Binding on this connection")
}
