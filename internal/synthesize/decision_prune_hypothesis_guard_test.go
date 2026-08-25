package synthesize

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// TestApplyPruneDecisions_RejectsHypothesisMerge regresses the
// architectural invariant: review never creates hypothesis-type facts.
// The distill path has had this server-side guard since the original PR
// (decision.go: "distill cannot create hypothesis-type facts"). The
// merge path inside ApplyPruneDecisions previously relied only on the
// prompt's enum to forbid hypothesis-typed merges — if the LLM ignored
// the prompt and emitted merged.type="hypothesis", the server would
// happily write it. This test pins the new server-side guard.
func TestApplyPruneDecisions_RejectsHypothesisMerge(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	// A merge whose merged.type is "hypothesis" must be rejected with a
	// warn — the merged fact is NOT written and source facts are NOT
	// deleted (because the merge never committed).
	merges := []MergeEntry{
		{
			Paths: []string{"kb/synth/a.md", "kb/synth/b.md"},
			Merged: mergedFact{
				Path:  "kb/synth/merged.md",
				Title: "Merged hypothesis",
				Body:  "claim",
				Type:  "hypothesis",
			},
		},
	}

	var warns []string
	onProgress := func(e ProgressEvent) {
		if e.Phase == "warn" {
			warns = append(warns, e.Message)
		}
	}

	stats, err := ApplyPruneDecisions(
		context.Background(),
		svc.Facts(),
		svc.Search(),
		nil,    // no per-fact decisions
		merges, // the hypothesis-type merge
		"review-test",
		onProgress,
		"agent/test",
		bareRefFixture,
		"kb",
	)
	require.NoError(t, err, "ApplyPruneDecisions itself must not error on a rejected merge")
	require.Equal(t, 0, stats.Merged, "no merge committed when type=hypothesis")

	// One warn should mention the rejection reason.
	var found bool
	for _, w := range warns {
		if strings.Contains(w, "hypothesis") && strings.Contains(w, "kb/synth/merged.md") {
			found = true
			break
		}
	}
	require.True(t, found, "expected warn mentioning hypothesis rejection; got warns: %v", warns)

	// The merged fact must NOT exist on the branch.
	got, err := svc.Search().GetByPath(context.Background(), "agent/test", "kb/synth/merged.md")
	require.NoError(t, err)
	require.Nil(t, got, "rejected merge must not have written a fact")
}

// TestApplyPruneDecisions_AcceptsSynthesisMerge is the positive control:
// a merge with merged.type="synthesis" still works as before. Without
// this, the new guard could silently break legitimate merges and we
// wouldn't notice.
func TestApplyPruneDecisions_AcceptsSynthesisMerge(t *testing.T) {
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))

	merges := []MergeEntry{
		{
			Paths: []string{}, // no source facts to delete (keeps the test minimal)
			Merged: mergedFact{
				Path:  "kb/synth/merged.md",
				Title: "Merged synthesis",
				Body:  "summary",
				Type:  "synthesis",
			},
		},
	}

	var warns []string
	onProgress := func(e ProgressEvent) {
		if e.Phase == "warn" {
			warns = append(warns, e.Message)
		}
	}

	stats, err := ApplyPruneDecisions(
		context.Background(),
		svc.Facts(),
		svc.Search(),
		nil, merges,
		"review-test",
		onProgress,
		"agent/test",
		bareRefFixture,
		"kb",
	)
	require.NoError(t, err)
	require.Equal(t, 1, stats.Merged, "synthesis-type merge must commit")
	require.Empty(t, warns, "no warns expected for valid synthesis merge; got: %v", warns)

	// The filename is UUID-normalized since #107a, so the merged fact is found
	// by title rather than by the path the fixture supplied.
	mergedPath := mergedFactPath(t, svc, "agent/test", "Merged synthesis")
	require.NotEmpty(t, mergedPath, "valid merge must have written a fact")
	got, err := svc.Search().GetByPath(context.Background(), "agent/test", mergedPath)
	require.NoError(t, err)
	require.NotNil(t, got, "valid merge must have written a fact")
	require.Equal(t, "Merged synthesis", got.Title)
}
