package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// anchorStore is a real Service whose agent branch carries an operator-signed
// enable (log, signers a) and one signed fact, with the operator as root.
func anchorStore(t *testing.T) (*Service, *foldFixture, plumbing.Hash) {
	t.Helper()
	f := newFoldFixture(t)
	svc, err := Open(filepath.Join(t.TempDir(), "v.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, testAgentBranch))
	svc.SetRootOfTrust(f.root)
	ctx := context.Background()
	svc.SetSigner(f.op)
	_, err = svc.Facts().WriteFact(ctx, testAgentBranch, ".knomit/ontology.yaml", f.ont(VerifyLog, f.a), "enable", "updated")
	require.NoError(t, err)
	svc.SetSigner(f.a)
	res, err := svc.Facts().WriteFact(ctx, testAgentBranch, "kb/notes/x.md", "---\ntype: observation\n---\n# x\n\nx\n", "learn: x", "created")
	require.NoError(t, err)
	return svc, f, plumbing.NewHash(res.CommitHash)
}

// TestVerifyAnchor_RoundTripAndRecompute: the anchor ref and its cached
// context round-trip; a lost cache row is recomputed by the root fold to the
// same context (never read from the anchor's file).
func TestVerifyAnchor_RoundTripAndRecompute(t *testing.T) {
	svc, f, tip := anchorStore(t)
	ctx := context.Background()
	rh := svc.rh

	start, actx, err := rh.verifyStart(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, plumbing.ZeroHash, start, "first contact starts at the root")
	require.Equal(t, VerifyOff, actx.Mode)

	res, err := rh.verifier(ctx).fold(start, actx, tip)
	require.NoError(t, err)
	require.Empty(t, res.Refused)
	require.Equal(t, tip, res.NewAnchor)
	require.Equal(t, VerifyLog, res.NewAnchorCtx.Mode)
	require.NoError(t, rh.saveVerify(ctx, "main", res))

	ref, err := rh.gits.Reference(verifiedRefName("main"))
	require.NoError(t, err)
	require.Equal(t, tip, ref.Hash())

	anchor, got, err := rh.verifyStart(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, tip, anchor)
	require.Equal(t, res.NewAnchorCtx, got)

	// Lose the cache row: recomputed, identical.
	_, err = rh.db.Exec(`DELETE FROM verify_context`)
	require.NoError(t, err)
	_, again, err := rh.verifyStart(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, res.NewAnchorCtx, again, "a recomputed context must equal the cached one")
	require.Equal(t, []string{keyLine(f.a)}, again.Signers)
}

// TestVerifyAnchor_OffNeverWritesTheRef: an off repo records an off-scan
// watermark in meta and never the anchor ref; a later verified disable
// retires an existing anchor.
func TestVerifyAnchor_OffNeverWritesTheRef(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "o.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, testAgentBranch))
	ctx := context.Background()
	svc.SetSigner(namedSigner(t, "x"))
	w, err := svc.Facts().WriteFact(ctx, testAgentBranch, "kb/notes/x.md", "---\ntype: observation\n---\n# x\n\nx\n", "learn: x", "created")
	require.NoError(t, err)
	tip := plumbing.NewHash(w.CommitHash)

	res, err := svc.rh.verifier(ctx).fold(plumbing.ZeroHash, verifyContext{Mode: VerifyOff}, tip)
	require.NoError(t, err)
	require.Zero(t, res.SigChecks, "an off repo performs no signature or M3 evaluation")
	require.NoError(t, svc.rh.saveVerify(ctx, "main", res))

	_, err = svc.rh.gits.Reference(verifiedRefName("main"))
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "off never writes the anchor ref")
	start, actx, err := svc.rh.verifyStart(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, tip, start, "the next fold resumes from the off-scan watermark")
	require.Equal(t, VerifyOff, actx.Mode)

	// Retire: an anchor exists, then a fold ends with verification off.
	require.NoError(t, svc.rh.gits.SetReference(plumbing.NewHashReference(verifiedRefName("main"), tip)))
	require.NoError(t, svc.rh.saveVerify(ctx, "main", foldResult{NewAnchor: tip, NewAnchorCtx: verifyContext{Mode: VerifyOff}}))
	_, err = svc.rh.gits.Reference(verifiedRefName("main"))
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound, "a verified disable retires the anchor")
}
