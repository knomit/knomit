package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

// seedValidPrinciple writes a fact that satisfies the must-have-designer
// rule via LearnHandler, and returns the path it was written to. It is a
// helper for update tests that need a pre-existing fact to mutate.
func seedValidPrinciple(t *testing.T, ctx context.Context) string {
	t.Helper()

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": "seed",
		"facts": []any{
			map[string]any{
				"topic":      "principles",
				"category":   "mission/foo",
				"title":      "Seeded Principle",
				"body":       "designer authored this.",
				"kind":       "pragmatic",
				"type":       "policy",
				"domain":     []any{},
				"confidence": 0.8,
				"sources":    1,
				"entities":   []any{"designer"},
				"refs":       []any{},
			},
		},
	}

	result, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, "seed must succeed; got error: %s", resultText(t, result))

	var payload struct {
		Commits []struct {
			File string `json:"file"`
			Hash string `json:"hash"`
		} `json:"commits"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, result)), &payload))
	require.Len(t, payload.Commits, 1)
	require.NotEmpty(t, payload.Commits[0].File)
	return payload.Commits[0].File
}

// TestUpdateHandler_RejectsFailingValidation seeds a valid principle fact,
// then attempts to update it in a way that violates the
// must-have-designer rule (drop the 'designer' entity). UpdateHandler must
// reject the change with an error referencing both the rule name and the
// rule's message.
func TestUpdateHandler_RejectsFailingValidation(t *testing.T) {
	ontology, err := fact.ParseOntology([]byte(principlesOntologyYAML))
	require.NoError(t, err)
	ri := newLearnTestRepo(t, ontology)
	ctx := repos.WithRepoInstance(context.Background(), ri)

	path := seedValidPrinciple(t, ctx)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"file":        path,
		"moment_name": "drop-designer",
		"updates": map[string]any{
			"entities": []any{},
		},
	}

	result, err := UpdateHandler()(ctx, req)
	require.NoError(t, err)
	require.True(t, result.IsError, "expected validation failure to surface as IsError result")

	text := resultText(t, result)
	require.Contains(t, text, "must-have-designer",
		"error must reference the rule name; got %q", text)
	require.Contains(t, text, "/knomit-principle",
		"error must include the rule's message; got %q", text)
}

// TestUpdateHandler_ReplacesRefs verifies refs replace-wholesale semantics:
// the provided list becomes the fact's refs verbatim (deduped), refs absent
// from the list are dropped, and omitting the refs field leaves the list
// unchanged — same contract as domain and entities.
func TestUpdateHandler_ReplacesRefs(t *testing.T) {
	svc, ctx, emb := newPrinciplesTestRepo(t)

	// Seed a valid principle to mutate.
	seed, err := LearnHandler(emb)(ctx, principleLearnReq("seed", 0.8, []any{"global"}))
	require.NoError(t, err)
	require.False(t, seed.IsError, "seed must succeed: %s", resultText(t, seed))
	path := mergedFactPath(t, seed)

	readRefs := func() []string {
		res, err := svc.Facts().ReadFact(context.Background(), "agent/test", path, nil)
		require.NoError(t, err)
		updated, err := fact.ParseFact(path, res.Content)
		require.NoError(t, err)
		return updated.Refs
	}
	update := func(updates map[string]any) {
		var req mcpgo.CallToolRequest
		req.Params.Arguments = map[string]any{
			"file":        path,
			"moment_name": "refs-test",
			"updates":     updates,
		}
		res, err := UpdateHandler()(ctx, req)
		require.NoError(t, err)
		require.False(t, res.IsError, "update must succeed: %s", resultText(t, res))
	}

	const stale = "https://example.com/spec-v1"
	const fresh = "https://example.com/spec-v2"

	// Establish an initial ref, duplicated in the input: stored once.
	update(map[string]any{"refs": []any{stale, stale}})
	require.Equal(t, []string{stale}, readRefs(), "duplicate input refs must be deduped")

	// Replacing with a different list drops the stale ref entirely.
	update(map[string]any{"refs": []any{fresh}})
	require.Equal(t, []string{fresh}, readRefs(), "refs must be replaced, not appended")

	// An update that omits refs leaves the list unchanged.
	update(map[string]any{"confidence": 0.9})
	require.Equal(t, []string{fresh}, readRefs(), "omitted refs field must not touch refs")
}

// TestUpdateHandler_SourcesVerbatimAndOmittedUnchanged covers the update row
// of the sources rule table. update is a manual correction path, trusted like
// learn-new: an explicit count is stored verbatim, and an omitted one leaves
// the existing count alone rather than resetting it to a default.
func TestUpdateHandler_SourcesVerbatimAndOmittedUnchanged(t *testing.T) {
	svc, ctx, emb := newPrinciplesTestRepo(t)

	seed, err := LearnHandler(emb)(ctx, principleLearnReq("seed", 0.8, []any{"global"}))
	require.NoError(t, err)
	require.False(t, seed.IsError, "seed must succeed: %s", resultText(t, seed))
	path := mergedFactPath(t, seed)

	readSources := func() int {
		res, rerr := svc.Facts().ReadFact(context.Background(), "agent/test", path, nil)
		require.NoError(t, rerr)
		updated, perr := fact.ParseFact(path, res.Content)
		require.NoError(t, perr)
		return updated.Sources
	}
	update := func(updates map[string]any) {
		var req mcpgo.CallToolRequest
		req.Params.Arguments = map[string]any{
			"file": path, "moment_name": "sources-test", "updates": updates,
		}
		res, uerr := UpdateHandler()(ctx, req)
		require.NoError(t, uerr)
		require.False(t, res.IsError, "update must succeed: %s", resultText(t, res))
	}

	update(map[string]any{"sources": 4})
	require.Equal(t, 4, readSources(), "an explicit sources must be stored verbatim")

	update(map[string]any{"confidence": 0.9})
	require.Equal(t, 4, readSources(), "an omitted sources must leave the existing count unchanged")

	update(map[string]any{"sources": 0})
	require.Equal(t, 0, readSources(), "an explicit 0 is legal (§2.2 requires only >= 0) and must survive")
}

// knomit_update must refuse a private path for the same reason knomit_learn
// refuses to allocate one: a fact under a dot-prefixed segment is skipped by
// the indexer, Verify and the OKF exporter alike, so a revision written there
// is committed and then permanently invisible — reported as success.
//
// The fixture plants the fact by hand (nothing knomit offers CREATES one) and
// the file therefore EXISTS, which is what makes this test able to fail:
// without the guard the handler's FactExists check passes and the update
// lands. The stored content is re-read afterwards to prove the refusal was a
// refusal, not a write followed by an error.
func TestUpdateHandler_RejectsPrivatePath(t *testing.T) {
	svc, ctx, _ := newPrinciplesTestRepo(t)

	const path = "kb/.drafts/aaaaaaaa.md"
	f := fact.NewFact(path)
	f.Title = "Hand-placed draft"
	f.Body = "Parked in the private stash."
	f.Type = fact.Observation
	f.Domain = []string{"testing"}
	f.Confidence = 0.8
	f.Sources = 1
	f.Entities = []string{}
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "agent/test", path, content, "seed private draft", "")
	require.NoError(t, err)

	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"file":        path,
		"moment_name": "touch-the-stash",
		"updates":     map[string]any{"body": "rewritten"},
	}
	result, err := UpdateHandler()(ctx, req)
	require.NoError(t, err)
	require.True(t, result.IsError, "a private path must be refused, not updated")
	require.Contains(t, resultText(t, result), "private",
		"the error must name the private-path rule")

	res, err := svc.Facts().ReadFact(ctx, "agent/test", path, nil)
	require.NoError(t, err)
	require.Equal(t, content, res.Content, "the refused update must not have landed")
}
