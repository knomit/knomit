package store

import (
	"context"
	"testing"

	"knomit/internal/engine/store/model"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExperimentWriteRefused_LeavesNoObjects (#554): a write onto an
// experiment whose owning agent cannot be named is refused BEFORE any blob or
// tree is stored, because objects are never deleted and a refused write
// would leave them behind as unreachable rows (Verify orphan_objects). The
// batch path used to resolve CommitSigs only after storing its blobs and
// trees; the single-file paths are the controls. Sabotage: move the
// CommitSigs call in batchWriteLocked back below the tree work — red: the
// batch subtests see the object count grow.
func TestExperimentWriteRefused_LeavesNoObjects(t *testing.T) {
	ctx := context.Background()
	content := "---\ntype: observation\n---\n# x\n\nbody\n"
	content2 := "---\ntype: observation\n---\n# y\n\nother body\n"

	// Each setup builds a store with an experiment branch that every write
	// refuses for the named reason.
	refusals := map[string]func(t *testing.T) (*Service, string, error){
		"orphan ref": func(t *testing.T) (*Service, string, error) {
			svc := newExperimentTestStore(t)
			require.NoError(t, svc.CreateBranch(ctx, "exp/ghost", testAgentBranch))
			return svc, "exp/ghost", model.ErrOrphanExperimentRef
		},
		"stale parent": func(t *testing.T) (*Service, string, error) {
			svc := newExperimentTestStore(t)
			_, err := svc.Merge().OpenExperiment(ctx, "stale", "", testAgentBranch)
			require.NoError(t, err)
			require.NoError(t, svc.CreateBranch(ctx, "agent/successor", testAgentBranch))
			require.NoError(t, svc.SetAgentBranchOwner(ctx, "agent/successor"))
			return svc, "exp/stale", model.ErrStaleExperimentParent
		},
	}
	for name, setup := range refusals {
		t.Run(name, func(t *testing.T) {
			svc, branch, want := setup(t)
			before, err := svc.HeadCommit(ctx, branch)
			require.NoError(t, err)
			base := countRows(t, svc, `SELECT COUNT(*) FROM objects`)

			check := func(what string, err error) {
				t.Helper()
				require.ErrorIs(t, err, want, what)
				now := countRows(t, svc, `SELECT COUNT(*) FROM objects`)
				assert.Equal(t, base, now, "%s: a refused write stores no object", what)
				base = now // so one leaking case does not mask the next
				after, herr := svc.HeadCommit(ctx, branch)
				require.NoError(t, herr)
				require.Equal(t, before, after, "%s: a refused write moves nothing", what)
			}

			_, err = svc.Facts().WriteFact(ctx, branch, "kb/x.md", content, "learn: x", "learn")
			check("WriteFact (control)", err)
			_, err = svc.Facts().DeleteFact(ctx, branch, "kb/base.md", "retract(x): kb/base.md")
			check("DeleteFact (control)", err)
			_, _, err = svc.Facts().BatchWriteFacts(ctx, branch, map[string]string{"kb/x.md": content, "kb/sub/y.md": content2}, nil, "move: x", "move")
			check("BatchWriteFacts writes", err)
			_, _, err = svc.Facts().BatchWriteFacts(ctx, branch, map[string]string{"kb/x.md": content}, []string{"kb/base.md"}, "move: x", "move")
			check("BatchWriteFacts write+delete", err)
			_, _, err = svc.Facts().BatchWriteFacts(ctx, branch, nil, []string{"kb/base.md"}, "retract: base", "retract")
			check("BatchWriteFacts delete-only", err)
		})
	}
}
