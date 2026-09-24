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

// #249 review. The decision loop runs BEFORE the merge loop, and a retract
// decision deletes its path on the agent branch. A member retracted earlier in
// the same call is gone by the time its merge runs, so reading member refs at
// the tip lost that member's refs: the judge-kept legacy ref read as newly
// added and the merge was warn-and-skipped, the very symptom #249's prior was
// meant to remove. Member refs are snapshotted before anything is deleted.
func TestApplyPruneDecisions_MergeKeepsRefsOfAMemberRetractedEarlierInTheCall(t *testing.T) {
	svc, branch := seedMergeMembers(t)
	var warns []string
	decisions := []PruneDecision{{Path: "kb/technology/a.md", Action: "retract"}}
	merges := []MergeEntry{{
		Paths: []string{"kb/technology/a.md", "kb/technology/b.md"},
		Merged: mergedFact{
			Path: "kb/technology/merged.md", Title: "Merged", Body: "merged body", Type: "observation",
			Refs: []string{"src://knomit/internal/a.go@ca1c272", "src://knomit/internal/b.go"},
		},
	}}
	_, err := ApplyPruneDecisions(context.Background(), svc.Facts(), svc.Search(), decisions, merges,
		"review-test", func(e ProgressEvent) {
			if e.Phase == "warn" && strings.Contains(e.Message, "rejected") {
				warns = append(warns, e.Message)
			}
		}, branch, bareRefFixture, "kb")
	require.NoError(t, err)
	require.Empty(t, warns, "a member's refs are carried even when the member was retracted first")
	mergedFactPath(t, svc, branch, "Merged")
}

// The same blind spot through an earlier MERGE: two merges in one call share a
// member, the first deletes it, and the second must still see its refs.
func TestApplyPruneDecisions_SecondMergeKeepsRefsOfAMemberTheFirstConsumed(t *testing.T) {
	svc, branch := seedMergeMembers(t)
	var warns []string
	merges := []MergeEntry{
		{
			Paths: []string{"kb/technology/a.md", "kb/technology/b.md"},
			Merged: mergedFact{Path: "kb/technology/m1.md", Title: "First", Body: "first", Type: "observation",
				Refs: []string{"src://knomit/internal/b.go"}},
		},
		{
			Paths: []string{"kb/technology/a.md"},
			Merged: mergedFact{Path: "kb/technology/m2.md", Title: "Second", Body: "second", Type: "observation",
				Refs: []string{"src://knomit/internal/a.go@ca1c272"}},
		},
	}
	_, err := ApplyPruneDecisions(context.Background(), svc.Facts(), svc.Search(), nil, merges,
		"review-test", func(e ProgressEvent) {
			if e.Phase == "warn" && strings.Contains(e.Message, "rejected") {
				warns = append(warns, e.Message)
			}
		}, branch, bareRefFixture, "kb")
	require.NoError(t, err)
	require.Empty(t, warns, "a member consumed by an earlier merge in the call still carries its refs")
}
