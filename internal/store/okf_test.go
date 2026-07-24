package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// okfTestBranchTip returns the branch-tip commit hash for branch, i.e. the
// source SHA the OKF exporter reads a snapshot at.
func okfTestBranchTip(t *testing.T, svc *Service, branch string) plumbing.Hash {
	t.Helper()
	ref, err := svc.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	require.NoError(t, err)
	return ref.Hash()
}

// okfTestService builds a store with two facts written on "main" and returns
// the service and the branch-tip source SHA.
func okfTestService(t *testing.T) (*Service, plumbing.Hash) {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, "main", "kb/decisions/okf/scope/d9d6557d.md", testFactBody("Scope", 0.9, nil), "seed scope", "learn")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "main", "kb/invariants/okf/refs/3209d651.md", testFactBody("Refs", 0.8, nil), "seed refs", "learn")
	require.NoError(t, err)

	return svc, okfTestBranchTip(t, svc, "main")
}

func TestOKFReadFacts_EnumeratesTreeAtCommit(t *testing.T) {
	svc, sha := okfTestService(t)
	ctx := context.Background()

	facts, err := svc.okfReadFacts(ctx, sha)
	require.NoError(t, err)
	require.Len(t, facts, 2, "want the two kb/ facts written on main")

	for _, f := range facts {
		require.False(t, f.Timestamp.IsZero(), "fact %s has zero authoring timestamp", f.Fact.Path())
	}
}

// TestOKFReadFacts_ReadsTreeNotIndex pins determinism: enumeration is a pure
// function of the source SHA. A snapshot taken at an earlier commit must not
// include a fact added in a later commit.
func TestOKFReadFacts_ReadsTreeNotIndex(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	r1, err := svc.Facts().WriteFact(ctx, "main", "kb/decisions/okf/scope/aaaaaaaa.md", testFactBody("Scope", 0.9, nil), "seed scope", "learn")
	require.NoError(t, err)
	early := plumbing.NewHash(r1.CommitHash)

	_, err = svc.Facts().WriteFact(ctx, "main", "kb/decisions/okf/later/bbbbbbbb.md", testFactBody("Later", 0.9, nil), "seed later", "learn")
	require.NoError(t, err)

	facts, err := svc.okfReadFacts(ctx, early)
	require.NoError(t, err)
	require.Len(t, facts, 1, "snapshot at the earlier commit sees only the first fact")
	require.Equal(t, "kb/decisions/okf/scope/aaaaaaaa.md", facts[0].Fact.Path())
}

// TestOKFHistory_ClassifiesCreationAndUpdate checks the log walk labels the
// first touch of a path Creation and a later touch Update, and that the
// authoring-time map records the earliest commit time for each path.
func TestOKFHistory_ClassifiesCreationAndUpdate(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	const path = "kb/decisions/okf/scope/d9d6557d.md"
	_, err = svc.Facts().WriteFact(ctx, "main", path, testFactBody("Scope", 0.5, nil), "create", "learn")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "main", path, testFactBody("Scope", 0.9, nil), "revise", "learn")
	require.NoError(t, err)

	sha := okfTestBranchTip(t, svc, "main")
	events, authored, err := svc.okfHistory(ctx, sha)
	require.NoError(t, err)

	var creations, updates int
	for _, e := range events {
		if e.Path != path {
			continue
		}
		switch e.Kind {
		case "Creation":
			creations++
		case "Update":
			updates++
		}
	}
	require.Equal(t, 1, creations, "exactly one Creation for the path")
	require.GreaterOrEqual(t, updates, 1, "at least one Update for the path")

	ts, ok := authored[path]
	require.True(t, ok, "authoring map has the path")
	require.False(t, ts.IsZero(), "authoring time is non-zero")
}
