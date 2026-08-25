package synthesize

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
)

// knomit#107a. prune's merge was the ONLY one of validateOutputPath's four
// callers that never ran normalizeFactPath — distill does it at decision.go's
// distill loop, discovery does it in discovery.go. validateOutputPath checks
// the ontology root and private paths, and nothing else: there is no collision
// check anywhere on that path.
//
// So an LLM-supplied merged.path naming a fact that already exists overwrote it
// WHOLE — body, refs, motifs, origin — with no warning and no trace. The
// corpus's own convention note recorded the hazard rather than defending it:
// "Merge path-uniqueness therefore depends on the LLM emitting a distinct path
// and on downstream dedup — NOT on UUID replacement. Do not assume a
// prune-merge output path is collision-proof by UUID."
//
// Designer ruling: NORMALIZE, matching the three sibling paths, with UUID
// filenames on merged facts accepted as the visible change.
func TestApplyPruneDecisions_MergeDoesNotOverwriteAnExistingFact(t *testing.T) {
	ctx := context.Background()
	const branch = "agent/test"
	const victim = "kb/technology/victim.md"

	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	// A live fact the merge is about to be aimed at. Its body is the tell: if
	// the merge overwrites it, this string is gone.
	const victimBody = "PRECIOUS-ORIGINAL-CONTENT"
	_, err = svc.Facts().WriteFact(ctx, branch, victim,
		"---\ntype: observation\n---\n# Victim\n\n"+victimBody, "seed victim", "test")
	require.NoError(t, err)

	// The two facts the merge legitimately subsumes.
	for _, p := range []string{"kb/technology/a.md", "kb/technology/b.md"} {
		_, err = svc.Facts().WriteFact(ctx, branch, p,
			"---\ntype: observation\n---\n# Src\n\nsource body", "seed "+p, "test")
		require.NoError(t, err)
	}

	// The LLM names the victim's path as its merge output. Nothing in the
	// prompt stops it, and nothing downstream checked.
	merges := []MergeEntry{{
		Paths: []string{"kb/technology/a.md", "kb/technology/b.md"},
		Merged: mergedFact{
			Path:  victim,
			Title: "Merged",
			Body:  "merged body",
			Type:  "observation",
		},
	}}

	_, err = ApplyPruneDecisions(ctx, svc.Facts(), svc.Search(), nil, merges,
		"review-test", func(ProgressEvent) {}, branch, bareRefFixture, "kb")
	require.NoError(t, err)

	// THE ASSERTION. The victim must still be there, unchanged.
	got, err := svc.Facts().ReadFact(ctx, branch, victim, nil)
	require.NoError(t, err, "the pre-existing fact must still exist")
	require.Contains(t, got.Content, victimBody,
		"the merge overwrote a live fact whole — body, refs, motifs and origin "+
			"replaced by an LLM-chosen path collision")
}

// mergedFactPath finds the fact a prune-merge wrote, by title.
//
// Tests used to look the merged fact up at the literal path the fixture put in
// merged.Path. Since knomit#107a that path's FILENAME is replaced by a UUID —
// the same normalization distill and discovery have always applied — so a
// literal lookup finds nothing. The title is the stable handle, and using it
// keeps those tests asserting what they were written to assert (sources
// pooling, hypothesis exclusion, unreadable-source warnings) rather than
// accidentally re-asserting a filename.
// It FAILS on a miss rather than returning "" (PR #131, LOW-5). Call sites in
// sources_pooling_test.go feed the result straight into readSources, so an
// empty path surfaced as a confusing ReadFact error about a path that is
// obviously wrong, instead of the true diagnosis: the merge wrote nothing.
// A helper that cannot find what it was asked for should say which.
func mergedFactPath(t *testing.T, svc *store.Service, branch, title string) string {
	t.Helper()
	facts, err := svc.Search().Search(context.Background(), branch, store.SearchOptions{Limit: 1000})
	require.NoError(t, err)
	for _, f := range facts {
		if f.Title == title {
			return f.Path
		}
	}
	require.FailNow(t, "no merged fact found",
		"no live fact titled %q on %s — the merge wrote nothing, which is a "+
			"different failure from the path being wrong", title, branch)
	return ""
}

// The merge must still WRITE something: normalizing must not be mistaken for
// rejecting. A fix that made the collision impossible by dropping the merge
// would silently undo a consolidation the judge asked for — which is the
// argument decision.go's DropInvalidMotifs comment makes twenty lines away,
// and the reason #107b (refuse-if-lossy) is a separate, undecided question.
func TestApplyPruneDecisions_MergeStillWritesUnderANormalizedPath(t *testing.T) {
	ctx := context.Background()
	const branch = "agent/test"

	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, branch))

	for _, p := range []string{"kb/technology/a.md", "kb/technology/b.md"} {
		_, err = svc.Facts().WriteFact(ctx, branch, p,
			"---\ntype: observation\n---\n# Src\n\nsource body", "seed "+p, "test")
		require.NoError(t, err)
	}

	merges := []MergeEntry{{
		Paths: []string{"kb/technology/a.md", "kb/technology/b.md"},
		Merged: mergedFact{
			Path:  "kb/technology/human-chosen-name.md",
			Title: "Merged",
			Body:  "MERGED-BODY-MARKER",
			Type:  "observation",
		},
	}}

	stats, err := ApplyPruneDecisions(ctx, svc.Facts(), svc.Search(), nil, merges,
		"review-test", func(ProgressEvent) {}, branch, bareRefFixture, "kb")
	require.NoError(t, err)
	require.Equal(t, 1, stats.Merged, "the merge must still commit")

	// It landed under a UUID filename in the directory the LLM chose — the
	// same shape distill has always produced.
	facts, err := svc.Search().Search(ctx, branch, store.SearchOptions{Limit: 100})
	require.NoError(t, err)

	var mergedPath string
	for _, f := range facts {
		if strings.Contains(f.Body, "MERGED-BODY-MARKER") {
			mergedPath = f.Path
		}
	}
	require.NotEmpty(t, mergedPath, "the merged fact must be on the branch")
	require.True(t, strings.HasPrefix(mergedPath, "kb/technology/"),
		"the LLM's DIRECTORY choice is kept; only the filename is replaced")
	require.NotContains(t, mergedPath, "human-chosen-name",
		"the LLM-emitted filename is replaced by a UUID, as on the distill path")
}
