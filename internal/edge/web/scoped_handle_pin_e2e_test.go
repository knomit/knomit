package web

// On a URL-scoped endpoint a binding handle's PIN (binding_handles.binding,
// what resolveHandle routes by) and its MOUNT (handle_mounts.mount_uid, the
// endpoint it works on) are one value. resolveHandle checks only the mount, so
// a handle whose pin named another knowledge base would be routed there: the
// 2026-09-17 cross-binding incident on a scoped endpoint. resolveHandle used to
// re-check pin != mount "anyway"; that check was unreachable and is gone, and
// this test is what stands in for it.
//
// It drives every path that creates a scoped handle, moves one, or changes
// what its pin or mount refers to, through the real router, and after each one
// sweeps the WHOLE control store for a handle whose pin is not its mount — not
// only the handles this test holds, so a path that minted one behind its back
// is caught too.

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/corpus/repos/control"
	"knomit/internal/engine/store"
	storemigrate "knomit/internal/engine/store/migrate"
	"knomit/internal/foundation/clientsessions"
	"knomit/internal/foundation/config"
	"knomit/internal/testsupport/testsigner"
)

// openControlStore opens (or reopens) the client-session store on path, the
// way the server does at start: same file, migrations replayed.
func openControlStore(t *testing.T, path string) (*clientsessions.Store, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL"+store.SyncDSNParam())
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, storemigrate.Control(db))
	return clientsessions.New(db, clientsessions.Policy{DeadAfter: time.Hour, HiddenAfter: 3 * time.Hour, Retention: 168 * time.Hour}), db
}

// serverOn builds the MCP router over m with st as its session store: a server
// (re)start that keeps the repos and the control DB.
func serverOn(m *repos.Manager, st *clientsessions.Store) http.Handler {
	m.SetClientSessions(st)
	s := &Server{Manager: m, ClientSessions: st, OntologyRoot: "kb"}
	s.buildMCPHandler()
	return s.NewAPIRouter()
}

// requireNoHandleOffItsMount fails if ANY scoped handle in the store has a pin
// other than its mount. The count of scoped handles is asserted too, so a sweep
// over an empty table cannot pass for one over the handles minted.
func requireNoHandleOffItsMount(t *testing.T, db *sql.DB, wantScoped int, step string) {
	t.Helper()
	var scoped, off int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(b.binding <> m.mount_uid), 0)
		   FROM handle_mounts m JOIN binding_handles b ON b.handle = m.handle`).Scan(&scoped, &off))
	require.Equal(t, wantScoped, scoped, "%s: scoped handles in the store", step)
	if off == 0 {
		return
	}
	rows, err := db.Query(`SELECT b.binding, m.mount_uid FROM handle_mounts m JOIN binding_handles b ON b.handle = m.handle
		WHERE b.binding <> m.mount_uid`)
	require.NoError(t, err)
	defer rows.Close()
	var bad []string
	for rows.Next() {
		var pin, mount string
		require.NoError(t, rows.Scan(&pin, &mount))
		bad = append(bad, pin+" on "+mount)
	}
	t.Fatalf("%s: %d scoped handle(s) pinned off their mount: %v", step, off, bad)
}

// requirePinIs asserts one handle's stored pin and mount are both want.
func requirePinIs(t *testing.T, st *clientsessions.Store, handle, want, step string) {
	t.Helper()
	pin, _, ok, err := st.BindingHandle(context.Background(), handle, time.Now())
	require.NoError(t, err)
	require.True(t, ok, "%s: handle missing", step)
	mount, err := st.HandleMount(context.Background(), handle)
	require.NoError(t, err)
	require.Equal(t, want, pin, "%s: pin", step)
	require.Equal(t, want, mount, "%s: mount", step)
}

func TestScopedHandle_PinIsItsMountOnEveryPath(t *testing.T) {
	ctx := context.Background()
	m := repos.New(ctx, repos.Deps{
		Cfg:     config.Config{Home: t.TempDir(), OntologyRoot: "kb"},
		AgentID: testsigner.FixtureAgentID("agent/test"), AgentBranch: "agent/test",
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	newE2EMount(t, m, "jobA-repo", false)
	newE2EMount(t, m, "jobB-repo", false)

	controlPath := filepath.Join(t.TempDir(), "control.db")
	st, db := openControlStore(t, controlPath)
	h := serverOn(m, st)

	lens, err := m.LensRegistry().Create(control.Lens{
		Name: "pair", WriteUID: m.Get("jobA-repo").UID(),
		Reads: []control.LensRead{{RepoUID: m.Get("jobA-repo").UID()}, {RepoUID: m.Get("jobB-repo").UID()}},
	})
	require.NoError(t, err)
	repoPin, lensPin := "repo:"+m.Get("jobA-repo").UID(), "lens:"+lens.UID

	repoM, lensM := repoMount(t, m, "jobA-repo"), "/lenses/pair/mcp"
	sidR, sidL := initAt(t, h, repoM), initAt(t, h, lensM)
	scoped := 0
	sweep := func(step string) { t.Helper(); requireNoHandleOffItsMount(t, db, scoped, step) }

	// knomit_bind, no arguments, on each kind of scoped endpoint.
	hR := bindAt(t, h, repoM, sidR, `{}`)
	hL := bindAt(t, h, lensM, sidL, `{}`)
	scoped += 2
	requirePinIs(t, st, hR, repoPin, "bind on the repo mount")
	requirePinIs(t, st, hL, lensPin, "bind on the lens mount")
	sweep("bind")

	// knomit_bind naming the URL's own repo / lens (accepted), and resuming an
	// existing experiment.
	bindAt(t, h, repoM, sidR, `{"repo":"jobA-repo"}`)
	bindAt(t, h, lensM, sidL, `{"lens":"pair"}`)
	openOutOfBand(t, m, "jobA-repo", "resume-me")
	hX := bindAt(t, h, repoM, sidR, `{"experiment":"resume-me"}`)
	scoped += 3
	requirePinIs(t, st, hX, repoPin, "bind resuming an experiment")
	sweep("bind naming the target / resuming")

	// knomit_experiment open WITHOUT a handle mints one (#686).
	hO := openNoHandle(t, h, repoM, sidR, "minted-r")
	hOL := openNoHandle(t, h, lensM, sidL, "minted-l")
	scoped += 2
	requirePinIs(t, st, hO, repoPin, "open-minted on the repo mount")
	requirePinIs(t, st, hOL, lensPin, "open-minted on the lens mount")
	sweep("open without a handle")

	// open THROUGH a handle moves it; sync, commit and rollback move it again.
	// None may mint, and none may touch the pin.
	for _, step := range []struct{ mount, sid, handle, args string }{
		{repoM, sidR, hR, `{"action":"open","name":"r-work"}`},
		{repoM, sidR, hR, `{"action":"sync"}`},
		{repoM, sidR, hR, `{"action":"commit"}`},
		{lensM, sidL, hL, `{"action":"open","name":"l-work"}`},
		{lensM, sidL, hL, `{"action":"rollback"}`},
		{repoM, sidR, hR, `{"action":"commit","name":"minted-r"}`},
	} {
		_, text, isErr := callExperiment(t, h, step.mount, step.sid, withBinding(step.handle, step.args))
		require.False(t, isErr, "%s: %s", step.args, text)
		sweep("experiment " + step.args)
	}
	requirePinIs(t, st, hR, repoPin, "after open/sync/commit through the handle")
	requirePinIs(t, st, hL, lensPin, "after open/rollback through the handle")

	// An operator moves the lens's WRITE repo while hL is live. The pin is the
	// lens's uid, not its write repo's, so nothing about the handle changes:
	// it follows the lens to the new write repo, exactly as a handle-less call
	// does, and is not refused.
	updated := lens
	updated.WriteUID = m.Get("jobB-repo").UID()
	_, err = m.LensRegistry().Update(updated)
	require.NoError(t, err)
	learnVia(t, h, lensM, sidL, hL, "after write-repo move via handle")
	require.True(t, branchHolding(t, m, "jobB-repo", "agent/test", "after write-repo move via handle"),
		"the handle must follow the lens to its new write repo")
	require.False(t, branchHolding(t, m, "jobA-repo", "agent/test", "after write-repo move via handle"),
		"the handle wrote to the lens's OLD write repo")
	requirePinIs(t, st, hL, lensPin, "after the lens's write repo moved")
	hL2 := bindAt(t, h, lensM, sidL, `{}`)
	scoped++
	requirePinIs(t, st, hL2, lensPin, "bind after the lens's write repo moved")
	sweep("lens write-repo change")

	// The lens is renamed while hL is live: the URL changes, the uid does not,
	// so the handle works on the renamed endpoint.
	changed, err := m.LensRegistry().Rename(lens.UID, "pair", "pair2")
	require.NoError(t, err)
	require.True(t, changed)
	lensM = "/lenses/pair2/mcp"
	sidL = initAt(t, h, lensM)
	learnVia(t, h, lensM, sidL, hL, "after rename via handle")
	require.True(t, branchHolding(t, m, "jobB-repo", "agent/test", "after rename via handle"))
	sweep("lens rename")

	// SERVER RESTART: the control DB is closed and reopened by a new store and a
	// new router; every MCP session is new. The persisted handles route as
	// before, and the reopened store holds no handle off its mount.
	require.NoError(t, db.Close())
	st, db = openControlStore(t, controlPath)
	h = serverOn(m, st)
	sweep("after restart")
	sidR, sidL = initAt(t, h, repoM), initAt(t, h, lensM)
	learnVia(t, h, repoM, sidR, hR, "after restart via repo handle")
	learnVia(t, h, lensM, sidL, hL, "after restart via lens handle")
	require.True(t, branchHolding(t, m, "jobA-repo", "agent/test", "after restart via repo handle"))
	require.True(t, branchHolding(t, m, "jobB-repo", "agent/test", "after restart via lens handle"))
	requirePinIs(t, st, hR, repoPin, "repo handle after restart")
	requirePinIs(t, st, hL, lensPin, "lens handle after restart")
	hAfter := bindAt(t, h, repoM, sidR, `{}`)
	scoped++
	requirePinIs(t, st, hAfter, repoPin, "bind after restart")
	sweep("bind after restart")

	// The lens is deleted and recreated under the same name: same URL, new uid.
	// The old handle's pin AND mount both name the old uid, so it is refused by
	// the mount check — the case a pin check would have had to catch is the
	// mount check's.
	require.NoError(t, m.LensRegistry().Delete("pair2"))
	_, err = m.LensRegistry().Create(control.Lens{
		Name: "pair2", WriteUID: m.Get("jobA-repo").UID(),
		Reads: []control.LensRead{{RepoUID: m.Get("jobA-repo").UID()}},
	})
	require.NoError(t, err)
	sidL = initAt(t, h, lensM)
	text, isErr := callToolAt(t, h, lensM, sidL, "knomit_learn", learnArgs(hL, "demo", "stale lens handle"))
	require.True(t, isErr, "a handle of the deleted lens must be refused, got %s", text)
	require.Equal(t, fmt.Sprintf("this binding handle belongs to another endpoint — omit `binding` to use this endpoint's %s %s, "+
		"or call knomit_bind here for a handle", "lens", "pair2"), text)
	require.False(t, branchHolding(t, m, "jobA-repo", "agent/test", "stale lens handle"))
	sweep("lens recreated")
}
