package mcp

import (
	"context"
	"errors"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
)

// Every tool on an unbound session must return a tool ERROR naming knomit_bind
// — never panic, and never a transport error. The agent reads the text, so the
// name of the tool it has to call next has to be in it.
func TestHandlers_UnboundSessionFailsClosed(t *testing.T) {
	ctx := repos.WithSessionScoped(context.Background())
	var req mcpgo.CallToolRequest
	for name, h := range map[string]func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error){
		"repos":       ReposHandler(),
		"explain":     ExplainHandler(),
		"query":       QueryHandler(),
		"learn":       LearnHandler(),
		"update":      UpdateHandler(),
		"retract":     RetractHandler(),
		"review":      ReviewHandler(),
		"hypothesize": HypothesizeHandler(),
	} {
		res, err := h(ctx, req)
		require.NoError(t, err, name)
		require.NotNil(t, res, name)
		require.True(t, res.IsError, name)
		require.Contains(t, resultText(t, res), "knomit_bind", name)
	}
}

// A stored pin that no longer resolves surfaces ITS reason, not the generic
// unbound text: "bind again" is useless advice if the agent cannot tell that
// the repo it picked is gone.
func TestHandlers_StoredBindingErrorSurfaces(t *testing.T) {
	ctx := repos.WithBindingError(repos.WithSessionScoped(context.Background()),
		errors.New(`bound repo "x" is not available — call knomit_bind again`))
	res, err := ReposHandler()(ctx, mcpgo.CallToolRequest{})
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), `bound repo "x"`)
}

// On the unscoped mount initialize can only carry the DEFAULT ontology — there
// is no repo yet — so the instructions must say to bind first. They must not
// carry a mount table: there are no mounts to describe.
func TestAfterInitialize_UnscopedMountSaysBindFirst(t *testing.T) {
	// A nil manager is safe here and keeps the test to the instructions path:
	// neither recordClientInfo nor profileFor dereferences it.
	srv := NewServer("kb", nil, false)

	instr := initializeInstructions(t, srv, repos.WithSessionScoped(context.Background()))
	require.Contains(t, instr, "knomit_bind")
	require.Contains(t, instr, "No repo bound yet")
	// No MOUNTS table: there are no mounts to describe yet. (The addendum does
	// mention the words "mount table" — as what knomit_bind will hand back —
	// so assert on the table's actual markup, not on the phrase.)
	require.NotContains(t, instr, "### Mounts")
	require.NotContains(t, instr, "| repo | id | branch | role | source |")
}

// A URL-scoped mount with nothing in context keeps the old instructions: the
// addendum is for the session-bound mount only.
func TestAfterInitialize_UnscopedAddendumOnlyWhenSessionScoped(t *testing.T) {
	// A nil manager is safe here and keeps the test to the instructions path:
	// neither recordClientInfo nor profileFor dereferences it.
	srv := NewServer("kb", nil, false)

	instr := initializeInstructions(t, srv, context.Background())
	require.NotContains(t, instr, "knomit_bind")
}
