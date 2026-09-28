package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"
)

// The causal-trace trailers (F07 PR 3), store half.

const trailerFactBody = "---\ntype: observation\nconfidence: 0.8\nsources: 1\n---\n# t\n\nbody\n"

func trailerStore(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	return svc
}

// appendTrailers writes the set as the LAST paragraph, in the fixed key
// order, omitting empty keys; a zero set leaves the message alone; the stamped
// value wins over a body line of the same key (TrailerValue: last paragraph,
// last occurrence). Sabotage: append without the blank line (the body's
// last paragraph absorbs the keys and a body `Knomit-Trace:` line wins).
func TestAppendTrailers_LastParagraphAndReadBack(t *testing.T) {
	full := Trailers{Trace: "t-1", Cause: strings.Repeat("a", 40), Trigger: "inbox"}
	msg := appendTrailers("learn: x\n\nbody para\n", full)
	require.Equal(t, "learn: x\n\nbody para\n\nKnomit-Trace: t-1\nKnomit-Cause: "+strings.Repeat("a", 40)+"\nKnomit-Trigger: inbox\n", msg)
	require.Equal(t, "t-1", TrailerValue(msg, TrailerTrace))
	require.Equal(t, strings.Repeat("a", 40), TrailerValue(msg, TrailerCause))
	require.Equal(t, "inbox", TrailerValue(msg, TrailerTrigger))

	require.Equal(t, "learn: x", appendTrailers("learn: x", Trailers{}), "a zero set stamps nothing")
	require.Equal(t, "learn: x\n\nKnomit-Trigger: inbox\n", appendTrailers("learn: x\n\n\n", Trailers{Trigger: "inbox"}), "empty keys are omitted; trailing newlines are normalised")

	// A body whose last line looks like a trailer: the stamped paragraph is
	// the LAST one, so the stamped value is what reads back.
	tricky := appendTrailers("learn: x\n\nKnomit-Trace: body-says-so", Trailers{Trace: "stamped"})
	require.Equal(t, "stamped", TrailerValue(tricky, TrailerTrace))
}

// WithTrailers reaches the three authored-commit builders through the ctx —
// WriteFact (writeFileExact), DeleteFact (deleteFile) and BatchWriteFacts
// (batchWriteLocked) — and the paragraph is INSIDE the signed payload: the
// signature verifies on the stored commit and fails once one trailer
// character changes. A write with no set on its ctx carries no paragraph.
// Sabotage: stamp after storeCommit (the altered-trailer check passes); drop
// one of the three sites (its commit has no paragraph).
func TestWithTrailers_StampedAtThreeSitesInsideTheSignature(t *testing.T) {
	svc := trailerStore(t)
	ctx := context.Background()
	set := Trailers{Trace: "t-1", Cause: strings.Repeat("c", 40), Trigger: "inbox"}
	stamped := WithTrailers(ctx, set)
	want := "\n\nKnomit-Trace: t-1\nKnomit-Cause: " + strings.Repeat("c", 40) + "\nKnomit-Trigger: inbox\n"

	// Site 1: WriteFact → writeFileExact.
	r, err := svc.Facts().WriteFact(stamped, "agent/test", "kb/t/a.md", trailerFactBody, "learn: a", "learn")
	require.NoError(t, err)
	msgOf := func(h string) string {
		info, err := svc.Triggers().CommitInfo(ctx, plumbing.NewHash(h))
		require.NoError(t, err)
		return info.Message
	}
	require.Equal(t, "learn: a"+want, msgOf(r.CommitHash))
	require.Equal(t, "inbox", TrailerValue(msgOf(r.CommitHash), TrailerTrigger))

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
	c, err := svc.rh.repo.CommitObject(plumbing.NewHash(r.CommitHash))
	require.NoError(t, err)
	signer, err := verifyCommitSignature(c)
	require.NoError(t, err)
	require.Equal(t, svc.SignerFingerprint(), signer.Fingerprint)
	altered := *c
	altered.Message = strings.Replace(c.Message, "Knomit-Trace: t-1", "Knomit-Trace: t-2", 1)
	_, err = verifyCommitSignature(&altered)
	require.Error(t, err, "a trailer cannot be altered without breaking the signature")
}
