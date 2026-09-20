package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// openTestExperiment forks an experiment from ri's agent branch and returns
// its branch name.
func openTestExperiment(t *testing.T, ri *repos.RepoInstance, name string) string {
	t.Helper()
	var branch string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		exp, err := svc.Experiments().OpenExperiment(context.Background(), name, "", ri.AgentBranch())
		require.NoError(t, err)
		branch = exp.Branch()
	}))
	return branch
}

// headOf returns a branch's tip, or "" when the branch does not resolve.
func headOf(t *testing.T, ri *repos.RepoInstance, branch string) string {
	t.Helper()
	var head string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		h, err := svc.Branches().HeadCommit(context.Background(), branch)
		if err != nil {
			return
		}
		head = h
	}))
	return head
}

func factExistsOn(t *testing.T, ri *repos.RepoInstance, branch, path string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		got, err := svc.Facts().FactExists(context.Background(), branch, path)
		require.NoError(t, err)
		exists = got
	}))
	return exists
}

// TestLearn_BoundToExperiment_LandsThereNotOnAgentBranch is the end of the
// threading: the handler asks the BINDING where the write goes, so a session
// bound to its own experiment authors on the experiment and the agent branch
// does not move.
//
// The agent-branch assertion is the falsifiable half. A handler still reading
// ri.AgentBranch() would return exactly the same success result — the fact
// would simply be on the wrong branch — so a test that only checked "learn
// succeeded" would pass against the unthreaded code.
func TestLearn_BoundToExperiment_LandsThereNotOnAgentBranch(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	branch := openTestExperiment(t, ri, "authoring")

	agentBefore := headOf(t, ri, ri.AgentBranch())
	require.NotEmpty(t, agentBefore)
	expBefore := headOf(t, ri, branch)
	require.Equal(t, agentBefore, expBefore, "precondition: the fork starts level with its parent")

	ctx := repos.WithBinding(context.Background(), repos.NewBindingOfRepo(ri, branch))
	path := seedPrincipleWithDomain(t, ctx, "on-the-experiment", "mission/store", "Experiment Fact", "store")
	require.NotEmpty(t, path)

	require.True(t, factExistsOn(t, ri, branch, path), "the fact is on the experiment")
	require.False(t, factExistsOn(t, ri, ri.AgentBranch(), path), "and NOT on the agent branch")

	require.NotEqual(t, expBefore, headOf(t, ri, branch), "the experiment tip advanced")
	require.Equal(t, agentBefore, headOf(t, ri, ri.AgentBranch()), "the agent branch tip did not")
}

// TestLearn_BoundToExperiment_StampsTheExperimentAsDestination: the write
// destination the caller is SHOWN has to be the branch the bytes went to.
// This stamp exists so an unintended destination is noticeable; one that
// always said "agent branch" would be noise inside an experiment.
func TestLearn_BoundToExperiment_StampsTheExperimentAsDestination(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	branch := openTestExperiment(t, ri, "stamped")

	b := repos.NewBindingOfRepo(ri, branch)
	d := describeWriteDestination(b)
	require.Equal(t, branch, d.Branch, "the stamp names the experiment, not the agent branch")
	require.Contains(t, d.summary("1 fact"), branch)
}

// TestRetract_BoundToExperiment_RemovesOnlyThere: a delete is a write, and it
// must be scoped to the experiment too — including the existence check that
// precedes it, which would otherwise consult the agent branch's tree.
func TestRetract_BoundToExperiment_RemovesOnlyThere(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())

	// Author on the agent branch FIRST, so the fork carries the fact.
	onAgent := repos.WithBinding(context.Background(), repos.NewBindingOfRepo(ri, ri.AgentBranch()))
	path := seedPrincipleWithDomain(t, onAgent, "shared-fact", "mission/store", "Shared Fact", "store")
	require.NotEmpty(t, path)

	branch := openTestExperiment(t, ri, "retracting")
	require.True(t, factExistsOn(t, ri, branch, path), "precondition: the fork carries the fact")
	agentBefore := headOf(t, ri, ri.AgentBranch())

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": "drop-it",
		"file":        path,
		"reason":      "trying life without it",
	}
	ctx := repos.WithBinding(context.Background(), repos.NewBindingOfRepo(ri, branch))
	result, err := RetractHandler()(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, "retract on an own experiment is accepted: %s", resultText(t, result))

	require.False(t, factExistsOn(t, ri, branch, path), "the fact is gone on the experiment")
	require.True(t, factExistsOn(t, ri, ri.AgentBranch(), path), "and still present on the agent branch")
	require.Equal(t, agentBefore, headOf(t, ri, ri.AgentBranch()), "the agent branch tip did not move")
}

// TestWriteHandlers_ForeignExperimentIsRefused: the gate still bites. An
// experiment forked from a branch that is not this instance's agent branch is
// a read-only view, exactly like main.
func TestWriteHandlers_ForeignExperimentIsRefused(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.Branches().CreateBranch(context.Background(), "agent/elsewhere", ri.AgentBranch()))
		_, err := svc.Experiments().OpenExperiment(context.Background(), "not-mine", "", "agent/elsewhere")
		require.NoError(t, err)
	}))

	ctx := repos.WithBinding(context.Background(), repos.NewBindingOfRepo(ri, "exp/not-mine"))
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{}
	result, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	require.True(t, result.IsError)
	text := resultText(t, result)
	require.Contains(t, text, "read-only view")
	require.Contains(t, text, `branch "exp/not-mine"`, "the refusal names the branch the caller is on")
	require.Contains(t, text, ri.AgentBranch(), "and the branch they could use instead")
}

// TestReposMountTable_ReportsTheExperimentAsWriteBranch: knomit_repos' bound
// section is how an agent confirms where it is writing, so inside an
// experiment it has to say the experiment.
func TestReposMountTable_ReportsTheExperimentAsWriteBranch(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	branch := openTestExperiment(t, ri, "mount-table")

	bound := boundFor(repos.NewBindingOfRepo(ri, branch))
	require.Len(t, bound.Mounts, 1)
	require.Equal(t, "read+write", bound.Mounts[0].Role)
	require.Equal(t, branch, bound.Mounts[0].Branch, "the mount READS the experiment")
	require.Equal(t, branch, bound.Mounts[0].WriteBranch, "and WRITES there too")

	onAgent := boundFor(repos.NewBindingOfRepo(ri, ri.AgentBranch()))
	require.Equal(t, ri.AgentBranch(), onAgent.Mounts[0].WriteBranch, "unchanged off an experiment")
}
