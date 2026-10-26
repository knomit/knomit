package synthesize

import (
	"context"
	"fmt"
	"strings"

	"knomit/internal/corpus/refs"
	"knomit/internal/corpus/synthesize/step"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/facts"
	"knomit/internal/engine/store/model"
)

// readHypothesis reads path at the branch tip and returns it only if it is a
// live hypothesis.
func readHypothesis(ctx context.Context, gs *facts.Index, branch, path string) error {
	r, err := gs.ReadFact(ctx, branch, path, nil)
	if err != nil {
		return fmt.Errorf("read hypothesis %s: %v", path, err)
	}
	h, err := fact.ParseFact(path, r.Content)
	if err != nil {
		return fmt.Errorf("parse hypothesis %s: %v", path, err)
	}
	if h.Type != fact.Hypothesis {
		return fmt.Errorf("%s is not a hypothesis", path)
	}
	return nil
}

// settleLine is the Knomit-Outcome value for a settled hypothesis.
func settleLine(path, outcome string) string { return path + " " + outcome }

// applyPruneSettle applies one prune "settle": every By fact gains the
// hypothesis in its refs and the hypothesis is deleted, in ONE commit whose
// last paragraph carries `Knomit-Outcome: <path> <outcome>`. A By fact that is
// itself a hypothesis, or that an earlier decision in this call removed,
// refuses the whole decision (nothing is written): predictions cannot settle
// predictions, and a verdict with no surviving citer would leave the retracted
// hypothesis uncited.
func applyPruneSettle(ctx context.Context, gs *facts.Index, gate refs.Gate, branch, recipe string,
	d step.PruneDecision, deleted map[string]bool) error {
	if d.Outcome != model.OutcomeConfirmed && d.Outcome != model.OutcomeRefuted {
		return fmt.Errorf("settle %s: outcome %q must be confirmed or refuted", d.Path, d.Outcome)
	}
	if len(d.By) == 0 {
		return fmt.Errorf("settle %s: by is empty", d.Path)
	}
	if deleted[d.Path] {
		return fmt.Errorf("settle %s: already removed in this call", d.Path)
	}
	if err := readHypothesis(ctx, gs, branch, d.Path); err != nil {
		return fmt.Errorf("settle: %v", err)
	}
	files := make(map[string]string, len(d.By))
	for _, by := range d.By {
		if deleted[by] {
			return fmt.Errorf("settle %s: by %s was removed earlier in this call", d.Path, by)
		}
		r, err := gs.ReadFact(ctx, branch, by, nil)
		if err != nil {
			return fmt.Errorf("settle %s: read by %s: %v", d.Path, by, err)
		}
		f, err := fact.ParseFact(by, r.Content)
		if err != nil {
			return fmt.Errorf("settle %s: parse by %s: %v", d.Path, by, err)
		}
		if f.Type == fact.Hypothesis {
			return fmt.Errorf("settle %s: by %s is a hypothesis; predictions cannot settle predictions", d.Path, by)
		}
		if len(f.ContextWarnings) > 0 {
			return fmt.Errorf("settle %s: by %s has a malformed context a rewrite would drop (%s)", d.Path, by, strings.Join(f.ContextWarnings, "; "))
		}
		canon, _, err := gate.Apply(ctx, by, fact.AppendUnique(append([]string(nil), f.Refs...), d.Path), f.Refs)
		if err != nil {
			return fmt.Errorf("settle %s: refs of %s: %v", d.Path, by, err)
		}
		f.Refs = canon
		content, err := fact.SerializeFact(f)
		if err != nil {
			return fmt.Errorf("settle %s: serialize %s: %v", d.Path, by, err)
		}
		files[by] = content
	}
	ctx = model.WithOutcomes(ctx, []string{settleLine(d.Path, d.Outcome)})
	msg := fmt.Sprintf("synthesize-%s: settle %s %s", recipe, d.Path, d.Outcome)
	if _, _, err := gs.BatchWriteFacts(ctx, branch, files, []string{d.Path}, msg, "settle"); err != nil {
		return fmt.Errorf("settle %s: write: %v", d.Path, err)
	}
	deleted[d.Path] = true
	return nil
}
