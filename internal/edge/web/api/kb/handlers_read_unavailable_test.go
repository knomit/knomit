package kb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store/index"
)

// Each read helper returns WithRead's refusal itself, not a zero-valued
// success. The HTTP tables above reach most of them; this pins every one,
// including those a route reaches only after another helper has already
// failed (motifClusterKey, motifDefinitions, motifAliasRows,
// motifVocabularyHealth, loadHighlights, topicFactByPath, incomingAtCommit,
// outgoingAtCommit, factExistsAt on the fact view's ref resolver).
func TestReadHelpers_ReturnAcquireRefusal(t *testing.T) {
	ri := newTestManagerWithRepos(t, "alpha").Get("alpha")
	ctx := context.Background()
	const branch, path, sha = "agent/test", "kb/ai/test.md", "0123456789abcdef0123456789abcdef01234567"

	errs := map[string]error{}
	_, errs["loadActivity"] = loadActivity(ctx, ri, branch, "")
	_, errs["readCompletions"] = readCompletions(ctx, ri, branch, "domain", "", 20)
	_, errs["domainStats"] = domainStats(ctx, ri, branch)
	_, errs["domainFacts"] = domainFacts(ctx, ri, branch, "ai")
	_, errs["loadStats"] = loadStats(ctx, ri, branch, "", "")
	_, _, errs["loadHighlights"] = loadHighlights(ctx, ri, branch, "", "")
	_, errs["motifClusters"] = motifClusters(ctx, ri, branch, "")
	_, errs["motifDefinitions"] = motifDefinitions(ctx, ri, branch, []string{"k"})
	_, errs["motifVocabularyHealth"] = motifVocabularyHealth(ctx, ri, branch, "")
	_, errs["motifAliasRows"] = motifAliasRows(ctx, ri, branch)
	_, errs["motifClusterKey"] = motifClusterKey(ctx, ri, branch, "k")
	_, _, errs["readFact"] = readFact(ctx, ri, hal.Anchor{Branch: branch}, path, false)
	_, errs["explainFact"] = explainFact(ctx, ri, branch, path)
	_, errs["incomingAtCommit"] = incomingAtCommit(ctx, ri, branch, path, sha)
	_, errs["outgoingAtCommit"] = outgoingAtCommit(ctx, ri, branch, path, sha)
	_, errs["factLiveAtCommit"] = factLiveAtCommit(ctx, ri, branch, path, sha)
	_, errs["factExistsAt"] = factExistsAt(ctx, ri, branch, path, sha)
	_, _, errs["recentFacts"] = recentFacts(ctx, ri, branch, index.SearchOptions{})
	_, _, _, errs["logPaginated"] = logPaginated(ctx, ri, branch, "", 10, "", "", "")
	_, _, errs["commitDetail"] = commitDetail(ctx, ri, branch, sha, "kb")
	_, errs["searchRepo"] = searchRepo(ctx, ri, nil, branch, index.SearchOptions{})
	_, errs["listTopicDir"] = listTopicDir(ctx, ri, branch, "kb")
	_, errs["topicFactByPath"] = topicFactByPath(ctx, ri, branch, path)

	for name, err := range errs {
		require.ErrorIs(t, err, repos.ErrStoreUnavailable, name)
	}
	require.NotErrorIs(t, errs["readFact"], errFactNotFound, "readFact: nothing was looked up, so not a 404")
	require.Len(t, errs, 23, "every helper is in the table")
}
