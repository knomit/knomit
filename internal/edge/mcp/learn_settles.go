package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/engine/store/model"
)

// learnSettle is one entry of a fact's request-only `settles`: the stored
// hypothesis the fact settles and the verdict. Like distinct_from it is never
// stored on the fact; the verdict lands in the commit's Knomit-Outcome trailer.
type learnSettle struct {
	Path    string `json:"path"`
	Outcome string `json:"outcome"`
}

// settleDecl is a validated settle: fact index, normalised hypothesis path, verdict.
type settleDecl struct {
	fact    int
	path    string
	outcome string
}

// applySettles validates every fact's `settles` and stages it: the fact cites
// the hypothesis, and the hypothesis joins retract (deleted by the same
// BatchWriteFacts commit as the fact). The settled paths are also added to
// retracting, so the dedup merge and the same-subject stage decline them and
// similarity subsumption never reports the same hypothesis a second time (the
// declared outcome wins). Nothing is written here.
//
// A hypothesis named in the same fact's distinct_from, or in the call's own
// retract, is refused: each says something else about it (keep it; retract it
// without a verdict) and silently picking one would drop the other.
func applySettles(
	ctx context.Context, b *repos.Binding, s mcpStore, writeBranch, ontologyRoot string,
	inputs []learnFactInput, facts []fact.Fact, paths []string, files map[string]string,
	retractPaths []string, retracting map[string]bool,
) ([]settleDecl, []string, error) {
	var decls []settleDecl
	var retract []string
	verdict := map[string]string{}
	for i, in := range inputs {
		for j, st := range in.Settles {
			where := fmt.Sprintf("fact %d: settles %d", i, j)
			if !model.ValidOutcome(st.Outcome) {
				return nil, nil, fmt.Errorf("%s: outcome %q must be %q or %q", where, st.Outcome, model.OutcomeConfirmed, model.OutcomeRefuted)
			}
			if strings.TrimSpace(st.Path) == "" {
				return nil, nil, fmt.Errorf("%s: path is empty", where)
			}
			p, err := federate.WriteRepoPath(b, st.Path)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %v", where, err)
			}
			p = fact.NormalizePath(ontologyRoot, p)
			if facts[i].Type == fact.Hypothesis {
				return nil, nil, fmt.Errorf("%s: a hypothesis cannot settle a hypothesis", where)
			}
			if namedIn(in.DistinctFrom, p) {
				return nil, nil, fmt.Errorf("%s: %s is also in this fact's distinct_from; settling retracts it and distinct_from keeps it apart", where, p)
			}
			for _, r := range retractPaths {
				if strings.EqualFold(r, p) {
					return nil, nil, fmt.Errorf("%s: %s is also in retract; drop it from retract, settles retracts it and records the outcome", where, p)
				}
			}
			read, err := s.facts.ReadFact(ctx, writeBranch, p, nil)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %s not found on this branch", where, p)
			}
			h, err := fact.ParseFact(p, read.Content)
			if err != nil || h.Type != fact.Hypothesis {
				return nil, nil, fmt.Errorf("%s: %s is not a hypothesis", where, p)
			}
			if prev, ok := verdict[strings.ToLower(p)]; ok && prev != st.Outcome {
				return nil, nil, fmt.Errorf("%s: %s is settled as both %s and %s in this call", where, p, prev, st.Outcome)
			}
			verdict[strings.ToLower(p)] = st.Outcome
			decls = append(decls, settleDecl{fact: i, path: p, outcome: st.Outcome})
			retracting[strings.ToLower(p)] = true
			retract = fact.AppendUnique(retract, p)
			facts[i].Refs = fact.AppendUnique(facts[i].Refs, p)
		}
		if len(in.Settles) > 0 {
			if err := reserialize(files, paths[i], facts[i]); err != nil {
				return nil, nil, fmt.Errorf("fact %d: serialize settles: %v", i, err)
			}
		}
	}
	return decls, retract, nil
}

// outcomePairs is the (path, outcome) pairs for model.WithOutcomes.
func outcomePairs(decls []settleDecl) []model.Outcome {
	out := make([]model.Outcome, 0, len(decls))
	for _, d := range decls {
		out = append(out, model.Outcome{Path: d.path, Outcome: d.outcome})
	}
	return out
}

// settleNotes is one short note per declared settle.
func settleNotes(decls []settleDecl) []string {
	sort.SliceStable(decls, func(a, b int) bool { return decls[a].fact < decls[b].fact })
	var notes []string
	for _, d := range decls {
		notes = append(notes, fmt.Sprintf("fact %d: hypothesis %s %s; retracted, cited in refs.", d.fact, d.path, d.outcome))
	}
	return notes
}
