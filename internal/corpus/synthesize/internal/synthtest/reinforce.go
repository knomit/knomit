package synthtest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
)

const (
	ReinforceSeedOnePath = "kb/gotchas/uitesting/agentclicks.md"
	ReinforceSeedTwoPath = "kb/technology/benchmarks/terminalscores.md"
	ReinforceExistingRef = "kb/conventions/verifiers/sandboxing.md"
	ReinforceTargetPath  = "kb/principles/evaluation/verifierreach.md"
)

// NewReinforceCorpus is a real store holding two seed facts, one unrelated fact
// the target already cites, and the target itself (a synthesis fact whose
// origin is explicit so the REINFORCE write contract does not skip it).
func NewReinforceCorpus(t *testing.T) *RestatementEnv {
	t.Helper()
	env := NewRestatementEnv(t, 0)
	ctx := context.Background()

	write := func(path, title string, refs []string, sources int) {
		f := fact.NewFact(path)
		f.Title = title
		f.Body = "Body of " + title + "."
		f.Type = fact.Observation
		f.Confidence = 0.8
		f.Sources = sources
		f.Refs = refs
		content, err := fact.SerializeFact(f)
		require.NoError(t, err)
		_, err = env.Svc.Facts().WriteFact(ctx, env.Branch, path, content, "write "+path, "test")
		require.NoError(t, err)
	}

	write(ReinforceSeedOnePath, "An agent testing a UI will execute JavaScript instead of clicking", nil, 1)
	write(ReinforceSeedTwoPath, "Coding agents cheated a terminal benchmark to 94%", nil, 1)
	write(ReinforceExistingRef, "Verifiers run in a sandbox the agent cannot write to", nil, 1)

	target := fact.NewFact(ReinforceTargetPath)
	target.Title = "The verifier is inside the agent's action space"
	target.Body = "Every recorded form of eval gaming is an action taken ON the measurement apparatus."
	target.Type = fact.Synthesis
	// Origin EXPLICIT. A synthesis fact with the line elided does not
	// round-trip — ParseFact defaults it to distilled and SerializeFact then
	// writes it — so under the write contract such a target is SKIPPED. Every
	// pipeline-written synthesis fact carries it; the fixture matches.
	target.Origin = fact.Distilled
	target.Confidence = 0.75
	target.Sources = 1
	target.Domain = []string{"evaluation"}
	target.Entities = []string{"verifier gaming"}
	target.Motifs = []string{"measure-becomes-target"}
	target.Refs = []string{ReinforceExistingRef}
	content, err := fact.SerializeFact(target)
	require.NoError(t, err)
	_, err = env.Svc.Facts().WriteFact(ctx, env.Branch, ReinforceTargetPath, content, "write target", "test")
	require.NoError(t, err)
	return env
}
