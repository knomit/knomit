package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// TestVerify_NoGeneratedRefs guards the Critical that motivated removing the
// server-side export: generated refs under refs/heads/* were enumerated as
// real branches, so verify reported one ERROR per bundle file (~1000 on a real
// corpus). Nothing may reintroduce a generated branch ref.
func TestVerify_NoGeneratedRefs(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, "main", "kb/decisions/x/aaaaaaaa.md",
		testFactBody("A", 0.9, nil), "seed", "learn")
	require.NoError(t, err)

	report, err := svc.Verify(ctx, VerifyOpts{})
	require.NoError(t, err)
	require.Empty(t, report.Issues, "a healthy repo must verify clean")
	require.Equal(t, []string{"main"}, report.Branches, "no generated refs may appear as branches")

	// And no okf/* ref exists at all — not merely absent from the branch list.
	iter, err := svc.rh.gits.IterReferences()
	require.NoError(t, err)
	require.NoError(t, iter.ForEach(func(ref *plumbing.Reference) error {
		require.NotContains(t, ref.Name().String(), "okf/",
			"a generated okf ref survived: %s", ref.Name())
		return nil
	}))
}
