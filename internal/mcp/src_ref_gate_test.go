package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// knomit#249, end to end: the refs.Gate src-form rule reaches knomit_learn and
// knomit_update. The rule itself is pinned in internal/refs; these tests pin
// the WIRING — that both tools pass every ref through the gate, and that
// update passes the fact's real prior refs, so a legacy ref it already carried
// is not re-judged.

const (
	gateTestCommit = "4154e92c8ff333435fd00c442489e855e4c3331e"
	gateTestBlob   = "36b1d45187d6a2c6ad18d591142227ad2a02a66e"
	gateTestFull   = "src://7b4887ce51d9/internal/x.go@" + gateTestCommit + ":" + gateTestBlob
)

// The three placeholder shapes a syntactic check CAN catch. An invented but
// well-formed 40-hex blob is the fourth, and no syntactic check can.
var srcPlaceholders = map[string]string{
	"non-hex blob":       "src://7b4887ce51d9/internal/x.go@" + gateTestCommit + ":BLOB_HASH_HERE",
	"non-12-hex repo id": "src://REPO_ID/internal/x.go@" + gateTestCommit + ":" + gateTestBlob,
	"missing :blob":      "src://7b4887ce51d9/internal/x.go@" + gateTestCommit,
}

func learnWithRefs(t *testing.T, ctx context.Context, title string, refs []string) *mcpgo.CallToolResult {
	t.Helper()
	refsAny := make([]any, len(refs))
	for i, r := range refs {
		refsAny[i] = r
	}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": "src-gate",
		"facts": []any{map[string]any{
			"topic":      "principles",
			"category":   "mission/refs",
			"title":      title,
			"body":       "designer authored " + title + ".",
			"kind":       "pragmatic",
			"type":       "policy",
			"domain":     []any{"refs"},
			"confidence": 0.8,
			"sources":    1,
			"entities":   []any{"designer"},
			"refs":       refsAny,
		}},
	}
	result, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	return result
}

func updateRefs(t *testing.T, ctx context.Context, path string, refs []string) *mcpgo.CallToolResult {
	t.Helper()
	refsAny := make([]any, len(refs))
	for i, r := range refs {
		refsAny[i] = r
	}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"file":        path,
		"moment_name": "src-gate",
		"updates":     map[string]any{"refs": refsAny},
	}
	result, err := UpdateHandler()(ctx, req)
	require.NoError(t, err)
	return result
}

func TestLearn_RejectsSrcRefPlaceholders(t *testing.T) {
	for name, ref := range srcPlaceholders {
		t.Run(name, func(t *testing.T) {
			_, ctx := fedRepo(t)
			result := learnWithRefs(t, ctx, "Holder", []string{ref})
			require.Truef(t, result.IsError, "learn must refuse a %s src ref", name)
			text := resultText(t, result)
			require.Contains(t, text, ref, "the error must echo the offending ref")
			// A non-hex blob in the new form is caught one step earlier, by
			// SerializeFact's shape rule, whose remedy names the real path;
			// the others reach refs.Gate. Both hand over the git command.
			require.Contains(t, text, "git rev-parse <commit>:")
		})
	}
}

func TestLearn_AcceptsFullFormSrcRef(t *testing.T) {
	_, ctx := fedRepo(t)
	result := learnWithRefs(t, ctx, "Holder", []string{gateTestFull})
	require.Falsef(t, result.IsError, "learn failed: %s", resultText(t, result))
}

func TestUpdate_RejectsSrcRefPlaceholders(t *testing.T) {
	for name, ref := range srcPlaceholders {
		t.Run(name, func(t *testing.T) {
			_, ctx := fedRepo(t)
			path := mergedFactPath(t, learnWithRefs(t, ctx, "Holder", nil))
			result := updateRefs(t, ctx, path, []string{ref})
			require.Truef(t, result.IsError, "update must refuse a %s src ref", name)
			require.Contains(t, resultText(t, result), ref)
		})
	}
}

// The fact already carries a legacy src ref — written before #249, the way
// 279 facts in knomit-kb do. It is seeded straight into the store, since no
// tool will write one any more. Update must still be able to edit the fact and
// add a well-formed ref beside the carried one; it must still refuse a legacy
// ref ADDED anew.
func TestUpdate_CarriedLegacySrcRefStaysEditable(t *testing.T) {
	ri, ctx := fedRepo(t)
	legacy := "src://knomit/internal/legacy.go@ca1c272"
	path := "kb/architecture/refs/legacy-holder.md"
	writeExplainFact(t, ctx, ri, path, "Legacy holder", 0.8, []string{legacy})

	result := updateRefs(t, ctx, path, []string{legacy, gateTestFull})
	require.Falsef(t, result.IsError,
		"a carried legacy ref must not block an update: %s", resultText(t, result))

	added := "src://knomit/internal/other.go@ca1c272"
	result = updateRefs(t, ctx, path, []string{legacy, gateTestFull, added})
	require.True(t, result.IsError, "a legacy src ref ADDED anew must be refused")
	text := resultText(t, result)
	require.Contains(t, text, added)
	require.NotContains(t, text, "legacy.go", "the carried ref is not a problem")
}
