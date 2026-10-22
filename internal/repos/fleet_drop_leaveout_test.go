package repos

import (
	"context"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
	"knomit/internal/store/storetest"
)

// #533 review 3 SF1: the foreign_commit dead end has a confirmed way out.
//
// A member, alpha, has TWO commits on the master's branch, brought in by one
// merge, and the master's judge refuses the first, so main is held. Both are
// alpha's own, signed with its key and pushed to the forge: the first also
// writes under ANOTHER member's folder (not-owner). The fetch judge refuses
// alpha's branch for it, so the merge is made on the master's copy
// directly, past that judge: no real entry path admits a member commit the
// publish judge then refuses, which is what this PR enforces. Alpha then
// leaves, so it will fix nothing. The drop of the named commit is refused
// with foreign_commit (alpha's second commit would be re-signed) and lists
// what leaving the whole merge out loses, by author and subject; nothing
// changes. Without the confirmation, or with a stale one, nothing changes
// either. With it, the merge is left out, the rest of the branch (beta's
// enrollment, made while main was held) is replayed, and main MOVES.
//
// SABOTAGE: have LeaveOutFleetMerge run a plain drop -> foreign_commit
// again, main stays held -> red.
func TestFleetDrop_ALeftMembersTwoCommitsCanBeLeftOutWholeOnceConfirmed(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	alpha := f.namedMember(t, "alpha")
	f.enrollAndJoin(t, alpha, "alpha")
	f.masterRounds(t)
	ari := alpha.fleetRepo()
	round(t, ari)
	require.Equal(t, fact.MemberActive, memberState(t, f.master, "alpha"))

	// Alpha's two commits, pushed to the forge.
	var side []string
	require.NoError(t, ari.WithRead(func(svc *store.Service) {
		for _, w := range []struct{ p, msg string }{
			{"kb/members/beta/one.md", "alpha work one"},
			{"kb/members/alpha/two.md", "alpha work two"},
		} {
			h, err := storetest.CommitFiles(svc, ari.AgentBranch(), map[string]string{w.p: rvPolicy}, w.msg)
			require.NoError(t, err)
			side = append(side, h)
		}
	}))
	round(t, ari)
	mainBefore := f.rev(t, "main")
	f.masterRounds(t)
	require.Equal(t, mainBefore, f.rev(t, "main"), "the fetch judge refused alpha's branch")
	// The master's copy merges it past that judge.
	require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
		require.NoError(t, svc.CreateBranch(ctx, "agent/alpha-held", f.mri.AgentBranch()))
		require.NoError(t, storetest.SetRef(svc, "refs/heads/agent/alpha-held", side[1]))
		_, err := svc.MergePushed(ctx, "agent/alpha-held", f.mri.AgentBranch(), plumbing.NewHash(side[1]), "")
		require.NoError(t, err)
		require.NoError(t, storetest.DropBranch(ctx, svc, "agent/alpha-held"))
	}))
	f.masterRounds(t)
	roundOnPush(t, f.mri, f.master.fleetPushed) // the sync loop records its push outcome
	require.Equal(t, mainBefore, f.rev(t, "main"), "main is held")
	st, err := f.master.FleetStatus(ctx)
	require.NoError(t, err)
	require.True(t, st.LastErrorMainHeld, st.LastError)
	named := st.LastErrorMainHeldCommit
	require.Equal(t, side[0], named, "the refusal names alpha's first commit: %s", st.LastError)

	// Alpha leaves: it will fix nothing on its branch.
	_, err = alpha.LeaveFleet(ctx)
	require.NoError(t, err)
	// Meanwhile the operator enrolls beta: held behind the refused commit.
	enr, err := f.master.EnrollFleetMember(ctx, "beta", keyLine("beta"), "")
	require.NoError(t, err)
	require.False(t, enr.Published, "main is held")
	f.masterRounds(t)
	require.Equal(t, mainBefore, f.rev(t, "main"), "still held")
	require.False(t, f.onForgeMain(t, "artifacts/fleet/certs/beta.pem"))

	tip := localTip(t, f.mri, f.mri.AgentBranch())
	_, err = f.master.DropFleetCommit(ctx, named)
	status, code := fleetCode(t, err)
	require.Equal(t, 409, status)
	require.Equal(t, "foreign_commit", code)
	fe := err.(*FleetError)
	require.Contains(t, fe.Msg, "leave the whole merge out")
	var listed []string
	for _, l := range fe.LeaveOut {
		listed = append(listed, l.Commit)
		if l.Commit == side[0] {
			require.Equal(t, "alpha", l.Agent)
			require.Equal(t, "alpha work one", l.Subject)
		}
	}
	require.Len(t, listed, 3, "the merge and alpha's two commits: %+v", fe.LeaveOut)
	require.Contains(t, listed, side[0])
	require.Contains(t, listed, side[1])
	require.Equal(t, tip, localTip(t, f.mri, f.mri.AgentBranch()), "the refusal changed nothing")

	// The fallback needs the confirmed list: without it, or with a stale one,
	// nothing changes.
	_, err = f.master.LeaveOutFleetMerge(ctx, named, nil)
	status, code = fleetCode(t, err)
	require.Equal(t, 422, status)
	require.Equal(t, "leave_out_unconfirmed", code)
	_, err = f.master.LeaveOutFleetMerge(ctx, named, listed[:1])
	status, code = fleetCode(t, err)
	require.Equal(t, 409, status)
	require.Equal(t, "leave_out_changed", code)
	require.Equal(t, tip, localTip(t, f.mri, f.mri.AgentBranch()))

	out, err := f.master.LeaveOutFleetMerge(ctx, named, listed)
	require.NoError(t, err)
	require.True(t, out.Published, out.PublishError)
	require.Len(t, out.LeftOut, 3)
	require.NotEqual(t, mainBefore, f.rev(t, "main"), "main moved")
	require.NotEmpty(t, f.mainLeafSerial(t, "beta"), "beta's enrollment, held behind the merge, is on main")
	require.False(t, f.onForgeMain(t, "kb/members/alpha/two.md"))
	require.False(t, f.onForgeMain(t, "kb/members/beta/one.md"))
	f.masterRounds(t)
	st, err = f.master.FleetStatus(ctx)
	require.NoError(t, err)
	require.False(t, st.LastErrorMainHeld, st.LastError)
}
