package store

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// TestSignatureReport: the dry run folds the upstream from the root without
// moving or writing anything, and tallies every signer with its claims, first
// commit, count, and whether it is listed or the operator. Accept-list matches
// are shown.
func TestSignatureReport(t *testing.T) {
	g := newGateFixture(t)
	files := with(g.baseFiles, g.ontPath, g.ont(VerifyLog, g.a))
	e := g.commit(files, g.op, g.init)
	a1 := g.commit(with(files, "kb/1.md", "1"), g.a, e)
	a2 := g.commit(with(files, "kb/1.md", "1", "kb/2.md", "2"), g.a, a1)
	uns := g.commit(with(files, "kb/1.md", "1", "kb/2.md", "2", "kb/3.md", "3"), nil, a2)
	str := g.commit(with(files, "kb/1.md", "1", "kb/2.md", "2", "kb/3.md", "3", "kb/4.md", "4"), g.stranger, uns)
	g.setOrigin(str)
	g.svc.SetAcceptList(testAccepts{uns.Hash})

	mainBefore := mustHeadHash(t, g.svc, "main")
	rep, err := g.svc.SignatureReport(context.Background(), "main", plumbing.ZeroHash)
	require.NoError(t, err)
	require.Equal(t, mainBefore, mustHeadHash(t, g.svc, "main"), "a dry run moves nothing")
	require.Equal(t, plumbing.ZeroHash, g.anchor(), "a dry run writes no anchor")

	require.Equal(t, VerifyLog, rep.Mode)
	require.Equal(t, uns.Hash.String(), rep.WouldAnchor, "the unsigned commit is waived by the accept list; the stranger is refused")
	require.Equal(t, str.Hash.String(), rep.Tip)
	require.GreaterOrEqual(t, rep.Unsigned, 1)
	require.Len(t, rep.Signers, 3, "operator, a, stranger")

	byFP := map[string]SignerSeen{}
	for _, s := range rep.Signers {
		byFP[s.Fingerprint] = s
	}
	opFP, _ := keyFingerprint(g.op.PublicKey())
	aFP, _ := keyFingerprint(g.a.PublicKey())
	sFP, _ := keyFingerprint(g.stranger.PublicKey())
	require.True(t, byFP[opFP].Operator)
	require.True(t, byFP[aFP].Listed)
	require.Equal(t, 2, byFP[aFP].Count)
	require.Equal(t, a1.Hash.String(), byFP[aFP].FirstCommit)
	require.Equal(t, []string{aFP[:8]}, byFP[aFP].Claims)
	require.False(t, byFP[sFP].Listed)
	require.Equal(t, keyLine(g.stranger), byFP[sFP].Key, "the key line is ready to paste into verify_signers")

	require.Len(t, rep.Accepted, 1)
	require.Equal(t, uns.Hash.String(), rep.Accepted[0].Commit)

	// --from limits the tally, never the fold.
	rep2, err := g.svc.SignatureReport(context.Background(), "main", a2.Hash)
	require.NoError(t, err)
	require.Equal(t, 2, rep2.Commits, "only the commits after --from are tallied")
	require.Equal(t, rep.WouldAnchor, rep2.WouldAnchor, "the fold still runs from the root")
}
