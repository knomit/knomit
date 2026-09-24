package synthesize

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// knomit#249, option A. A prune-merge REPLACES its member facts, and its refs
// are mostly theirs, carried over by the judge. Those refs were accepted when
// the members were written, so they are prior: the merge is not adding them.
// Passing prior = nil made every one of them "newly added", and since #249
// refuses a newly added legacy src ref, a merge touching any of the facts that
// carry one was warn-and-skipped — silently undoing a consolidation the judge
// asked for.

func seedMergeMembers(t *testing.T) (*store.Service, string) {
	t.Helper()
	const branch = "agent/test"
	svc, err := store.Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	members := map[string]string{
		"kb/technology/a.md": "src://knomit/internal/a.go@ca1c272",
		"kb/technology/b.md": "src://knomit/internal/b.go",
	}
	for p, ref := range members {
		_, err := svc.Facts().WriteFact(context.Background(), branch, p,
			"---\ntype: observation\nrefs: ['"+ref+"']\n---\n# Src\n\nsource body", "seed "+p, "test")
		require.NoError(t, err)
	}
	return svc, branch
}

func runMerge(t *testing.T, svc *store.Service, branch, title string, refs []string) []string {
	t.Helper()
	var warns []string
	merges := []MergeEntry{{
		Paths: []string{"kb/technology/a.md", "kb/technology/b.md"},
		Merged: mergedFact{
			Path:  "kb/technology/merged.md",
			Title: title,
			Body:  "merged body",
			Type:  "observation",
			Refs:  refs,
		},
	}}
	_, err := ApplyPruneDecisions(context.Background(), svc.Facts(), svc.Search(), nil, merges,
		"review-test", func(e ProgressEvent) {
			if e.Phase == "warn" {
				warns = append(warns, e.Message)
			}
		}, branch, bareRefFixture, "kb")
	require.NoError(t, err)
	return warns
}

func TestApplyPruneDecisions_MergeCarryingMembersLegacySrcRefsIsWritten(t *testing.T) {
	svc, branch := seedMergeMembers(t)
	warns := runMerge(t, svc, branch, "Merged", []string{
		"src://knomit/internal/a.go@ca1c272",
		"src://knomit/internal/b.go",
	})
	require.Empty(t, warns, "the members' own refs are carried, not added")

	got, err := svc.Facts().ReadFact(context.Background(), branch,
		mergedFactPath(t, svc, branch, "Merged"), nil)
	require.NoError(t, err)
	require.Contains(t, got.Content, "internal/a.go@ca1c272")
	require.Contains(t, got.Content, "internal/b.go")
}

func TestApplyPruneDecisions_MergeInventingALegacySrcRefIsRejected(t *testing.T) {
	svc, branch := seedMergeMembers(t)
	invented := "src://knomit/internal/invented.go@ca1c272"
	warns := runMerge(t, svc, branch, "Merged", []string{
		"src://knomit/internal/a.go@ca1c272",
		invented,
	})
	require.Len(t, warns, 1, "a src ref no member carried is the judge's own, and is judged")
	require.Contains(t, warns[0], invented)
	require.False(t, strings.Contains(warns[0], "internal/a.go"), "the carried ref is not a problem")
}
