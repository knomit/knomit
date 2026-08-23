package synthesize

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// The seed scan has TWO paths to a fact.Fact — the full scan projects a search
// hit (factFromSearchResult), the incremental scan parses the file
// (fact.ParseFact) — and factFromSearchResult's own doc comment promises they
// "yield the same type".
//
// This asserts that promise GENERICALLY rather than field by field. The defect
// it was written for dropped Motifs, but a motif-shaped assertion would not
// have caught the next field to go missing, and the bug is not "motifs were
// forgotten" — it is "two projections of one thing were allowed to disagree".
//
// The consequence when they do disagree is invisible: a full-scan session (any
// FIRST session on a corpus) hands prune, distill and bridging a different fact
// from the one an incremental session hands them, and every test that happens
// to run one path passes.
func TestSeedScanPaths_ProduceTheSameFact(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)

	const path = "kb/gotchas/uitesting/agentclicks.md"
	f := fact.NewFact(path)
	f.Title = "An agent testing a UI will execute JavaScript instead of clicking"
	f.Body = "Driving app state directly bypasses the path the verifier believes it is checking."
	f.Type = fact.Observation
	f.Kind = fact.Epistemic
	f.Domain = []string{"evaluation", "coding-agents"}
	f.Entities = []string{"Cognition", "Devin"}
	f.Motifs = []string{"measure-becomes-target"}
	f.Confidence = 0.8
	f.Sources = 1
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = env.svc.Facts().WriteFact(ctx, env.branch, path, content, "write", "test")
	require.NoError(t, err)

	// Path 1 — the full scan's projection of a search hit.
	results, err := env.svc.Search().Search(ctx, env.branch, store.SearchOptions{Limit: 100})
	require.NoError(t, err)
	require.Len(t, results, 1)
	fromSearch := factFromSearchResult(results[0])

	// Path 2 — the incremental scan's parse of the same bytes.
	read, err := env.svc.Facts().ReadFact(ctx, env.branch, path, nil)
	require.NoError(t, err)
	fromParse, err := fact.ParseFact(path, read.Content)
	require.NoError(t, err)

	// Field by field, but exhaustively and by NAME, so a field added to
	// fact.Fact and forgotten in one projection shows up here.
	require.Equal(t, fromParse.Path(), fromSearch.Path())
	require.Equal(t, fromParse.Title, fromSearch.Title)
	require.Equal(t, fromParse.Body, fromSearch.Body)
	require.Equal(t, fromParse.Kind, fromSearch.Kind)
	require.Equal(t, fromParse.Type, fromSearch.Type)
	require.Equal(t, fromParse.Domain, fromSearch.Domain)
	require.Equal(t, fromParse.Entities, fromSearch.Entities)
	require.Equal(t, fromParse.Motifs, fromSearch.Motifs)
	require.Equal(t, fromParse.Confidence, fromSearch.Confidence)
	require.Equal(t, fromParse.Sources, fromSearch.Sources)
	require.Equal(t, fromParse.Refs, fromSearch.Refs)

	// ORIGIN — a SECOND, DIFFERENT divergence this test found once it was
	// written generically, and one this phase has deliberately not changed.
	//
	// ParseFact applies the type-aware default on read (an authored
	// observation with no `origin:` in its frontmatter parses as "authored"),
	// while the index stores the elided value and the projection copies it
	// verbatim, so the full-scan path yields "". Nothing today is misled: every
	// consumer tests for fact.Discovered specifically, and neither "" nor
	// "authored" is that. It is pinned here rather than normalised away,
	// because the day a consumer starts asking "is this authored?" the two scan
	// paths will answer differently and this is the line that says so.
	//
	// Reported to the design authority; changing it is a decision for whoever
	// owns the origin contract, not for the phase that noticed.
	require.Equal(t, fact.Authored, fromParse.Origin, "the parse path defaults it")
	require.Empty(t, string(fromSearch.Origin), "the projection carries the elided value")

	// And the precondition that makes the assertions above mean something: the
	// fixture must actually CARRY the fields it claims to compare (lesson 5 —
	// two empty values are equal and prove nothing).
	require.NotEmpty(t, fromParse.Motifs)
	require.NotEmpty(t, fromParse.Entities)
	require.NotEmpty(t, fromParse.Domain)
}
