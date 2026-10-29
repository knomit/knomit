package mcp

import (
	"context"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
)

// knomit_hypothesize continues a session_id only on the branch the session
// was opened on, on BOTH continuation paths (current=true and the answer):
// the handler passes the binding's write branch to the engine on every path,
// so a caller bound to another branch of the same repo reads the session
// exactly as one that does not exist. The caller on the session's own branch
// is served.
//
// Sabotage: build either continuation with synthesize.NewHypothesizer (no
// branch) and the experiment-bound caller is served the agent branch's
// session on that path.
func TestHypothesize_SessionIDContinuesOnlyOnItsBranch(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	svc := changesSvc(t, ri)
	ctx := context.Background()
	agent := ri.AgentBranch()
	exp, err := svc.Merge().OpenExperiment(ctx, "elsewhere", "", agent)
	require.NoError(t, err)

	sess, err := svc.Pipeline().CreatePipelineSession(ctx, "hypothesize", agent, "")
	require.NoError(t, err)
	marked, err := svc.Pipeline().MarkPipelineSessionPlanned(ctx, sess.ID)
	require.NoError(t, err)
	require.True(t, marked)

	call := func(branch string, args map[string]any) string {
		t.Helper()
		var req mcpgo.CallToolRequest
		req.Params.Arguments = args
		res, err := HypothesizeHandler()(repos.WithBinding(ctx, repos.NewBindingOfRepo(ri, branch)), req)
		require.NoError(t, err)
		return resultText(t, res)
	}
	for name, args := range map[string]func(id string) map[string]any{
		"current":  func(id string) map[string]any { return map[string]any{"session_id": id, "current": true} },
		"continue": func(id string) map[string]any { return map[string]any{"session_id": id, "response": ""} },
	} {
		foreign := call(exp.Branch(), args(sess.ID))
		ghost := call(exp.Branch(), args("no-such-session"))
		require.Contains(t, ghost, `session "no-such-session" not found`, name)
		require.Equal(t, ghost, strings.ReplaceAll(foreign, sess.ID, "no-such-session"),
			"%s: a session on another branch must read exactly like one that does not exist", name)
		require.NotContains(t, call(agent, args(sess.ID)), "not found", "%s: the session's own branch is served", name)
	}
}
