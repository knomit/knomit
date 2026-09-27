package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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
	require.Contains(t, resultText(t, res), "body would read back differently")
	require.Equal(t, "abc", readBody(t, svc, opsSlot))
}

// A newline in a title used to reach the roundtrip gate; the title rule now
// refuses it first, naming the field.
func TestUpdate_MultilineTitleRejects(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "abc")
	res := callUpdate(t, ctx, map[string]any{
		"file": opsSlot, "moment_name": "m",
		"updates": map[string]any{"title": "two\nlines"},
	})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "updates.title: title must be a single line")
}

func TestUpdate_TitleEdgeWhitespaceIsTrimmedNotRejected(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "abc")
	res := callUpdate(t, ctx, map[string]any{
		"file": opsSlot, "moment_name": "m",
		"updates": map[string]any{"title": "  New title \t "},
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
	c2 := headCommit(t, svc)

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
	writeSlot(t, ctx, svc, "slot body secret, rewritten")
	tip := writeRaw(t, ctx, svc, ".knomit/jobs/x/other.md", "unrelated again")

	args := opsArgs(opsSlot, replace("rewritten", "edited"))
	args["if_commit"] = readAt
	res := callUpdate(t, ctx, args)
	require.True(t, res.IsError)
	text := resultText(t, res)
	require.Contains(t, text, "current_commit")
	require.Contains(t, text, tip, "current_commit is the write-branch tip")
	require.NotContains(t, text, "secret")
	require.Equal(t, "slot body secret, rewritten", readBody(t, svc, opsSlot))
}

func TestUpdateOps_IfCommitUnknownOrAbsentRejects(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	beforeSlot := writeRaw(t, ctx, svc, ".knomit/jobs/x/other.md", "unrelated")
	writeSlot(t, ctx, svc, "abc")

	for _, c := range []string{strings.Repeat("0", 40), beforeSlot} {
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

// A malformed if_commit must be refused outright: GetString would read a
// number or an array as "" and silently switch the guard off.
func TestUpdateOps_IfCommitMalformedRejects(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "abc")
	for _, v := range []any{"not-a-hash", 42, []any{"x"}, strings.Repeat("A", 40)} {
		args := opsArgs(opsSlot, replace("abc", "x"))
		args["if_commit"] = v
		res := callUpdate(t, ctx, args)
		require.True(t, res.IsError, "if_commit %v must reject", v)
		require.Contains(t, resultText(t, res), "40-character")
	}
	require.Equal(t, "abc", readBody(t, svc, opsSlot))

	// null and "" are how some clients send an unset optional: no guard.
	for i, v := range []any{nil, ""} {
		args := opsArgs(opsSlot, map[string]any{"op": "append", "text": fmt.Sprintf("unset %d", i)})
		args["if_commit"] = v
		res := callUpdate(t, ctx, args)
		require.False(t, res.IsError, "if_commit %#v must read as absent: %s", v, resultText(t, res))
	}
}

// After an experiment lands with a {body} resolution, the slot's bytes exist
// only in the merge commit. A stale if_commit must get back a current_commit
// that a retry can use: re-reading there and sending it as if_commit passes.
func TestUpdateOps_IfCommitAfterMergeReturnsUsableTip(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	seed := writeSlot(t, ctx, svc, "base")

	_, err := svc.Experiments().OpenExperiment(ctx, "e", "", "agent/test")
	require.NoError(t, err)
	f := fact.NewFact(opsSlot)
	f.Title, f.Body, f.Type = "Job state", "experiment side", fact.Observation
	f.Domain, f.Confidence, f.Sources, f.Entities = []string{"jobs"}, 0.8, 1, []string{}
	expSide, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "exp/e", opsSlot, expSide, "exp edit", "")
	require.NoError(t, err)
	writeSlot(t, ctx, svc, "agent side")
	f.Body = "merged by hand"
	merged, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Experiments().CommitExperiment(ctx, "e", map[string]store.Resolution{
		opsSlot: {Body: []byte(merged)},
	})
	require.NoError(t, err)
	require.Equal(t, "merged by hand", readBody(t, svc, opsSlot))

	args := opsArgs(opsSlot, replace("merged by hand", "merged, then edited"))
	args["if_commit"] = seed
	res := callUpdate(t, ctx, args)
	require.True(t, res.IsError)
	tip := headCommit(t, svc)
	require.Contains(t, resultText(t, res), "current_commit: "+tip)

	args["if_commit"] = tip
	res = callUpdate(t, ctx, args)
	require.False(t, res.IsError, "a retry with if_commit=current_commit must pass: %s", resultText(t, res))
	require.Equal(t, "merged, then edited", readBody(t, svc, opsSlot))
}

// The if_commit check and the write are one atomic step: of N concurrent
// callers holding the same if_commit, exactly one lands and the rest are
// refused — none silently overwrites another.
func TestUpdateOps_IfCommitConcurrentCallersOneWins(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	readAt := writeSlot(t, ctx, svc, "counter")

	const n = 8
	results := make(chan *mcpgo.CallToolResult, n)
	for i := range n {
		go func() {
			args := opsArgs(opsSlot, map[string]any{"op": "append", "text": fmt.Sprintf("writer %d", i)})
			args["if_commit"] = readAt
			var req mcpgo.CallToolRequest
			req.Params.Arguments = args
			res, err := UpdateHandler()(ctx, req)
			if err != nil {
				res = mcpgo.NewToolResultError(err.Error())
			}
			results <- res
		}()
	}
	wins := 0
	for range n {
		res := <-results
		if !res.IsError {
			wins++
			continue
		}
		require.Contains(t, resultText(t, res), "current_commit")
	}
	require.Equal(t, 1, wins, "exactly one caller holding the same if_commit may land")
	require.Equal(t, 1, strings.Count(readBody(t, svc, opsSlot), "writer "))
}

// Without if_commit, ops are still computed from the bytes read at the start
// of the call; a write that lands in between must not be overwritten by them.
func TestUpdateOps_ConcurrentCallersNeverLoseAWrite(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "log")

	const n = 8
	done := make(chan bool, n)
	for i := range n {
		go func() {
			var req mcpgo.CallToolRequest
			req.Params.Arguments = opsArgs(opsSlot, map[string]any{"op": "append", "text": fmt.Sprintf("entry %d", i)})
			res, err := UpdateHandler()(ctx, req)
			done <- err == nil && !res.IsError
		}()
	}
	landed := 0
	for range n {
		if <-done {
			landed++
		}
	}
	require.GreaterOrEqual(t, landed, 1)
	require.Equal(t, landed, strings.Count(readBody(t, svc, opsSlot), "entry "),
		"every call that reported success must be in the body")
}

// A body whose fence count is already odd (valid CommonMark: a ```` fence
// quoting a ``` line) stays editable; only an edit that unbalances an even
// count is refused.
func TestUpdateOps_AlreadyOddFenceBodyStaysEditable(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "````md\n```\n````\n\nold tail")
	res := callUpdate(t, ctx, opsArgs(opsSlot, replace("old tail", "new tail")))
	require.False(t, res.IsError, resultText(t, res))

	res = callUpdate(t, ctx, map[string]any{
		"file": opsSlot, "moment_name": "m",
		"updates": map[string]any{"body": "````md\n```\n````\n\nrewritten"},
	})
	require.False(t, res.IsError, "the body path judges the edit against the existing body: %s", resultText(t, res))
}

func TestUpdateOps_DeltaCountsStoredBytes(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)
	writeSlot(t, ctx, svc, "abc")
	res := callUpdate(t, ctx, opsArgs(opsSlot, map[string]any{"op": "append", "text": "def\n\n"}))
	require.False(t, res.IsError, resultText(t, res))
	var payload struct {
		Ops []struct {
			Delta int `json:"delta"`
		} `json:"ops"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &payload))
	require.Equal(t, len("abc\n\ndef")-len("abc"), payload.Ops[0].Delta,
		"trailing whitespace is not stored, so it is not counted")
}

// An em dash and an en dash share their first two UTF-8 bytes; the windows
// must start at the character, not mid-rune, so neither quotes as \x bytes.
func TestFirstDifference_CutsOnRuneBoundaries(t *testing.T) {
	d := firstDifference("a—b", "a–b")
	require.Equal(t, `at byte 1, sent "—b", would read "–b"`, d)

	long := strings.Repeat("é", 20)
	d = firstDifference("x"+long, "y"+long)
	require.NotContains(t, d, `\x`, "a window cut mid-rune would quote raw bytes: %s", d)
	require.Equal(t, "at byte 0", d[:9])
	require.NotContains(t, firstDifference("abc", "abcdef"), `\x`)
}
