package repos

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
	"knomit/internal/store/storetest"
)

// #532's leave duty under the judges (both on, as in production): on a
// forge-hosted fleet alpha joins, leaves with a `left` record naming its leaf
// (leaf_serial), the master revokes that leaf with no call, the master
// enrolls alpha again and alpha rejoins. Every write passes the judges and
// the writers' validation:
//   - alpha's join and its left record (members-are-policy, leaf_serial in
//     the body) pass the fetch judge: no refusal is recorded for agent/alpha;
//   - the master's automatic revoke (its CRL and revocation statement), its
//     merges of alpha's branch, and its re-enroll (a leaf that REPLACES the
//     revoked one: leaf-renewal) pass the judge of its own commits: main
//     moves every time and GET /fleet holds no refusal;
//   - a merge of a branch forked before a leaf the master later revoked is
//     not refused for that leaf (artifact-pki judges a pki file a merge adds,
//     not one another parent already holds);
//
// and the judge is live in this very fleet: a commit on the master's branch
// that fails members-are-policy holds main back.
// SABOTAGE: drop judgeOwnRange from PublishUpstream -> the last step's
// invalid commit reaches main -> red. (The inAnotherParent skip is pinned by
// store TestArtifacts_AMergeBringingALeafAParentHoldsAddsNothing and by
// TestFleetLeave_TheDutyWaitsForAnUnpublishedCRLAndChainsEachRevocation,
// whose delta merge meets it.)
func TestFleetJudgeOwn_LeaveAutoRevokeAndRejoinPassTheJudge(t *testing.T) {
	ctx := context.Background()
	f := newForgeFleet(t, nil)
	installMasterLeaf(t, f)
	alpha := f.namedMember(t, "alpha")
	clean := func(step string) {
		t.Helper()
		st, err := f.master.FleetStatus(ctx)
		require.NoError(t, err)
		require.Empty(t, st.LastError, "%s: the master's last error", step)
		require.False(t, st.LastErrorMainHeld, step)
		require.Empty(t, f.mri.peers.Refusals()["agent/alpha"].Reason, "%s: the fetch judge refused alpha", step)
		require.Equal(t, localTip(t, f.mri, f.mri.AgentBranch()), f.rev(t, "main"), "%s: the forge's main is the master's branch", step)
	}

	require.Equal(t, RecordPending, f.enrollAndJoin(t, alpha, "alpha").RecordState)
	f.masterRounds(t)
	clean("join")
	require.Equal(t, fact.MemberActive, memberState(t, f.master, "alpha"))

	serial := f.mainLeafSerial(t, "alpha")
	_, err := alpha.LeaveFleet(ctx)
	require.NoError(t, err)
	f.masterRounds(t)
	clean("leave and the automatic revoke")
	rec, err := runGitOut(f.bare, "show", "main:"+memberRecordPath("kb", "alpha"))
	require.NoError(t, err)
	require.Contains(t, rec, "state: left")
	require.Contains(t, rec, "leaf_serial: "+serial+"\n", "the left record with leaf_serial passed validation and the judge")
	require.Equal(t, int64(2), f.mainCRLNumber(t), "the automatic revoke is on main")
	author, err := runGitOut(f.bare, "log", "-1", "--format=%ae", "main", "--", "artifacts/fleet/crl.pem")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(strings.TrimSpace(author), "master+"), "the revoke is the master's own commit: %q", author)
	require.Equal(t, fact.MemberRevoked, memberState(t, f.master, "alpha"))

	require.Equal(t, RecordPending, f.enrollAndJoin(t, alpha, "alpha").RecordState, "the re-enroll was published: main moved")
	f.masterRounds(t)
	clean("rejoin")
	require.Equal(t, fact.MemberActive, memberState(t, f.master, "alpha"), "the rejoin is on main")
	require.NotEqual(t, serial, f.mainLeafSerial(t, "alpha"), "a fresh leaf replaced the revoked one")

	// The judge is live here: an invalid commit on the master's branch holds
	// main back.
	mainBefore := f.rev(t, "main")
	require.NoError(t, f.mri.WithRead(func(svc *store.Service) {
		_, err := storetest.CommitFiles(svc, f.mri.AgentBranch(), map[string]string{"kb/members/master/note.md": notPolicy}, "a bad note")
		require.NoError(t, err)
	}))
	roundOnPush(t, f.mri, f.master.fleetPushed)
	require.Equal(t, mainBefore, f.rev(t, "main"))
	st, err := f.master.FleetStatus(ctx)
	require.NoError(t, err)
	require.True(t, st.LastErrorMainHeld, st.LastError)
}
