package remotesync

import (
	"context"
	"errors"
	"fmt"
	"io"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"

	storegit "knomit/internal/engine/store/git"
)

// ErrHistoryProbeTooLarge ends a ProbeBranchHistory whose transfer exceeded
// its budget: nothing about the remote branch was established.
var ErrHistoryProbeTooLarge = errors.New("the origin's history is too large to inspect within the probe budget")

// BranchHistory is what ProbeBranchHistory established about a remote branch,
// measured against one local branch of this store.
type BranchHistory struct {
	// Absent: the remote has no such branch, or no refs at all. Nothing else
	// is set.
	Absent bool
	// Root is the remote branch's root commit by RootCommit's rule (a
	// first-parent walk from the tip): the knowledge base the branch holds.
	Root string
	// Shared: the remote branch and the local branch have a common ancestor.
	// False when the local branch does not exist.
	Shared bool
}

// ProbeBranchHistory fetches remoteBranch from url into memory, against this
// store's objects, and reports which knowledge base it holds and whether it
// shares history with localBranch. It writes nothing to the store: the fetched
// objects and the ref live in a memory overlay that reads through to the
// store, so the fetch advertises the store's branch heads as haves and the
// remote sends only what the store lacks (a remote ahead of the local branch
// costs its new commits, an unrelated one its whole history). budget bounds
// that transfer twice (<= 0: unbounded): the raw pack as it arrives, before
// any of it is parsed, and the inflated objects the parse stores. Past either
// the answer is ErrHistoryProbeTooLarge.
//
// The caller gates url (the local-origin policy) and resolves auth; this is a
// clone/fetch primitive like FetchOrigin and validates nothing itself.
func (ri *Index) ProbeBranchHistory(ctx context.Context, url string, auth transport.AuthMethod, remoteBranch, localBranch string, budget int64) (BranchHistory, error) {
	local := ri.rh.vr.Storer()
	overlay := &probeStorage{Storage: memory.NewStorage(), local: local, budget: budget}
	dst := plumbing.NewRemoteReferenceName("probe", remoteBranch)
	rem := gogit.NewRemote(overlay, &gogitconfig.RemoteConfig{Name: "probe", URLs: []string{url}})
	err := rem.FetchContext(ctx, &gogit.FetchOptions{
		RemoteName: "probe",
		Auth:       auth,
		RefSpecs:   []gogitconfig.RefSpec{gogitconfig.RefSpec(fmt.Sprintf("+refs/heads/%s:%s", remoteBranch, dst))},
		Tags:       gogit.NoTags,
	})
	var noMatch gogit.NoMatchingRefSpecError
	switch {
	case err == nil, errors.Is(err, gogit.NoErrAlreadyUpToDate):
	case errors.As(err, &noMatch), errors.Is(err, transport.ErrEmptyRemoteRepository):
		return BranchHistory{Absent: true}, nil
	case errors.Is(err, ErrHistoryProbeTooLarge):
		return BranchHistory{}, fmt.Errorf("%w (%d bytes)", ErrHistoryProbeTooLarge, budget)
	default:
		return BranchHistory{}, err
	}
	ref, err := overlay.Storage.Reference(dst)
	if err != nil {
		return BranchHistory{}, fmt.Errorf("probe: the fetched ref: %w", err)
	}
	tip, err := object.GetCommit(overlay, ref.Hash())
	if err != nil {
		return BranchHistory{}, fmt.Errorf("probe: the fetched tip: %w", err)
	}
	root := tip
	for len(root.ParentHashes) > 0 {
		if err := ctx.Err(); err != nil {
			return BranchHistory{}, err
		}
		if root, err = object.GetCommit(overlay, root.ParentHashes[0]); err != nil {
			return BranchHistory{}, fmt.Errorf("probe: walk to the root: %w", err)
		}
	}
	out := BranchHistory{Root: root.Hash.String()}
	lref, err := local.Reference(plumbing.NewBranchReferenceName(localBranch))
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return out, nil
	}
	if err != nil {
		return BranchHistory{}, err
	}
	lc, err := object.GetCommit(overlay, lref.Hash())
	if err != nil {
		return BranchHistory{}, fmt.Errorf("probe: the local %s: %w", localBranch, err)
	}
	bases, err := tip.MergeBase(lc)
	if err != nil {
		return BranchHistory{}, fmt.Errorf("probe: merge base: %w", err)
	}
	out.Shared = len(bases) > 0
	return out, nil
}

// probeStorage is ProbeBranchHistory's overlay: writes (objects, refs, config)
// go to memory, object reads fall through to the store, and the reference
// listing adds the store's branch heads so the fetch negotiates them as haves.
// The store itself is only read.
type probeStorage struct {
	*memory.Storage
	local  *storegit.Storer
	budget int64
	spent  int64
}

// PackfileWriter bounds the fetched RAW pack by the budget as it arrives
// (NewBudgetedPackWriter); SetEncodedObject then bounds the inflated objects
// the parse stores. The parse targets p, so a thin pack's bases are read
// through to the store.
func (p *probeStorage) PackfileWriter() (io.WriteCloser, error) {
	return NewBudgetedPackWriter(p, p.budget, ErrHistoryProbeTooLarge), nil
}

func (p *probeStorage) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	if p.budget > 0 {
		p.spent += obj.Size()
		if p.spent > p.budget {
			return plumbing.ZeroHash, ErrHistoryProbeTooLarge
		}
	}
	return p.Storage.SetEncodedObject(obj)
}

func (p *probeStorage) EncodedObject(t plumbing.ObjectType, h plumbing.Hash) (plumbing.EncodedObject, error) {
	obj, err := p.Storage.EncodedObject(t, h)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return p.local.EncodedObject(t, h)
	}
	return obj, err
}

func (p *probeStorage) HasEncodedObject(h plumbing.Hash) error {
	if err := p.Storage.HasEncodedObject(h); err == nil {
		return nil
	}
	return p.local.HasEncodedObject(h)
}

func (p *probeStorage) EncodedObjectSize(h plumbing.Hash) (int64, error) {
	n, err := p.Storage.EncodedObjectSize(h)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return p.local.EncodedObjectSize(h)
	}
	return n, err
}

func (p *probeStorage) IterReferences() (storer.ReferenceIter, error) {
	var refs []*plumbing.Reference
	add := func(it storer.ReferenceIter, err error, headsOnly bool) error {
		if err != nil {
			return err
		}
		return it.ForEach(func(r *plumbing.Reference) error {
			if !headsOnly || r.Name().IsBranch() {
				refs = append(refs, r)
			}
			return nil
		})
	}
	mine, err := p.Storage.IterReferences()
	if err := add(mine, err, false); err != nil {
		return nil, err
	}
	theirs, err := p.local.IterReferences()
	if err := add(theirs, err, true); err != nil {
		return nil, err
	}
	return storer.NewReferenceSliceIter(refs), nil
}
