package facts

import (
	"context"
	"fmt"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/storer"

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

// maxDeletionWalk bounds how far back DeletionPeers looks for the commit that
// removed a path. Reflect runs at the end of the session that made it.
const maxDeletionWalk = 4000

// DeletionPeers returns the fact paths that the newest commit on branch that
// DELETED path added or modified (the settling or retracting commit's other
// writes), or nil when no such commit is found within the walk bound. The
// commit is the one whose first parent has path and whose own tree does not.
func (rh *Repo) DeletionPeers(ctx context.Context, branch, path string) ([]string, error) {
	head, err := rh.vr.ResolveRef(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("deletion peers: resolve %s: %w", branch, err)
	}
	it, err := rh.vr.Repository().Log(&gogit.LogOptions{From: head})
	if err != nil {
		return nil, fmt.Errorf("deletion peers: log: %w", err)
	}
	defer it.Close()
	var peers []string
	found := false
	n := 0
	err = it.ForEach(func(c *object.Commit) error {
		if n++; n > maxDeletionWalk {
			return storer.ErrStop
		}
		if c.NumParents() == 0 {
			return nil
		}
		p, err := c.Parent(0)
		if err != nil {
			return nil
		}
		ct, err := c.Tree()
		if err != nil {
			return nil
		}
		pt, err := p.Tree()
		if err != nil {
			return nil
		}
		if _, err := ct.FindEntry(path); err == nil {
			return nil
		}
		if _, err := pt.FindEntry(path); err != nil {
			return nil
		}
		changes, err := object.DiffTree(pt, ct)
		if err != nil {
			return err
		}
		for _, ch := range changes {
			if ch.To.Name != "" {
				peers = append(peers, ch.To.Name)
			}
		}
		found = true
		return storer.ErrStop
	})
	if err != nil {
		return nil, fmt.Errorf("deletion peers: walk: %w", err)
	}
	if !found {
		return nil, nil
	}
	return peers, nil
}
