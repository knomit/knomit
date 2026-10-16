package synthesize

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// RuleValidation on the review pipeline's writes (user ruling 2026-10-16):
// review commits through the store's write cores, which validate every fact
// against the ontology at the branch's tip, so a review write that would
// leave a fact failing a rule is refused at the source with the rule's
// message (reported as a warn, as every review write failure is) and
// nothing lands. Two writers on a repository on the code preset:
//   - prune's confidence update of a LEGACY principle that lacks the
//     designer entity (an edit that does not fix it): refused, the file is
//     unchanged;
//   - a prune merge whose output is a principle without the designer entity
//     (a new invalid principle): refused, nothing is written and the members
//     stay.
//
// SABOTAGE: drop validateWrite from writeFileExact -> the update lands ->
// red; drop it from batchWriteLocked -> the merge lands -> red.
func TestReview_AWriteThatFailsTheOntologyIsRefusedAtTheSource(t *testing.T) {
	ctx := context.Background()
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	ontYAML, err := fact.CodeOntology().Serialize()
	require.NoError(t, err)
	principle := func(body string) string {
		f := fact.NewFact("placeholder.md")
		f.Title, f.Body = "A principle "+body, body
		f.Kind, f.Type = fact.Pragmatic, fact.Policy
		f.Confidence, f.Sources, f.Domain = 0.9, 1, []string{"global"}
		out, err := fact.SerializeFact(f)
		require.NoError(t, err)
		return out
	}
	const legacy, a, b = "kb/principles/ui/legacy.md", "kb/principles/ui/a.md", "kb/principles/ui/b.md"
	const branch = "agent/test"
	// Written by the repository's init, where no rule runs: legacy content.
	require.NoError(t, svc.InitRepo(ctx, map[string]string{
		fact.OntologyFile: string(ontYAML),
		legacy:            principle("legacy"),
		a:                 principle("a"),
		b:                 principle("b"),
	}, branch))
	read := func(p string) string {
		res, err := svc.Facts().ReadFact(ctx, branch, p, nil)
		require.NoError(t, err)
		return res.Content
	}
	before := read(legacy)

	sink, warns := collectWarns()
	_, err = ApplyPruneDecisions(ctx, svc.Facts(), svc.Search(),
		[]PruneDecision{{Path: legacy, Action: "update", Confidence: 0.5}},
		[]MergeEntry{{
			Paths:  []string{a, b},
			Merged: mergedFact{Path: "kb/principles/ui/merged.md", Title: "Merged principle", Body: "merged", Type: "observation"},
		}},
		"review-test", sink, branch, bareRefFixture, "kb", fact.CodeOntology())
	require.NoError(t, err)

	joined := strings.Join(*warns, "\n")
	require.Contains(t, joined, "update write "+legacy, "the update was refused: %s", joined)
	require.Contains(t, joined, "must-have-designer-entity")
	require.Contains(t, joined, "principles must be authored via /knomit-principle")
	require.Equal(t, before, read(legacy), "the legacy principle is unchanged")

	_, err = svc.Facts().ReadFact(ctx, branch, "kb/principles/ui/merged.md", nil)
	require.Error(t, err, "the invalid merge output was never written")
	require.NotEmpty(t, read(a), "the merge's members stay")
	require.GreaterOrEqual(t, strings.Count(joined, "must-have-designer-entity"), 2, "both writes refused: %s", joined)
}
