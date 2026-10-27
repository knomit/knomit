package repos

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/corpus/repos/control"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/remotesync"
	"knomit/internal/foundation/config"
	"knomit/internal/foundation/pki"
	"knomit/internal/foundation/pki/pkitest"
	"knomit/internal/platform/fileuri"
	"knomit/internal/testsupport/gitfixture"
	"knomit/internal/testsupport/testsigner"
)

// fleetOntologyYAML is the built-in fleet ontology, serialized.
func fleetOntologyYAML(t *testing.T) string {
	t.Helper()
	y, err := fact.FleetOntology().Serialize()
	require.NoError(t, err)
	return string(y)
}

// mountFleetByDoor mounts the fleet remote at url as name through the fleet
// code's door and records it as this instance's fleet repository (what
// JoinFleet does, without the bundle, record and push), and fails the test
// unless it is the fleet repository.
func mountFleetByDoor(t *testing.T, m *Manager, name, url string) *RepoInstance {
	t.Helper()
	ri, err := m.Create(context.Background(), CreateSpec{Name: name, Mode: "clone", Origin: &OriginSpec{URL: url}, fleetDoor: true}, nil)
	require.NoError(t, err)
	require.True(t, ri.IsFleet(), "fixture: %s is a fleet repository", name)
	require.NoError(t, recordFleetRepo(m.ControlDB(), ri.UID()))
	return ri
}

// mountFleetMember is a member's half of a join on a fleet remote that holds
// no root (so no bundle can be checked against it): mountFleetByDoor, the
// own record written and pushed, state registered.
func mountFleetMember(t *testing.T, m *Manager, name, url string) *RepoInstance {
	t.Helper()
	ri := mountFleetByDoor(t, m, name, url)
	require.NoError(t, m.writeOwnRecord(context.Background(), ri, fact.MemberActive))
	require.NoError(t, m.pushAgentBranch(context.Background(), ri))
	require.NoError(t, setFleetState(m.ControlDB(), FleetRegistered, fixedNow()))
	return ri
}

// Every generic create door that NAMES an ontology refuses the reserved id
// with ErrFleetOntologyReserved, from the preflight (a status before the
// 202) and from Create, leaving no trace: a preset by name, custom YAML,
// initialize with a preset or YAML, and a template in mode template or
// initialize.
// SABOTAGE: drop refuseReservedOntology from CreatePreflight -> the preset and
// YAML preflight rows go red; drop the IsFleetOntology check in
// ResolveTemplate -> the template rows go red (a local fleet repo is
// created).
func TestFleetReserved_CreateDoorsRefuse(t *testing.T) {
	root := t.TempDir()
	m := tmplManager(t, root, "tmpl-host")
	fleetY := fleetOntologyYAML(t)
	mount(t, m, "src", "clone", sourceRepo(t, root, "src", sourceSpec{files: map[string]string{
		".knomit/templates/fleet/.knomit/ontology.yaml": fleetY,
	}}))
	target := sourceRepo(t, root, "target", sourceSpec{noOntol: true, files: map[string]string{"x.txt": "x"}})
	tmpl := &TemplateRef{Repo: "src", Name: "fleet"}
	for name, spec := range map[string]CreateSpec{
		"preset":            {Mode: "preset", OntologyPreset: "fleet"},
		"custom":            {Mode: "custom", OntologyYAML: fleetY},
		"template":          {Mode: "template", Template: tmpl},
		"initialize-preset": {Mode: "initialize", OntologyPreset: "fleet", Origin: &OriginSpec{URL: target}},
		"initialize-yaml":   {Mode: "initialize", OntologyYAML: fleetY, Origin: &OriginSpec{URL: target}},
		"initialize-tmpl":   {Mode: "initialize", Template: tmpl, Origin: &OriginSpec{URL: target}},
	} {
		t.Run(name, func(t *testing.T) {
			spec.Name = "new-fleet"
			before := repoFiles(t, m)
			require.ErrorIs(t, m.CreatePreflight(context.Background(), spec), ErrFleetOntologyReserved, "preflight")
			_, err := m.Create(context.Background(), spec, nil)
			require.ErrorIs(t, err, ErrFleetOntologyReserved, "create")
			noTraceOf(t, m, "new-fleet", before)
		})
	}
	// Control: the same doors create an ordinary repo.
	_, err := m.Create(context.Background(), CreateSpec{Name: "plain", Mode: "preset", OntologyPreset: "default"}, nil)
	require.NoError(t, err)
}

// The doors that take the ontology FROM the remote (clone, subscribe) are
// refused at Identify, and the failed create is rolled back. The fleet code's
// door (CreateSpec.fleetDoor, JoinFleet) mounts the same remote.
// SABOTAGE: make admitFleetOntology return nil -> the clone row mounts a
// fleet repository -> red.
func TestFleetReserved_CloneAndSubscribeRefusedAtIdentify(t *testing.T) {
	dir := t.TempDir()
	url := seedFleetRemote(t, filepath.Join(dir, "fleet.git"))
	m := newFleetManager(t, dir)
	for _, mode := range []string{"clone", "subscribe"} {
		before := repoFiles(t, m)
		_, err := m.Create(context.Background(), CreateSpec{Name: "f-" + mode, Mode: mode, Origin: &OriginSpec{URL: url}}, nil)
		require.ErrorIs(t, err, ErrFleetOntologyReserved, mode)
		noTraceOf(t, m, "f-"+mode, before)
		require.Nil(t, m.fleetRepo(), "%s: no fleet repository is mounted", mode)
	}
	ri := mountFleetByDoor(t, m, "fleet", url)
	require.Same(t, ri, m.fleetRepo(), "control: the fleet door mounts it")
}

// A fleet repository is never restored: Identify refuses the restored store
// (Restore is not a fleet door), and the repo stays archived.
// SABOTAGE: pass fleetDoor true from Restore's mountExisting -> restored ->
// red.
func TestFleetReserved_RestoreRefused(t *testing.T) {
	dir := t.TempDir()
	m := newFleetManager(t, dir)
	mountFleetByDoor(t, m, "fleet", seedFleetRemote(t, filepath.Join(dir, "fleet.git")))
	// The generic archive is refused (below); the unguarded one is how a
	// fleet repository from an older build ends up archived.
	info, err := m.archive("fleet", control.ArchiveReason{Source: control.ArchiveBySystem, Reason: "test"})
	require.NoError(t, err)
	_, err = m.Restore(info.ID, "")
	require.ErrorIs(t, err, ErrFleetOntologyReserved)
	require.Nil(t, m.Get("fleet"), "not mounted")
	got, err := m.GetArchived(info.ID)
	require.NoError(t, err, "still archived")
	require.Equal(t, info.ID, got.ID)
}

// The fleet repository is never archived or deleted through the generic
// doors, in any fleet state (leave and decommission remove it).
// SABOTAGE: restore a state switch in guardFleetRemoval -> the standalone
// row archives it -> red.
func TestFleetReserved_ArchiveAndDeleteRefusedInEveryState(t *testing.T) {
	dir := t.TempDir()
	m := newFleetManager(t, dir)
	mountFleetByDoor(t, m, "fleet", seedFleetRemote(t, filepath.Join(dir, "fleet.git")))
	for _, state := range []string{FleetStandalone, FleetRegistering, FleetRegistered} {
		require.NoError(t, setFleetState(m.ControlDB(), state, fixedNow()))
		_, err := m.Archive("fleet")
		status, code := fleetCode(t, err)
		require.Equal(t, http.StatusConflict, status, "archive, %s", state)
		require.Equal(t, "fleet_repository", code, "archive, %s", state)
		status, code = fleetCode(t, m.DeleteRepo("fleet", "test"))
		require.Equal(t, http.StatusConflict, status, "delete, %s", state)
		require.Equal(t, "fleet_repository", code, "delete, %s", state)
		require.NotNil(t, m.Get("fleet"), "still mounted, %s", state)
	}
	// Control: an ordinary repo archives.
	createRepo(t, m, "plain")
	_, err := m.Archive("plain")
	require.NoError(t, err)
}

// The fleet repository is read-only for every authored write: no branch is
// writable (the classification every MCP and REST write asks), it may not be
// a lens's write repo, and it may be a lens's read mount.
// SABOTAGE: drop the IsFleet arm of WritableBranch -> red; drop the
// ErrLensWriteFleet check -> the lens is created -> red.
func TestFleetReserved_ReadOnlyForAuthoredWrites(t *testing.T) {
	dir := t.TempDir()
	m := newFleetManager(t, dir)
	fl := mountFleetByDoor(t, m, "fleet", seedFleetRemote(t, filepath.Join(dir, "fleet.git")))
	kb := createRepo(t, m, "kb")
	require.NoError(t, fl.OntologyError(), "reached: the fleet ontology is established")
	require.NotEmpty(t, fl.AgentBranch(), "reached: the fleet repository has an agent branch")
	require.False(t, fl.WritableBranch(fl.AgentBranch()), "its agent branch is not writable")
	require.True(t, kb.WritableBranch(kb.AgentBranch()), "control: an ordinary repo's is")

	_, err := m.CreateLens(context.Background(), control.Lens{Name: "via-fleet", WriteUID: fl.UID()})
	require.ErrorIs(t, err, ErrLensWriteFleet)
	_, err = m.CreateLens(context.Background(), control.Lens{Name: "reads-fleet", WriteUID: kb.UID(), Reads: []control.LensRead{{RepoUID: fl.UID()}}})
	require.NoError(t, err, "a lens may read the fleet repository")
}

// A store swap (connect) never installs a fleet-id store, and never replaces
// the fleet repository's own store; both are refused before anything changes.
// SABOTAGE: drop the incomingFleet check in guardSwap -> the swap installs
// the fleet store (and Identify then refuses it, leaving kb Unavailable) ->
// red on the sentinel and on kb still being a knowledge base.
func TestFleetReserved_SwapRefused(t *testing.T) {
	dir := t.TempDir()
	m := newFleetManager(t, dir)
	fl := mountFleetByDoor(t, m, "fleet", seedFleetRemote(t, filepath.Join(dir, "fleet.git")))
	kb := createRepo(t, m, "kb")

	snapshot := func(ri *RepoInstance) string {
		t.Helper()
		require.NoError(t, ri.WithRead(func(s *store.Service) { require.NoError(t, s.Checkpoint()) }))
		tmp := filepath.Join(t.TempDir(), "copy.db")
		copyDB(t, m.RepoPath(ri.UID()), tmp)
		return tmp
	}
	err := swapStore(m, kb, snapshot(fl))
	require.ErrorIs(t, err, ErrFleetOntologyReserved, "a fleet store never arrives by connect")
	require.Equal(t, "general", kb.Ontology().ID, "kb keeps its own store")

	err = swapStore(m, fl, snapshot(kb))
	require.ErrorIs(t, err, ErrFleetOntologyReserved, "the fleet repository is never connected")
	require.True(t, fl.IsFleet(), "the fleet repository keeps its store")
}

// fleetTestDeps is newFleetManager's Deps, for a test that restarts the
// manager on the same home.
func fleetTestDeps(dir string) Deps {
	return Deps{
		Cfg:     config.Config{Home: filepath.Join(dir, "home"), OntologyRoot: "kb", LocalOriginRoot: dir},
		AgentID: testsigner.FixtureAgentID("agent/test-fleet"), AgentBranch: "agent/test-fleet",
		Signer:  testsigner.Named("fleet-test"),
		Machine: Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	}
}

// restartManager closes m and starts a new manager on the same home.
func restartManager(t *testing.T, m *Manager, dir string) *Manager {
	t.Helper()
	require.NoError(t, m.Close())
	m = New(context.Background(), fleetTestDeps(dir))
	require.NoError(t, m.Start())
	t.Cleanup(func() { m.Close() })
	return m
}

// requireUnavailableFleetConflict asserts name is registered, not mounted,
// and listed unavailable with CauseFleetConflict and the reserved-id refusal.
func requireUnavailableFleetConflict(t *testing.T, m *Manager, name string) {
	t.Helper()
	require.True(t, m.Get(name) == nil, "%s is not mounted", name)
	for _, u := range m.Unavailable() {
		if u.Record.Name == name {
			require.Equal(t, CauseFleetConflict, u.Cause, "%s", name)
			require.Contains(t, u.Detail, ErrFleetOntologyReserved.Error(), "%s: refused through the reserved-id path", name)
			return
		}
	}
	t.Fatalf("%s is not listed unavailable", name)
}

// The reviewer's scenario (PR #520 F1): an ordinary knowledge base is cloned,
// its upstream later switches its ontology to id `fleet`, the sync brings
// that onto the agent branch, and the instance restarts. Boot admits a
// fleet-id ontology only for THIS instance's recorded fleet repository, so the
// switched repo is refused through the reserved-id path: unmounted, listed
// unavailable, not the fleet repository, and still archivable and purgeable
// as an ordinary repo. The instance's real fleet repository (registered
// through the fleet door, which records it) mounts as before.
// SABOTAGE: make openRegistered pass fleetDoor true for every row -> the
// switched repo is admitted as a second fleet -> red.
func TestFleetReserved_BootAdmitsOnlyTheRecordedFleet(t *testing.T) {
	for _, withFleet := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "registered"}[withFleet], func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()

			// An ordinary knowledge base upstream.
			bare := filepath.Join(dir, "kb.git")
			require.NoError(t, os.MkdirAll(bare, 0o755))
			gitfixture.InitBare(t, bare, "main")
			work := t.TempDir()
			runGit(t, "", "clone", bare, work)
			require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(OntologyPath)), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(work, OntologyPath),
				[]byte("id: general\nname: General\ntopics:\n  notes:\n    description: d\n"), 0o644))
			runGit(t, work, "add", "-A")
			runGit(t, work, "commit", "-q", "-m", "kb")
			runGit(t, work, "push", "-q", "origin", "main")
			runGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")

			m := newFleetManager(t, dir)
			fleetName := ""
			if withFleet {
				mountFleetMember(t, m, "fleet", seedFleetRemote(t, filepath.Join(dir, "fleet.git")))
				fleetName = m.fleetRepo().Name()
			}
			kb, err := m.Create(ctx, CreateSpec{Name: "kb", Mode: "clone", Origin: &OriginSpec{URL: fileuri.New(bare)}}, nil)
			require.NoError(t, err)
			require.Equal(t, "general", kb.Ontology().ID, "fixture: an ordinary clone")

			// Upstream switches the ontology to the fleet's; the sync merges it.
			require.NoError(t, os.WriteFile(filepath.Join(work, OntologyPath), []byte(fleetOntologyYAML(t)), 0o644))
			runGit(t, work, "commit", "-q", "-am", "now a fleet")
			runGit(t, work, "push", "-q", "origin", "main")
			require.NoError(t, kb.WithRead(func(svc *store.Service) {
				remote, err := svc.Remote().GetRemote("origin")
				require.NoError(t, err)
				auth, err := makeRemoteAuthFn(m.deps.Cfg.Remote, m.deps.KeyPath)(remote)
				require.NoError(t, err)
				_, err = svc.Remote().Sync(ctx, kb.AgentBranch(), auth)
				require.NoError(t, err)
				raw, err := svc.OntologyAt(ctx, kb.AgentBranch())
				require.NoError(t, err)
				o, err := fact.ParseOntology(raw)
				require.NoError(t, err)
				require.True(t, fact.IsFleetOntology(o), "reached: the agent branch now holds a fleet ontology")
			}))

			m = restartManager(t, m, dir)
			requireUnavailableFleetConflict(t, m, "kb")
			if withFleet {
				require.NotNil(t, m.Get(fleetName), "the recorded fleet repository mounts")
				require.Equal(t, fleetName, m.fleetRepo().Name(), "and it is the fleet repository")
			} else {
				require.Nil(t, m.fleetRepo(), "a standalone instance has no fleet repository")
			}

			info, err := m.Archive("kb")
			require.NoError(t, err, "the switched repo archives as an ordinary repo")
			require.NoError(t, m.Purge(info.ID), "and purges")
			if withFleet {
				require.NotNil(t, m.Get(fleetName), "the fleet repository is untouched")
			}
		})
	}
}

// Two registered fleet-id repositories at boot (a hand-edited control.db, a
// home from an older build): only the recorded one is mounted; the other is
// left unmounted and listed unavailable with CauseFleetConflict, and nothing
// is archived.
// SABOTAGE: make openRegistered pass fleetDoor true for every row -> both
// mounted -> red.
func TestFleetReserved_BootLeavesAnUnrecordedFleetUnmounted(t *testing.T) {
	dir := t.TempDir()
	m := newFleetManager(t, dir)
	mountFleetByDoor(t, m, "fleet-a", seedFleetRemote(t, filepath.Join(dir, "a.git")))
	// A second fleet with its own root commit (the same seed in the same
	// second would be the same knowledge base, refused as a local copy),
	// mounted last, so it is the recorded one.
	other := filepath.Join(dir, "b.git")
	seedFleetRemote(t, other)
	work := t.TempDir()
	runGit(t, "", "clone", other, work)
	runGit(t, work, "commit", "--amend", "-q", "-m", "another fleet")
	runGit(t, work, "push", "-q", "-f", "origin", "main")
	mountFleetByDoor(t, m, "fleet-b", fileuri.New(other))

	m = restartManager(t, m, dir)
	require.NotNil(t, m.Get("fleet-b"), "the recorded fleet repository is mounted")
	require.Equal(t, "fleet-b", m.fleetRepo().Name())
	requireUnavailableFleetConflict(t, m, "fleet-a")
	archived, err := m.ListArchived()
	require.NoError(t, err)
	require.Empty(t, archived, "nothing is archived")
}

// staleFleetYAML is the built-in fleet ontology WITH `consensus: auto`: what
// a release before the master merged on every hosting wrote. It still
// declares the artifacts section, so the fleet reader finds
// artifacts/fleet/root.crt on main.
func staleFleetYAML(t *testing.T) string {
	t.Helper()
	const anchor = "  verify_signatures: enforce\n"
	builtIn := fleetOntologyYAML(t)
	require.Equal(t, 1, strings.Count(builtIn, anchor), "fixture: one verify_signatures line")
	stale := strings.Replace(builtIn, anchor, anchor+"  consensus: auto\n", 1)
	require.NotEqual(t, builtIn, stale, "fixture: the stale ontology differs")
	o, err := fact.ParseOntology([]byte(stale))
	require.NoError(t, err)
	require.True(t, fact.IsFleetOntology(o), "fixture: still a fleet ontology")
	_, ok := o.PKILayout()
	require.True(t, ok, "fixture: still declares the fleet's root")
	return stale
}

// seedFleetRemoteWithRoot is a bare fleet remote whose main holds ontYAML
// and the fleet root certificate rootPEM at artifacts/fleet/root.crt.
func seedFleetRemoteWithRoot(t *testing.T, bare, ontYAML string, rootPEM []byte) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(bare, 0o755))
	gitfixture.InitBare(t, bare, "main")
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	// main names the master (fact.FleetMasterPath): the root holder these
	// fixtures mount is "test-fleet".
	for path, body := range map[string][]byte{OntologyPath: []byte(ontYAML), "artifacts/fleet/" + pki.RootCertFile: rootPEM,
		fact.FleetMasterPath: []byte(fact.RenderFleetMaster("test-fleet"))} {
		require.NoError(t, os.MkdirAll(filepath.Join(work, filepath.Dir(path)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(work, path), body, 0o644))
	}
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-q", "-m", "fleet")
	runGit(t, work, "push", "-q", "origin", "main")
	runGit(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return fileuri.New(bare)
}

// installFleetRoot copies root's key and certificate into m's <home>/fleet,
// as a fleet create (or PUT /api/v1/fleet/root) leaves them.
func installFleetRoot(t *testing.T, m *Manager, root *pkitest.Fleet) {
	t.Helper()
	require.NoError(t, os.MkdirAll(m.fleetRootDir(), 0o700))
	for _, name := range []string{pki.RootKeyFile, pki.RootCertFile} {
		raw, err := os.ReadFile(filepath.Join(root.Dir, name))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(m.fleetRootDir(), name), raw, 0o600))
	}
}

// refreshFixture mounts, through the fleet door, a fleet repository whose
// main holds staleFleetYAML and root A's certificate. home is what
// <home>/fleet holds when it mounts: "master" root A, "stray" another root
// (a stray root.key from another fleet), "cert-only" root A's certificate
// without its key, "member" nothing.
func refreshFixture(t *testing.T, home string) (m *Manager, ri *RepoInstance, stale string, rootA *pkitest.Fleet) {
	t.Helper()
	dir := t.TempDir()
	stale = staleFleetYAML(t)
	rootA = pkitest.New(t)
	rootPEM, err := os.ReadFile(filepath.Join(rootA.Dir, pki.RootCertFile))
	require.NoError(t, err)
	url := seedFleetRemoteWithRoot(t, filepath.Join(dir, "fleet.git"), stale, rootPEM)
	m = newFleetManager(t, dir)
	switch home {
	case "master":
		installFleetRoot(t, m, rootA)
	case "stray":
		installFleetRoot(t, m, pkitest.New(t))
	case "cert-only":
		installFleetRoot(t, m, rootA)
		require.NoError(t, os.Remove(filepath.Join(m.fleetRootDir(), pki.RootKeyFile)))
	}
	return m, mountFleetByDoor(t, m, "fleet", url), stale, rootA
}

// agentOntology is the ontology file on ri's agent branch.
func agentOntology(t *testing.T, ri *RepoInstance) string {
	t.Helper()
	got, err := testService(t, ri).Facts().ReadFact(context.Background(), ri.AgentBranch(), OntologyPath, nil)
	require.NoError(t, err)
	return got.Content
}

// The fleet ontology is knomit's own: on the MASTER (this fleet's root key
// and certificate are in <home>/fleet) a stored fleet ontology that differs
// from the built-in one is replaced by it wholesale, policy included.
// SABOTAGE: return before the write in refreshFleetOntology -> the stored
// file stays stale -> red.
func TestFleetOntologyRefresh_MasterReplacesItWholesale(t *testing.T) {
	_, ri, _, _ := refreshFixture(t, "master")
	require.Equal(t, fleetOntologyYAML(t), agentOntology(t, ri), "the master writes the built-in fleet ontology")
	require.Same(t, fact.FleetOntology(), ri.Ontology(), "and the built-in one is in force")
}

// A member (no fleet root key file) never refreshes the fleet ontology: it
// never writes a dot path on the fleet repository.
// SABOTAGE: drop the isFleetMaster condition -> the member rewrites the file
// -> red.
func TestFleetOntologyRefresh_MemberLeavesItAlone(t *testing.T) {
	_, ri, stale, _ := refreshFixture(t, "member")
	require.Equal(t, stale, agentOntology(t, ri), "the stored file is left byte for byte")
	require.True(t, ri.IsFleet(), "reached: the repo is the fleet repository")
	require.NotSame(t, fact.FleetOntology(), ri.Ontology(), "the stored ontology is in force")
}

// A member holding a stray root.key, with the root certificate of ANOTHER
// fleet beside it (an earlier fleet that was never decommissioned), is not
// this fleet's master (PR #520 F3): the key file alone does not make it one,
// its root.crt must equal artifacts/fleet/root.crt on the fleet's main. It
// leaves the ontology alone.
// SABOTAGE: drop the root certificate comparison from isFleetMaster (answer
// true once the key file is there) -> the member rewrites the file -> red.
func TestFleetOntologyRefresh_StrayRootKeyIsNotTheMaster(t *testing.T) {
	m, ri, stale, _ := refreshFixture(t, "stray")
	_, err := os.Stat(filepath.Join(m.fleetRootDir(), pki.RootKeyFile))
	require.NoError(t, err, "reached: a root key file is in <home>/fleet")
	require.Equal(t, stale, agentOntology(t, ri), "the stored file is left byte for byte")
	require.NoError(t, ri.WithRead(func(svc *store.Service) {
		require.False(t, m.isFleetMaster(svc), "not this fleet's master")
	}))
}

// This fleet's root certificate without the key beside it (a member that
// keeps a copy of root.crt) does not make the master either: both halves are
// required.
// SABOTAGE: drop the root.key check from isFleetMaster -> the member
// rewrites the file -> red.
func TestFleetOntologyRefresh_RootCertWithoutKeyIsNotTheMaster(t *testing.T) {
	m, ri, stale, _ := refreshFixture(t, "cert-only")
	_, err := pki.LoadRootCert(filepath.Join(m.fleetRootDir(), pki.RootCertFile))
	require.NoError(t, err, "reached: this fleet's root certificate is in <home>/fleet")
	require.Equal(t, stale, agentOntology(t, ri), "the stored file is left byte for byte")
}

// A refused restore on the MASTER writes nothing into the archived store
// (PR #520 F5). Identify loads the ontology, and so runs the refresh, BEFORE
// admitFleetOntology refuses the mount; the refresh's fleetDoor condition is
// the only thing that keeps a master from committing the built-in ontology
// into the archived repository's agent branch.
// SABOTAGE: drop `|| !r.fleetDoor` from refreshFleetOntology -> the archived
// store's ontology is rewritten -> red.
func TestFleetReserved_RefusedRestoreOnTheMasterWritesNothing(t *testing.T) {
	m, ri, stale, rootA := refreshFixture(t, "member")
	uid := ri.UID()
	o, err := m.originOf(ri)
	require.NoError(t, err)
	origin := o.URL
	info, err := m.archive("fleet", control.ArchiveReason{Source: control.ArchiveBySystem, Reason: "test"})
	require.NoError(t, err)
	installFleetRoot(t, m, rootA) // this instance is now the fleet's master

	archived := func() (string, bool) {
		t.Helper()
		tmp := filepath.Join(t.TempDir(), "copy.db")
		copyDB(t, m.RepoPath(uid), tmp)
		svc, err := store.Open(tmp)
		require.NoError(t, err)
		defer svc.Close()
		svc.SetOntologyRoot("kb")
		svc.Remote().SetOrigin(&remotesync.Origin{URL: origin, Branch: "main"})
		require.NoError(t, svc.OpenRepo())
		got, err := svc.Facts().ReadFact(context.Background(), "agent/test-fleet", OntologyPath, nil)
		require.NoError(t, err)
		return got.Content, m.isFleetMaster(svc)
	}
	before, master := archived()
	require.Equal(t, stale, before, "fixture: the archived store holds the stale ontology")
	require.True(t, master, "reached: a restore would run on this fleet's master")

	_, err = m.Restore(info.ID, "")
	require.ErrorIs(t, err, ErrFleetOntologyReserved)
	after, _ := archived()
	require.Equal(t, before, after, "the archived store's ontology is byte for byte what it was")
}
