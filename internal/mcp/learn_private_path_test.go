package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
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

// THE hole this guards: a server-owned loose file is protected by NAME, but
// nothing stopped that name being reused as a DIRECTORY. store's buildTree
// drops an existing entry of the same name whatever its mode, so writing
// .knomit/ontology.yaml/x.md replaces the ontology BLOB with a tree — after
// which every later read fails, loadOntology falls through to the embedded
// default, and every subsequent fact is validated against the wrong taxonomy.
// Verified end-to-end before the fix: the learn succeeded and the ontology
// read then failed with "blob: object not found".
func TestLearn_CannotShadowAServerOwnedFileWithADirectory(t *testing.T) {
	ctx := agentCtx(t)
	ri := repos.RepoFromContext(ctx)
	require.NotNil(t, ri)

	// Seed the real blob: "the write was refused" only means something if the
	// file it would have destroyed is actually there to destroy.
	const ontologyYAML = "id: source-code\nname: Source Code Knowledge\n"
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		_, err := svc.Facts().WriteFact(context.Background(), ri.AgentBranch(),
			fact.OntologyFile, ontologyYAML, "test: seed ontology", "updated")
		require.NoError(t, err)
	}))

	before := readOntologyBlob(t, ri)
	require.Equal(t, ontologyYAML, before, "fixture must actually hold an ontology")

	for _, p := range []string{
		fact.OntologyFile + "/x.md",
		fact.OntologyFile + "/deeper/x.md",
	} {
		result := learnAtPath(t, ctx, p, "t", "b")
		require.Truef(t, result.IsError, "learn must refuse %s", p)
		require.Containsf(t, resultText(t, result), ".knomit/<area>/", "path %s", p)
	}

	require.Equal(t, before, readOntologyBlob(t, ri),
		"the ontology must still be readable and unchanged")
}

// readOntologyBlob reads .knomit/ontology.yaml straight out of the store, which
// is what turns "the write was refused" into "the ontology survived".
func readOntologyBlob(t *testing.T, ri *repos.RepoInstance) string {
	t.Helper()
	var content string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		res, err := svc.Facts().ReadFact(context.Background(), ri.AgentBranch(), fact.OntologyFile, nil)
		require.NoError(t, err)
		content = res.Content
	}))
	return content
}

// The refusal must arrive from the AUTHORIZATION predicate, not from the
// storage layer at write time: learn is all-or-nothing, so a path store will
// reject must never be authorized — it would take the rest of the batch down
// with an error naming the wrong cause.
func TestLearn_PrivatePath_DotDotAnywhereRefused(t *testing.T) {
	ctx := agentCtx(t)
	for _, p := range []string{".knomit/jobs/..hidden/x.md", ".knomit/jobs/a..b/x.md"} {
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

// Two inputs naming the SAME explicit path is a caller bug that used to be
// invisible. validateAndBuildFacts keys its pending-write map by path, so the
// second input overwrote the first, while the response was built from
// len(facts) — the call reported two facts written, returned two commit
// entries for one file, and only the second body ever reached git. The
// FactExists slot gate cannot catch it: neither path exists yet.
//
// Knowledge facts cannot hit this (UUID leaves are unique); it arrived with
// explicit paths, and the check covers every path anyway because a silently
// dropped fact is the same failure whatever minted the collision.
func TestLearn_DuplicateExplicitPathsInOneCallRefused(t *testing.T) {
	ctx := agentCtx(t)
	result := callTool(t, LearnHandler(), ctx, map[string]any{
		"moment_name": "job-run",
		"facts": []any{
			map[string]any{"path": jobSlot, "title": "first", "body": "body one"},
			map[string]any{"path": jobSlot, "title": "second", "body": "body two"},
		},
	})
	require.True(t, result.IsError, "two inputs for one slot must be refused, not silently collapsed")
	text := resultText(t, result)
	require.Contains(t, text, jobSlot, "the error must name the colliding path")
	require.Regexp(t, `(?i)duplicate|same path|twice`, text,
		"the error must say WHAT is wrong, not just fail: %s", text)

	// All-or-nothing: a refused batch writes nothing, so the slot is still
	// free for a correct call.
	require.False(t, learnAtPath(t, ctx, jobSlot, "crawl-state", "run 1").IsError,
		"the refused batch must not have consumed the slot")
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
