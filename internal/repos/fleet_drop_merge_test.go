package repos

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/store"
	"knomit/internal/store/storetest"
)

const rvPolicy = "---\nkind: pragmatic\ntype: policy\n---\n# alpha work\n\nwork\n"

// #533 review S1, the reviewer's probe made a test: the master merged a
// member's work while its main was held by a bad own commit; then (the
// revoked row) the operator revokes the member, still held; then Drop. The
// drop replays the merge commit as a merge of the same parents, so the
// member's work merged before the revocation reaches main, whatever the
// merger now decides about the member's branch (the revoked member's branch
// is refused and demoted by the merger's re-judge, which before this fix lost
// the work). The control row has no revocation.
//
// SABOTAGE: leave every merge out of DropUnpublishedCommit's replay (the
// pre-fix behaviour) -> the revoked row's work never reaches main -> red.
func TestFleetDrop_AMemberWorkMergedWhileMainWasHeldSurvivesTheDrop(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "control", true: "revoked"}[revoke], func(t *testing.T) {
			ctx := context.Background()
			f := newForgeFleet(t, nil)
			installMasterLeaf(t, f)
			alpha := f.namedMember(t, "alpha")
			f.enrollAndJoin(t, alpha, "alpha")
			f.masterRounds(t)
			ari := alpha.fleetRepo()
			round(t, ari)

			// main held by a bad own commit (past the writers, which refuse it).
			var bad string
			require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
				h, err := storetest.CommitFiles(svc, f.mri.AgentBranch(), map[string]string{"kb/members/master/note.md": notPolicy}, "bad")
				require.NoError(t, err)
				bad = h
			}))
			// alpha's legitimate work, pushed and merged by the master.
			require.NoError(t, ari.WithRead(func(svc *store.Service) {
				_, err := svc.Facts().WriteFact(ctx, ari.AgentBranch(), "kb/members/alpha/work.md", rvPolicy, "work", "created")
				require.NoError(t, err)
			}))
			round(t, ari)
			f.masterRounds(t)
			require.False(t, f.onForgeMain(t, "kb/members/alpha/work.md"), "main is held")
			require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
				_, ok, err := svc.FileAt(f.mri.AgentBranch(), "kb/members/alpha/work.md", 1<<20)
				require.NoError(t, err)
				require.True(t, ok, "fixture: the master merged alpha's work while main was held")
			}))
			if !revoke {
				// The tip is the master's merge of alpha's work: a merge
				// cannot be dropped (422 merge_commit), and nothing changes.
				tip := localTip(t, f.mri, f.mri.AgentBranch())
				_, err := f.master.DropFleetCommit(ctx, tip)
				status, code := fleetCode(t, err)
				require.Equal(t, 422, status)
				require.Equal(t, "merge_commit", code)
				require.Equal(t, tip, localTip(t, f.mri, f.mri.AgentBranch()))
			}
			if revoke {
				_, err := f.master.RevokeFleetMember(ctx, "alpha", "test")
				require.NoError(t, err)
			}
			out, err := f.master.DropFleetCommit(ctx, bad)
			require.NoError(t, err)
			require.True(t, out.Published, out.PublishError)
			require.Zero(t, out.MergesDropped, "no merge brought the dropped commit in")
			require.True(t, f.onForgeMain(t, "kb/members/alpha/work.md"), "alpha's work merged before the drop is on main")
			require.False(t, f.onForgeMain(t, "kb/members/master/note.md"), "the dropped commit's fact is not")
			f.masterRounds(t)
			require.True(t, f.onForgeMain(t, "kb/members/alpha/work.md"), "and stays there")
		})
	}
}
