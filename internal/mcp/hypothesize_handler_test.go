package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// What remains of the hypothesize tests in this package after the engine
// extraction: the handler shell and the wire converter. Everything that used to
// be tested here through hypothesizeStart / hypothesizeContinue /
// hypothesizeNextItem — seed scanning, the watermark gate, the claim protocol,
// backward-discover priorities — is engine behaviour now and is covered in
// internal/synthesize (hypothesize_engine_test.go, pipeline_shared_test.go).

// mcpToolRequest builds a CallToolRequest from the given key-value arguments.
func mcpToolRequest(t *testing.T, params map[string]interface{}) mcpgo.CallToolRequest {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = params
	return req
}

// synthFactContent returns a valid serialized synthesis fact body. Built via
// fact.SerializeFact so the content round-trips through ParseFact (a body needs
// a # heading; raw YAML strings don't).
func synthFactContent(t *testing.T, path, title string) string {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = title
	f.Body = "synthesis body"
	f.Type = fact.Synthesis
	f.Origin = fact.Distilled
	f.Confidence = 0.8
	f.Sources = 1
	f.Domain = []string{"x"}
	out, err := fact.SerializeFact(f)
	require.NoError(t, err)
	return out
}

// newHypothesizeHandlerCtx opens a store on agent/test, binds a RepoInstance
// into a context the handler can resolve, and returns both.
func newHypothesizeHandlerCtx(t *testing.T) (context.Context, *store.Service) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  "agent/test",
		Svc:          svc,
		OntologyRoot: "kb",
	})
	return repos.WithRepoInstance(context.Background(), ri), svc
}

// callHypothesize invokes the handler and decodes its JSON payload.
func callHypothesize(t *testing.T, ctx context.Context, params map[string]interface{}) *HypothesizeResult {
	t.Helper()
	res, err := HypothesizeHandler()(ctx, mcpToolRequest(t, params))
	require.NoError(t, err)
	require.False(t, res.IsError, "handler returned a tool error: %+v", res.Content)

	text, ok := mcpgo.AsTextContent(res.Content[0])
	require.True(t, ok, "handler must return text content")
	var out HypothesizeResult
	require.NoError(t, json.Unmarshal([]byte(text.Text), &out))
	return &out
}

// TestHypothesizeHandler_WireShape pins the converter: the engine's tool-neutral
// result must project onto the hypothesize wire types without gaining or losing
// fields. `fact` carries the work item's raw stored payload (hypothesize is the
// tool that ships its payload verbatim), `instructions` carries the rendered
// prompt, and `id` is the D2 echo token.
//
// The absence of `summary` is asserted deliberately: HypothesizeResult has never
// had one, and the engine's stats describe mutations this tool does not perform.
func TestHypothesizeHandler_WireShape(t *testing.T) {
	ctx, svc := newHypothesizeHandlerCtx(t)

	_, err := svc.Facts().WriteFact(ctx, "agent/test", "kb/arch/a.md",
		synthFactContent(t, "kb/arch/a.md", "T"), "seed", "")
	require.NoError(t, err)

	start := callHypothesize(t, ctx, map[string]interface{}{})
	require.NotEmpty(t, start.SessionID)
	require.False(t, start.Done)
	require.NotNil(t, start.Item)
	require.Equal(t, "hypothesize", start.Item.Type)
	require.NotZero(t, start.Item.ID, "item.id must be echoed so a stale answer can be rejected")
	require.Contains(t, start.Item.Instructions, "WORKFLOW")
	require.NotNil(t, start.Progress)

	// The `fact` field is the item's payload, not a re-render of it.
	var shipped fact.Fact
	require.NoError(t, json.Unmarshal(start.Item.Fact, &shipped))
	require.Equal(t, "kb/arch/a.md", shipped.Path())
	require.Equal(t, fact.Synthesis, shipped.Type)

	// Raw-JSON check: no summary key, before or after completion.
	raw, err := HypothesizeHandler()(ctx, mcpToolRequest(t, map[string]interface{}{
		"session_id": start.SessionID,
		"response":   "ack",
		"item_id":    float64(start.Item.ID),
	}))
	require.NoError(t, err)
	text, ok := mcpgo.AsTextContent(raw.Content[0])
	require.True(t, ok)
	require.NotContains(t, text.Text, `"summary"`,
		"HypothesizeResult must not grow a summary field")

	var done HypothesizeResult
	require.NoError(t, json.Unmarshal([]byte(text.Text), &done))
	require.True(t, done.Done)
	require.Equal(t, start.SessionID, done.SessionID)
	require.Nil(t, done.Item)
}

// TestHypothesizeHandler_ZeroSeeds_ReportsSession pins D5 at the wire level: a
// start with nothing to do now creates and completes a real session, so
// `session_id` is populated on the done turn where it used to be empty. The
// field already existed in the JSON, so this is additive.
func TestHypothesizeHandler_ZeroSeeds_ReportsSession(t *testing.T) {
	ctx, _ := newHypothesizeHandlerCtx(t)

	out := callHypothesize(t, ctx, map[string]interface{}{})
	require.True(t, out.Done)
	require.NotEmpty(t, out.SessionID, "the zero-seed path now reports the session it completed")
	require.Nil(t, out.Item)
}

// TestHypothesizeHandler_StaleItemID_Rejected covers the D2 guard reaching the
// wire: answering with an item_id that is not the current item must be refused
// rather than applied to whatever happens to be current.
func TestHypothesizeHandler_StaleItemID_Rejected(t *testing.T) {
	ctx, svc := newHypothesizeHandlerCtx(t)

	_, err := svc.Facts().WriteFact(ctx, "agent/test", "kb/arch/a.md",
		synthFactContent(t, "kb/arch/a.md", "T"), "seed", "")
	require.NoError(t, err)

	start := callHypothesize(t, ctx, map[string]interface{}{})
	require.NotNil(t, start.Item)

	res, err := HypothesizeHandler()(ctx, mcpToolRequest(t, map[string]interface{}{
		"session_id": start.SessionID,
		"response":   "ack",
		"item_id":    float64(start.Item.ID + 999),
	}))
	require.NoError(t, err)
	require.True(t, res.IsError, "a response aimed at a stale item must be rejected")
}

// TestHypothesizeHandler_EffortValidation_OnlyContinue checks that an invalid
// effort on a CONTINUE call does not block session advancement. Effort is a
// start-time dial; validating it on continue would let a bad argument wedge a
// live session with no way for the agent to advance it.
func TestHypothesizeHandler_EffortValidation_OnlyContinue(t *testing.T) {
	ctx, svc := newHypothesizeHandlerCtx(t)

	_, err := svc.Facts().WriteFact(ctx, "agent/test", "kb/arch/a.md",
		synthFactContent(t, "kb/arch/a.md", "T"), "seed", "")
	require.NoError(t, err)

	start := callHypothesize(t, ctx, map[string]interface{}{})
	require.False(t, start.Done)

	out := callHypothesize(t, ctx, map[string]interface{}{
		"session_id": start.SessionID,
		"response":   "ack",
		"effort":     "bogus",
	})
	require.True(t, out.Done)

	// An invalid effort on START, by contrast, is a caller error.
	res, err := HypothesizeHandler()(ctx, mcpToolRequest(t, map[string]interface{}{"effort": "bogus"}))
	require.NoError(t, err)
	require.True(t, res.IsError, "an invalid effort on start must be reported to the caller")
}

// Both long-running synthesis tools must advertise optional task support:
// starting a session can take minutes on a large corpus, and a client that does
// not know the call is task-augmentable will abandon it at its tool-call
// timeout. knomit_review has declared it since the tasks work landed;
// knomit_hypothesize is the same shape of call and must match.
func TestSynthesisToolsAdvertiseOptionalTaskSupport(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool mcpgo.Tool
	}{
		{"hypothesize", hypothesizeTool()},
		{"review", reviewTool()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NotNil(t, tc.tool.Execution, "tool must declare execution behaviour")
			require.Equal(t, mcpgo.TaskSupportOptional, tc.tool.Execution.TaskSupport)
		})
	}
}
