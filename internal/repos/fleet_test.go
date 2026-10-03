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

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/platform/fileuri"
	"knomit/internal/store"
	"knomit/internal/testsupport/testsigner"
)

// seedFleetRemote makes a bare fleet repository: main holds the fleet
// ontology preset and nothing else.
func seedFleetRemote(t *testing.T, bare string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(bare, 0o755))
	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)
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

// newFleetManager is a started manager that can sign (its member record is a
// commit) and may use local origins under dir.
func newFleetManager(t *testing.T, dir string) *Manager {
	t.Helper()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: filepath.Join(dir, "home"), OntologyRoot: "kb", LocalOriginRoot: dir},
		AgentBranch: "agent/test-fleet",
		Signer:      testsigner.Named("fleet-test"),
		Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	})
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "home"), 0o755))
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

// mergeOnRemote is the human accepting: merge the instance's agent branch into
// the fleet's main on the remote.
func mergeOnRemote(t *testing.T, bare, agentBranch string) {
	t.Helper()
	work := t.TempDir()
	runGit(t, "", "clone", bare, work)
	runGit(t, work, "-c", "user.email=h@h", "-c", "user.name=h", "merge", "--no-ff", "-m", "accept", "origin/"+agentBranch)
	runGit(t, work, "push", "origin", "main")
}

// R1: register -> the record is on the agent branch on the remote, state
// registered, record pending; the human merges; after a sync the record is
// active. last_error stays empty on success.
func TestFleet_RegisterPendingThenActive(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemote(t, bare)
	m := newFleetManager(t, dir)

	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, FleetStandalone, st.State)
	require.Equal(t, "test-fleet", st.AgentID)

	st, err = m.RegisterFleet(ctx, url, "", "")
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State, "a successful push completes registering")
	require.Equal(t, "pending", st.RecordState, "not merged into the fleet's main yet")
	require.Empty(t, st.LastError)
	require.NotNil(t, st.RegisteredAt)
	agent := m.fleetRepo().AgentBranch()
	rec := remoteRecord(t, bare, agent)
	require.Contains(t, rec, "state: active")
	require.Contains(t, rec, "agent: test-fleet")

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

// R2: unregister writes left, pushes, THEN unmounts; the remote's agent
// branch carries the departure.
func TestFleet_UnregisterPushesThenUnmounts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemote(t, bare)
	m := newFleetManager(t, dir)
	_, err := m.RegisterFleet(ctx, url, "", "")
	require.NoError(t, err)
	agent := m.fleetRepo().AgentBranch()

	st, err := m.UnregisterFleet(ctx)
	require.NoError(t, err)
	require.Equal(t, FleetStandalone, st.State)
	require.Nil(t, m.fleetRepo(), "unmounted after the push")
	require.Contains(t, remoteRecord(t, bare, agent), "state: left", "the departure reached the fleet's origin")
}

// R3: the departure's push fails (remote unreachable): state unregistering,
// still mounted, last_error set, and PUT is refused for ANY fleet; once the
// remote is back, the retry unmounts and returns to standalone.
func TestFleet_UnregisterIsDurable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemote(t, bare)
	other := seedFleetRemote(t, filepath.Join(dir, "other.git"))
	m := newFleetManager(t, dir)
	_, err := m.RegisterFleet(ctx, url, "", "")
	require.NoError(t, err)
	agent := m.fleetRepo().AgentBranch()

	hidden := filepath.Join(dir, "fleet.git.away")
	require.NoError(t, os.Rename(bare, hidden))
	st, err := m.UnregisterFleet(ctx)
	require.NoError(t, err)
	require.Equal(t, FleetUnregistering, st.State)
	require.NotEmpty(t, st.LastError, "the failed push is reported")
	require.NotNil(t, m.fleetRepo(), "never unmounted before the push succeeds")

	for _, u := range []string{url, other} {
		_, err = m.RegisterFleet(ctx, u, "", "")
		status, code := fleetCode(t, err)
		require.Equal(t, http.StatusConflict, status)
		require.Equal(t, "unregistration_pending", code)
	}

	require.NoError(t, os.Rename(hidden, bare))
	m.FleetRetry(ctx)
	st, err = m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, FleetStandalone, st.State)
	require.Empty(t, st.LastError, "cleared by the successful push")
	require.Nil(t, m.fleetRepo())
	require.Contains(t, remoteRecord(t, bare, agent), "state: left")
}

// R3b: while unregistering, archiving the fleet repository by hand is the
// escape hatch: it ends the retries and returns to standalone.
func TestFleet_ArchiveByHandEndsUnregistering(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemote(t, bare)
	m := newFleetManager(t, dir)
	_, err := m.RegisterFleet(ctx, url, "", "")
	require.NoError(t, err)
	name := m.fleetRepo().Name()

	_, err = m.Archive(name)
	status, code := fleetCode(t, err)
	require.Equal(t, http.StatusConflict, status, "a bare archive while registered is refused")
	require.Equal(t, "use_unregister", code)

	require.NoError(t, os.Rename(bare, bare+".away"))
	st, err := m.UnregisterFleet(ctx)
	require.NoError(t, err)
	require.Equal(t, FleetUnregistering, st.State)
	_, err = m.Archive(name)
	require.NoError(t, err)
	st, err = m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, FleetStandalone, st.State)
}

// R4: every other refusal, with its code; a failed clone leaves nothing
// mounted, stays standalone and reports last_error.
func TestFleet_Refusals(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	url := seedFleetRemote(t, filepath.Join(dir, "fleet.git"))
	other := seedFleetRemote(t, filepath.Join(dir, "other.git"))
	kb := seedBareRemoteWithOntology(t, filepath.Join(dir, "kb.git"), true)
	m := newFleetManager(t, dir)

	_, err := m.UnregisterFleet(ctx)
	status, code := fleetCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "not_registered", code)

	_, err = m.RegisterFleet(ctx, kb, "", "")
	status, code = fleetCode(t, err)
	require.Equal(t, http.StatusUnprocessableEntity, status)
	require.Equal(t, "not_a_fleet", code)
	require.Empty(t, m.Names(), "the non-fleet mount is removed")

	_, err = m.RegisterFleet(ctx, fileuri.New(filepath.Join(dir, "missing.git")), "", "")
	status, code = fleetCode(t, err)
	require.Equal(t, http.StatusBadGateway, status)
	require.Equal(t, "clone_failed", code)
	st, err := m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, FleetStandalone, st.State)
	require.NotEmpty(t, st.LastError, "a failed clone is never silent")

	_, err = m.RegisterFleet(ctx, url, "", "")
	require.NoError(t, err)
	st, err = m.FleetStatus(ctx)
	require.NoError(t, err)
	require.Empty(t, st.LastError, "cleared by the successful push")

	_, err = m.RegisterFleet(ctx, other, "", "")
	status, code = fleetCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "already_registered", code)

	_, err = m.RegisterFleet(ctx, url, "", "")
	require.NoError(t, err, "the same fleet again refreshes the record")

	require.NoError(t, setFleetState(m.ControlDB(), FleetRegistering, fixedNow()))
	_, err = m.RegisterFleet(ctx, url, "", "")
	status, code = fleetCode(t, err)
	require.Equal(t, http.StatusConflict, status)
	require.Equal(t, "registration_pending", code)
	require.True(t, strings.HasPrefix(m.fleetRepo().Name(), "fleet"))
}

func fixedNow() time.Time { return time.Unix(1790000000, 0) }

// A failed first push keeps the state registering with last_error set; the
// next successful push (the sync loop's retry) completes it to registered and
// clears the error. Pushes of other repositories change nothing.
func TestFleet_RegisteringRetriedOnPush(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	url := seedFleetRemote(t, filepath.Join(dir, "fleet.git"))
	m := newFleetManager(t, dir)
	_, err := m.RegisterFleet(ctx, url, "", "")
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
