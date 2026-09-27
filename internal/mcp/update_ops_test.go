package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

const opsSlot = ".knomit/jobs/x/y.md"

// callUpdate runs knomit_update with args and returns the result.
func callUpdate(t *testing.T, ctx context.Context, args map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	res, err := UpdateHandler()(ctx, req)
	require.NoError(t, err)
	return res
}

// writeSlot plants a private-state fact with body at opsSlot and returns the
// commit that wrote it.
func writeSlot(t *testing.T, ctx context.Context, svc *store.Service, body string) string {
	t.Helper()
	return writeRaw(t, ctx, svc, opsSlot, body)
}

func writeRaw(t *testing.T, ctx context.Context, svc *store.Service, path, body string) string {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = "Job state"
	f.Body = body
	f.Type = fact.Observation
	f.Domain = []string{"jobs"}
	f.Confidence = 0.8
	f.Sources = 1
	f.Entities = []string{}
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	res, err := svc.Facts().WriteFact(ctx, "agent/test", path, content, "seed "+path, "")
	require.NoError(t, err)
	return res.CommitHash
}

func readContent(t *testing.T, svc *store.Service, path string) string {
	t.Helper()
	res, err := svc.Facts().ReadFact(context.Background(), "agent/test", path, nil)
	require.NoError(t, err)
	return res.Content
}

func readBody(t *testing.T, svc *store.Service, path string) string {
	t.Helper()
	f, err := fact.ParseFact(path, readContent(t, svc, path))
	require.NoError(t, err)
	return f.Body
}

func headCommit(t *testing.T, svc *store.Service) string {
	t.Helper()
	h, err := svc.Branches().HeadCommit(context.Background(), "agent/test")
	require.NoError(t, err)
	return h
}

func replace(old, new string) map[string]any {
	return map[string]any{"op": "str_replace", "old_str": old, "new_str": new}
}

func opsArgs(file string, ops ...map[string]any) map[string]any {
	list := make([]any, len(ops))
	for i, o := range ops {
		list[i] = o
	}
	return map[string]any{"file": file, "moment_name": "ops", "ops": list}
}

func TestUpdateOps_PrivateSlotReplaceAndResponse(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "The claim is wrong.\n\nKeep this.")

	res := callUpdate(t, ctx, opsArgs(opsSlot,
		replace("The claim is wrong.", "The claim is right."),
		map[string]any{"op": "append", "text": "Appended."},
	))
	require.False(t, res.IsError, resultText(t, res))
	require.Equal(t, "The claim is right.\n\nKeep this.\n\nAppended.", readBody(t, svc, opsSlot))

	var payload struct {
		File   string `json:"file"`
		Commit string `json:"commit"`
		Ops    []struct {
			Op    string `json:"op"`
			Delta int    `json:"delta"`
		} `json:"ops"`
	}
	text := resultText(t, res)
	require.NoError(t, json.Unmarshal([]byte(text), &payload))
	require.Equal(t, opsSlot, payload.File)
	require.Equal(t, headCommit(t, svc), payload.Commit)
	require.Len(t, payload.Ops, 2)
	require.Equal(t, "str_replace", payload.Ops[0].Op)
	require.Equal(t, 0, payload.Ops[0].Delta)
	require.Equal(t, "append", payload.Ops[1].Op)
	require.Equal(t, len("\n\nAppended."), payload.Ops[1].Delta)
	require.NotContains(t, text, "Keep this.", "the response must not echo the body")
}

func TestUpdateOps_ZeroAndMultipleMatchesReject(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "the gate -- it rejects. foo and foo.")
	before := readContent(t, svc, opsSlot)

	res := callUpdate(t, ctx, opsArgs(opsSlot, replace("the gate — it rejects", "x")))
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "op 0: 0 matches")
	require.Contains(t, resultText(t, res), "U+2014")

	res = callUpdate(t, ctx, opsArgs(opsSlot, replace("foo", "bar")))
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "op 0: 2 matches at offsets")

	require.Equal(t, before, readContent(t, svc, opsSlot))
}

func TestUpdateOps_AtomicOnLaterFailure(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "a b c")
	before := readContent(t, svc, opsSlot)
	head := headCommit(t, svc)

	res := callUpdate(t, ctx, opsArgs(opsSlot,
		replace("a", "A"), replace("b", "B"), replace("missing", "x")))
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "op 2:")
	require.Equal(t, before, readContent(t, svc, opsSlot), "no op may land when a later op fails")
	require.Equal(t, head, headCommit(t, svc), "a failed call must not commit")
}

func TestUpdateOps_SequentialAnchoringOneCommit(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "one two")
	head := headCommit(t, svc)

	res := callUpdate(t, ctx, opsArgs(opsSlot,
		replace("two", "two INSERTED"), replace("INSERTED", "three")))
	require.False(t, res.IsError, resultText(t, res))
	require.Equal(t, "one two three", readBody(t, svc, opsSlot))

	last, err := svc.Facts().LastCommitTouching(ctx, "agent/test", opsSlot)
	require.NoError(t, err)
	require.Equal(t, headCommit(t, svc), last)
	at, err := svc.Facts().ReadFact(ctx, "agent/test", opsSlot, &store.ReadFactOpts{AtCommit: head})
	require.NoError(t, err)
	parsed, err := fact.ParseFact(opsSlot, at.Content)
	require.NoError(t, err)
	require.Equal(t, "one two", parsed.Body, "the pre-call commit still holds the original body")
}

func TestUpdateOps_BodyAndOpsTogetherRejects(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "abc")
	args := opsArgs(opsSlot, replace("abc", "x"))
	args["updates"] = map[string]any{"body": "whole new body"}
	res := callUpdate(t, ctx, args)
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "body")
	require.Contains(t, resultText(t, res), "ops")
	require.Equal(t, "abc", readBody(t, svc, opsSlot))
}

func TestUpdateOps_NeitherUpdatesNorOpsRejects(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "abc")
	res := callUpdate(t, ctx, map[string]any{"file": opsSlot, "moment_name": "m"})
	require.True(t, res.IsError)
}

func TestUpdateOps_UnknownOpFieldRejects(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "abc abc")
	res := callUpdate(t, ctx, opsArgs(opsSlot,
		map[string]any{"op": "str_replace", "old_str": "abc", "new_str": "x", "replace_all": true}))
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "replace_all")
}

func TestUpdateOps_CombinesWithFrontmatterUpdates(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "abc")
	args := opsArgs(opsSlot, replace("abc", "xyz"))
	args["updates"] = map[string]any{"confidence": 0.5}
	res := callUpdate(t, ctx, args)
	require.False(t, res.IsError, resultText(t, res))
	f, err := fact.ParseFact(opsSlot, readContent(t, svc, opsSlot))
	require.NoError(t, err)
	require.Equal(t, "xyz", f.Body)
	require.Equal(t, 0.5, f.Confidence)
}

// A CR LF pair is normalised to LF by ParseFact, so a body carrying one would
// be committed as bytes no reader ever sees back. The roundtrip gate refuses.
func TestUpdateOps_RoundtripFailureRejects(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "abc")
	res := callUpdate(t, ctx, opsArgs(opsSlot, replace("abc", "line one\r\nline two")))
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "roundtrip")
	require.Equal(t, "abc", readBody(t, svc, opsSlot))
}

func TestUpdate_TitleThatBreaksRoundtripRejects(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "abc")
	res := callUpdate(t, ctx, map[string]any{
		"file": opsSlot, "moment_name": "m",
		"updates": map[string]any{"title": "two\nlines"},
	})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "roundtrip")
}

func TestUpdate_TitleEdgeWhitespaceIsTrimmedNotRejected(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "abc")
	res := callUpdate(t, ctx, map[string]any{
		"file": opsSlot, "moment_name": "m",
		"updates": map[string]any{"title": "  New title \n"},
	})
	require.False(t, res.IsError, resultText(t, res))
	f, err := fact.ParseFact(opsSlot, readContent(t, svc, opsSlot))
	require.NoError(t, err)
	require.Equal(t, "New title", f.Title)
}

func TestUpdate_UnbalancedFenceRejectsOnBothPaths(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "abc")

	res := callUpdate(t, ctx, opsArgs(opsSlot, replace("abc", "```go\ncode")))
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "fence")

	res = callUpdate(t, ctx, map[string]any{
		"file": opsSlot, "moment_name": "m",
		"updates": map[string]any{"body": "text\n```\nunclosed"},
	})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "fence")
	require.Equal(t, "abc", readBody(t, svc, opsSlot))

	res = callUpdate(t, ctx, opsArgs(opsSlot, replace("abc", "```go\ncode\n```")))
	require.False(t, res.IsError, resultText(t, res))
}

func TestUpdateOps_PublicFactRunsOntologyValidation(t *testing.T) {
	svc, ctx, emb := newPrinciplesTestRepo(t)
	seed, err := LearnHandler(emb)(ctx, principleLearnReq("seed", 0.8, []any{"global"}))
	require.NoError(t, err)
	require.False(t, seed.IsError, resultText(t, seed))
	path := mergedFactPath(t, seed)

	res := callUpdate(t, ctx, opsArgs(path, replace("authored", "wrote")))
	require.False(t, res.IsError, resultText(t, res))
	require.Equal(t, "designer wrote this principle.", readBody(t, svc, path))

	args := opsArgs(path, replace("wrote", "penned"))
	args["updates"] = map[string]any{"entities": []any{}}
	res = callUpdate(t, ctx, args)
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "must-have-designer")
	require.Equal(t, "designer wrote this principle.", readBody(t, svc, path))
}

func TestUpdate_IfCommitOnBodyPath(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	c1 := writeSlot(t, ctx, svc, "version one secret")

	res := callUpdate(t, ctx, map[string]any{
		"file": opsSlot, "moment_name": "m", "if_commit": c1,
		"updates": map[string]any{"body": "version two secret"},
	})
	require.False(t, res.IsError, resultText(t, res))
	c2, err := svc.Facts().LastCommitTouching(ctx, "agent/test", opsSlot)
	require.NoError(t, err)

	res = callUpdate(t, ctx, map[string]any{
		"file": opsSlot, "moment_name": "m", "if_commit": c1,
		"updates": map[string]any{"body": "stale write"},
	})
	require.True(t, res.IsError)
	text := resultText(t, res)
	require.Contains(t, text, "current_commit")
	require.Contains(t, text, c2)
	require.NotContains(t, text, "secret", "a mismatch must never return the body")
	require.Equal(t, "version two secret", readBody(t, svc, opsSlot))
}

// The documented flow: read the slot with knomit_explain, keep its commit,
// and pass it back as if_commit. An unrelated commit in between must not
// invalidate it.
func TestUpdateOps_IfCommitFromExplainPasses(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "slot body")

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"file": opsSlot}
	res, err := ExplainHandler()(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	var explained struct {
		Facts []struct {
			Commit string `json:"commit"`
		} `json:"facts"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &explained))
	require.NotEmpty(t, explained.Facts)

	writeRaw(t, ctx, svc, ".knomit/jobs/x/other.md", "unrelated")

	args := opsArgs(opsSlot, replace("slot body", "slot body, edited"))
	args["if_commit"] = explained.Facts[0].Commit
	res = callUpdate(t, ctx, args)
	require.False(t, res.IsError, resultText(t, res))
	require.Equal(t, "slot body, edited", readBody(t, svc, opsSlot))
}

// if_commit compares bytes, not hashes: any commit at which the file read as
// it reads now passes — here a later HEAD that did not touch the slot.
func TestUpdateOps_IfCommitPrivateSlotUnrelatedCommitPasses(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "slot body")
	readAt := writeRaw(t, ctx, svc, ".knomit/jobs/x/other.md", "unrelated one")
	require.Equal(t, readAt, headCommit(t, svc), "explain-style commit is HEAD")
	writeRaw(t, ctx, svc, ".knomit/jobs/x/other.md", "unrelated two")

	args := opsArgs(opsSlot, replace("slot body", "slot body, edited"))
	args["if_commit"] = readAt
	res := callUpdate(t, ctx, args)
	require.False(t, res.IsError, resultText(t, res))
	require.Equal(t, "slot body, edited", readBody(t, svc, opsSlot))
}

func TestUpdateOps_IfCommitPrivateSlotRewrittenRejects(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "slot body secret")
	readAt := writeRaw(t, ctx, svc, ".knomit/jobs/x/other.md", "unrelated")
	rewritten := writeSlot(t, ctx, svc, "slot body secret, rewritten")
	writeRaw(t, ctx, svc, ".knomit/jobs/x/other.md", "unrelated again")

	args := opsArgs(opsSlot, replace("rewritten", "edited"))
	args["if_commit"] = readAt
	res := callUpdate(t, ctx, args)
	require.True(t, res.IsError)
	text := resultText(t, res)
	require.Contains(t, text, "current_commit")
	require.Contains(t, text, rewritten, "current_commit is the last commit that touched the file")
	require.NotContains(t, text, "secret")
	require.Equal(t, "slot body secret, rewritten", readBody(t, svc, opsSlot))
}

func TestUpdateOps_IfCommitUnknownOrAbsentRejects(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	beforeSlot := writeRaw(t, ctx, svc, ".knomit/jobs/x/other.md", "unrelated")
	writeSlot(t, ctx, svc, "abc")

	for _, c := range []string{strings.Repeat("0", 40), "not-a-hash", beforeSlot} {
		args := opsArgs(opsSlot, replace("abc", "x"))
		args["if_commit"] = c
		res := callUpdate(t, ctx, args)
		require.True(t, res.IsError, "if_commit %q must reject", c)
		require.Contains(t, resultText(t, res), "current_commit")
	}
	require.Equal(t, "abc", readBody(t, svc, opsSlot))
}

func TestUpdateTool_ServesOpsAndIfCommit(t *testing.T) {
	raw, err := json.Marshal(updateTool())
	require.NoError(t, err)
	var tool struct {
		InputSchema struct {
			Required   []string `json:"required"`
			Properties struct {
				Ops struct {
					Description string `json:"description"`
					Items       struct {
						Properties map[string]any `json:"properties"`
					} `json:"items"`
				} `json:"ops"`
				IfCommit struct {
					Description string `json:"description"`
				} `json:"if_commit"`
			} `json:"properties"`
		} `json:"inputSchema"`
	}
	require.NoError(t, json.Unmarshal(raw, &tool))
	d := tool.InputSchema.Properties.Ops.Description
	require.Contains(t, strings.ToLower(d), "exactly once")
	require.Contains(t, d, "updates.body")
	for _, k := range []string{"op", "old_str", "new_str", "text"} {
		require.Contains(t, tool.InputSchema.Properties.Ops.Items.Properties, k)
	}
	require.Contains(t, tool.InputSchema.Properties.IfCommit.Description, "current_commit")
	require.NotContains(t, tool.InputSchema.Required, "updates",
		"an ops-only call must be expressible")
}
