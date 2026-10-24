package manage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
)

// Each lifecycle read helper returns WithRead's refusal itself, not a
// zero-valued success. web's TestReadHelpers_ReturnAcquireRefusal pins the KB
// helpers; these three moved here with their handlers (listRepoBranches and
// readBranchRoot behind GET /branches, /pushed-branches and the branch root,
// getOrigin behind GET /origin).
func TestReadHelpers_ReturnAcquireRefusal(t *testing.T) {
	ri := newTestManagerWithRepos(t, "alpha").Get("alpha")
	ctx := context.Background()
	const branch = "agent/test"

	errs := map[string]error{}
	_, errs["listRepoBranches"] = listRepoBranches(ctx, ri)
	_, errs["readBranchRoot"] = readBranchRoot(ctx, ri, branch)
	_, errs["getOrigin"] = getOrigin(ctx, ri)

	for name, err := range errs {
		require.ErrorIs(t, err, repos.ErrStoreUnavailable, name)
	}
	require.Len(t, errs, 3, "every helper is in the table")
}
