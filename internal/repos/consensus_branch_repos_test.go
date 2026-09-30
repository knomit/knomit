package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/platform/fileuri"
	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

// The probe answers what the create would adopt, by the store's own rule: a
// remote holding main AND trunk with HEAD on trunk is trunk.
//
// SABOTAGE: restore the "prefer main" loop in probe.go resolveUpstream → the
// probe answers main → red.
func TestProbeOrigin_HeadNotMainByName(t *testing.T) {
	root := t.TempDir()
	m := newProbeTestManager(t, root)
	bare := filepath.Join(root, "remote.git")
	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	for _, b := range []string{"main", "trunk"} {
		runGit(t, work, "checkout", "-B", b)
		require.NoError(t, os.WriteFile(filepath.Join(work, "seed.txt"), []byte(b), 0o644))
		runGit(t, work, "add", "seed.txt")
		runGit(t, work, "commit", "-m", "seed "+b)
		runGit(t, work, "push", "origin", b)
	}
	runGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/trunk")

	got, err := m.ProbeOrigin(context.Background(), OriginSpec{URL: fileuri.New(bare)})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"main", "trunk"}, got.Branches)
	require.Equal(t, "trunk", got.UpstreamBranch)
}

func TestProbeResolveUpstream(t *testing.T) {
	require.Equal(t, "trunk", resolveUpstream("", "trunk", []string{"main", "trunk"}))
	require.Equal(t, "", resolveUpstream("", "agent/x", []string{"agent/x", "main", "trunk"}), "undecidable: the create would refuse")
	require.Equal(t, "develop", resolveUpstream("develop", "", nil), "an empty remote seeds the requested branch")
	require.Equal(t, store.DefaultConsensusBranch, resolveUpstream("", "", nil), "and only an unnamed one gets the named default")
}

// An origin row never gets a branch nobody chose.
//
// SABOTAGE: restore `org.Branch = "main"` (Set) or `branch = "main"`
// (SetBranch) → the write succeeds with main → red.
func TestOrigins_RefuseAnEmptyBranch(t *testing.T) {
	r, o := openTestOrigins(t, testCrypt(t))
	require.NoError(t, r.Insert(RepoRecord{UID: "u1", Name: "alpha", State: StateActive, Profile: "code", CreatedAt: 1}))

	require.ErrorIs(t, o.Set("u1", Origin{URL: "https://example.test/kb.git"}), store.ErrNoConsensusBranch)
	got, err := o.Get("u1")
	require.NoError(t, err)
	require.Nil(t, got, "nothing is stored for a refused write")

	require.NoError(t, o.Set("u1", Origin{URL: "https://example.test/kb.git", Branch: "trunk"}))
	require.ErrorIs(t, o.SetBranch("u1", ""), store.ErrNoConsensusBranch)
	got, err = o.Get("u1")
	require.NoError(t, err)
	require.Equal(t, "trunk", got.Branch, "the stored branch is unchanged")
}

// seedFleetRemoteOn is seedFleetRemote on a branch other than main.
func seedFleetRemoteOn(t *testing.T, bare, branch string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(bare, 0o755))
	runGit(t, "", "init", "--bare", "--initial-branch="+branch, bare)
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	runGit(t, work, "checkout", "-B", branch)
	ont, err := fact.FleetOntology().Serialize()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(OntologyPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, OntologyPath), ont, 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "fleet")
	runGit(t, work, "push", "origin", branch)
	runGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/"+branch)
	return fileuri.New(bare)
}

// A fleet repository on trunk whose origin is then removed still reads its
// member records (and this instance's own keys, E4) at trunk: the recorded
// consensus branch, not a branch called main.
//
// SABOTAGE: restore `upstream := "main"` in fleetMembersAtMain → FleetMembers
// errors/reads nothing → red. In ownFleetKeys → no keys → red.
func TestFleet_TrunkWithoutOriginReadsTheRecordedBranch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemoteOn(t, bare, "trunk")
	m := newFleetManager(t, dir)
	_, err := m.RegisterFleet(ctx, url, "", "")
	require.NoError(t, err)
	agent := m.fleetRepo().AgentBranch()

	// The human accepts: merge the agent branch into trunk on the remote.
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	runGit(t, work, "checkout", "trunk")
	runGit(t, work, "-c", "user.email=h@h", "-c", "user.name=h", "merge", "--no-ff", "-m", "accept", "origin/"+agent)
	runGit(t, work, "push", "origin", "trunk")
	syncFleet(t, m)

	// Remove the origin, the way DeleteOrigin does to the two stores.
	ri := m.fleetRepo()
	require.NoError(t, m.Origins().Delete(ri.UID()))
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		svc.SetOrigin(nil)
		require.Equal(t, "trunk", svc.UpstreamBranch())
	}))

	members, err := m.FleetMembers(ctx)
	require.NoError(t, err)
	require.Len(t, members, 1)
	keys := m.ownFleetKeys()
	require.NotEmpty(t, keys, "E4 reads this instance's keys at trunk")
	require.True(t, fact.SameKey(keys[0], testsigner.Named("fleet-test").PublicKey()))
}
