package manage

import (
	"context"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/model"
)

// listRepoBranches reads the branch list from a repo's store via WithRead. A
// store that cannot be reached (e.g. the repo is still opening) is WithRead's
// error, returned as-is: the handlers answer it 503 through httpx.WriteStoreError,
// not an empty list that reads as "this repo has no branches".
func listRepoBranches(ctx context.Context, ri *repos.RepoInstance) ([]model.Branch, error) {
	var (
		out []model.Branch
		err error
	)
	if aerr := ri.WithRead(func(svc *store.Service) {
		out, err = svc.ListBranches(ctx)
	}); aerr != nil {
		return out, aerr
	}
	return out, err
}

// readBranchRoot reads head + index watermark via the store. Plan
// 02 may expand this to include more fields (branch metadata, last commit
// time) as those handlers come online.
func readBranchRoot(ctx context.Context, ri *repos.RepoInstance, branch string) (branchRootInfo, error) {
	var (
		info branchRootInfo
		err  error
	)
	if aerr := ri.WithRead(func(svc *store.Service) {
		head, herr := svc.HeadCommit(ctx, branch)
		if herr != nil {
			err = herr
			return
		}
		info.Head = head
		idx, ierr := svc.IndexManager().SyncWatermark(ctx, branch)
		if ierr == nil {
			info.IndexCommit = idx
		}
	}); aerr != nil {
		return info, aerr
	}
	return info, err
}
