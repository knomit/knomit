package repos

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/platform/fileuri"
	"knomit/internal/store"
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
// code's door (what RegisterFleet does), and fails the test unless it is the
// fleet repository.
func mountFleetByDoor(t *testing.T, m *Manager, name, url string) *RepoInstance {
	t.Helper()
	ri, err := m.Create(context.Background(), CreateSpec{Name: name, Mode: "clone", Origin: &OriginSpec{URL: url}, fleetDoor: true}, nil)
	require.NoError(t, err)
	require.True(t, ri.IsFleet(), "fixture: %s is a fleet repository", name)
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
// door (CreateSpec.fleetDoor, RegisterFleet) mounts the same remote.
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
	info, err := m.archive("fleet", ArchiveReason{Source: ArchiveBySystem, Reason: "test"})
	require.NoError(t, err)
	_, err = m.Restore(info.ID, "")
	require.ErrorIs(t, err, ErrFleetOntologyReserved)
	require.Nil(t, m.Get("fleet"), "not mounted")
	got, err := m.GetArchived(info.ID)
	require.NoError(t, err, "still archived")
	require.Equal(t, info.ID, got.ID)
}

// The fleet repository is never archived or deleted through the generic
// doors, in any fleet state (the unregistering escape hatch is gone too).
// SABOTAGE: restore the state switch in guardFleetRemoval -> the standalone
// and unregistering rows archive it -> red.
func TestFleetReserved_ArchiveAndDeleteRefusedInEveryState(t *testing.T) {
	dir := t.TempDir()
	m := newFleetManager(t, dir)
	mountFleetByDoor(t, m, "fleet", seedFleetRemote(t, filepath.Join(dir, "fleet.git")))
	for _, state := range []string{FleetStandalone, FleetRegistering, FleetRegistered, FleetUnregistering} {
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

	_, err := m.CreateLens(context.Background(), Lens{Name: "via-fleet", WriteUID: fl.UID()})
	require.ErrorIs(t, err, ErrLensWriteFleet)
	_, err = m.CreateLens(context.Background(), Lens{Name: "reads-fleet", WriteUID: kb.UID(), Reads: []LensRead{{RepoUID: fl.UID()}}})
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

// Two registered fleet repositories at boot (a hand-edited control.db, an
// older build): the first by name is mounted, the other is left unmounted and
// listed unavailable with CauseFleetConflict; nothing is archived.
// SABOTAGE: drop leaveRivalFleetsUnmounted from Start -> both mounted -> red.
func TestFleetReserved_BootLeavesTheSecondFleetUnmounted(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	require.NoError(t, os.MkdirAll(home, 0o755))
	deps := Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: dir},
		AgentBranch: "agent/test-fleet",
		Signer:      testsigner.Named("fleet-test"),
		Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	}
	m := New(context.Background(), deps)
	require.NoError(t, m.Start())
	mountFleetByDoor(t, m, "fleet-b", seedFleetRemote(t, filepath.Join(dir, "b.git")))
	// A second fleet with its own root commit (the same seed in the same
	// second would be the same knowledge base, refused as a local copy).
	other := filepath.Join(dir, "a.git")
	seedFleetRemote(t, other)
	work := t.TempDir()
	runGit(t, "", "clone", other, work)
	runGit(t, work, "commit", "--amend", "-q", "-m", "another fleet")
	runGit(t, work, "push", "-q", "-f", "origin", "main")
	mountFleetByDoor(t, m, "fleet-a", fileuri.New(other))
	require.NoError(t, m.Close())

	m = New(context.Background(), deps)
	require.NoError(t, m.Start())
	t.Cleanup(func() { m.Close() })
	require.NotNil(t, m.Get("fleet-a"), "the first by name is mounted")
	require.Equal(t, "fleet-a", m.fleetRepo().Name())
	require.Nil(t, m.Get("fleet-b"), "the second is not")
	var found bool
	for _, u := range m.Unavailable() {
		if u.Record.Name == "fleet-b" {
			found = true
			require.Equal(t, CauseFleetConflict, u.Cause)
			require.Contains(t, u.Detail, `"fleet-a" is mounted`)
		}
	}
	require.True(t, found, "fleet-b is listed unavailable")
	archived, err := m.ListArchived()
	require.NoError(t, err)
	require.Empty(t, archived, "nothing is archived")
}
