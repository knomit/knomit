package kb

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/engine/store"
)

// headOf is the commit at the tip of ri's read branch.
func headOf(t *testing.T, ri *repos.RepoInstance) string {
	t.Helper()
	var h string
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		var err error
		h, err = svc.HeadCommit(context.Background(), ri.ReadBranch())
		require.NoError(t, err)
	}))
	require.Len(t, h, 40)
	return h
}

func commitsOf(rs []resolvedFact) map[string]string {
	out := map[string]string{}
	for _, r := range rs {
		if r.Commit != "" {
			out[r.Handle] = r.Repo + "@" + r.Commit
		}
	}
	return out
}

// A commit hash (7 to 40 hex) resolves in the ONE repo the turn worked in, to
// its full hash, when that repo holds the commit.
func TestFactsResolve_CommitInTheTurnsRepo(t *testing.T) {
	m := collisionManager(t)
	head := headOf(t, m.Get("beta"))
	got := postResolvePrefer(t, m, nil, []string{"beta"}, head[:7], head[:12], head)
	require.Equal(t, map[string]string{
		head[:7]: "beta@" + head, head[:12]: "beta@" + head, head: "beta@" + head,
	}, commitsOf(got))
	for _, r := range got {
		require.Equal(t, "agent/test", r.Branch)
		require.Empty(t, r.Path, "a commit item carries no fact")
	}
}

// With no single repo known (no preference, or a lens standing for two
// repos), a hash is not looked up: it stays code.
func TestFactsResolve_CommitWithoutASingleRepoStaysCode(t *testing.T) {
	m := collisionManager(t)
	head := headOf(t, m.Get("beta"))
	require.Empty(t, commitsOf(postResolvePrefer(t, m, nil, nil, head[:7], head)))
	require.Empty(t, commitsOf(postResolvePrefer(t, m, nil, []string{"alpha", "beta"}, head[:7], head)))

	lm, _ := newTestLensManager(t, "one", "two")
	_, err := lm.CreateLens(context.Background(), control.Lens{Name: "eng", WriteUID: lm.Get("one").UID(),
		Reads: []control.LensRead{{RepoUID: lm.Get("two").UID()}}})
	require.NoError(t, err)
	lh := headOf(t, lm.Get("one"))
	require.Empty(t, commitsOf(postResolvePrefer(t, lm, nil, []string{"eng"}, lh)))
	require.Equal(t, map[string]string{lh: "one@" + lh}, commitsOf(postResolvePrefer(t, lm, nil, []string{"one"}, lh)),
		"fixture: the same hash resolves when one repo is named")
}

// A hash the repo does not hold stays code, as does one in a repo the caller
// cannot read.
func TestFactsResolve_CommitNotInTheRepoStaysCode(t *testing.T) {
	m := collisionManager(t)
	alphaHead := headOf(t, m.Get("alpha"))
	require.Empty(t, commitsOf(postResolvePrefer(t, m, nil, []string{"beta"}, alphaHead, alphaHead[:9], "0000000")))
	onlyAlpha := func(r string) bool { return r == "alpha" }
	betaHead := headOf(t, m.Get("beta"))
	require.Empty(t, commitsOf(postResolvePrefer(t, m, onlyAlpha, []string{"beta"}, betaHead)))
}

// An 8-hex handle that is BOTH a fact's short id and a commit's prefix in the
// same repo is the fact.
func TestFactsResolve_AFactIdIsNotACommit(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha")
	writeResolveFact(t, svcs["alpha"], "kb/x/y/11111111.md", "observation", "First", "a", "0.7")
	h := headOf(t, m.Get("alpha"))
	writeResolveFact(t, svcs["alpha"], "kb/x/y/"+h[:8]+".md", "observation", "Named like a commit", "a", "0.7")
	require.Equal(t, map[string]string{h[:7]: "alpha@" + h}, commitsOf(postResolvePrefer(t, m, nil, []string{"alpha"}, h[:7])),
		"fixture: the prefix is a commit on the branch")

	got := postResolvePrefer(t, m, nil, []string{"alpha"}, h[:8])
	require.Equal(t, map[string]string{h[:8]: "alpha:Named like a commit"}, titles(got))
	require.Empty(t, commitsOf(got))
}

// An abbreviation two commits share is ambiguous, and a hash the index lists
// but whose commit object the repository does not hold is not linked: both
// stay code.
func TestFactsResolve_AmbiguousOrMissingCommitStaysCode(t *testing.T) {
	m := collisionManager(t)
	real := headOf(t, m.Get("beta"))
	orig := listCommits
	t.Cleanup(func() { listCommits = orig })

	listCommits = func(ctx context.Context, svc *store.Service, branch, prefix string) ([]string, error) {
		return []string{real, prefix + strings.Repeat("0", 40-len(prefix))}, nil
	}
	require.Empty(t, commitsOf(postResolvePrefer(t, m, nil, []string{"beta"}, real[:7])))

	missing := strings.Repeat("f", 40)
	listCommits = func(ctx context.Context, svc *store.Service, branch, prefix string) ([]string, error) {
		return []string{missing}, nil
	}
	require.Empty(t, commitsOf(postResolvePrefer(t, m, nil, []string{"beta"}, "fffffff")))

	listCommits = orig
	require.Equal(t, map[string]string{real[:7]: "beta@" + real}, commitsOf(postResolvePrefer(t, m, nil, []string{"beta"}, real[:7])),
		"fixture: with the real index the same prefix resolves")
}
