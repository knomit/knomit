package repos

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/platform/version"
	"knomit/internal/testsupport/testsigner"
)

// bootFleet starts a manager on dir's home with cfg shaped by mut, and waits
// for Start's one-shot fleet reconcile to finish. Calling it again on the same
// dir (after closing the previous one) is a restart.
func bootFleet(t *testing.T, dir string, mut func(*config.Config)) (*Manager, func()) {
	t.Helper()
	cfg := config.Config{Home: filepath.Join(dir, "home"), OntologyRoot: "kb", LocalOriginRoot: dir}
	if mut != nil {
		mut(&cfg)
	}
	require.NoError(t, os.MkdirAll(cfg.Home, 0o755))
	m := New(context.Background(), Deps{
		Cfg:                   cfg,
		AgentBranch:           "agent/test-fleet",
		Signer:                testsigner.Named("fleet-test"),
		DisableBackgroundSync: true,
	})
	require.NoError(t, m.Start(), "the fleet reconcile never fails Start")
	m.fleetBootWg.Wait()
	var once sync.Once
	closeFn := func() { once.Do(func() { _ = m.Close() }) }
	t.Cleanup(closeFn)
	return m, closeFn
}

func withAddresses(a ...string) func(*config.Config) {
	return func(c *config.Config) { c.ExternalAddresses = a }
}

// recordVersions lists the commits on rev of the bare fleet remote that
// changed this instance's member record, newest first.
func recordVersions(t *testing.T, bare, rev string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "log", "--format=%H", rev, "--", memberRecordPath("kb", "test-fleet")).Output()
	require.NoError(t, err)
	return strings.Fields(string(out))
}

// remoteHead is the fleet remote's tip of rev.
func remoteHead(t *testing.T, bare, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "rev-parse", rev).Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// commitsBetween counts ALL commits in from..to on the fleet remote — not
// only those that change the record: an identical-content commit is noise
// too, and `git log -- <path>` would not show it.
func commitsBetween(t *testing.T, bare, from, to string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "rev-list", "--count", from+".."+to).Output()
	require.NoError(t, err)
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	require.NoError(t, err)
	return n
}

// captureLog swaps the global logger for a buffer for the rest of the test.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureLog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	old := log.Logger
	log.Logger = zerolog.New(buf).Level(zerolog.DebugLevel)
	t.Cleanup(func() { log.Logger = old })
	return buf
}

// registerAt registers m with the fleet at url and pushes, returning the
// fleet repository's agent branch.
func registerAt(t *testing.T, m *Manager, url string) string {
	t.Helper()
	st, err := m.RegisterFleet(context.Background(), url, "", "")
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State)
	return m.fleetRepo().AgentBranch()
}

// F10-1: registration copies external_addresses verbatim — nothing from
// loopback_hosts, the bind host or the listen port — and an empty list is
// no line in the record, [] and a notice in the status; it still succeeds.
func TestFleetRecord_RegisterCopiesAddressesVerbatim(t *testing.T) {
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemote(t, bare)
	m, _ := bootFleet(t, dir, func(c *config.Config) {
		c.ExternalAddresses = []string{"https://h1v302.tail5113a7.ts.net", "http://10.0.0.5:8080"}
		c.Host, c.Port = "box.lan", "19278"
		c.Auth.LoopbackHosts = []string{"h1v302.tail5113a7.ts.net", "code.example"}
	})
	agent := registerAt(t, m, url)
	rec := remoteRecord(t, bare, agent)
	require.Contains(t, rec, "\naddresses: https://h1v302.tail5113a7.ts.net http://10.0.0.5:8080\n")
	require.NotContains(t, rec, "19278", "the listen port is not an address")
	require.NotContains(t, rec, "code.example", "loopback_hosts are not addresses")
	require.NotContains(t, rec, "box.lan", "the bind host is not an address")
	st, err := m.FleetStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"https://h1v302.tail5113a7.ts.net", "http://10.0.0.5:8080"}, st.ExternalAddresses)
	require.Empty(t, st.Notice)

	dir2 := t.TempDir()
	bare2 := filepath.Join(dir2, "fleet.git")
	url2 := seedFleetRemote(t, bare2)
	m2, _ := bootFleet(t, dir2, func(c *config.Config) {
		c.Host, c.Port = "box.lan", "19278"
		c.Auth.LoopbackHosts = []string{"h1v302.tail5113a7.ts.net"}
	})
	agent2 := registerAt(t, m2, url2)
	rec2 := remoteRecord(t, bare2, agent2)
	require.NotContains(t, rec2, "addresses:", "nothing configured, nothing advertised")
	st2, err := m2.FleetStatus(context.Background())
	require.NoError(t, err)
	require.NotNil(t, st2.ExternalAddresses)
	require.Empty(t, st2.ExternalAddresses)
	require.Equal(t, NoAddressesNotice, st2.Notice)
	require.NotNil(t, st2.Record)
	require.Equal(t, []string{}, st2.Record.Addresses)
}

// F10-2: a restart with nothing changed writes nothing and logs "current";
// a restart after external_addresses changed writes exactly ONE new version
// with the new list, state untouched, and logs which fields changed.
func TestFleetRecord_BootReconcile(t *testing.T) {
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemote(t, bare)
	m, closeM := bootFleet(t, dir, withAddresses("https://old.example"))
	agent := registerAt(t, m, url)
	mergeOnRemote(t, bare, agent)
	syncFleet(t, m)
	m.FleetRetry(context.Background())
	before := recordVersions(t, bare, agent)
	require.Len(t, before, 1)
	head0 := remoteHead(t, bare, agent)
	closeM()

	logs := captureLog(t)
	m, closeM = bootFleet(t, dir, withAddresses("https://old.example"))
	m.FleetRetry(context.Background())
	require.Equal(t, head0, remoteHead(t, bare, agent), "an unchanged record: no commit at all")
	require.Contains(t, logs.String(), "fleet record current")
	require.NotContains(t, logs.String(), "fleet record updated")
	closeM()

	m, _ = bootFleet(t, dir, withAddresses("https://new.example", "https://new2.example:8443"))
	m.FleetRetry(context.Background())
	require.Equal(t, 1, commitsBetween(t, bare, head0, agent), "exactly one update commit")
	require.Equal(t, head0, remoteHead(t, bare, agent+"^"))
	after := recordVersions(t, bare, agent)
	require.Len(t, after, 2)
	require.Equal(t, before[0], after[1])
	rec := remoteRecord(t, bare, agent)
	require.Contains(t, rec, "\naddresses: https://new.example https://new2.example:8443\n")
	require.Contains(t, rec, "\nstate: active\n", "state is not an advertised field")
	require.Contains(t, logs.String(), "fleet record updated: addresses")

	// The update is pending until merged into the fleet's main.
	st, err := m.FleetStatus(context.Background())
	require.NoError(t, err)
	require.True(t, st.RecordPendingUpdate)
	require.NotNil(t, st.RecordCurrent)
	require.True(t, *st.RecordCurrent, "the agent head now matches the config")
	mergeOnRemote(t, bare, agent)
	syncFleet(t, m)
	st, err = m.FleetStatus(context.Background())
	require.NoError(t, err)
	require.False(t, st.RecordPendingUpdate, "merged: nothing pending")
}

// F10-3: the reconcile keeps state as the record has it, whatever it is.
// Only registering and unregistering change state.
func TestFleetRecord_RefreshKeepsStateAndNotes(t *testing.T) {
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemote(t, bare)
	m, _ := bootFleet(t, dir, withAddresses("https://old.example"))
	registerAt(t, m, url)
	ri := m.fleetRepo()
	cur, found, err := m.ownRecordAt(ri, ri.AgentBranch())
	require.NoError(t, err)
	require.True(t, found)
	// A record whose state the operator (or an older writer) set otherwise,
	// with notes: the refresh must carry both over untouched.
	odd := cur.Member
	odd.State, odd.Notes = fact.MemberRevoked, "revoked by the operator on 2026-09-28"
	require.NoError(t, m.writeRecord(context.Background(), ri, odd, "test: odd state", cur.Blob))

	m.deps.Cfg.ExternalAddresses = []string{"https://new.example"}
	changed, err := m.refreshOwnRecord(context.Background(), ri)
	require.NoError(t, err)
	require.Equal(t, []string{"addresses"}, changed)
	got, _, err := m.ownRecordAt(ri, ri.AgentBranch())
	require.NoError(t, err)
	require.Equal(t, fact.MemberRevoked, got.State)
	require.Equal(t, "revoked by the operator on 2026-09-28", got.Notes)
	require.Equal(t, []string{"https://new.example"}, got.Addresses)
}

// F10-4: a new knomit version refreshes the capabilities at the next boot.
func TestFleetRecord_VersionChangeRefreshesCapabilities(t *testing.T) {
	orig, origCommit := version.Version, version.Commit
	t.Cleanup(func() { version.Version, version.Commit = orig, origCommit })
	version.Version, version.Commit = "0.5.0", "aaaaaaa"

	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemote(t, bare)
	m, closeM := bootFleet(t, dir, withAddresses("https://x.example"))
	agent := registerAt(t, m, url)
	require.Contains(t, remoteRecord(t, bare, agent), "version=0.5.0.aaaaaaa")
	head0 := remoteHead(t, bare, agent)
	closeM()

	version.Version, version.Commit = "0.6.0", "bbbbbbb"
	m, _ = bootFleet(t, dir, withAddresses("https://x.example"))
	m.FleetRetry(context.Background())
	require.Equal(t, 1, commitsBetween(t, bare, head0, agent))
	rec := remoteRecord(t, bare, agent)
	require.Contains(t, rec, "version=0.6.0.bbbbbbb")
	require.NotContains(t, rec, "0.5.0")
}

// F10-5: os and arch come from the Go runtime (through hostPlatform), not
// from a constant: the default IS runtime.GOOS/GOARCH, and whatever it
// returns is what the record carries.
func TestFleetRecord_OSArchFromRuntime(t *testing.T) {
	goos, goarch := hostPlatform()
	require.Equal(t, [2]string{runtime.GOOS, runtime.GOARCH}, [2]string{goos, goarch})

	orig := hostPlatform
	t.Cleanup(func() { hostPlatform = orig })
	hostPlatform = func() (string, string) { return "plan9", "mips64le" }
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemote(t, bare)
	m, _ := bootFleet(t, dir, func(c *config.Config) { c.Git.Serve = true })
	agent := registerAt(t, m, url)
	rec := remoteRecord(t, bare, agent)
	require.Contains(t, rec, "\ncapabilities: arch=mips64le os=plan9 read_only=false version="+version.String()+"\n")
	require.Contains(t, rec, "\ngit: /git\n", "git.serve mounts /git")
}

// F10-6: git: /git only when this instance mounts /git.
func TestFleetRecord_GitOnlyWhenServed(t *testing.T) {
	m := &Manager{deps: Deps{AgentBranch: "agent/x"}}
	require.Empty(t, m.advertised().Git, "git.serve off")
	m.deps.Cfg.Git.Serve = true
	require.Equal(t, "/git", m.advertised().Git)
	m.deps.Cfg.ReadOnly = true
	require.Empty(t, m.advertised().Git, "read-only never mounts /git")
	require.Equal(t, "true", m.advertised().Capabilities["read_only"])
}

// F10-7: standalone and unregistering boots write nothing, even with a
// changed config.
func TestFleetRecord_BootWritesNothingUnlessRegistered(t *testing.T) {
	// Standalone: nothing mounted, nothing to write, Start is fine.
	logs := captureLog(t)
	bootFleet(t, t.TempDir(), withAddresses("https://x.example"))
	require.NotContains(t, logs.String(), "fleet record")

	// Unregistering: the departure's push failed; the restart with a new
	// address must not write an update on top of the "left" record.
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemote(t, bare)
	m, closeM := bootFleet(t, dir, withAddresses("https://old.example"))
	agent := registerAt(t, m, url)
	head0 := remoteHead(t, bare, agent)
	require.NoError(t, os.Rename(bare, bare+".away"))
	st, err := m.UnregisterFleet(context.Background())
	require.NoError(t, err)
	require.Equal(t, FleetUnregistering, st.State)
	closeM()

	m, _ = bootFleet(t, dir, withAddresses("https://new.example"))
	require.NoError(t, os.Rename(bare+".away", bare))
	m.FleetRetry(context.Background())
	require.Equal(t, 1, commitsBetween(t, bare, head0, agent), "the departure only, no boot update")
	rec := remoteRecord(t, bare, agent)
	require.Contains(t, rec, "state: left")
	require.Contains(t, rec, "addresses: https://old.example")
}

// F10-8: a read-only instance never reconciles (it never pushes).
func TestFleetRecord_ReadOnlyBootWritesNothing(t *testing.T) {
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemote(t, bare)
	m, closeM := bootFleet(t, dir, withAddresses("https://old.example"))
	agent := registerAt(t, m, url)
	ri := m.fleetRepo()
	before, _, err := m.ownRecordAt(ri, agent)
	require.NoError(t, err)
	closeM()

	logs := captureLog(t)
	m, _ = bootFleet(t, dir, func(c *config.Config) { c.ExternalAddresses = []string{"https://new.example"}; c.ReadOnly = true })
	after, _, err := m.ownRecordAt(m.fleetRepo(), agent)
	require.NoError(t, err)
	require.Equal(t, before.Blob, after.Blob)
	require.Contains(t, logs.String(), "read-only")
}

// F10-9: the fleet repository cannot be opened at boot: WARN, startup
// completes, nothing else changes.
func TestFleetRecord_UnopenableFleetRepoWarns(t *testing.T) {
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemote(t, bare)
	m, closeM := bootFleet(t, dir, withAddresses("https://old.example"))
	registerAt(t, m, url)
	uid := m.fleetRepo().UID()
	dbPath := m.RepoPath(uid)
	closeM()
	require.NoError(t, os.Rename(dbPath, dbPath+".away"))

	logs := captureLog(t)
	m, _ = bootFleet(t, dir, withAddresses("https://new.example"))
	require.Nil(t, m.fleetRepo())
	require.Contains(t, logs.String(), `"level":"warn"`)
	require.Contains(t, logs.String(), "fleet repository is not open")
	st, err := m.FleetStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State, "the state machine is untouched")
}

// F10-10: re-registering with the same fleet runs the same comparison: no
// commit when current, ONE when a field changed.
func TestFleetRecord_ReRegisterCompares(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemote(t, bare)
	m, _ := bootFleet(t, dir, withAddresses("https://old.example"))
	agent := registerAt(t, m, url)
	head0 := remoteHead(t, bare, agent)

	registerAt(t, m, url)
	require.Equal(t, head0, remoteHead(t, bare, agent), "unchanged: no commit at all")

	m.deps.Cfg.ExternalAddresses = []string{"https://new.example"}
	st, err := m.RegisterFleet(ctx, url, "", "")
	require.NoError(t, err)
	require.Equal(t, FleetRegistered, st.State)
	require.Equal(t, 1, commitsBetween(t, bare, head0, agent), "changed: one commit")
	require.Contains(t, remoteRecord(t, bare, agent), "addresses: https://new.example")
}

// F10-11: the record stays ONE policy fact at one path; earlier addresses
// are earlier versions of it, never a list kept inside the fact.
func TestFleetRecord_HistoryIsVersions(t *testing.T) {
	dir := t.TempDir()
	bare := filepath.Join(dir, "fleet.git")
	url := seedFleetRemote(t, bare)
	m, closeM := bootFleet(t, dir, withAddresses("https://a.example"))
	agent := registerAt(t, m, url)
	closeM()
	m, _ = bootFleet(t, dir, withAddresses("https://b.example"))
	m.FleetRetry(context.Background())

	vs := recordVersions(t, bare, agent)
	require.Len(t, vs, 2)
	oldRec, err := exec.Command("git", "-C", bare, "show", vs[1]+":"+memberRecordPath("kb", "test-fleet")).Output()
	require.NoError(t, err)
	require.Contains(t, string(oldRec), "addresses: https://a.example")
	cur := remoteRecord(t, bare, agent)
	require.Contains(t, cur, "addresses: https://b.example")
	require.NotContains(t, cur, "a.example", "no history inside the fact")
	require.Contains(t, cur, "type: policy")

	out, err := exec.Command("git", "-C", bare, "ls-tree", "-r", "--name-only", agent, "kb/members/").Output()
	require.NoError(t, err)
	require.Equal(t, []string{memberRecordPath("kb", "test-fleet")}, strings.Fields(string(out)), "one record file")
}
