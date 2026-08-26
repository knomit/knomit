package synthesize

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// A merge used to be a write followed by N separate deletes. An interruption
// between them left the corpus holding BOTH the merged fact and the originals
// it replaced — a duplicate set manufactured by the mechanism that exists to
// remove duplicates, and one no later session can recognise as half-done.
//
// The designer's ruling was to make that state impossible by construction
// rather than to detect and refuse it, so the assertion is about the COMMIT
// COUNT: one merge, one commit. Counting is what discriminates — every
// end-state assertion (merged fact present, sources gone) is identical whether
// the work took one commit or four.

// branchCommits counts commits on the branch.
func branchCommits(t *testing.T, env *restatementEnv) int {
	t.Helper()
	entries, err := env.svc.Search().Log(context.Background(), env.branch, "")
	require.NoError(t, err)
	return len(entries)
}

// mergedTitle is how the merged fact is FOUND. Its path is not predictable:
// normalizeFactPath (#107a) replaces the filename with a fresh uuid, which is
// exactly what makes the merged fact unable to collide with a source. Looking
// it up by the literal fixture path is the mistake five existing tests already
// had to be corrected for.
const mergedTitle = "Merged claim"

// mergeTwoInto runs ApplyPruneDecisions with a single merge subsuming the two
// given paths, and returns the merged fact's ACTUAL path.
func mergeTwoInto(t *testing.T, env *restatementEnv, a, b string) (*ReviewStats, string) {
	t.Helper()
	out := "kb/alpha/merged-result.md"
	var mf mergedFact
	require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf(`{
		"path":%q,"title":"Merged claim","body":"Both recordings, unioned.",
		"type":"observation","domain":["alpha"],"confidence":0.9,"sources":2,
		"entities":["Widget"],"refs":[]}`, out)), &mf))

	d := env.deps()
	stats, err := ApplyPruneDecisions(context.Background(), env.svc.Facts(), env.svc.Search(),
		nil, []MergeEntry{{Paths: []string{a, b}, Merged: mf}},
		reviewTool, d.OnProgress, env.branch, "", "kb")
	require.NoError(t, err)
	return stats, mergedFactPath(t, env.svc, env.branch, mergedTitle)
}

func TestApplyPruneMerge_WriteAndSourceDeletesShareOneCommit(t *testing.T) {
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/alpha/one.md", "One", "first recording")
	env.writeFact("kb/alpha/two.md", "Two", "second recording")

	before := branchCommits(t, env)
	stats, _ := mergeTwoInto(t, env, "kb/alpha/one.md", "kb/alpha/two.md")
	after := branchCommits(t, env)

	require.Equal(t, 1, stats.Merged, "fixture: the merge must actually have applied")
	require.Len(t, stats.Retired, 2, "fixture: both sources must have been retired")

	require.Equal(t, 1, after-before,
		"a merge must land as ONE commit — a write plus %d separate deletes leaves the "+
			"corpus holding the merged fact AND its originals if anything interrupts it",
		len(stats.Retired))
}

// TestApplyPruneMerge_AlreadyMissingSourceDoesNotAbortTheMerge — batchWrite
// REFUSES a delete of a missing path, so an absent source would take down a
// merge that is otherwise complete. That is a new failure mode created by
// batching, and it is the reason the delete list is filtered rather than
// passed through.
func TestApplyPruneMerge_AlreadyMissingSourceDoesNotAbortTheMerge(t *testing.T) {
	ctx := context.Background()
	env := newRestatementEnv(t, 0)
	env.writeFact("kb/alpha/one.md", "One", "first recording")
	env.writeFact("kb/alpha/two.md", "Two", "second recording")

	// A source that is already gone by the time the merge runs.
	_, err := env.svc.Facts().DeleteFact(ctx, env.branch, "kb/alpha/two.md", "gone already")
	require.NoError(t, err)
	exists, err := env.svc.Facts().FactExists(ctx, env.branch, "kb/alpha/two.md")
	require.NoError(t, err)
	require.False(t, exists, "fixture: the source must really be absent")

	stats, out := mergeTwoInto(t, env, "kb/alpha/one.md", "kb/alpha/two.md")

	require.Equal(t, 1, stats.Merged,
		"an already-absent source is not an error — the merge must still land")
	merged, err := env.svc.Facts().FactExists(ctx, env.branch, out)
	require.NoError(t, err)
	require.True(t, merged, "the merged fact must have been written")
	require.Equal(t, []string{"kb/alpha/one.md"}, stats.Retired,
		"only the source this call actually removed is reported as retired")
}
