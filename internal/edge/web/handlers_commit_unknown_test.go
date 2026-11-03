package web

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/engine/store/storetest"
)

// A commit the repo does not hold is a CLIENT error: 404 with the problem's
// `code` set to commit_not_in_repo, never a 500 (user ruling 2026-11-03).
// The realistic case is the lens one: the UI keeps the asOf commit of mount
// alpha and reads a fact on mount beta at it. So the commit used here is a
// REAL commit of alpha, written on purpose, that beta has never seen — and
// the fact path exists on beta, so the 404 can only come from the commit.

func requireCommitNotInRepo(t *testing.T, s *Server, url string) {
	t.Helper()
	rec := getAPI(t, s, url)
	require.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", url, rec.Body.String())
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"), url)
	var p struct {
		Type   string `json:"type"`
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
		Code   string `json:"code"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p), rec.Body.String())
	require.Equal(t, "commit_not_in_repo", p.Code, "%s: %s", url, rec.Body.String())
	require.Equal(t, "Commit not found", p.Title, url)
	require.Equal(t, http.StatusNotFound, p.Status, url)
}

func TestCommitAnchoredRoutes_UnknownCommitIs404CommitNotInRepo(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha", "beta")
	s := &Server{Manager: m, OntologyRoot: "kb"}

	write := func(repo, path string) string {
		t.Helper()
		res, err := svcs[repo].Facts().WriteFact(t.Context(), "agent/test", path,
			commitsFactText("T "+repo), "add "+path, "learn")
		require.NoError(t, err)
		return res.CommitHash
	}
	alphaSHA := write("alpha", "kb/x/f.md")
	betaSHA := write("beta", "kb/x/f.md")
	require.NotEqual(t, alphaSHA, betaSHA)

	beta := "/repos/beta/branches/agent:test"
	for _, url := range []string{
		beta + "/commits/" + alphaSHA,
		beta + "/commits/" + alphaSHA + "/facts/kb/x/f.md",
		beta + "/commits/" + alphaSHA + "/facts/kb/x/f.md?fallback=before",
		beta + "/commits/" + alphaSHA + "/facts/kb/x/f.md/incoming",
		beta + "/commits/" + alphaSHA + "/facts/kb/x/f.md/incoming?fallback=before",
		beta + "/commits/" + alphaSHA + "/facts/kb/x/f.md/outgoing",
		beta + "/commits/" + alphaSHA + "/facts/kb/x/f.md/materialize",
		beta + "/commits/" + alphaSHA + "/facts/kb/x/f.md/materialize/pages",
		beta + "/commits/" + alphaSHA + "/facts/.knomit/ontology.yaml",
		beta + "/commits/" + alphaSHA + "/topics",
		beta + "/commits/" + alphaSHA + "/topics/x",
		// A value that is not a hash at all is no commit of this repo either.
		beta + "/commits/not-a-sha/facts/kb/x/f.md",
		// The commit-list cursors name a commit too.
		beta + "/commits?before=" + alphaSHA,
		beta + "/commits?from=" + alphaSHA,
		beta + "/commits?after=" + alphaSHA,
		beta + "/facts/kb/x/f.md/commits?before=" + alphaSHA,
		beta + "/facts/kb/x/f.md/commits?from=" + alphaSHA,
		beta + "/facts/kb/x/f.md/commits?after=" + alphaSHA,
	} {
		requireCommitNotInRepo(t, s, url)
	}

	// The same reads at beta's OWN commit succeed: the 404 above is the
	// commit's, not the route's or the path's.
	for _, url := range []string{
		beta + "/commits/" + betaSHA,
		beta + "/commits/" + betaSHA + "/facts/kb/x/f.md",
		beta + "/commits/" + betaSHA + "/facts/kb/x/f.md/incoming",
		beta + "/commits?from=" + betaSHA,
		beta + "/facts/kb/x/f.md/commits?from=" + betaSHA,
	} {
		rec := getAPI(t, s, url)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", url, rec.Body.String())
	}
	// A known commit on the not-implemented topics route stays 501.
	rec := getAPI(t, s, beta+"/commits/"+betaSHA+"/topics")
	require.Equal(t, http.StatusNotImplemented, rec.Code, rec.Body.String())

	// A commit beta DOES hold that changed no file has zero commit_log rows
	// (rows are one per changed path). The write path never makes one (an
	// identical rewrite returns the head), but a merge whose side is already
	// in the first parent's tree, or an empty commit arriving from a remote,
	// does. A cursor naming it is NOT commit_not_in_repo: it is 400
	// commit_not_in_log, and the commit-anchored read at it still works.
	noop, err := storetest.CommitFiles(svcs["beta"], "agent/test",
		map[string]string{"kb/x/f.md": commitsFactText("T beta")}, "empty: tree unchanged")
	require.NoError(t, err)
	require.NotEqual(t, betaSHA, noop)
	// A later ordinary write records the branch's new commits in commit_log;
	// the empty one gets no row.
	write("beta", "kb/x/g.md")
	for _, url := range []string{
		beta + "/commits?from=" + noop,
		beta + "/facts/kb/x/f.md/commits?before=" + noop,
	} {
		rec := getAPI(t, s, url)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", url, rec.Body.String())
		require.Contains(t, rec.Body.String(), `"code":"commit_not_in_log"`, url)
	}
	rec = getAPI(t, s, beta+"/commits/"+noop+"/facts/kb/x/f.md")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// A missing FACT at a known commit keeps its own 404, without the code:
	// the UI must be able to tell the two apart.
	rec = getAPI(t, s, beta+"/commits/"+betaSHA+"/facts/kb/x/missing.md")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	var p map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, "Fact not found", p["title"])
	require.NotContains(t, p, "code")
}
