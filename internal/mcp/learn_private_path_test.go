package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

const jobSlot = ".knomit/jobs/ae/crawl-state.md"

// TestLearnTool_PathDescriptionIsGeneric verifies the knomit_learn schema's
// `path` field describes the RULE, not one job's folder: it must mention
// `<area>` and must never anchor agents to "jobs" specifically. knomit
// accepts any area name under .knomit/, and an example naming "jobs" reads
// as the only valid choice even though it is just one caller's choice.
func TestLearnTool_PathDescriptionIsGeneric(t *testing.T) {
	raw, err := json.Marshal(learnTool())
	require.NoError(t, err)
	var tool struct {
		InputSchema struct {
			Properties struct {
				Facts struct {
					Items struct {
						Properties struct {
							Path struct {
								Description string `json:"description"`
							} `json:"path"`
						} `json:"properties"`
					} `json:"items"`
				} `json:"facts"`
			} `json:"properties"`
		} `json:"inputSchema"`
	}
	require.NoError(t, json.Unmarshal(raw, &tool))
	desc := tool.InputSchema.Properties.Facts.Items.Properties.Path.Description
	require.NotEmpty(t, desc)
	require.Contains(t, desc, "<area>")
	require.NotContains(t, desc, "jobs")
}

// learnAtPath is the private-state learn call under test.
func learnAtPath(t *testing.T, ctx context.Context, path, title, body string) *mcpgo.CallToolResult {
	t.Helper()
	return callTool(t, LearnHandler(), ctx, map[string]any{
		"moment_name": "job-run",
		"facts": []any{
			map[string]any{
				"path":       path,
				"title":      title,
				"body":       body,
				"kind":       "pragmatic",
				"type":       "policy",
				"domain":     []any{},
				"confidence": 0.9,
				"sources":    1,
				"entities":   []any{},
				"refs":       []any{},
			},
		},
	})
}

// THE test: the round trip the crawl job's correctness rests on. It walks
// crawl-state's revision history to rebuild ALREADY_CRAWLED, so learn → update
// → update → explain must yield three revisions newest-first.
func TestLearn_PrivatePath_RoundTripsThroughExplainHistory(t *testing.T) {
	ctx := agentCtx(t)

	require.False(t, learnAtPath(t, ctx, jobSlot, "crawl-state", "run 1").IsError)

	// knomit_update takes a REQUIRED `updates` OBJECT (internal/mcp/update.go:32)
	// with body/title/confidence nested inside — NOT a top-level "body".
	// Verified against the tool schema; a top-level body is silently ignored,
	// so this test would pass while updating nothing.
	for _, body := range []string{"run 2", "run 3"} {
		result := callTool(t, UpdateHandler(), ctx, map[string]any{
			"file":        jobSlot,
			"moment_name": "job-run",
			"updates":     map[string]any{"body": body},
		})
		require.Falsef(t, result.IsError, "update failed: %s", resultText(t, result))
	}

	result := callTool(t, ExplainHandler(), ctx, map[string]any{"file": jobSlot})
	require.False(t, result.IsError, resultText(t, result))
	text := resultText(t, result)
	require.Contains(t, text, "run 3", "HEAD body")
	require.Contains(t, text, `"history"`, "explain must return revision history")
}

// Supplying both is a caller bug. Silently ignoring one would hide it.
func TestLearn_PrivatePath_RejectsTopicAndCategory(t *testing.T) {
	ctx := agentCtx(t)
	result := callTool(t, LearnHandler(), ctx, map[string]any{
		"moment_name": "m",
		"facts": []any{map[string]any{
			"path": jobSlot, "topic": "principles", "category": "mission/foo",
			"title": "t", "body": "b",
		}},
	})
	require.True(t, result.IsError)
	require.Contains(t, resultText(t, result), "cannot be combined")
}

func TestLearn_PrivatePath_OutsideWritableRootRefused(t *testing.T) {
	ctx := agentCtx(t)
	for _, p := range []string{"kb/.drafts/x.md", ".github/x.md", ".knomit/x.md", ".knomitjobs/x.md"} {
		result := learnAtPath(t, ctx, p, "t", "b")
		require.Truef(t, result.IsError, "learn must refuse %s", p)
		require.Containsf(t, resultText(t, result), ".knomit/<area>/", "path %s", p)
	}
}

// Adding `path` made the schema's required list ["title","body"] — topic and
// category left it, so "no path, no topic, no category" is an input shape the
// JSON Schema used to reject client-side and now lets through to the handler.
// It must produce a clear error rather than panicking or, worse, allocating a
// fact at the ontology root.
func TestLearn_NoPathNoTopicNoCategory_Refused(t *testing.T) {
	ctx := agentCtx(t)
	result := callTool(t, LearnHandler(), ctx, map[string]any{
		"moment_name": "m",
		"facts":       []any{map[string]any{"title": "t", "body": "b"}},
	})
	require.True(t, result.IsError, "a fact with no placement at all must be refused")
	text := resultText(t, result)
	require.Contains(t, text, "fact 0", "the error must say WHICH fact is unplaced")
	require.Regexp(t, `(?i)topic|category|path`, text,
		"the error must name the missing field, not just fail: %s", text)
}

// Topic without category is the other half of the same hole: BuildFactPath
// would otherwise mint a fact directly under the topic directory.
func TestLearn_TopicWithoutCategory_Refused(t *testing.T) {
	ctx := agentCtx(t)
	result := callTool(t, LearnHandler(), ctx, map[string]any{
		"moment_name": "m",
		"facts":       []any{map[string]any{"topic": "architecture", "title": "t", "body": "b"}},
	})
	require.True(t, result.IsError, "a fact with a topic but no category must be refused")
	require.Contains(t, resultText(t, result), "category")
}

// A named slot is allocated ONCE. A second learn would start a second history
// for one slot, and a walk over either half silently under-reports.
func TestLearn_PrivatePath_ThatAlreadyExistsRefused(t *testing.T) {
	ctx := agentCtx(t)
	require.False(t, learnAtPath(t, ctx, jobSlot, "crawl-state", "run 1").IsError)

	result := learnAtPath(t, ctx, jobSlot, "crawl-state", "run 2")
	require.True(t, result.IsError, "a second learn on the same slot must fail")
	require.Contains(t, resultText(t, result), "knomit_update",
		"the error must point at the tool that IS correct here")
}

// Invisibility is the feature, not a side effect.
func TestLearn_PrivatePath_IsNotDiscoverable(t *testing.T) {
	ctx := agentCtx(t)
	require.False(t, learnAtPath(t, ctx, jobSlot, "UniqueCrawlStateTitle", "run 1").IsError)

	result := callTool(t, QueryHandler(), ctx, map[string]any{"text": "UniqueCrawlStateTitle"})
	require.False(t, result.IsError, resultText(t, result))
	require.NotContains(t, resultText(t, result), jobSlot,
		"private state must never surface in query results")
}
