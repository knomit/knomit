package repos

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"knomit/internal/corpus/repos/control"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/config"
	"knomit/internal/foundation/pki"
	"knomit/internal/foundation/pki/pkitest"
	"knomit/internal/platform/fileuri"
	"knomit/internal/testsupport/gitfixture"
	"knomit/internal/testsupport/testsigner"
)

// seedFleetRemote makes a bare fleet repository: main holds the fleet
// ontology preset and nothing else.
func seedFleetRemote(t *testing.T, bare string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(bare, 0o755))
	gitfixture.InitBare(t, bare, "main")
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	ont, err := fact.FleetOntology().Serialize()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(OntologyPath)), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, OntologyPath), ont, 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "fleet")
	runGit(t, work, "push", "origin", "main")
	runGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return fileuri.New(bare)
}

// seedJoinableFleet is seedFleetRemote plus a fresh root's root.crt and CRL
// on main, what a forge create leaves: a fleet a bundle of that root joins.
// The bare repository logs every ref update (pushCount).
func seedJoinableFleet(t *testing.T, bare string) (string, *pkitest.Fleet) {
	t.Helper()
	url := seedFleetRemote(t, bare)
	root := pkitest.New(t)
	files := map[string][]byte{}
	for _, name := range []string{pki.RootCertFile, pki.CRLFile} {
		b, err := os.ReadFile(filepath.Join(root.Dir, name))
		require.NoError(t, err)
		files["artifacts/fleet/"+name] = b
	}
	pushToFleetRemote(t, bare, files)
	runGit(t, bare, "config", "core.logAllRefUpdates", "always")
	return url, root
}

// pushCount is how many times ref was updated on the bare repository since
// seedJoinableFleet turned its ref log on: every push that moved it.
func pushCount(t *testing.T, bare, ref string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bare, "logs", "refs", "heads", filepath.FromSlash(ref)))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	require.NoError(t, err)
	return strings.Count(string(raw), "\n")
}

// joinTestFleet joins m to the fleet at url with a bundle root issues for
// m's key and agent id (POST /fleet/join).
func joinTestFleet(t *testing.T, m *Manager, url string, root *pkitest.Fleet) (FleetStatus, error) {
	t.Helper()
	mem := root.Enroll(t, m.AgentID()+"@"+pki.AgentEmailDomain, m.deps.KeyPath)
	j, err := m.JoinFleet(context.Background(), FleetJoinSpec{Bundle: pkitest.WithURL(t, root.Bundle(t, mem), url)})
	return j, err
}

// newFleetManager is a started manager that can sign (its member record is a
// commit) with the key in its key file (a join's bundle is checked against
// it), holds its trust material in <home>/pki, and may use local origins
// under dir.
func newFleetManager(t *testing.T, dir string) *Manager {
	t.Helper()
	home := filepath.Join(dir, "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	key, err := testsigner.WriteNamedKey(home, "fleet-test")
	require.NoError(t, err)
	m := New(context.Background(), Deps{
		Cfg: config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: dir,
			TLS: config.TLSConfig{Dir: filepath.Join(home, "pki")}},
		AgentID: testsigner.FixtureAgentID("agent/test-fleet"), AgentBranch: "agent/test-fleet",
		Signer:  testsigner.Named("fleet-test"),
		KeyPath: key,
		Machine: Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { m.Close() })
	return m
}

func fleetCode(t *testing.T, err error) (int, string) {
	t.Helper()
	var fe *FleetError
	require.True(t, errors.As(err, &fe), "want a FleetError, got %v", err)
	return fe.Status, fe.Code
}

// remoteRecord reads this instance's member record from the bare fleet
// remote at rev (an agent branch or main); "" when absent.
func remoteRecord(t *testing.T, bare, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "show", rev+":"+memberRecordPath("kb", "test-fleet")).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// syncFleet runs one fetch+reconcile of the mounted fleet repository, as the
// (disabled) background loop would.
func syncFleet(t *testing.T, m *Manager) {
	t.Helper()
	ri := m.fleetRepo()
	require.NotNil(t, ri)
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		remote, err := svc.Remote().GetRemote("origin")
		require.NoError(t, err)
		auth, err := makeRemoteAuthFn(m.deps.Cfg.Remote, m.deps.KeyPath)(remote)
		require.NoError(t, err)
		_, err = svc.Remote().Sync(context.Background(), ri.AgentBranch(), auth)
		require.NoError(t, err)
	}))
}

// mergeOnRemote stands in for the master accepting: merge the instance's
// agent branch into the fleet's main on the remote.
func mergeOnRemote(t *testing.T, bare, agentBranch string) {
	t.Helper()
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	runGit(t, work, "-c", "user.email=h@h", "-c", "user.name=h", "merge", "--no-ff", "-m", "accept", "origin/"+agentBranch)
	runGit(t, work, "push", "origin", "main")
}

// requireNothingJoined: m holds no fleet in any form — no repository, no
// recorded uid, no installed trust material, state standalone.
func requireNothingJoined(t *testing.T, m *Manager, why string) {
	t.Helper()
	require.Nil(t, m.fleetRepo(), why+": no fleet repository is mounted")
	require.Empty(t, m.Names(), why+": no repository at all")
	uid, err := recordedFleetRepo(m.ControlDB())
	require.NoError(t, err)
	require.Empty(t, uid, why+": no fleet repository is recorded")
	for _, f := range []string{pki.InstanceCertFile, pki.RootCertFile, pki.CRLFile} {
		_, err := os.Lstat(filepath.Join(m.deps.Cfg.TLS.Dir, f))
		require.ErrorIs(t, err, os.ErrNotExist, why+": %s is not installed", f)
	}
	row, err := m.fleetDB()
	require.NoError(t, err)
	require.Equal(t, FleetStandalone, row.State, why)
}

// Join: the bundle is installed, the clone mounted and recorded, the record
// written and pushed ONCE; state registered at once (the push landed), the
// record pending until the master merges it into main; after a sync it is
// active and carries this instance's CURRENT key.
// SABOTAGE: drop the InstallBundle call in JoinFleet -> instance.crt is
// absent -> red; drop recordFleetRepo -> the recorded uid is empty -> red.
func TestFleet_JoinPushesTheRecordOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url, root := seedJoinableFleet(t, bare)
	m := newFleetManager(t, dir)

	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, FleetStandalone, st.State)
	require.Equal(t, "test-fleet", st.AgentID)
	require.Empty(t, st.Hosting, "no fleet repository, no hosting")
	require.Equal(t, filepath.Join(m.deps.Cfg.Home, "fleet"), st.RootDir)

	st, err = joinTestFleet(t, m, url, root)
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State, "the join's push landed")
	require.Equal(t, "pending", st.RecordState, "not merged into the fleet's main yet")
	require.Empty(t, st.LastError)
	require.NotNil(t, st.RegisteredAt)
	require.False(t, st.Master)
	require.Equal(t, url, st.Hosting, "a member's copy syncs with the forge")
	agent := m.fleetRepo().AgentBranch()
	// The mount's first sync round pushes the bare agent branch; the join
	// pushes once more, with the record.
	require.Equal(t, 2, pushCount(t, bare, agent), "the mount's round, then exactly one join push")
	rec := remoteRecord(t, bare, agent)
	require.Contains(t, rec, "state: active")
	require.Contains(t, rec, "agent: test-fleet")
	uid, err := recordedFleetRepo(m.ControlDB())
	require.NoError(t, err)
	require.Equal(t, m.fleetRepo().UID(), uid, "recorded: the fleet mounts again after a restart")
	installed, err := pki.InstalledRoot(m.deps.Cfg.TLS.Dir)
	require.NoError(t, err)
	wantRoot, err := pki.RootID(root.Root.Cert)
	require.NoError(t, err)
	require.Equal(t, wantRoot, installed.Fingerprint, "the bundle is installed")
	_, err = os.Stat(filepath.Join(m.deps.Cfg.TLS.Dir, pki.InstanceCertFile))
	require.NoError(t, err)

	mergeOnRemote(t, bare, agent)
	syncFleet(t, m)
	st, err = m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, fact.MemberActive, st.RecordState)
	members, err := m.FleetMembers(ctx)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.True(t, fact.SameKey(members[0].Key, testsigner.Named("fleet-test").PublicKey()), "the record carries this instance's CURRENT key")
}

// Leave: `left` on the agent branch, ONE push, then the fleet is removed here:
// the repository, its read mount in a lens (the lens is kept), the installed
// material; state standalone.
// SABOTAGE: drop dropFromLenses from removeFleetHere -> the archive refuses
// the lens member (ErrRepoInUseByLens) -> red; drop pki.Uninstall -> red.
func TestFleet_LeavePushesOnceThenRemovesEverything(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url, root := seedJoinableFleet(t, bare)
	m := newFleetManager(t, dir)
	_, err := joinTestFleet(t, m, url, root)
	require.NoError(t, err)
	fl := m.fleetRepo()
	agent, flUID := fl.AgentBranch(), fl.UID()
	kb := createRepo(t, m, "kb")
	_, err = m.CreateLens(ctx, control.Lens{Name: "reads-fleet", WriteUID: kb.UID(), Reads: []control.LensRead{{RepoUID: flUID}}})
	require.NoError(t, err)
	before := pushCount(t, bare, agent)

	out, err := m.LeaveFleet(ctx)
	require.NoError(t, err)
	require.True(t, out.Pushed)
	require.Empty(t, out.PushError)
	require.Equal(t, before+1, pushCount(t, bare, agent), "exactly one push")
	require.Contains(t, remoteRecord(t, bare, agent), "state: left", "the departure reached the fleet's origin")
	require.Equal(t, FleetStandalone, out.State)
	require.Nil(t, m.fleetRepo())
	require.Equal(t, []string{"kb"}, m.Names(), "only the fleet repository is removed")
	l, ok, err := m.LensRegistry().Get("reads-fleet")
	require.NoError(t, err)
	require.True(t, ok, "the lens stays")
	for _, r := range l.Reads {
		require.NotEqual(t, flUID, r.RepoUID, "the fleet repository is dropped from the lens")
	}
	require.Len(t, l.Reads, 1, "the lens reads its write repo only")
	for _, f := range []string{pki.InstanceCertFile, pki.RootCertFile, pki.CRLFile} {
		_, err := os.Lstat(filepath.Join(m.deps.Cfg.TLS.Dir, f))
		require.ErrorIs(t, err, os.ErrNotExist, "%s is uninstalled", f)
	}

	_, err = m.LeaveFleet(ctx)
	status, code := fleetCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "not_registered", code)
}

// Leave works offline: the origin is gone, the push fails (reported), and
// the fleet is removed here all the same.
func TestFleet_LeaveWorksOffline(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url, root := seedJoinableFleet(t, bare)
	m := newFleetManager(t, dir)
	_, err := joinTestFleet(t, m, url, root)
	require.NoError(t, err)
	require.NoError(t, os.Rename(bare, bare+".away"))

	out, err := m.LeaveFleet(ctx)
	require.NoError(t, err)
	require.False(t, out.Pushed)
	require.NotEmpty(t, out.PushError, "the failed push is reported")
	requireNothingJoined(t, m, "after an offline leave")
}

// A refused join leaves nothing behind, whatever refused it: no repository,
// no recorded uid, no installed material, state standalone, and no agent
// branch on the forge. Then a good join, and a second join is fleet_present.
// SABOTAGE: make refuse() skip undoJoin -> the not_a_fleet row keeps its
// mount and instance.crt -> red.
func TestFleet_RefusedJoinLeavesNothingBehind(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url, root := seedJoinableFleet(t, bare)
	otherBare := filepath.Join(dir, "other.git")
	other, _ := seedJoinableFleet(t, otherBare)
	kb := seedBareRemoteWithOntology(t, filepath.Join(dir, "kb.git"), true)
	m := newFleetManager(t, dir)
	self := func() []byte {
		return root.Bundle(t, root.Enroll(t, "test-fleet@"+pki.AgentEmailDomain, m.deps.KeyPath))
	}

	rejecting := filepath.Join(dir, "rejecting.git")
	rejURL, rejRoot := seedJoinableFleet(t, rejecting)
	hook := filepath.Join(rejecting, "hooks", "pre-receive")
	require.NoError(t, os.WriteFile(hook, []byte("#!/bin/sh\necho 'no-cert: the leaf is not on main yet' >&2\nexit 1\n"), 0o755))

	for _, tc := range []struct {
		name   string
		url    string
		bundle []byte
		status int
		code   string
		forge  string
	}{
		{"not a fleet", kb, self(), http.StatusUnprocessableEntity, "not_a_fleet", ""},
		{"no repository", fileuri.New(filepath.Join(dir, "missing.git")), self(), http.StatusBadGateway, "clone_failed", ""},
		{"another fleet's root", other, self(), http.StatusUnprocessableEntity, "root_differs", otherBare},
		{"another key", url, root.Bundle(t, root.Enroll(t, "test-fleet@"+pki.AgentEmailDomain)), http.StatusUnprocessableEntity, "key_mismatch", bare},
		{"another agent", url, root.Bundle(t, root.Enroll(t, "someone@"+pki.AgentEmailDomain, m.deps.KeyPath)), http.StatusUnprocessableEntity, "agent_mismatch", bare},
		{"push refused", rejURL, rejRoot.Bundle(t, rejRoot.Enroll(t, "test-fleet@"+pki.AgentEmailDomain, m.deps.KeyPath)), http.StatusConflict, "join_refused", rejecting},
	} {
		_, err := m.JoinFleet(ctx, FleetJoinSpec{Bundle: pkitest.WithURL(t, tc.bundle, tc.url)})
		status, code := fleetCode(t, err)
		require.Equal(t, tc.status, status, tc.name)
		require.Equal(t, tc.code, code, tc.name)
		requireNothingJoined(t, m, tc.name)
		if tc.forge != "" {
			require.Empty(t, gitRevIn(t, tc.forge, "agent/test-fleet"), tc.name+": no agent branch on the forge")
		}
	}
	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, st.LastError, "a refused join is never silent")

	_, err = joinTestFleet(t, m, url, root)
	require.NoError(t, err)
	_, err = joinTestFleet(t, m, url, root)
	status, code := fleetCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "fleet_present", code)
	_, err = joinTestFleet(t, m, other, root)
	_, code = fleetCode(t, err)
	require.Equal(t, "fleet_present", code)
	require.True(t, strings.HasPrefix(m.fleetRepo().Name(), "fleet"))
}

// gitRevIn is the commit at refs/heads/ref in the git repository at dir, ""
// when absent.
func gitRevIn(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "-q", "refs/heads/"+ref).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func fixedNow() time.Time { return time.Unix(1790000000, 0) }

// A failed push keeps the state registering with last_error set (a forge
// create whose publish of main failed); the next successful push (the sync
// loop's retry) completes it to registered and clears the error. Pushes of
// other repositories change nothing.
func TestFleet_RegisteringRetriedOnPush(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	url, root := seedJoinableFleet(t, filepath.Join(dir, "fleet.git"))
	m := newFleetManager(t, dir)
	_, err := joinTestFleet(t, m, url, root)
	require.NoError(t, err)
	name := m.fleetRepo().Name()
	require.NoError(t, setFleetState(m.ControlDB(), FleetRegistering, fixedNow()))

	m.fleetPushed("some-other-repo", nil)
	st, _ := m.FleetStatus(ctx)
	require.Equal(t, FleetRegistering, st.State, "another repo's push is not the fleet's")

	m.fleetPushed(name, errors.New("dial tcp: connection refused"))
	st, _ = m.FleetStatus(ctx)
	require.Equal(t, FleetRegistering, st.State)
	require.Equal(t, "dial tcp: connection refused", st.LastError)

	m.fleetPushed(name, nil)
	st, _ = m.FleetStatus(ctx)
	require.Equal(t, FleetRegistered, st.State)
	require.Empty(t, st.LastError)
}

// POST /fleet/join:check on a forge-hosted fleet: the bundle's URL is not an
// instance of the bundle's fleet, so hosting forge, the bundle's root
// fingerprint, nothing installed; a bundle for another key is key_mismatch
// before anything is reached.
func TestCheckFleetJoin_Forge(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	url, root := seedJoinableFleet(t, filepath.Join(dir, "fleet.git"))
	m := newFleetManager(t, dir)
	bundle := pkitest.WithURL(t, root.Bundle(t, root.Enroll(t, "test-fleet@"+pki.AgentEmailDomain, m.deps.KeyPath)), url)
	chk, err := m.CheckFleetJoin(ctx, FleetJoinSpec{Bundle: bundle})
	require.NoError(t, err)
	rootID, err := pki.RootID(root.Root.Cert)
	require.NoError(t, err)
	require.Equal(t, FleetJoinCheck{URL: url, Hosting: "forge", RootFingerprint: rootID, AgentID: "test-fleet"}, chk)
	requireNothingJoined(t, m, "check")

	// cert is never an input: the forge probe vets the caller's method.
	_, err = m.CheckFleetJoin(ctx, FleetJoinSpec{Bundle: bundle, AuthMethod: "cert"})
	status, code := fleetCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "auth_invalid", code)
	require.ErrorContains(t, err, ErrCertNotAnInput.Error())
	requireNothingJoined(t, m, "cert check")

	other := pkitest.WithURL(t, root.Bundle(t, root.Enroll(t, "test-fleet@"+pki.AgentEmailDomain)), url)
	_, err = m.CheckFleetJoin(ctx, FleetJoinSpec{Bundle: other})
	_, code = fleetCode(t, err)
	require.Equal(t, "key_mismatch", code)
	requireNothingJoined(t, m, "key_mismatch check")
}
