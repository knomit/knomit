package facts

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/engine/store/internal/vcs"
	"knomit/internal/engine/store/model"
)

// Settlements is what the commits of one review session say about deleted
// facts (#641).
type Settlements struct {
	// Outcomes maps a hypothesis path to the verdict a Knomit-Outcome trailer
	// recorded: "confirmed" or "refuted". The latest word wins.
	Outcomes map[string]string
	// Written maps a deleted path to the fact paths the commit that deleted it
	// added or modified: the candidates that may cite it. The latest deleting
	// commit wins. Merge commits are skipped (their first-parent diff is the
	// whole merged branch); the merged branch's own commits are walked.
	Written map[string][]string
}

// SettlementsSince walks the commits on branch that are not reachable from
// since (the review watermark; "" means the whole history) ONCE and returns
// their Settlements. One walk serves both consumers, so the verdicts and the
// deleting commit's writes cannot be read from different ranges.
func (rh *Repo) SettlementsSince(ctx context.Context, branch, since string) (Settlements, error) {
	head, err := rh.vr.ResolveRef(ctx, branch)
	if err != nil {
		return Settlements{}, fmt.Errorf("settlements: resolve %s: %w", branch, err)
	}
	var stop map[plumbing.Hash]bool
	if since != "" {
		old, ok := model.ParseFullHash(since)
		if !ok {
			return Settlements{}, fmt.Errorf("settlements: since %q is not a full commit hash", since)
		}
		if stop, err = rh.AncestorSet(ctx, old); err != nil {
			return Settlements{}, err
		}
	}
	out := Settlements{Outcomes: map[string]string{}, Written: map[string][]string{}}
	var diffErr error
	// WalkHistory visits parents before children, so a later commit overwrites
	// an earlier one.
	if err := vcs.WalkHistory(rh.vr.Storer(), head, stop, func(c *object.Commit) {
		for _, v := range model.TrailerValues(c.Message, model.TrailerOutcome) {
			if path, outcome, ok := model.ParseOutcome(v); ok {
				out.Outcomes[path] = outcome
			}
		}
		if c.NumParents() != 1 || diffErr != nil {
			return
		}
		p, err := c.Parent(0)
		if err != nil {
			diffErr = err
			return
		}
		pt, err := p.Tree()
		if err != nil {
			diffErr = err
			return
		}
		ct, err := c.Tree()
		if err != nil {
			diffErr = err
			return
		}
		changes, err := object.DiffTree(pt, ct)
		if err != nil {
			diffErr = err
			return
		}
		var deleted, written []string
		for _, ch := range changes {
			switch {
			case ch.To.Name == "":
				deleted = append(deleted, ch.From.Name)
			default:
				written = append(written, ch.To.Name)
			}
		}
		for _, d := range deleted {
			out.Written[d] = written
		}
	}); err != nil {
		return Settlements{}, fmt.Errorf("settlements: walk from %s: %w", head, err)
	}
	if diffErr != nil {
		return Settlements{}, fmt.Errorf("settlements: diff: %w", diffErr)
	}
	return out, nil
}
