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

// ---- #349: the agent's trace entries

// T9 Format: Run follows Trigger; the agent's own keys follow Run, sorted by
// key whatever order they were given in; a Run-only (or Extra-only) set is not
// zero; an empty Extra value is omitted. The F07 script paragraph (Trace,
// Cause, Trigger) is byte-identical to before. Sabotage: leave Run out of
// IsZero; swap Run and Trigger; stop sorting Extra.
func TestAppendTrailers_RunAndExtraOrder(t *testing.T) {
	cause := strings.Repeat("a", 40)
	run := "run-" + strings.Repeat("b", 32)
	full := Trailers{Trace: "t-1", Cause: cause, Trigger: "inbox", Run: run,
		Extra: []TrailerEntry{{"Zeta", "z"}, {"Ticket", "ABC-12"}, {"Alpha", "a"}}}
	require.Equal(t, "learn: x\n\nKnomit-Trace: t-1\nKnomit-Cause: "+cause+"\nKnomit-Trigger: inbox\nKnomit-Run: "+run+
		"\nAlpha: a\nTicket: ABC-12\nZeta: z\n", appendTrailers("learn: x", full))
	require.Equal(t, []TrailerEntry{{"Zeta", "z"}, {"Ticket", "ABC-12"}, {"Alpha", "a"}}, full.Extra, "the caller's slice is not reordered")
	require.Equal(t, run, TrailerValue(appendTrailers("learn: x", full), TrailerRun))
	require.Equal(t, "ABC-12", TrailerValue(appendTrailers("learn: x", full), "ticket"))

	require.False(t, Trailers{Run: run}.IsZero(), "a Run-only set is a set")
	require.False(t, Trailers{Extra: []TrailerEntry{{"Ticket", "x"}}}.IsZero(), "an Extra-only set is a set")
	require.True(t, Trailers{Extra: []TrailerEntry{}}.IsZero())
	require.Equal(t, "learn: x\n\nKnomit-Run: "+run+"\n", appendTrailers("learn: x", Trailers{Run: run}))
	require.Equal(t, "learn: x\n\nTicket: x\n", appendTrailers("learn: x", Trailers{Extra: []TrailerEntry{{"Empty", ""}, {"Ticket", "x"}}}))

	// The F07 three-key paragraph is unchanged.
	require.Equal(t, "learn: x\n\nKnomit-Trace: t-1\nKnomit-Cause: "+cause+"\nKnomit-Trigger: inbox\n",
		appendTrailers("learn: x", Trailers{Trace: "t-1", Cause: cause, Trigger: "inbox"}))
}

// WithAgentTrace: an agent set reaches the commit; a ctx that already carries
// knomit's set (a script or recipe write) is REFUSED — even for a zero agent
// set, and knomit's set is left as it was; Knomit-Trigger is refused; a zero
// set on a plain ctx stamps nothing (D-mint). Sabotage: let WithAgentTrace
// overwrite or merge into an existing set.
func TestWithAgentTrace_RefusesOverKnomitsSet(t *testing.T) {
	svc := trailerStore(t)
	ctx := context.Background()
	run := "run-" + strings.Repeat("d", 32)
	agent := Trailers{Trace: "task-7", Run: run, Extra: []TrailerEntry{{"Ticket", "ABC-12"}}}

	stamped, err := WithAgentTrace(ctx, agent)
	require.NoError(t, err)
	r, err := svc.Facts().WriteFact(stamped, "agent/test", "kb/t/a.md", trailerFactBody, "learn: a", "learn")
	require.NoError(t, err)
	info, err := svc.Triggers().CommitInfo(ctx, plumbing.NewHash(r.CommitHash))
	require.NoError(t, err)
	require.Equal(t, "learn: a\n\nKnomit-Trace: task-7\nKnomit-Run: "+run+"\nTicket: ABC-12\n", info.Message)

	knomits := Trailers{Trace: "story", Cause: strings.Repeat("c", 40), Trigger: "inbox"}
	host := WithTrailers(ctx, knomits)
	got, err := WithAgentTrace(host, agent)
	require.ErrorIs(t, err, ErrTraceOverride)
	require.Equal(t, knomits, trailersFromContext(got), "knomit's set is untouched")
	_, err = WithAgentTrace(host, Trailers{})
	require.ErrorIs(t, err, ErrTraceOverride, "a passed trace is refused on a host write even when empty")

	_, err = WithAgentTrace(ctx, Trailers{Trigger: "wake"})
	require.ErrorIs(t, err, ErrTraceTrigger)

	plain, err := WithAgentTrace(ctx, Trailers{})
	require.NoError(t, err)
	require.True(t, trailersFromContext(plain).IsZero())
	_, set := plain.Value(trailersCtxKey{}).(Trailers)
	require.False(t, set, "a zero agent set attaches nothing")
}
