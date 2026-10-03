package store

import (
	"context"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// cmContextFact is cmFact carrying a context map.
func cmContextFact(t *testing.T, body string, conf float64, ctx map[string]any) string {
	t.Helper()
	f := fact.NewFact("kb/notes/f.md")
	f.Title, f.Body = "Shared fact", body
	f.Kind, f.Type = fact.Epistemic, fact.Observation
	f.Confidence, f.Sources = conf, 1
	f.Domain, f.Refs, f.Entities = []string{"store"}, []string{}, []string{}
	f.Context = ctx
	s, err := fact.SerializeFact(f)
	require.NoError(t, err)
	return s
}

// C8 (F22): a context key both sides changed differently is settled by the
// strategy and recorded on the commit's Knomit-Merge trailer by KEY NAME —
// the values never reach the trailer. The key one side changed alone keeps
// that side's value.
func TestReplay_MergeFacts_ContextPerKeyTrailerNamesKeysOnly(t *testing.T) {
	r := newCMRepo(t)
	const agent = "agent/replay-abcd1234"
	r.branch(agent, "main")
	seed := r.write(agent, "kb/notes/f.md", cmContextFact(t, "base", 0.7, map[string]any{"task": "t-1", "verdict": "unsure"}))
	require.NoError(t, r.svc.rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), seed)))
	require.NoError(t, r.svc.rh.writeAgentBase(agent, seed))
	// The agent (more confident) changes verdict; main changes verdict
	// differently AND changes task alone.
	_, err := r.svc.Facts().WriteFact(context.Background(), agent, "kb/notes/f.md",
		cmContextFact(t, "base", 0.9, map[string]any{"task": "t-1", "verdict": "agree"}), "learn: f", "learn")
	require.NoError(t, err)
	r.write("main", "kb/notes/f.md", cmContextFact(t, "base", 0.5, map[string]any{"task": "t-2", "verdict": "disagree"}))

	_, err = r.svc.rh.reconcileAgent(context.Background(), agent, "main", stratMerge, true)
	require.NoError(t, err)
	tip := r.tip(agent)
	_, out := r.blob(tip, "kb/notes/f.md")
	f := r.parse(out)
	require.Equal(t, map[string]any{"task": "t-2", "verdict": "agree"}, f.Context,
		"task: main's one-sided change; verdict: the more confident side's")
	lines := TrailerValues(tip.Message, TrailerMerge)
	require.Len(t, lines, 1, tip.Message)
	require.Contains(t, lines[0], "decided=confidence,context.verdict")
	for _, v := range []string{"agree", "disagree", "unsure", "t-2"} {
		require.False(t, strings.Contains(lines[0], v), "the trailer names keys, never values: %q in %q", v, lines[0])
	}
}
