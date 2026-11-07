package kb

import (
	"context"
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

// An 8-hex handle that is a fact's short id is the fact, not a commit.
func TestFactsResolve_AFactIdIsNotACommit(t *testing.T) {
	m := collisionManager(t)
	got := postResolvePrefer(t, m, nil, []string{"alpha"}, "eeeeeeee")
	require.Equal(t, map[string]string{"eeeeeeee": "alpha:Only alpha"}, titles(got))
	require.Empty(t, commitsOf(got))
}
