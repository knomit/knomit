package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
)

// updateOut is the part of knomit_update's result these tests read. Commit is
// a pointer so "absent" and "empty" are told apart.
type updateOut struct {
	File      string  `json:"file"`
	Commit    *string `json:"commit"`
	Unchanged bool    `json:"unchanged"`
	Summary   string  `json:"summary"`
	WrittenTo struct {
		Branch string `json:"branch"`
	} `json:"written_to"`
}

func runUpdate(t *testing.T, ctx context.Context, path string, updates map[string]any) updateOut {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"file":        path,
		"moment_name": "noop-test",
		"updates":     updates,
	}
	res, err := UpdateHandler()(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, "update must succeed: %s", resultText(t, res))
	var out updateOut
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &out))
	return out
}

// An update whose final bytes equal the fact's current revision writes
// nothing (#432): the result says unchanged, carries NO commit, its summary
// does not say "wrote", and the branch tip does not move. A real change after
// it still reports its commit.
//
// SABOTAGE: drop the blob-hash comparison from writeFileExact -> the update
// makes an empty commit, the tip moves and the result carries a commit -> red.
// SABOTAGE: drop the Unchanged branch from the update handler -> the result
// carries the tip as "commit" and says "wrote 1 fact revision" -> red.
func TestUpdateHandler_IdenticalUpdateIsUnchanged(t *testing.T) {
	svc, ctx, emb := newPrinciplesTestRepo(t)
	seed, err := LearnHandler(emb)(ctx, principleLearnReq("seed", 0.8, []any{"global"}))
	require.NoError(t, err)
	require.False(t, seed.IsError, "seed must succeed: %s", resultText(t, seed))
	path := mergedFactPath(t, seed)

	tip, err := svc.HeadCommit(context.Background(), "agent/test")
	require.NoError(t, err)

	// The confidence it already has: the serialised fact is byte-identical.
	out := runUpdate(t, ctx, path, map[string]any{"confidence": 0.8})
	require.True(t, out.Unchanged, "identical content is a no-op")
	require.Nil(t, out.Commit, "no commit is reported for a write that made none")
	require.Equal(t, path, out.File)
	require.Equal(t, "agent/test", out.WrittenTo.Branch)
	require.True(t, strings.HasPrefix(out.Summary, "no change: "+path+" already has this content"),
		"the summary must not say wrote: %q", out.Summary)
	now, err := svc.HeadCommit(context.Background(), "agent/test")
	require.NoError(t, err)
	require.Equal(t, tip, now, "the branch tip did not move")

	// A real change commits and says so.
	out = runUpdate(t, ctx, path, map[string]any{"confidence": 0.6})
	require.False(t, out.Unchanged)
	require.NotNil(t, out.Commit)
	now, err = svc.HeadCommit(context.Background(), "agent/test")
	require.NoError(t, err)
	require.Equal(t, *out.Commit, now)
	require.NotEqual(t, tip, now)
}

// The comparison is of the FINAL bytes, after the update path canonicalises
// refs, never of what the caller sent: a fact stored with a BARE local ref
// (kb/x.md — a hand edit or an old write) is rewritten to kb://<id>/kb/x.md by
// the refs gate, so re-saving it unchanged is a real commit once. After that
// the same re-save is a no-op.
//
// SABOTAGE: compare the caller's input (or the pre-gate fact) instead of the
// serialised bytes -> the first re-save reads as unchanged and the bare ref
// stays on disk -> red.
func TestUpdateHandler_BareRefResaveCommitsOnceThenUnchanged(t *testing.T) {
	svc, ctx, emb := newPrinciplesTestRepo(t)
	seed, err := LearnHandler(emb)(ctx, principleLearnReq("seed", 0.8, []any{"global"}))
	require.NoError(t, err)
	require.False(t, seed.IsError, "seed must succeed: %s", resultText(t, seed))
	path := mergedFactPath(t, seed)

	// Store the fact with a bare self-ref, bypassing the gate.
	bg := context.Background()
	cur, err := svc.Facts().ReadFact(bg, "agent/test", path, nil)
	require.NoError(t, err)
	f, err := fact.ParseFact(path, cur.Content)
	require.NoError(t, err)
	f.Refs = []string{path}
	raw, err := fact.SerializeFact(f)
	require.NoError(t, err)
	w, err := svc.Facts().WriteFact(bg, "agent/test", path, raw, "fixture: bare ref", "update")
	require.NoError(t, err)
	require.False(t, w.Unchanged, "fixture: the bare-ref version is a new revision")
	tip := w.CommitHash

	out := runUpdate(t, ctx, path, map[string]any{"confidence": 0.8})
	require.False(t, out.Unchanged, "canonicalising the bare ref changes the bytes")
	require.NotNil(t, out.Commit)
	require.NotEqual(t, tip, *out.Commit)
	after, err := svc.Facts().ReadFact(bg, "agent/test", path, nil)
	require.NoError(t, err)
	af, err := fact.ParseFact(path, after.Content)
	require.NoError(t, err)
	require.Len(t, af.Refs, 1)
	require.True(t, strings.HasPrefix(af.Refs[0], "kb://"), "the ref was canonicalised: %v", af.Refs)

	tip, err = svc.HeadCommit(bg, "agent/test")
	require.NoError(t, err)
	out = runUpdate(t, ctx, path, map[string]any{"confidence": 0.8})
	require.True(t, out.Unchanged, "the canonical version re-saved is a no-op")
	require.Nil(t, out.Commit)
	now, err := svc.HeadCommit(bg, "agent/test")
	require.NoError(t, err)
	require.Equal(t, tip, now)
}

// #432 on the mission template's coordination signals (learn_dedup: off
// topics). A lease re-arm (the work-task recipe's and claims.js's
// knomit.update(path, {expires: now + …})) always moves expires, so it is a
// real commit and still re-arms the due trigger. An update that leaves the
// signal byte-identical — the same expires again — is a no-op: no commit. No
// trigger can depend on that commit, because tree episodes are read from a
// tree diff (DiffFacts) and due fires from the expires instant, and neither
// changes.
//
// SABOTAGE: drop the blob-hash comparison from writeFileExact -> the
// same-expires update commits -> red.
func TestUpdateHandler_SignalLeaseRearmCommitsSameLeaseUnchanged(t *testing.T) {
	ctx, emb, svc := newMissionDedupRepo(t, missionOntology(t, "inbox", true))
	e1 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	r, err := LearnHandler(emb)(ctx, signalReq("inbox", "peer-bbbbbbbb/working", "task-1", e1))
	require.NoError(t, err)
	require.False(t, r.IsError, resultText(t, r))
	path := mergedFactPath(t, r)
	bg := context.Background()
	tip, err := svc.HeadCommit(bg, "agent/test")
	require.NoError(t, err)

	out := runUpdate(t, ctx, path, map[string]any{"expires": e1.Format(time.RFC3339)})
	require.True(t, out.Unchanged, "the same lease again changes nothing")
	require.Nil(t, out.Commit)
	now, err := svc.HeadCommit(bg, "agent/test")
	require.NoError(t, err)
	require.Equal(t, tip, now)

	e2 := e1.Add(5 * time.Minute)
	out = runUpdate(t, ctx, path, map[string]any{"expires": e2.Format(time.RFC3339)})
	require.False(t, out.Unchanged, "a re-arm moves expires: a real write")
	require.NotNil(t, out.Commit)
	now, err = svc.HeadCommit(bg, "agent/test")
	require.NoError(t, err)
	require.Equal(t, *out.Commit, now)
	got, err := svc.Facts().ReadFact(bg, "agent/test", path, nil)
	require.NoError(t, err)
	f, err := fact.ParseFact(path, got.Content)
	require.NoError(t, err)
	require.Equal(t, e2.Format(time.RFC3339), f.Expires)
}
