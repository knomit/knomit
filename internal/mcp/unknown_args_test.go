package mcp

import (
	"context"
	"path/filepath"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// newReviewTestRepoWithStore mirrors newLearnTestRepo but also returns the
// store, so a test can read pipeline state (the watermark) directly rather
// than inferring whether a session ran from the shape of a response.
func newReviewTestRepoWithStore(t *testing.T) (*repos.RepoInstance, *store.Service) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		UID:          nextTestRepoUID(),
		AgentBranch:  "agent/test",
		Svc:          svc,
		Ontology:     fact.CodeOntology(),
		OntologyRoot: "kb",
	})
	return ri, svc
}

// knomit#122. An MCP tool call is a JSON object with no arity check, so a
// caller can invent a parameter and the server runs a DIFFERENT, VALID, SILENT
// operation. That is what happened on knomit-kb (#121): every scoped
// knomit_review call after a context compaction carried
//
//	{"effort": "medium", "scope": "{\"entities\": [\"installAndRestart\"]}"}
//
// — a `scope` key, which does not exist on knomit_review, holding stringified
// JSON. The key was never read, so the calls ran as WHOLE-CORPUS incremental
// passes, and an unscoped completion ADVANCES THE WATERMARK. One malformed
// call converted a populated corpus into permanent sub-millisecond done:true
// walls, with hundreds of facts unreachable except via a full scan.
//
// The order of the fix matters and is asserted here: unknown keys are rejected
// FIRST. Type-validating the KNOWN keys — the original proposal — would not
// have caught this, because the failing key is never read at all.
const (
	// The verbatim shape of the failing call, from the operator's transcript.
	badScopeKey   = "scope"
	badScopeValue = `{"entities": ["installAndRestart"]}`
)

// The exact #121 payload must be refused by knomit_review, and the refusal
// must name the offending key — a caller that invented a parameter needs to be
// told which one, or it re-sends the same call.
func TestReviewHandler_RejectsUnknownArgumentKey(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	onAgent := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"effort":    "medium",
		badScopeKey: badScopeValue,
	}
	result, err := ReviewHandler()(onAgent, req)
	require.NoError(t, err)
	require.True(t, result.IsError,
		"an unrecognised argument key must be an error, not a silent whole-corpus pass")

	text := resultText(t, result)
	require.Contains(t, text, badScopeKey, "the error names the offending key")
	require.Contains(t, text, "entities",
		"the error lists the valid keys, so the caller can correct itself")
}

// The assertion that actually encodes #122's severity: the malformed call must
// not RUN. Returning an error is not enough on its own — a guard placed after
// StartSession would still return an error, and would still have done the
// damage. The damage has a name and a durable instrument: an unscoped session's
// completion advances the review watermark, which is what converted knomit-kb
// into permanent done:true walls.
//
// So this test reads the watermark, and it proves the instrument can move by
// moving it: a valid unscoped call on the same repo advances it. Without that
// second half the check would pass on a repo where the watermark never advances
// for unrelated reasons.
func TestReviewHandler_UnknownArgumentKeyDoesNotAdvanceWatermark(t *testing.T) {
	ri, svc := newReviewTestRepoWithStore(t)
	onAgent := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")
	ctx := context.Background()

	before, err := svc.Pipeline().GetPipelineWatermark(ctx, "review", "agent/test")
	require.NoError(t, err)

	var bad mcpgo.CallToolRequest
	bad.Params.Arguments = map[string]any{"effort": "medium", badScopeKey: badScopeValue}
	result, err := ReviewHandler()(onAgent, bad)
	require.NoError(t, err)
	require.True(t, result.IsError)

	after, err := svc.Pipeline().GetPipelineWatermark(ctx, "review", "agent/test")
	require.NoError(t, err)
	require.Equal(t, before, after,
		"a rejected call must not have run a session — the watermark moved, "+
			"which is exactly the harm #122 describes")

	// The instrument works: a VALID unscoped call does advance it. Without
	// this, an always-frozen watermark would make the check above vacuous.
	var good mcpgo.CallToolRequest
	good.Params.Arguments = map[string]any{"effort": "medium"}
	result, err = ReviewHandler()(onAgent, good)
	require.NoError(t, err)
	require.False(t, result.IsError, "control call must run: %s", resultText(t, result))

	moved, err := svc.Pipeline().GetPipelineWatermark(ctx, "review", "agent/test")
	require.NoError(t, err)
	require.NotEqual(t, before, moved,
		"control: an accepted unscoped call DOES advance the watermark, so the "+
			"assertion above is capable of failing")
}

// Same for knomit_hypothesize: it shares parseEffortAndScope and the same
// silent-drop exposure.
func TestHypothesizeHandler_RejectsUnknownArgumentKey(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	onAgent := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"effort":    "medium",
		badScopeKey: badScopeValue,
	}
	result, err := HypothesizeHandler()(onAgent, req)
	require.NoError(t, err)
	require.True(t, result.IsError, "unrecognised key must be refused")
	require.Contains(t, resultText(t, result), badScopeKey)
}

// The working form must keep working. This is the control that stops the fix
// from being "reject everything": the operator's correct call — the one whose
// sibling transcripts kept working through the same window — still runs.
func TestReviewHandler_AcceptsDeclaredArgumentKeys(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	onAgent := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"effort":   "medium",
		"entities": []any{"installAndRestart"},
	}
	result, err := ReviewHandler()(onAgent, req)
	require.NoError(t, err)
	require.False(t, result.IsError,
		"the correct call form must still run: %s", resultText(t, result))
}

// Transport metadata is not a caller mistake. MCP clients attach `_meta` and
// other underscore-prefixed keys of their own accord; rejecting those would
// break working clients to catch a bug they do not have.
func TestReviewHandler_AllowsUnderscorePrefixedKeys(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	onAgent := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"effort": "medium",
		"_meta":  map[string]any{"progressToken": "abc"},
	}
	result, err := ReviewHandler()(onAgent, req)
	require.NoError(t, err)
	require.False(t, result.IsError,
		"underscore-prefixed transport metadata must pass: %s", resultText(t, result))
}

// The allowlist is DERIVED FROM THE TOOL SCHEMA, never hand-maintained beside
// it. A hand-written list is a second declaration of the same thing, and the
// two drift the first time a parameter is added — at which point the new
// parameter is rejected as unknown and the tool is broken by its own guard.
//
// This test drives the helper with every key each tool declares, so adding a
// parameter to the schema cannot break it and removing the derivation would
// fail it.
func TestRejectUnknownArguments_AcceptsEveryDeclaredKey(t *testing.T) {
	for _, tool := range []mcpgo.Tool{reviewTool(), hypothesizeTool()} {
		t.Run(tool.Name, func(t *testing.T) {
			require.NotEmpty(t, tool.InputSchema.Properties,
				"a tool with no declared properties would make this check vacuous")

			args := map[string]any{}
			for key := range tool.InputSchema.Properties {
				args[key] = "x"
			}
			var req mcpgo.CallToolRequest
			req.Params.Arguments = args

			require.NoError(t, rejectUnknownArguments(req, tool),
				"every key the tool declares must be accepted")
		})
	}
}

// A call carrying no arguments at all is the documented way to start a
// session, and must not be caught by the guard.
func TestRejectUnknownArguments_AcceptsEmptyArguments(t *testing.T) {
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{}
	require.NoError(t, rejectUnknownArguments(req, reviewTool()))
}
