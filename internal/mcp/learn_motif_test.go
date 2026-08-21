package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

// riFrom pulls the write RepoInstance back out of a handler context, so these
// tests can reuse readFactAt (write_path_test.go) instead of shipping a
// near-duplicate keyed on *store.Service.
func riFrom(t *testing.T, ctx context.Context) *repos.RepoInstance {
	t.Helper()
	b := repos.BindingFromContext(ctx)
	require.NotNil(t, b)
	return b.Write()
}

// motifMergeFixture builds a new/existing pair whose winner is the one the
// caller asked for, asserts that intent against newFactWins so the fixture
// cannot silently invert, and returns the merge result.
func motifMergeFixture(t *testing.T, newMotifs, existingMotifs []string, newWins bool) fact.Fact {
	t.Helper()
	nf := fact.NewFact("kb/alpha/new.md")
	nf.Title = "New"
	nf.Body = "New body"
	nf.Type = fact.Observation
	nf.Confidence = 0.9
	nf.Sources = 1
	nf.Motifs = newMotifs

	ex := fact.NewFact("kb/alpha/existing.md")
	ex.Title = "Existing"
	ex.Body = "Existing body"
	ex.Type = fact.Observation
	ex.Confidence = 0.5
	ex.Sources = 1
	ex.Motifs = existingMotifs

	if !newWins {
		nf.Confidence, ex.Confidence = 0.5, 0.9
	}
	require.Equal(t, newWins, newFactWins(nf, ex), "fixture must produce the intended winner")
	return mergeFacts(nf, ex, "kb/alpha/existing.md")
}

// TestMergeFacts_MotifsWinnerFirstTrimAtCap is the roadmap's conformance case,
// verbatim: winner has 2, loser has 3, and exactly the winner's 2 plus the
// loser's FIRST 1 survive, in that order.
func TestMergeFacts_MotifsWinnerFirstTrimAtCap(t *testing.T) {
	merged := motifMergeFixture(t,
		[]string{"win-one", "win-two"},
		[]string{"lose-one", "lose-two", "lose-three"},
		true)
	require.Equal(t, []string{"win-one", "win-two", "lose-one"}, merged.Motifs)
}

// TestMergeFacts_MotifsFollowTheWinnerNotTheCaller — same inputs, opposite
// winner, so the order flips. This is what distinguishes winner-first from the
// incoming-first union that domain and entities happen to use; without it a
// test could pass on either implementation.
func TestMergeFacts_MotifsFollowTheWinnerNotTheCaller(t *testing.T) {
	merged := motifMergeFixture(t,
		[]string{"new-one", "new-two"},
		[]string{"old-one", "old-two", "old-three"},
		false)
	require.Equal(t, []string{"old-one", "old-two", "old-three"}, merged.Motifs)
}

// TestMergeFacts_DomainAndEntitiesStayIncomingFirst pins the deliberate local
// inconsistency, so a later tidying pass cannot "make them consistent" without
// a failing test to argue with. Domain and entities are uncapped, so their
// order is cosmetic; motifs are capped, so order decides what survives.
func TestMergeFacts_DomainAndEntitiesStayIncomingFirst(t *testing.T) {
	nf := fact.NewFact("kb/alpha/new.md")
	nf.Title, nf.Body, nf.Type = "New", "New body", fact.Observation
	nf.Confidence, nf.Sources = 0.5, 1 // loses
	nf.Domain = []string{"new-domain"}
	nf.Entities = []string{"NewEntity"}

	ex := fact.NewFact("kb/alpha/existing.md")
	ex.Title, ex.Body, ex.Type = "Existing", "Existing body", fact.Observation
	ex.Confidence, ex.Sources = 0.9, 1 // wins
	ex.Domain = []string{"old-domain"}
	ex.Entities = []string{"OldEntity"}

	require.False(t, newFactWins(nf, ex))
	merged := mergeFacts(nf, ex, "kb/alpha/existing.md")

	require.Equal(t, []string{"new-domain", "old-domain"}, merged.Domain,
		"domain stays incoming-first even when the incoming fact loses")
	require.Equal(t, []string{"NewEntity", "OldEntity"}, merged.Entities,
		"entities stay incoming-first even when the incoming fact loses")
}

func TestMergeFacts_MotifsDeduplicateAcrossParents(t *testing.T) {
	merged := motifMergeFixture(t,
		[]string{"shared-shape"},
		[]string{"shared-shape", "other-shape"},
		true)
	require.Equal(t, []string{"shared-shape", "other-shape"}, merged.Motifs)
}

func TestMergeFacts_MotifsEmptyStaysEmpty(t *testing.T) {
	merged := motifMergeFixture(t, nil, nil, true)
	require.Nil(t, merged.Motifs)
}

// TestMergeFacts_MergedFactSerializes — an over-cap union that was not trimmed
// would fail SerializeFact and abort the whole knomit_learn call, turning a
// routine merge into a user-visible error. Trimming in the merge is what keeps
// that from happening.
func TestMergeFacts_MergedFactSerializes(t *testing.T) {
	merged := motifMergeFixture(t,
		[]string{"win-one", "win-two", "win-three"},
		[]string{"lose-one", "lose-two", "lose-three"},
		true)
	merged.Domain = []string{}
	merged.Entities = []string{}
	merged.Refs = []string{}
	_, err := fact.SerializeFact(merged)
	require.NoError(t, err)
	require.Len(t, merged.Motifs, fact.MaxMotifs)
}

// motifLearnReq is one observation carrying motifs, with a body whose LENGTH
// is controlled — newLenEmbedder keys similarity on length, so two requests
// with equal-length bodies dedup-match.
func motifLearnReq(moment, body string, confidence float64, motifs []any) mcpgo.CallToolRequest {
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": moment,
		"facts": []any{
			map[string]any{
				"topic":      "gotchas",
				"category":   "build/tooling",
				"title":      "A build gotcha",
				"body":       body,
				"type":       "observation",
				"domain":     []any{"build"},
				"confidence": confidence,
				"sources":    1,
				"entities":   []any{"Bazel"},
				"motifs":     motifs,
				"refs":       []any{},
			},
		},
	}
	return req
}

// TestLearnHandler_MotifsSurviveDedupMergeAcrossSessions is Phase 1's named
// dynamics question, asked end-to-end: a motif written in session N must
// survive the dedup merge in session N+1, and the survivors must be the
// winner's first.
//
// It drives the real knomit_learn handler rather than mergeFacts directly. The
// unit tests next door already pin the merge function; what they cannot see is
// whether the handler carries motifs into it at all, whether the merged fact
// serializes, and whether what lands on disk matches — three places a correct
// merge function still ends up storing nothing.
func TestLearnHandler_MotifsSurviveDedupMergeAcrossSessions(t *testing.T) {
	_, ctx, emb := newPrinciplesTestRepo(t)

	// Session N: two motifs, lower confidence, so it LOSES the merge below.
	body := "the toolchain silently falls back when the variable is unset...."
	r1, err := LearnHandler(emb)(ctx, motifLearnReq("session-n", body, 0.6,
		[]any{"silent-fallback", "config-drift"}))
	require.NoError(t, err)
	require.False(t, r1.IsError, "seed write must succeed: %s", resultText(t, r1))

	seedPath := mergedFactPath(t, r1)
	seed := readFactAt(t, riFrom(t, ctx), seedPath)
	require.Equal(t, []string{"silent-fallback", "config-drift"}, seed.Motifs,
		"session N must actually store its motifs, or the merge below proves nothing")

	// Session N+1: same body length so dedup matches, higher confidence so the
	// INCOMING fact wins, and three motifs of its own.
	r2, err := LearnHandler(emb)(ctx, motifLearnReq("session-n-plus-1", body, 0.9,
		[]any{"unmonitored-expiry", "harness-over-model", "capital-influx"}))
	require.NoError(t, err)
	require.False(t, r2.IsError, "the merge must not fail the call: %s", resultText(t, r2))

	mergedPath := mergedFactPath(t, r2)
	merged := readFactAt(t, riFrom(t, ctx), mergedPath)

	// A MERGE must have happened, not two separate facts. Without these two
	// lines every assertion below also holds when dedup missed entirely and
	// session N+1 simply minted its own fact carrying its own three motifs —
	// the test would pass while proving nothing about the merge at all.
	require.Equal(t, seedPath, mergedPath,
		"session N+1 must have merged INTO the seed, not landed at its own path")
	require.Equal(t, 2, merged.Sources, "a merge sums sources")
	require.Equal(t, 0.9, merged.Confidence, "the new fact must have won the merge")

	require.Equal(t,
		[]string{"unmonitored-expiry", "harness-over-model", "capital-influx"},
		merged.Motifs,
		"the winner's three motifs fill the cap; the loser's are dropped, not interleaved")
}

// TestLearnHandler_MotifsMergeWinnerFirstOnDisk — the same path with room left
// over, so the loser's motifs actually appear and their ORDER is observable on
// disk rather than only in the merge function's return value.
func TestLearnHandler_MotifsMergeWinnerFirstOnDisk(t *testing.T) {
	_, ctx, emb := newPrinciplesTestRepo(t)

	body := "the toolchain silently falls back when the variable is unset...."
	r1, err := LearnHandler(emb)(ctx, motifLearnReq("session-n", body, 0.6,
		[]any{"config-drift"}))
	require.NoError(t, err)
	require.False(t, r1.IsError, resultText(t, r1))

	r2, err := LearnHandler(emb)(ctx, motifLearnReq("session-n-plus-1", body, 0.9,
		[]any{"silent-fallback"}))
	require.NoError(t, err)
	require.False(t, r2.IsError, resultText(t, r2))

	mergedPath := mergedFactPath(t, r2)
	require.Equal(t, mergedFactPath(t, r1), mergedPath, "the two writes must have merged")
	merged := readFactAt(t, riFrom(t, ctx), mergedPath)
	require.Equal(t, 2, merged.Sources, "a merge sums sources")
	require.Equal(t, []string{"silent-fallback", "config-drift"}, merged.Motifs,
		"winner first, then the loser's — on disk, not just in memory")
}

// TestLearnHandler_MalformedMotifFailsTheCall — the gate is real at the tool
// boundary, not only in a unit test of SerializeFact.
func TestLearnHandler_MalformedMotifFailsTheCall(t *testing.T) {
	_, ctx, emb := newPrinciplesTestRepo(t)

	r, err := LearnHandler(emb)(ctx, motifLearnReq("bad", "a body of some length here.", 0.8,
		[]any{"onlyoneword"}))
	require.NoError(t, err)
	require.True(t, r.IsError, "a malformed motif must fail the call, not be silently dropped")
	require.Contains(t, resultText(t, r), "kebab-case words")
}

// TestLearnHandler_SubjectMotifIsDroppedSilently — the other half of the
// contract at the same boundary: the call SUCCEEDS and the fact stores without
// the motif.
func TestLearnHandler_SubjectMotifIsDroppedSilently(t *testing.T) {
	_, ctx, emb := newPrinciplesTestRepo(t)

	// "bazel-build" is entity ∪ domain for motifLearnReq's fixtures.
	r, err := LearnHandler(emb)(ctx, motifLearnReq("subject", "a body of some length here.", 0.8,
		[]any{"bazel-build", "silent-fallback"}))
	require.NoError(t, err)
	require.False(t, r.IsError, "the strip must not fail the call: %s", resultText(t, r))

	stored := readFactAt(t, riFrom(t, ctx), mergedFactPath(t, r))
	require.Equal(t, []string{"silent-fallback"}, stored.Motifs)
}
