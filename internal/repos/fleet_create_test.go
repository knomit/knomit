package repos

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

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
	// The boot rollback of an unfinished create takes the fleet lock: a test
	// that starts its own create (or holds the lock) must not race it.
	m.fleetBootWg.Wait()
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

// setFleetHooks installs the fleet create's test seams for one test.
func setFleetHooks(t *testing.T, h fleetCreateHooks) {
	t.Helper()
	fleetHooksMu.Lock()
	fleetTestHooks = h
	fleetHooksMu.Unlock()
	t.Cleanup(func() {
		fleetHooksMu.Lock()
		fleetTestHooks = fleetCreateHooks{}
		fleetHooksMu.Unlock()
	})
}

// failAt is a step hook that fails (or, with died, ends the process at) one
// step, once, and records that the create got there.
func failAt(step string, err error, reached *bool) func(string) error {
	return func(s string) error {
		if s != step || *reached {
			return nil
		}
		*reached = true
		return err
	}
}

// requireNoFleetCreated: a refused or rolled-back create left nothing — no
// fleet repository mounted, registered or recorded, no fleet root directory
// or staging directory, no [tls].dir (the fixtures start without one, so
// the rollback removes the one the create made, its CRL watermark with it),
// no create marker, the state standalone.
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
	for _, d := range []string{m.fleetRootDir(), m.fleetRootStagingDir(), m.deps.Cfg.TLS.Dir} {
		_, err = os.Stat(d)
		require.ErrorIs(t, err, os.ErrNotExist, "%s is gone", d)
	}
	mk, err := readFleetCreateMarker(m.ControlDB())
	require.NoError(t, err)
	require.Nil(t, mk, "no create marker")
	st, err := m.FleetStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, FleetStandalone, st.State)
}

// requireNoForgeBranch: the forge holds no agent branch of this instance.
func requireNoForgeBranch(t *testing.T, bare string) {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "for-each-ref", "--format=%(refname)", "refs/heads/agent/").Output()
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(string(out)), "the forge keeps no agent branch")
}

// Review B1 (PR #528): GET /fleet's host.listening is the mTLS listener's
// LIVE state, apart from listener_on (configured). With nobody reporting it
// is false; configured but not started it is false with the reason; bound it
// is true with no reason; and the reason is never shown for a listener that
// is not configured at all.
// SABOTAGE: Listening = ListenerOn -> the no_certificate row reads true ->
// red; ignore the reported reason -> red.
func TestFleetStatus_ListeningIsTheLiveListener(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.True(t, st.Host.ListenerOn, "fixture: [tls].addr is set")
	require.False(t, st.Host.Listening, "nobody reported a bound listener")

	m.SetTLSListener(func() (string, string) { return "", "no_certificate" })
	st, err = m.FleetStatus(ctx)
	require.NoError(t, err)
	require.False(t, st.Host.Listening)
	require.Equal(t, "no_certificate", st.Host.ListenerReason)

	m.SetTLSListener(func() (string, string) { return "127.0.0.1:18931", "" })
	st, err = m.FleetStatus(ctx)
	require.NoError(t, err)
	require.True(t, st.Host.Listening)
	require.Empty(t, st.Host.ListenerReason)

	off := newFleetHost(t, t.TempDir(), func(d *Deps) { d.Cfg.TLS.Addr = "" })
	off.SetTLSListener(func() (string, string) { return "", "off" })
	st, err = off.FleetStatus(ctx)
	require.NoError(t, err)
	require.False(t, st.Host.ListenerOn)
	require.False(t, st.Host.Listening)
	require.Empty(t, st.Host.ListenerReason, "no reason for a listener that is not configured")
}

// Review S2 (PR #528): last_error_refused is read from the recorded error
// alone: a push refused at the TLS handshake (pki.ErrRefusedByPeer, as the
// transport wraps it) says so, any other failure does not, and a success
// clears both.
// SABOTAGE: LastErrorRefused = LastError != "" -> the unreachable row reads
// true -> red.
func TestFleetStatus_LastErrorRefusedIsTheHandshakeRefusal(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	db := m.ControlDB()
	for _, row := range []struct {
		err     error
		refused bool
	}{
		{fmt.Errorf("push agent branch: %w: remote error: tls: bad certificate", pki.ErrRefusedByPeer), true},
		{errors.New("dial tcp 127.0.0.1:18931: connect: connection refused"), false},
		{nil, false},
	} {
		require.NoError(t, recordFleetAttempt(db, row.err, time.Now()))
		st, err := m.FleetStatus(ctx)
		require.NoError(t, err)
		require.Equal(t, row.refused, st.LastErrorRefused, "%v", row.err)
		if row.err == nil {
			require.Empty(t, st.LastError)
		}
	}
}

// #528 N1: last_error_commit_refused is the host's judge refusing a commit
// of this copy's push (the receive-pack text, as the push error carries it):
// the page then never says "Retrying". The handshake refusal, a dial
// failure and a success do not set it.
// SABOTAGE: LastErrorCommitRefused = false -> the judged row reads false ->
// red.
func TestFleetStatus_LastErrorCommitRefusedIsTheJudgesRefusal(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	db := m.ControlDB()
	judged := errors.New("Push: knomit: commit 5a6d21e8 " + store.CommitRefusedByJudge +
		": host-record: agent \"alpha\" changes artifacts/fleet/host: only the fleet's master writes where the fleet is hosted")
	for _, row := range []struct {
		err     error
		refused bool
	}{
		{judged, true},
		{fmt.Errorf("push agent branch: %w: remote error: tls: bad certificate", pki.ErrRefusedByPeer), false},
		{errors.New("dial tcp 127.0.0.1:18931: connect: connection refused"), false},
		{nil, false},
	} {
		require.NoError(t, recordFleetAttempt(db, row.err, time.Now()))
		st, err := m.FleetStatus(ctx)
		require.NoError(t, err)
		require.Equal(t, row.refused, st.LastErrorCommitRefused, "%v", row.err)
	}
}

// PR #528 nit 11: with a repository already called fleet, a standalone
// GET /fleet's would_serve and the create agree on a free name (fleet-2):
// the URL the Hosting step shows is where members join, and the create is
// not refused for the name.
// SABOTAGE: would_serve names defaultFleetRepoName again -> /git/fleet -> red;
// the create keeps the bare default -> refused for the name -> red.
func TestCreateFleet_DefaultNameTakenUsesAFreeOne(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	createRepo(t, m, "fleet")
	pre, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"https://fleet-host.example:19279/git/fleet-2"}, pre.Host.WouldServe)
	st, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	require.Equal(t, "fleet-2", st.FleetRepo)
	require.Equal(t, pre.Host.WouldServe, st.FleetURLs, "members join where the Hosting step said")
}

// A fleet hosted here (no origin): a fresh root is minted into <home>/fleet
// (root.key a plain owner-only file), the repository holds root, leaf and
// record on main at once, the bundle is installed, and this instance is the
// master. Sabotage: drop the AdvanceLocalUpstream call -> main has no root
// and the record is pending -> red.
func TestCreateFleet_HostedHere(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)

	// Before the create, a standalone instance says where members would join
	// a fleet created here (the New fleet wizard's Hosting step), built by
	// the server like every fleet URL.
	// SABOTAGE: no would_serve on a standalone instance -> red.
	pre, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, FleetStandalone, pre.State)
	require.Equal(t, []string{"https://fleet-host.example:19279/git/fleet"}, pre.Host.WouldServe)

	st, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State)
	require.Equal(t, "fleet", st.FleetRepo)
	require.Equal(t, "none", st.Hosting, "hosted here: no origin")
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
	// GET /fleet's hosting fields: the hub's own URLs, the record on main,
	// what it offers, its key and the installed root.
	require.Equal(t, []string{"https://fleet-host.example:19279/git/fleet"}, st.FleetURLs, "the hub: one URL per https external address")
	require.Equal(t, st.FleetURLs, st.HostRecord, "main records the same")
	require.Equal(t, FleetHostOffer{Listener: "127.0.0.1:0", ListenerOn: true, ExternalAddresses: []string{"https://fleet-host.example:19279"}}, st.Host,
		"the hub hosts already: no would_serve")
	require.NotNil(t, st.FollowsHost)
	require.True(t, *st.FollowsHost, "the hub is the host")
	require.Equal(t, rootID, st.RootFingerprint)
	require.True(t, strings.HasPrefix(st.PublicKey, "ssh-ed25519 "), st.PublicKey)
	require.Equal(t, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(m.deps.Signer.PublicKey()))), st.PublicKey)

	f, err := m.FleetAtMain(ctx)
	require.NoError(t, err)
	require.NotNil(t, f.PKI)
	require.True(t, root.Cert.Equal(f.PKI.Root), "main's root.crt is the minted root")
	ri := m.fleetRepo()
	require.True(t, m.isFleetMaster(testService(t, ri)), "the creator is the master")
	uid, err := recordedFleetRepo(m.ControlDB())
	require.NoError(t, err)
	require.Equal(t, ri.UID(), uid, "recorded as this instance's fleet repository")
	hosted, err := m.fleetHostAtMain(ri)
	require.NoError(t, err)
	require.Equal(t, []string{"https://fleet-host.example:19279/git/fleet"}, hosted, "main records where the fleet is hosted: here")
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
	require.Len(t, hs, 6, "the forge's README commit, then the create's five: %+v", hs)
	require.Equal(t, []string{"README.md"}, hs[0].paths)
	require.Equal(t, []string{OntologyPath}, hs[1].paths, "the ontology commit")
	require.ElementsMatch(t, []string{"artifacts/fleet/crl.pem", "artifacts/fleet/root.crt"}, hs[2].paths,
		"ROOT FIRST: the root and the CRL, and nothing else")
	serial := issuedSerials(t, m.deps.Cfg.Home)[0]
	require.ElementsMatch(t, []string{"artifacts/fleet/certs/test-fleet.pem", "artifacts/fleet/issued/" + serial + ".pem"}, hs[3].paths,
		"then the creator's leaf and its one issuance record")
	require.Equal(t, []string{memberRecordPath("kb", "test-fleet")}, hs[4].paths, "then the record")
	require.Equal(t, []string{fact.FleetHostPath}, hs[5].paths, "then where the fleet is hosted, last")
	hosted, err := m.fleetHostAtMain(m.fleetRepo())
	require.NoError(t, err)
	require.Equal(t, []string{fileuri.New(bare)}, hosted, "the forge hosts it")
	require.Equal(t, fileuri.New(bare), st.Hosting)
	require.Equal(t, []string{fileuri.New(bare)}, st.FleetURLs, "a copy with an origin: the record on main")
	require.Equal(t, []string{fileuri.New(bare)}, st.HostRecord)
	require.NotNil(t, st.FollowsHost)
	require.True(t, *st.FollowsHost, "the copy syncs with the host the record names")
	require.Equal(t, []string{"https://fleet-host.example:19279/git/fleet"}, st.Host.WouldServe,
		"the master on a forge: the URLs members would use if it hosted the fleet here")

	v, err := store.CheckRange(store.RangeInput{
		KBDir: bare, Main: hs[1].hash, Candidate: "refs/heads/main",
		FleetDir: bare, FleetRev: "refs/heads/main",
	})
	require.NoError(t, err)
	require.Equal(t, store.VerifyEnforce, v.Mode, "the fleet ontology declares enforce")
	require.Equal(t, 4, v.Checked, "root, leaf, record and host commits")
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

// trustFile installs one file of fleet trust material in [tls].dir.
func trustFile(name string) func(t *testing.T, m *Manager) {
	return func(t *testing.T, m *Manager) {
		require.NoError(t, os.MkdirAll(m.deps.Cfg.TLS.Dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(m.deps.Cfg.TLS.Dir, name), []byte("x"), 0o600))
	}
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
		{name: "root.crt installed", setup: trustFile(pki.RootCertFile), status: http.StatusConflict, code: "trust_installed"},
		{name: "crl.pem installed", setup: trustFile(pki.CRLFile), status: http.StatusConflict, code: "trust_installed"},
		{name: "instance.crt installed", setup: trustFile(pki.InstanceCertFile), status: http.StatusConflict, code: "trust_installed"},
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
		// #527 F1: a forge origin no member's join could clone.
		{name: "http origin", spec: FleetCreateSpec{Origin: &OriginSpec{URL: "http://gitea.lan/org/fleet.git"}},
			status: http.StatusUnprocessableEntity, code: "origin_invalid"},
		{name: "ftp origin", spec: FleetCreateSpec{Origin: &OriginSpec{URL: "ftp://forge.example/fleet.git"}},
			status: http.StatusUnprocessableEntity, code: "origin_invalid"},
		// PR #528 S6: a URL that does not parse is refused without its
		// userinfo in the text (asserted below for every row).
		{name: "unparseable origin with a token", spec: FleetCreateSpec{Origin: &OriginSpec{URL: "https://u:tok@exa mple.com/x"}},
			status: http.StatusUnprocessableEntity, code: "origin_invalid"},
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
			require.NotContains(t, err.Error(), "tok@", "a refusal's text carries no userinfo")
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

// The hub's fleet URLs are one per external address that is https://, with
// /git/<repo> after it (a trailing slash dropped); an http:// address is no
// fleet URL: members reach the hub's mTLS listener over https only.
// SABOTAGE: keep the non-https address -> red.
func TestHubFleetURLs_HTTPSAddressesOnly(t *testing.T) {
	m := newFleetHost(t, t.TempDir(), func(d *Deps) {
		d.Cfg.ExternalAddresses = []string{"https://a.example/", "http://b.example:8080", "HTTPS://c.example:7443"}
	})
	_, err := m.CreateFleet(context.Background(), FleetCreateSpec{})
	require.NoError(t, err)
	require.Equal(t, []string{"https://a.example/git/fleet", "HTTPS://c.example:7443/git/fleet"}, m.hubFleetURLs(m.fleetRepo()))
}
