package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSubjectLabelDF_CountsEachTokenOncePerFact — a token carried on BOTH a
// fact's domain and its path must count once for that fact. Counting
// occurrences instead would let path-heavy corpora inflate their own umbrella
// cut, which is a threshold moving because of how facts are FILED.
func TestSubjectLabelDF_CountsEachTokenOncePerFact(t *testing.T) {
	ctx := context.Background()
	svc, branch := newTokenDFFixture(t)

	// "evaluation" appears on both facts — once via domain, and for the first
	// fact also via its path.
	_, err := svc.Facts().WriteFact(ctx, branch, "kb/technology/ai/evaluation/258174a7.md",
		testFactBodyWithDomainAndEntities("bench", []string{"evaluation"}, []string{"Terminal Bench"}),
		"init a", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, branch, "kb/gotchas/ai/agents/ui-testing/77b3e628.md",
		testFactBodyWithDomainAndEntities("ui", []string{"evaluation"}, []string{"Cognition"}),
		"init b", "")
	require.NoError(t, err)

	got, err := svc.Search().SubjectLabelDF(ctx, branch)
	require.NoError(t, err)

	require.Equal(t, 2, got.LiveFacts)
	require.Equal(t, 2, got.DF["evaluation"], "carried by both facts, once each")
	require.Equal(t, 1, got.DF["cognition"])
	// Precondition (lesson 5): the two values MUST differ, or this fixture
	// cannot tell a working count from a constant.
	require.NotEqual(t, got.DF["evaluation"], got.DF["cognition"])
}

// TestSubjectLabelDF_IsTheSameTokenisationTheStripUses — the gate built on this
// distribution asks of a pair what the write-time subject strip asks of one
// fact. If the two tokenised differently, the gate would be reading a
// vocabulary the corpus does not actually have.
func TestSubjectLabelDF_IsTheSameTokenisationTheStripUses(t *testing.T) {
	ctx := context.Background()
	svc, branch := newTokenDFFixture(t)

	_, err := svc.Facts().WriteFact(ctx, branch, "kb/gotchas/store/resolver/e5d04257.md",
		testFactBodyWithDomainAndEntities("x", []string{"build tooling"}, []string{"Antigravity"}),
		"init", "")
	require.NoError(t, err)

	got, err := svc.Search().SubjectLabelDF(ctx, branch)
	require.NoError(t, err)

	// Multi-word tags split; path segments contribute; the .md extension does
	// not. Exactly fact.SubjectTokens's answer, because it IS that function.
	require.Equal(t, 1, got.DF["build"])
	require.Equal(t, 1, got.DF["tooling"])
	require.Equal(t, 1, got.DF["resolver"])
	require.Equal(t, 1, got.DF["antigravity"])
	require.Zero(t, got.DF["md"], "the .md extension is not a subject claim")
}

// TestSubjectLabelDF_ExcludesFactsNotLiveOnTheBranch — the denominator and the
// counts are both about what is LIVE. A deleted fact that still counted would
// make every ratio derived from this distribution drift upward forever.
func TestSubjectLabelDF_ExcludesFactsNotLiveOnTheBranch(t *testing.T) {
	ctx := context.Background()
	svc, branch := newTokenDFFixture(t)

	_, err := svc.Facts().WriteFact(ctx, branch, "kb/technology/ai/evaluation/258174a7.md",
		testFactBodyWithDomainAndEntities("bench", []string{"evaluation"}, nil), "init", "")
	require.NoError(t, err)

	before, err := svc.Search().SubjectLabelDF(ctx, branch)
	require.NoError(t, err)
	require.Equal(t, 1, before.DF["evaluation"], "precondition: it counted while live")

	_, err = svc.Facts().DeleteFact(ctx, branch, "kb/technology/ai/evaluation/258174a7.md", "retract")
	require.NoError(t, err)

	got, err := svc.Search().SubjectLabelDF(ctx, branch)
	require.NoError(t, err)
	require.Zero(t, got.LiveFacts)
	require.Empty(t, got.DF)
}
