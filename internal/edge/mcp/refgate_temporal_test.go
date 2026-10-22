package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// THE TEMPORAL CONTRACT, end to end.
//
// kb/principles/philosophy/historical-not-current: "Every ref, edge, and
// provenance link resolves at the commit point in time of the referrer — never
// at HEAD." A write-time ref gate is the easiest place in the system to break
// that, in two distinct ways, and both were live:
//
//  1. Re-judging a ref the fact ALREADY carried. B cited A when B was written;
//     that resolved then and is a fact about the past. Once A is retracted,
//     re-checking B's ref at today's HEAD made B uneditable — a retraction
//     anywhere in history froze every fact that ever cited it.
//
//  2. Asking a narrower question than the reader. A retracted fact keeps a
//     navigable last-valid blob, so FactExistsAt resolves it and the UI renders
//     a ref to it as a live `fact` link. A gate using live-only existence
//     rejected writes whose refs the rest of the system resolves fine.
//
// Both are asserted below against the real handlers, not against the gate's
// unit surface, because both were reachable only through them.

// retractFact is the MCP retract call, so these tests exercise the same path a
// user does rather than deleting behind the tool's back.
func retractFact(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"file": path, "moment_name": "retract", "reason": "superseded",
	}
	r, err := RetractHandler()(ctx, req)
	require.NoError(t, err)
	require.False(t, r.IsError, "retract failed: %s", resultText(t, r))
}

// (1) Editing a fact must not re-litigate the citations it already carried.
func TestUpdate_DoesNotRejudgeRefsWrittenAtAnEarlierCommit(t *testing.T) {
	svc, ctx, emb := newOriginTestRepo(t)

	rA, err := LearnHandler(emb)(ctx, learnReq("seed-a", map[string]any{
		"topic": "technology", "category": "ai/agents",
		"title": "Target fact that will later be retracted",
		"body":  "Some grounded observation about memory portability in agents.",
		"type":  "observation", "confidence": 0.8, "sources": 2,
		"domain": []any{"ai"}, "entities": []any{"memory"}, "refs": []any{},
	}))
	require.NoError(t, err)
	require.False(t, rA.IsError, resultText(t, rA))
	pathA := mergedFactPath(t, rA)

	rB, err := LearnHandler(emb)(ctx, learnReq("seed-b", map[string]any{
		"topic": "technology", "category": "security/advisories",
		"title": "Citing fact whose ref resolved when it was written",
		"body":  "This cited A at the moment it was authored — a fact about history.",
		"type":  "observation", "confidence": 0.7, "sources": 1,
		"domain": []any{"security"}, "entities": []any{"x"},
		"refs": []any{pathA},
	}))
	require.NoError(t, err)
	require.False(t, rB.IsError, resultText(t, rB))
	pathB := mergedFactPath(t, rB)

	retractFact(t, ctx, pathA)
	live, err := svc.Facts().FactExists(context.Background(), "agent/test", pathA)
	require.NoError(t, err)
	require.False(t, live, "precondition: A must no longer be live on the branch")

	// Change only the title. No refs field is sent at all — B's ref list is
	// carried forward verbatim.
	var ureq mcpgo.CallToolRequest
	ureq.Params.Arguments = map[string]any{
		"file": pathB, "moment_name": "retitle-b",
		"updates": map[string]any{"title": "A better title for the citing fact"},
	}
	ur, err := UpdateHandler()(ctx, ureq)
	require.NoError(t, err)
	require.False(t, ur.IsError,
		"editing a title must not re-litigate a ref written at an earlier commit: %s",
		resultText(t, ur))

	// And the ref survives the edit: history is preserved, not scrubbed.
	updated := readBack(t, svc, pathB)
	require.NotEmpty(t, updated.Refs, "the historical citation must still be there")
}

// (2) A NEW ref to a retracted-but-reachable fact is accepted, because that is
// what the reader resolves. This is the lineage citation a superseding fact
// makes: "this replaces the thing we retracted."
func TestLearn_AcceptsNewRefToRetractedButReachableFact(t *testing.T) {
	svc, ctx, emb := newOriginTestRepo(t)

	rA, err := LearnHandler(emb)(ctx, learnReq("seed-a", map[string]any{
		"topic": "technology", "category": "ai/agents",
		"title": "An observation that gets retracted before anyone cites it",
		"body":  "Superseded knowledge that nonetheless remains part of the record.",
		"type":  "observation", "confidence": 0.8, "sources": 2,
		"domain": []any{"ai"}, "entities": []any{"memory"}, "refs": []any{},
	}))
	require.NoError(t, err)
	require.False(t, rA.IsError, resultText(t, rA))
	pathA := mergedFactPath(t, rA)

	retractFact(t, ctx, pathA)

	// The reader still resolves it — that is the historical-graph invariant.
	resolves, err := svc.FactQuery().FactExistsAt(context.Background(), "agent/test", pathA, "")
	require.NoError(t, err)
	require.True(t, resolves,
		"precondition: a retracted fact stays reachable by walk-back, so the UI renders it as a fact link")

	rB, err := LearnHandler(emb)(ctx, learnReq("cite-retracted", map[string]any{
		"topic": "technology", "category": "security/advisories",
		"title": "A fact that supersedes the retracted one and says so",
		"body":  "Citing what it replaces is lineage, and the target is still navigable.",
		"type":  "observation", "confidence": 0.9, "sources": 1,
		"domain": []any{"security"}, "entities": []any{"x"},
		"refs": []any{pathA},
	}))
	require.NoError(t, err)
	require.False(t, rB.IsError,
		"the gate must accept a ref the reader resolves; asking a narrower question than "+
			"the reader means rejecting writes the UI would render as live links: %s",
		resultText(t, rB))
}

// A path that never existed in this repo's history is still rejected — the two
// exemptions above must not degrade into "anything goes".
func TestLearn_StillRejectsRefToAPathThatNeverExisted(t *testing.T) {
	_, ctx, emb := newOriginTestRepo(t)

	r, err := LearnHandler(emb)(ctx, learnReq("typo", map[string]any{
		"topic": "technology", "category": "ai/agents",
		"title": "A fact citing a path nobody ever wrote",
		"body":  "The citation is a typo and there is no version of it anywhere in history.",
		"type":  "observation", "confidence": 0.7, "sources": 1,
		"domain": []any{"ai"}, "entities": []any{"x"},
		"refs": []any{"kb/technology/ai/agents/deadbeef.md"},
	}))
	require.NoError(t, err)
	require.True(t, r.IsError, "a ref to a path with no version in history must still be rejected")
	require.Contains(t, resultText(t, r), "deadbeef.md")
}
