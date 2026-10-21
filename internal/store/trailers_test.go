package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/store/model"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/verify"
)

// The causal-trace trailers (F07 PR 3), store half.

const trailerFactBody = "---\ntype: observation\nconfidence: 0.8\nsources: 1\n---\n# t\n\nbody\n"

func trailerStore(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
	return svc
}

// WithTrailers reaches the three authored-commit builders through the ctx —
// WriteFact (writeFileExact), DeleteFact (deleteFile) and BatchWriteFacts
// (batchWriteLocked) — and the paragraph is INSIDE the signed payload: the
// signature verifies on the stored commit and fails once one trailer
// character changes. A write with no set on its ctx carries no paragraph.
// Sabotage: stamp after StoreCommit (the altered-trailer check passes); drop
// one of the three sites (its commit has no paragraph).
func TestWithTrailers_StampedAtThreeSitesInsideTheSignature(t *testing.T) {
	svc := trailerStore(t)
	ctx := context.Background()
	set := model.Trailers{Trace: "t-1", Cause: strings.Repeat("c", 40), Trigger: "inbox"}
	stamped := model.WithTrailers(ctx, set)
	want := "\n\nKnomit-Trace: t-1\nKnomit-Cause: " + strings.Repeat("c", 40) + "\nKnomit-Trigger: inbox\n"

	// Site 1: WriteFact → writeFileExact.
	r, err := svc.Facts().WriteFact(stamped, "agent/test", "kb/t/a.md", trailerFactBody, "learn: a", "learn")
	require.NoError(t, err)
	msgOf := func(h string) string {
		info, err := svc.FactsRepo().CommitInfo(ctx, plumbing.NewHash(h))
		require.NoError(t, err)
		return info.Message
	}
	require.Equal(t, "learn: a"+want, msgOf(r.CommitHash))
	require.Equal(t, "inbox", model.TrailerValue(msgOf(r.CommitHash), model.TrailerTrigger))

	// Site 3: BatchWriteFacts → batchWriteLocked.
	bh, _, err := svc.Facts().BatchWriteFacts(stamped, "agent/test", map[string]string{"kb/t/b.md": trailerFactBody}, nil, "learn: b", "learn")
	require.NoError(t, err)
	require.Equal(t, "learn: b"+want, msgOf(bh))

	// Site 2: DeleteFact → deleteFile.
	dh, err := svc.Facts().DeleteFact(stamped, "agent/test", "kb/t/b.md", "retract: b")
	require.NoError(t, err)
	require.Equal(t, "retract: b"+want, msgOf(dh))

	// No set on the ctx: no paragraph (MCP, REST and synthesize are unchanged).
	plain, err := svc.Facts().WriteFact(ctx, "agent/test", "kb/t/c.md", trailerFactBody, "learn: c", "learn")
	require.NoError(t, err)
	require.Equal(t, "learn: c", msgOf(plain.CommitHash))

	// The paragraph is signed: the stored commit verifies with this store's
	// key; the same commit with one trailer character changed does not.
	c, err := svc.rh.Repository().CommitObject(plumbing.NewHash(r.CommitHash))
	require.NoError(t, err)
	signer, err := verify.VerifyCommitSignature(c)
	require.NoError(t, err)
	require.Equal(t, svc.FactsRepo().SignerFingerprint(), signer.Fingerprint)
	altered := *c
	altered.Message = strings.Replace(c.Message, "Knomit-Trace: t-1", "Knomit-Trace: t-2", 1)
	_, err = verify.VerifyCommitSignature(&altered)
	require.Error(t, err, "a trailer cannot be altered without breaking the signature")
}

// ---- #349: the agent's trace entries

// WithAgentTrace: an agent set reaches the commit; a ctx that already carries
// knomit's set (a script or recipe write) is REFUSED — even for a zero agent
// set, and knomit's set is left as it was; Knomit-Trigger is refused; a zero
// set on a plain ctx stamps nothing (D-mint). Sabotage: let WithAgentTrace
// overwrite or merge into an existing set.
func TestWithAgentTrace_RefusesOverKnomitsSet(t *testing.T) {
	svc := trailerStore(t)
	ctx := context.Background()
	run := "run-" + strings.Repeat("d", 32)
	agent := model.Trailers{Trace: "task-7", Run: run, Extra: []model.TrailerEntry{{Key: "Ticket", Value: "ABC-12"}}}

	stamped, err := model.WithAgentTrace(ctx, agent)
	require.NoError(t, err)
	r, err := svc.Facts().WriteFact(stamped, "agent/test", "kb/t/a.md", trailerFactBody, "learn: a", "learn")
	require.NoError(t, err)
	info, err := svc.FactsRepo().CommitInfo(ctx, plumbing.NewHash(r.CommitHash))
	require.NoError(t, err)
	require.Equal(t, "learn: a\n\nKnomit-Trace: task-7\nKnomit-Run: "+run+"\nTicket: ABC-12\n", info.Message)

	knomits := model.Trailers{Trace: "story", Cause: strings.Repeat("c", 40), Trigger: "inbox"}
	host := model.WithTrailers(ctx, knomits)
	got, err := model.WithAgentTrace(host, agent)
	require.ErrorIs(t, err, model.ErrTraceOverride)
	require.Equal(t, knomits, model.TrailersFromContext(got), "knomit's set is untouched")
	_, err = model.WithAgentTrace(host, model.Trailers{})
	require.ErrorIs(t, err, model.ErrTraceOverride, "a passed trace is refused on a host write even when empty")

	_, err = model.WithAgentTrace(ctx, model.Trailers{Trigger: "wake"})
	require.ErrorIs(t, err, model.ErrTraceTrigger)

	plain, err := model.WithAgentTrace(ctx, model.Trailers{})
	require.NoError(t, err)
	require.True(t, model.TrailersFromContext(plain).IsZero())
	set := vcs.HasTrailersForTest(plain)
	require.False(t, set, "a zero agent set attaches nothing")
}
