package facts

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/engine/store/internal/vcs"
	"knomit/internal/engine/store/model"
)

// OutcomesSince returns the hypothesis outcomes (Knomit-Outcome trailers,
// #641) recorded by commits on branch that are not reachable from since:
// hypothesis path -> "confirmed" | "refuted". A commit that does not carry the
// trailer contributes nothing; a path settled twice keeps the latest word
// (the walk is oldest first, the last sighting wins).
func (rh *Repo) OutcomesSince(ctx context.Context, branch, since string) (map[string]string, error) {
	head, err := rh.vr.ResolveRef(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("outcomes: resolve %s: %w", branch, err)
	}
	old, ok := model.ParseFullHash(since)
	if !ok {
		return nil, fmt.Errorf("outcomes: since %q is not a full commit hash", since)
	}
	seen, err := rh.AncestorSet(ctx, old)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	// WalkHistory visits parents before children, so a later settle of the
	// same path overwrites an earlier one.
	if err := vcs.WalkHistory(rh.vr.Storer(), head, seen, func(c *object.Commit) {
		for _, v := range model.TrailerValues(c.Message, model.TrailerOutcome) {
			if path, outcome, ok := model.ParseOutcome(v); ok {
				out[path] = outcome
			}
		}
	}); err != nil {
		return nil, fmt.Errorf("outcomes: walk from %s: %w", head, err)
	}
	return out, nil
}
