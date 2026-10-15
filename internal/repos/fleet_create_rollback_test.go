package repos

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/pki/pkitest"
	"knomit/internal/platform/fileuri"
)

var errInjected = errors.New("injected failure")

// A failure after ANY step rolls all of it back: the repository and its
// record, the staging directory and <home>/fleet, the installed material and
// the [tls].dir the create made, the marker, the state. And the instance
// creates a fleet afterwards: nothing blocks it.
// Sabotage: drop any one undo step of undoFleetCreate -> the row of the
// step after which its leftover exists finds it -> red.
func TestCreateFleet_FailureAtEachStepRollsBack(t *testing.T) {
	ctx := context.Background()
	for _, step := range fleetCreateSteps {
		t.Run(step, func(t *testing.T) {
			m := newFleetHost(t, t.TempDir(), nil)
			reached := false
			setFleetHooks(t, fleetCreateHooks{step: failAt(step, errInjected, &reached)})

			_, err := m.CreateFleet(ctx, FleetCreateSpec{})
			require.ErrorIs(t, err, errInjected)
			require.True(t, reached, "the create got to %s", step)
			requireNoFleetCreated(t, m)

			st, err := m.CreateFleet(ctx, FleetCreateSpec{})
			require.NoError(t, err)
			require.Equal(t, FleetRegistered, st.State)
			require.Len(t, issuedSerials(t, m.deps.Cfg.Home), 1, "a fresh root: one issuance")
		})
	}
}

// A create that DIED at any step (no rollback ran: a crash or a kill) is
// rolled back at the next start, from its marker, and a fresh create then
// succeeds.
// Sabotage: drop the recovery from startFleetBootReconcile -> the marker and
// the leftovers are there after the restart -> red.
func TestCreateFleet_DiedAtEachStepIsRolledBackAtBoot(t *testing.T) {
	ctx := context.Background()
	for _, step := range fleetCreateSteps {
		t.Run(step, func(t *testing.T) {
			deps := fleetHostDeps(t, t.TempDir())
			m := startFleetHost(t, deps)
			reached := false
			setFleetHooks(t, fleetCreateHooks{step: failAt(step, errFleetCreateDied, &reached)})
			_, err := m.CreateFleet(ctx, FleetCreateSpec{Name: "crew"})
			require.ErrorIs(t, err, errFleetCreateDied)
			require.True(t, reached)
			mk, err := readFleetCreateMarker(m.ControlDB())
			require.NoError(t, err)
			require.NotNil(t, mk, "a create that died leaves its marker")
			require.Equal(t, "crew", mk.RepoName)
			require.NoError(t, m.Close())

			m = startFleetHost(t, deps)
			m.fleetBootWg.Wait()
			requireNoFleetCreated(t, m)
			require.Nil(t, m.Get("crew"))

			st, err := m.CreateFleet(ctx, FleetCreateSpec{Name: "crew"})
			require.NoError(t, err)
			require.Equal(t, FleetRegistered, st.State)
			require.Equal(t, fact.MemberActive, st.RecordState)
		})
	}
}

// Without a restart, the next create rolls the dead one back first.
// Sabotage: drop recoverFleetCreate from CreateFleet -> the pre-flight sees
// the half-made fleet (fleet_present) -> red.
func TestCreateFleet_DiedIsRolledBackByTheNextCreate(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	reached := false
	setFleetHooks(t, fleetCreateHooks{step: failAt("committed", errFleetCreateDied, &reached)})
	_, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.ErrorIs(t, err, errFleetCreateDied)
	require.NotNil(t, m.Get("fleet"), "the dead create's repository is still mounted")

	st, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State)
	require.Len(t, issuedSerials(t, m.deps.Cfg.Home), 1, "the new fleet's root, not the dead one's")
	_, err = os.Stat(m.fleetRootStagingDir())
	require.ErrorIs(t, err, os.ErrNotExist)
}

// B1. A forge create that fails after its repository exists deletes the
// agent branch it pushed (with a lease on that hash), so a retry on the same
// forge succeeds instead of finding an initialized branch.
// Sabotage: drop retractFleetBranch from undoFleetCreate -> the forge keeps
// agent/test-fleet and the retry fails ("already has a knomit ontology") ->
// red.
func TestCreateFleet_ForgeFailureRetractsThePushedBranch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := forgeFleetRemote(t, dir)
	readme := gitRev(t, bare, "refs/heads/main")
	m := newFleetHost(t, dir, nil)
	reached := false
	setFleetHooks(t, fleetCreateHooks{step: failAt("installed", errInjected, &reached)})

	_, err := m.CreateFleet(ctx, FleetCreateSpec{Origin: &OriginSpec{URL: fileuri.New(bare)}})
	require.ErrorIs(t, err, errInjected)
	require.True(t, reached)
	requireNoFleetCreated(t, m)
	requireNoForgeBranch(t, bare)
	require.Equal(t, readme, gitRev(t, bare, "refs/heads/main"), "the forge's main is untouched")

	st, err := m.CreateFleet(ctx, FleetCreateSpec{Origin: &OriginSpec{URL: fileuri.New(bare)}})
	require.NoError(t, err, "a retry on the same forge succeeds")
	require.Equal(t, FleetRegistered, st.State)
	require.Equal(t, fact.MemberActive, st.RecordState, "201: main holds the record")
}

// A forge create that died after its repo create but BEFORE the repository
// was recorded: at boot the marker admits that repository through the fleet
// door, so the rollback can open it and delete the branch it pushed.
// Sabotage: drop the marker's name from Start's fleet-door admission -> the
// repository is unavailable (fleet_conflict), the rollback deletes it without
// the forge branch -> red.
func TestCreateFleet_ForgeDiedBeforeTheRecordIsRolledBackAtBoot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := forgeFleetRemote(t, dir)
	deps := fleetHostDeps(t, dir)
	m := startFleetHost(t, deps)
	reached := false
	setFleetHooks(t, fleetCreateHooks{step: failAt("repo", errFleetCreateDied, &reached)})
	_, err := m.CreateFleet(ctx, FleetCreateSpec{Origin: &OriginSpec{URL: fileuri.New(bare)}})
	require.ErrorIs(t, err, errFleetCreateDied)
	require.NotEmpty(t, gitRev(t, bare, "refs/heads/agent/test-fleet"), "the dead create pushed its agent branch")
	require.NoError(t, m.Close())

	m = startFleetHost(t, deps)
	m.fleetBootWg.Wait()
	requireNoFleetCreated(t, m)
	requireNoForgeBranch(t, bare)

	st, err := m.CreateFleet(ctx, FleetCreateSpec{Origin: &OriginSpec{URL: fileuri.New(bare)}})
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State)
}

// N2. Trust material someone else installs while a create runs (a desktop
// or CLI install) makes the create's own install fail with RootDiffers, and
// the rollback leaves that material alone: it removes only an installed root
// that is the create's own.
// Sabotage: drop the installed-root check in uninstallFreshTrust -> the
// foreign files (and [tls].dir) are removed -> red.
func TestCreateFleet_ForeignInstallIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	tlsDir := m.deps.Cfg.TLS.Dir
	foreign := pkitest.New(t)
	member := foreign.Enroll(t, "someone@agents.knomit.io")
	setFleetHooks(t, fleetCreateHooks{step: func(s string) error {
		if s == "committed" {
			foreign.Install(t, member, tlsDir)
		}
		return nil
	}})

	_, err := m.CreateFleet(ctx, FleetCreateSpec{})
	var differs *pki.RootDiffersError
	require.ErrorAs(t, err, &differs, "the create's own install meets the foreign root")
	for _, f := range []string{pki.RootCertFile, pki.CRLFile, pki.InstanceCertFile} {
		_, err := os.Stat(filepath.Join(tlsDir, f))
		require.NoError(t, err, "the foreign %s is untouched", f)
	}
	installed, err := pki.InstalledRoot(tlsDir)
	require.NoError(t, err)
	foreignID, err := pki.RootID(foreign.Root.Cert)
	require.NoError(t, err)
	require.Equal(t, foreignID, installed.Fingerprint)
	require.Nil(t, m.fleetRepo())
	_, err = os.Stat(m.fleetRootStagingDir())
	require.ErrorIs(t, err, os.ErrNotExist)
	mk, err := readFleetCreateMarker(m.ControlDB())
	require.NoError(t, err)
	require.Nil(t, mk)
}

// N1. A root written into <home>/fleet while a create runs (knomit identity
// init-root) is never removed by the rollback: the create mints into its
// staging directory, and the move into a non-empty <home>/fleet fails.
// Sabotage: make removeCreatedRoot remove <home>/fleet whatever it holds ->
// the other root key is gone -> red.
func TestCreateFleet_RootWrittenMeanwhileIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	other := pkitest.New(t)
	setFleetHooks(t, fleetCreateHooks{step: func(s string) error {
		if s == "installed" {
			require.NoError(t, os.MkdirAll(m.fleetRootDir(), 0o700))
			for _, f := range []string{pki.RootKeyFile, pki.RootCertFile} {
				raw, err := os.ReadFile(filepath.Join(other.Dir, f))
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(m.fleetRootDir(), f), raw, 0o600))
			}
		}
		return nil
	}})
	_, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.ErrorContains(t, err, "no longer empty")
	got, err := pki.LoadRoot(m.fleetRootDir())
	require.NoError(t, err, "the other root is still there")
	require.True(t, other.Root.Cert.Equal(got.Cert))
	require.Nil(t, m.fleetRepo())
	_, err = os.Stat(m.fleetRootStagingDir())
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(m.deps.Cfg.TLS.Dir)
	require.ErrorIs(t, err, os.ErrNotExist, "the create's own install is removed")
}

// S1, S3. A forge create whose publish of main fails is registering with
// last_error (202), and it stays registering across a loop push while
// publishing still fails, even when something set it to registered
// meanwhile. The loop push after the publish works publishes main and only
// then enters registered.
// Sabotage: in fleetPushed, enter registered on any successful push -> the
// first retry reads registered while the forge's main is the README -> red.
// Sabotage: drop the explicit registering in publishFleetMain's failure path
// -> the state the hook set (registered) survives -> red.
func TestCreateFleet_ForgePublishFailureStaysRegistering(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := forgeFleetRemote(t, dir)
	readme := gitRev(t, bare, "refs/heads/main")
	m := newFleetHost(t, dir, nil)
	refuse := true
	calls := 0
	setFleetHooks(t, fleetCreateHooks{beforePublish: func() error {
		calls++
		if calls == 1 {
			// A concurrent loop push that entered registered meanwhile.
			require.NoError(t, setFleetState(m.ControlDB(), FleetRegistered, time.Now()))
		}
		if refuse {
			return errors.New("main is protected")
		}
		return nil
	}})

	st, err := m.CreateFleet(ctx, FleetCreateSpec{Origin: &OriginSpec{URL: fileuri.New(bare)}})
	require.NoError(t, err)
	require.Equal(t, FleetRegistering, st.State, "202: main was not published")
	require.Contains(t, st.LastError, "main is protected")
	require.Equal(t, "pending", st.RecordState)
	require.Equal(t, readme, gitRev(t, bare, "refs/heads/main"))

	m.FleetRetry(ctx)
	require.Equal(t, 2, calls, "the loop push tried to publish main")
	st, err = m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, FleetRegistering, st.State, "a push of the agent branch alone does not register the master")
	require.Contains(t, st.LastError, "main is protected")

	refuse = false
	m.FleetRetry(ctx)
	st, err = m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State)
	require.Empty(t, st.LastError)
	require.Equal(t, fact.MemberActive, st.RecordState)
	require.Equal(t, gitRev(t, bare, "refs/heads/agent/test-fleet"), gitRev(t, bare, "refs/heads/main"), "main published")
}

// S3. Hosted here, a main that did not follow the agent branch leaves the
// record pending, which the REST layer answers 202 (createdFleetStatus).
func TestCreateFleet_HostedHereMainNotFollowedIsPending(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	setFleetHooks(t, fleetCreateHooks{beforePublish: func() error { return errors.New("ref lock") }})
	st, err := m.CreateFleet(ctx, FleetCreateSpec{})
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State)
	require.Equal(t, "pending", st.RecordState, "main does not hold the record yet")
}

// N8. Two creates at once, made deterministic: the first is parked inside
// its create (marker written), the second must then WAIT for the fleet lock
// (the contended hook) rather than run. Released, the first wins, the second
// is fleet_present, the winner's root survives.
// Sabotage: drop lockFleetOp from CreateFleet -> the second create runs while
// the first is parked (it returns, or never reports a wait) -> red.
func TestCreateFleet_ConcurrentCreatesOneWins(t *testing.T) {
	ctx := context.Background()
	m := newFleetHost(t, t.TempDir(), nil)
	parked := make(chan struct{})
	release := make(chan struct{})
	waiting := make(chan struct{}, 1)
	first := true
	setFleetHooks(t, fleetCreateHooks{
		step: func(s string) error {
			if s == "marked" && first {
				first = false
				close(parked)
				<-release
			}
			return nil
		},
		contended: func() { waiting <- struct{}{} },
	})

	errA := make(chan error, 1)
	go func() { _, err := m.CreateFleet(ctx, FleetCreateSpec{Name: "fleet-a"}); errA <- err }()
	<-parked
	errB := make(chan error, 1)
	go func() { _, err := m.CreateFleet(ctx, FleetCreateSpec{Name: "fleet-b"}); errB <- err }()
	select {
	case <-waiting:
	case err := <-errB:
		t.Fatalf("the second create ran while the first held the create: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the second create neither waited nor returned")
	}
	close(release)

	require.NoError(t, <-errA, "the first create wins")
	_, code := fleetCode(t, <-errB)
	require.Equal(t, "fleet_present", code)
	require.Equal(t, "fleet-a", m.fleetRepo().Name())
	_, err := pki.LoadRoot(m.fleetRootDir())
	require.NoError(t, err, "the winner's root survives")
	require.Len(t, issuedSerials(t, m.deps.Cfg.Home), 1)
}

// N7. Every operation that changes this instance's fleet membership takes
// the fleet lock: while it is held, each one waits.
// Sabotage: drop lockFleetOp from UnregisterFleet -> its row returns at once
// -> red.
func TestFleetOps_TakeTheFleetLock(t *testing.T) {
	ctx := context.Background()
	for name, op := range map[string]func(m *Manager) error{
		"create": func(m *Manager) error { _, err := m.CreateFleet(ctx, FleetCreateSpec{}); return err },
		"register": func(m *Manager) error {
			_, err := m.RegisterFleet(ctx, "https://forge.invalid/fleet.git", "", "")
			return err
		},
		"unregister": func(m *Manager) error { _, err := m.UnregisterFleet(ctx); return err },
	} {
		t.Run(name, func(t *testing.T) {
			m := newFleetHost(t, t.TempDir(), nil)
			waiting := make(chan struct{}, 1)
			setFleetHooks(t, fleetCreateHooks{contended: func() { waiting <- struct{}{} }})
			m.fleetOpMu.Lock()
			done := make(chan struct{})
			go func() { _ = op(m); close(done) }()
			select {
			case <-waiting:
			case <-done:
				m.fleetOpMu.Unlock()
				t.Fatal("ran without the fleet lock")
			case <-time.After(30 * time.Second):
				t.Fatal("neither waited nor returned")
			}
			m.fleetOpMu.Unlock()
			<-done
		})
	}
}
