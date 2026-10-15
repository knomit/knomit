package repos

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/platform/fileuri"
	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

// fleetHostDeps are the Deps of an instance that can found a fleet: it signs
// with the test signer, whose private key is on disk at KeyPath (the bundle
// install checks the leaf against it), has the mTLS listener and an
// external address configured, and may use local origins under dir.
func fleetHostDeps(t *testing.T, dir string) Deps {
	t.Helper()
	home := filepath.Join(dir, "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	keyPath, err := testsigner.WriteFallbackKey(home)
	require.NoError(t, err)
	return Deps{
		Cfg: config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: dir,
			TLS:               config.TLSConfig{Dir: filepath.Join(home, "pki"), Addr: "127.0.0.1:0"},
			ExternalAddresses: []string{"https://fleet-host.example:19279"}},
		AgentBranch: "agent/test-fleet",
		Signer:      testsigner.Signer(),
		KeyPath:     keyPath,
		Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	}
}

func startFleetHost(t *testing.T, deps Deps) *Manager {
	t.Helper()
	m := New(context.Background(), deps)
	require.NoError(t, m.Start())
	t.Cleanup(func() { m.Close() })
	return m
}

// newFleetHost is a started manager on a fresh dir; mod adjusts its Deps.
func newFleetHost(t *testing.T, dir string, mod func(*Deps)) *Manager {
	t.Helper()
	deps := fleetHostDeps(t, dir)
	if mod != nil {
		mod(&deps)
	}
	return startFleetHost(t, deps)
}

// forgeFleetRemote is a bare repository with one commit on main (README), as
// a forge's new repository is; mode initialize cuts the agent branch from it.
func forgeFleetRemote(t *testing.T, dir string) string {
	t.Helper()
	bare := filepath.Join(dir, "fleet.git")
	require.NoError(t, os.MkdirAll(bare, 0o755))
	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "README.md"), []byte("fleet\n"), 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "-c", "user.email=h@h", "-c", "user.name=h", "commit", "-m", "init")
	runGit(t, work, "push", "origin", "main")
	return bare
}

type histCommit struct {
	hash  string
	paths []string
}

// historyOf is rev's first-parent history in the plain repository at git,
// OLDEST first: each commit's hash and the paths it changed.
func historyOf(t *testing.T, git, rev string) []histCommit {
	t.Helper()
	out, err := exec.Command("git", "-C", git, "log", "--reverse", "--first-parent", "--format=@%H", "--name-only", rev).Output()
	require.NoError(t, err)
	var hs []histCommit
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "@"):
			hs = append(hs, histCommit{hash: line[1:]})
		default:
			hs[len(hs)-1].paths = append(hs[len(hs)-1].paths, line)
		}
	}
	return hs
}

func gitRev(t *testing.T, git, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", git, "rev-parse", rev).Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// issuedSerials reads the fleet root directory's local issuance log.
func issuedSerials(t *testing.T, home string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, pki.FleetRootDirName, pki.IssuedLogFile))
	require.NoError(t, err)
	var serials []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		i := strings.Index(line, `"serial":"`)
		require.GreaterOrEqual(t, i, 0, line)
		rest := line[i+len(`"serial":"`):]
		serials = append(serials, rest[:strings.IndexByte(rest, '"')])
	}
	return serials
}

// requireNoFleetCreated: a refused or rolled-back create left nothing — no
// fleet repository mounted, registered or recorded, no fleet root directory,
// no trust material in [tls].dir, the state standalone.
func requireNoFleetCreated(t *testing.T, m *Manager) {
	t.Helper()
	require.Nil(t, m.fleetRepo())
	recs, err := m.Repos().List(StateActive)
	require.NoError(t, err)
	for _, r := range recs {
		require.NotEqual(t, "fleet", r.Name, "the fleet repository is not registered")
	}
	uid, err := recordedFleetRepo(m.ControlDB())
	require.NoError(t, err)
	require.Empty(t, uid, "no fleet repository is recorded")
	_, err = os.Stat(m.fleetRootDir())
	require.ErrorIs(t, err, os.ErrNotExist, "no fleet root directory")
	for _, f := range []string{pki.RootCertFile, pki.CRLFile, pki.InstanceCertFile} {
		_, err := os.Stat(filepath.Join(m.deps.Cfg.TLS.Dir, f))
		require.ErrorIs(t, err, os.ErrNotExist, "no %s installed", f)
	}
	st, err := m.FleetStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, FleetStandalone, st.State)
}

// A fleet hosted here (no origin): a fresh root is minted into <home>/fleet
// (root.key a plain owner-only file), the repository holds root, leaf and
// record on main at once, the bundle is installed, and this instance is the
// master. Sabotage: drop the AdvanceLocalUpstream call -> main has no root
// and the record is pending -> red.
func TestCreateFleet_HostedHere(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)

	st, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State)
	require.Equal(t, "fleet", st.FleetRepo)
	require.Empty(t, st.FleetURL, "hosted here: no origin")
	require.Equal(t, fact.MemberActive, st.RecordState, "main holds the active record at once")

	dir := m.fleetRootDir()
	ki, err := os.Stat(filepath.Join(dir, pki.RootKeyFile))
	require.NoError(t, err, "the root key is a plain file")
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o600), ki.Mode().Perm())
	}
	root, err := pki.LoadRoot(dir)
	require.NoError(t, err, "the key opens without input")
	require.Len(t, issuedSerials(t, m.deps.Cfg.Home), 1)

	installed, err := pki.InstalledRoot(m.deps.Cfg.TLS.Dir)
	require.NoError(t, err)
	rootID, err := pki.RootID(root.Cert)
	require.NoError(t, err)
	require.Equal(t, rootID, installed.Fingerprint, "the bundle is this instance's TLS identity")

	f, err := m.FleetAtMain(ctx)
	require.NoError(t, err)
	require.NotNil(t, f.PKI)
	require.True(t, root.Cert.Equal(f.PKI.Root), "main's root.crt is the minted root")
	ri := m.fleetRepo()
	require.True(t, m.isFleetMaster(testService(t, ri)), "the creator is the master")
	uid, err := recordedFleetRepo(m.ControlDB())
	require.NoError(t, err)
	require.Equal(t, ri.UID(), uid, "recorded as this instance's fleet repository")
}

// The created fleet mounts again after a restart: CreateFleet records it in
// instance_fleet_repo, the one uid boot admits through the fleet door.
// Sabotage: drop the recordFleetRepo call -> after the restart the repo is
// unavailable with cause fleet_conflict -> red.
func TestCreateFleet_MountedAfterRestart(t *testing.T) {
	ctx := context.Background()
	deps := fleetHostDeps(t, t.TempDir())
	m := startFleetHost(t, deps)
	_, err := m.CreateFleet(ctx, FleetCreateSpec{Name: "crew"})
	require.NoError(t, err)
	uid := m.fleetRepo().UID()

	require.NoError(t, m.Close())
	m = startFleetHost(t, deps)
	ri := m.fleetRepo()
	require.NotNil(t, ri, "the fleet repository is mounted after the restart; unavailable: %+v", m.Unavailable())
	require.Equal(t, "crew", ri.Name())
	require.Equal(t, uid, ri.UID())
	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State)
	require.Equal(t, fact.MemberActive, st.RecordState)
}

// A forge-hosted fleet: the agent branch carries the root-first sequence and
// is pushed, then main on the forge is set to its tip — 201 means main holds
// the record — and the gate admits every create commit under enforce.
// Sabotage: skip the PublishUpstream call -> the forge's main is still the
// README commit -> red; write the record before the root -> the history
// rows are out of order -> red.
func TestCreateFleet_ForgeRootFirstThenMainPublished(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := forgeFleetRemote(t, dir)
	m := newFleetHost(t, dir, nil)

	st, err := m.CreateFleet(ctx, FleetCreateSpec{Origin: &OriginSpec{URL: fileuri.New(bare)}})
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State, "main on the forge holds the record")
	require.Empty(t, st.LastError)
	require.Equal(t, fact.MemberActive, st.RecordState, "local main followed the published main")

	agent := "refs/heads/agent/test-fleet"
	require.Equal(t, gitRev(t, bare, agent), gitRev(t, bare, "refs/heads/main"), "the forge's main is the agent tip")
	hs := historyOf(t, bare, "refs/heads/main")
	require.Len(t, hs, 5, "the forge's README commit, then the create's four: %+v", hs)
	require.Equal(t, []string{"README.md"}, hs[0].paths)
	require.Equal(t, []string{OntologyPath}, hs[1].paths, "the ontology commit")
	require.ElementsMatch(t, []string{"artifacts/fleet/crl.pem", "artifacts/fleet/root.crt"}, hs[2].paths,
		"ROOT FIRST: the root and the CRL, and nothing else")
	serial := issuedSerials(t, m.deps.Cfg.Home)[0]
	require.ElementsMatch(t, []string{"artifacts/fleet/certs/test-fleet.pem", "artifacts/fleet/issued/" + serial + ".pem"}, hs[3].paths,
		"then the creator's leaf and its one issuance record")
	require.Equal(t, []string{memberRecordPath("kb", "test-fleet")}, hs[4].paths, "then the record, last")

	v, err := store.CheckRange(store.RangeInput{
		KBDir: bare, Main: hs[1].hash, Candidate: "refs/heads/main",
		FleetDir: bare, FleetRev: "refs/heads/main",
	})
	require.NoError(t, err)
	require.Equal(t, store.VerifyEnforce, v.Mode, "the fleet ontology declares enforce")
	require.Equal(t, 3, v.Checked, "root, leaf and record commits")
	require.Empty(t, v.Refused)
}

// The master's write of main is never forced: a main on the forge that the
// agent tip does not contain is refused by the forge as an ordinary push
// error, and the forge's main is unchanged.
// Sabotage: spell PublishUpstream's refspec with a leading '+' -> the push
// overwrites the forge's main -> red.
func TestCreateFleet_MainIsNeverForced(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := forgeFleetRemote(t, dir)
	m := newFleetHost(t, dir, nil)
	_, err := m.CreateFleet(ctx, FleetCreateSpec{Origin: &OriginSpec{URL: fileuri.New(bare)}})
	require.NoError(t, err)

	// Someone writes the forge's main past the master.
	work := t.TempDir()
	runGit(t, "", "clone", "--branch", "main", bare, work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "stray.txt"), []byte("x\n"), 0o644))
	runGit(t, work, "add", "-A")
	runGit(t, work, "-c", "user.email=h@h", "-c", "user.name=h", "commit", "-m", "stray")
	runGit(t, work, "push", "origin", "main")
	stray := gitRev(t, bare, "refs/heads/main")

	ri := m.fleetRepo()
	require.NoError(t, m.writeOwnRecord(ctx, ri, fact.MemberActive), "a new agent tip")
	svc := testService(t, ri)
	remote, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	auth, err := makeRemoteAuthFn(m.deps.Cfg.Remote, m.deps.KeyPath)(remote)
	require.NoError(t, err)
	_, err = svc.Remote().PublishUpstream(ctx, ri.AgentBranch(), auth)
	require.Error(t, err, "a non-fast-forward main is refused")
	require.Contains(t, err.Error(), "non-fast-forward")
	require.Equal(t, stray, gitRev(t, bare, "refs/heads/main"), "the forge's main is unchanged")
}

// Every pre-flight refuses with its code, before anything is written.
// Sabotage: delete any one check -> its row creates a fleet (or fails later
// with another error) -> red.
func TestCreateFleet_PreflightRefusals(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		mod    func(*Deps)
		setup  func(t *testing.T, m *Manager)
		spec   FleetCreateSpec
		status int
		code   string
	}{
		{name: "no listener", mod: func(d *Deps) { d.Cfg.TLS.Addr = "" },
			status: http.StatusUnprocessableEntity, code: "listener_required"},
		{name: "no external addresses", mod: func(d *Deps) { d.Cfg.ExternalAddresses = nil },
			status: http.StatusUnprocessableEntity, code: "addresses_required"},
		{name: "knomit-hosted origin", spec: FleetCreateSpec{Origin: &OriginSpec{URL: "knomit+https://hub.example:19279/git/fleet"}},
			status: http.StatusUnprocessableEntity, code: "knomit_origin"},
		{name: "trust installed", setup: func(t *testing.T, m *Manager) {
			require.NoError(t, os.MkdirAll(m.deps.Cfg.TLS.Dir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(m.deps.Cfg.TLS.Dir, pki.CRLFile), []byte("x"), 0o600))
		}, status: http.StatusConflict, code: "trust_installed"},
		{name: "root present", setup: func(t *testing.T, m *Manager) {
			require.NoError(t, os.MkdirAll(m.fleetRootDir(), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(m.fleetRootDir(), pki.RootKeyFile), []byte("old"), 0o600))
		}, status: http.StatusConflict, code: "root_present"},
		{name: "recorded fleet not mounted", setup: func(t *testing.T, m *Manager) {
			createRepo(t, m, "other")
			require.NoError(t, recordFleetRepo(m.ControlDB(), m.Get("other").UID()))
		}, status: http.StatusConflict, code: "fleet_present"},
		{name: "state not standalone", setup: func(t *testing.T, m *Manager) {
			require.NoError(t, setFleetState(m.ControlDB(), FleetRegistering, time.Now()))
		}, status: http.StatusConflict, code: "fleet_present"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newFleetHost(t, t.TempDir(), tc.mod)
			var keep []byte
			if tc.setup != nil {
				tc.setup(t, m)
				keep, _ = os.ReadFile(filepath.Join(m.fleetRootDir(), pki.RootKeyFile))
			}
			_, err := m.CreateFleet(ctx, tc.spec)
			status, code := fleetCode(t, err)
			require.Equal(t, tc.status, status, err.Error())
			require.Equal(t, tc.code, code, err.Error())
			require.Nil(t, m.fleetRepo())
			require.Nil(t, m.Get("fleet"), "no repository was created")
			if tc.code == "root_present" {
				got, err := os.ReadFile(filepath.Join(m.fleetRootDir(), pki.RootKeyFile))
				require.NoError(t, err)
				require.Equal(t, keep, got, "the old root key is untouched")
				return
			}
			_, err = os.Stat(m.fleetRootDir())
			require.ErrorIs(t, err, os.ErrNotExist, "no root was minted")
		})
	}
}

// A second create on a fleet instance is fleet_present and changes nothing.
func TestCreateFleet_RepeatIsFleetPresent(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	_, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	orig := m.fleetRepo()

	_, err = m.CreateFleet(ctx, FleetCreateSpec{Name: "fleet-two"})
	_, code := fleetCode(t, err)
	require.Equal(t, "fleet_present", code)
	require.Same(t, orig, m.fleetRepo())
	require.Nil(t, m.Get("fleet-two"))
	require.Len(t, issuedSerials(t, m.deps.Cfg.Home), 1, "no second leaf was issued")
}

// A failure after the last step rolls back ALL of it: the repository and its
// record, <home>/fleet with the root key, the installed trust material, the
// fleet state. Sabotage: drop any one undo -> its leftover is found -> red.
func TestCreateFleet_FailureRollsBackEverything(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	reached := false
	afterFleetInstall = func() error {
		reached = true
		// The create really got this far: all of it exists right now.
		require.NotNil(t, m.Get("fleet"))
		_, err := os.Stat(filepath.Join(m.fleetRootDir(), pki.RootKeyFile))
		require.NoError(t, err)
		_, err = os.Stat(filepath.Join(m.deps.Cfg.TLS.Dir, pki.InstanceCertFile))
		require.NoError(t, err)
		return errors.New("injected failure")
	}
	t.Cleanup(func() { afterFleetInstall = nil })

	_, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.ErrorContains(t, err, "injected failure")
	require.True(t, reached)
	requireNoFleetCreated(t, m)
	require.Nil(t, m.Get("fleet"))

	// And the instance can create a fleet afterwards: nothing blocks it.
	afterFleetInstall = nil
	st, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State)
}

// An install the bundle check refuses (the leaf is for the signer's key, the
// key file at KeyPath is another) fails the create after the repository and
// the root exist; the rollback removes both and installs nothing.
func TestCreateFleet_RefusedInstallRollsBack(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), func(d *Deps) { d.Signer = testsigner.Named("another-key") })
	_, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.ErrorIs(t, err, pki.ErrKeyMismatch)
	requireNoFleetCreated(t, m)
}

// Two creates at once: exactly one founds the fleet, the other is
// fleet_present, and the winner's root survives (without the mutex the
// loser's NewRoot fails and its rollback removes the winner's <home>/fleet).
// Sabotage: drop the fleetOpMu lock -> both pass the pre-flight -> red (not
// on every run: it is a race).
func TestCreateFleet_ConcurrentCreatesOneWins(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.CreateFleet(ctx, FleetCreateSpec{Name: []string{"fleet-a", "fleet-b"}[i]})
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		var fe *FleetError
		require.True(t, errors.As(err, &fe), "the loser is refused at the pre-flight: %v", err)
		require.Equal(t, "fleet_present", fe.Code)
	}
	require.Equal(t, 1, ok)
	_, err := pki.LoadRoot(m.fleetRootDir())
	require.NoError(t, err, "the winner's root survives")
	require.Len(t, issuedSerials(t, m.deps.Cfg.Home), 1)
}
