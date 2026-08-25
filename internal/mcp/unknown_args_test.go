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

// The hypothesize analogue of the watermark test above, and it exists because
// its absence was caught in review (PR #126, HIGH-2).
//
// TestHypothesizeHandler_RejectsUnknownArgumentKey below asserts only IsError,
// which cannot see WHERE the guard runs: with the guard moved after the engine
// it still returns an error, and the whole internal/mcp package stays green.
// Verified by running exactly that sabotage.
//
// The harm is not hypothetical and not smaller than review's. NewHypothesizer
// drives the SAME Pipeline.StartSession; an empty seed pool reaches
// completeSession, and an unscoped completion advances the HYPOTHESIZE
// watermark for that branch. Same walling mechanism, same corpus, different
// watermark key.
func TestHypothesizeHandler_UnknownArgumentKeyDoesNotAdvanceWatermark(t *testing.T) {
	ri, svc := newReviewTestRepoWithStore(t)
	onAgent := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")
	ctx := context.Background()

	before, err := svc.Pipeline().GetPipelineWatermark(ctx, "hypothesize", "agent/test")
	require.NoError(t, err)

	var bad mcpgo.CallToolRequest
	bad.Params.Arguments = map[string]any{"effort": "medium", badScopeKey: badScopeValue}
	result, err := HypothesizeHandler()(onAgent, bad)
	require.NoError(t, err)
	require.True(t, result.IsError)

	after, err := svc.Pipeline().GetPipelineWatermark(ctx, "hypothesize", "agent/test")
	require.NoError(t, err)
	require.Equal(t, before, after,
		"a rejected call must not have run a session — the hypothesize watermark "+
			"moved, which walls this tool exactly as #121 walled review")

	// Control: a VALID unscoped call DOES advance it, so the assertion above is
	// capable of failing.
	var good mcpgo.CallToolRequest
	good.Params.Arguments = map[string]any{"effort": "medium"}
	result, err = HypothesizeHandler()(onAgent, good)
	require.NoError(t, err)
	require.False(t, result.IsError, "control call must run: %s", resultText(t, result))

	moved, err := svc.Pipeline().GetPipelineWatermark(ctx, "hypothesize", "agent/test")
	require.NoError(t, err)
	require.NotEqual(t, before, moved,
		"control: an accepted unscoped call DOES advance the hypothesize watermark")
}

// The handler must pass its OWN tool's schema to the guard. Handing it the
// sibling tool's schema compiles, passes every test that existed at review
// time, and makes knomit_review reject `page`, `item_id` and
// `completion_token` — the entire paging protocol (PR #126, MEDIUM-1).
//
// AcceptsEveryDeclaredKey cannot see this: it hands the helper a tool and then
// checks that tool's own keys, so the pairing is correct by construction. That
// is the test-calls-the-helper disguise again — this test drives the HANDLER.
func TestReviewHandler_AcceptsPagingArgumentKeys(t *testing.T) {
	ri := newLearnTestRepo(t, fact.CodeOntology())
	onAgent := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")

	// The paging protocol's three keys, which exist on knomit_review and NOT on
	// knomit_hypothesize. session_id is present so the call is a continue —
	// what matters is that the guard does not reject these keys.
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"session_id":       "no-such-session",
		"page":             float64(2),
		"item_id":          float64(7),
		"completion_token": "tok",
	}
	result, err := ReviewHandler()(onAgent, req)
	require.NoError(t, err)

	// The call fails on the session, not on the arguments. Asserting the
	// MESSAGE is the point: "IsError" is true either way, and this test exists
	// precisely because a coarser assertion could not tell the two apart.
	text := resultText(t, result)
	require.NotContains(t, text, "unknown argument",
		"the paging keys belong to knomit_review and must pass its guard — "+
			"handing the guard the wrong tool's schema breaks paging silently")
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

// Two shapes mean "no arguments", and BOTH must pass: an empty object, and
// Arguments left nil entirely. The nil path is the one the guard's own
// early-return handles, and it was previously untested — an omission caught in
// review (PR #126, LOW-2). Calling with no arguments is the documented way to
// start a session, so a guard that rejected either shape would break the
// tool's primary entry point.
func TestRejectUnknownArguments_AcceptsBothNoArgumentShapes(t *testing.T) {
	t.Run("empty object", func(t *testing.T) {
		var req mcpgo.CallToolRequest
		req.Params.Arguments = map[string]any{}
		require.NoError(t, rejectUnknownArguments(req, reviewTool()))
	})

	t.Run("nil arguments", func(t *testing.T) {
		var req mcpgo.CallToolRequest // Params.Arguments left nil
		require.NoError(t, rejectUnknownArguments(req, reviewTool()),
			"a call with no arguments at all starts a session and must pass")
	})
}

// PR #126, MEDIUM-2. `arguments` that is present but NOT a JSON object
// bypassed the guard completely: GetArguments type-asserts to map[string]any
// and yields nil for anything else, the guard early-returned nil, and the call
// RAN — unscoped, advancing the watermark. That is #121's exact consequence
// reached by a second route, and the route is wire-reachable: mcp-go
// unmarshals `arguments` into a bare `any` and performs no schema validation
// on the server path.
//
// The guard's comment claimed "the ordinary per-argument accessors handle it".
// They do not: they silently default, which is the whole failure mode.
//
// Ruled consistent with the hard-error decision on unknown keys — same class,
// same silent-unscoped consequence.
func TestReviewHandler_RejectsNonObjectArguments(t *testing.T) {
	ri, svc := newReviewTestRepoWithStore(t)
	onAgent := repos.WithBranch(repos.WithRepoInstance(context.Background(), ri), "agent/test")
	ctx := context.Background()

	before, err := svc.Pipeline().GetPipelineWatermark(ctx, "review", "agent/test")
	require.NoError(t, err)

	// The #121 payload, delivered as a JSON STRING rather than an object.
	var req mcpgo.CallToolRequest
	req.Params.Arguments = `{"effort": "medium", "scope": "{\"entities\": [\"x\"]}"}`

	result, err := ReviewHandler()(onAgent, req)
	require.NoError(t, err)
	require.True(t, result.IsError,
		"arguments that are not a JSON object must be rejected, not silently "+
			"treated as no arguments at all")

	after, err := svc.Pipeline().GetPipelineWatermark(ctx, "review", "agent/test")
	require.NoError(t, err)
	require.Equal(t, before, after,
		"the rejected call must not have run — this is #121's consequence "+
			"reached by a second route")
}
