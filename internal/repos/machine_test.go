package repos

import (
	"context"
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/platform/fileuri"
	"knomit/internal/store"
)

// ---------------------------------------------------------------- fixtures

// gatedEmbedder embeds like testEmbedder until it is armed; then every batch
// blocks until released or until its ctx ends. A Rebuild holds lockBranch for
// its whole run, so an armed embedder is how a test makes the index job HOLD
// a branch lock — the shape the machine's cancel-all-then-drain exists for.
type gatedEmbedder struct {
	testEmbedder
	armed   atomic.Bool
	once    sync.Once
	started chan struct{}
	release chan struct{}
	relOnce sync.Once
}

// unblock lets every held and later batch through. Idempotent.
func (e *gatedEmbedder) unblock() { e.relOnce.Do(func() { close(e.release) }) }

func newGatedEmbedder() *gatedEmbedder {
	return &gatedEmbedder{started: make(chan struct{}), release: make(chan struct{})}
}

func (e *gatedEmbedder) EmbedDocuments(ctx context.Context, titles, bodies []string) ([][]float32, error) {
	if e.armed.Load() {
		e.once.Do(func() { close(e.started) })
		select {
		case <-e.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return e.testEmbedder.EmbedDocuments(ctx, titles, bodies)
}

func (e *gatedEmbedder) EmbedShortStrings(ctx context.Context, texts []string) ([][]float32, error) {
	return e.EmbedDocuments(ctx, texts, make([]string, len(texts)))
}

func (e *gatedEmbedder) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-e.started:
	case <-time.After(30 * time.Second):
		t.Fatal("the index job never reached the embedder")
	}
}

// armedHook is an Options.Hook that forwards to g only once armed, so the
// mount a fixture runs first is not held.
type armedHook struct {
	armed atomic.Bool
	g     *gate
}

func (h *armedHook) hook(s StageID, p string, ctx context.Context) {
	if h.armed.Load() {
		h.g.hook(s, p, ctx)
	}
}

// mfix is one manager with one created repo, a local-origin root, an embedder
// that can be made to hold the index job inside a branch lock, and a hook that
// can be armed to hold one stage point.
type mfix struct {
	m    *Manager
	ri   *RepoInstance
	root string
	emb  *gatedEmbedder
	hk   *armedHook
}

func newMFix(t *testing.T, stage StageID, point string, synchronous bool) *mfix {
	t.Helper()
	f := &mfix{root: t.TempDir(), emb: newGatedEmbedder(), hk: &armedHook{g: newGate(stage, point)}}
	home := t.TempDir()
	f.m = New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: f.root},
		AgentBranch: "agent/test",
		KeyPath:     filepath.Join(home, "agent.key"),
		Embedder:    f.emb,
		Machine:     Options{Synchronous: synchronous, CrashBackoff: testCrashBackoff, Hook: f.hk.hook},
	})
	require.NoError(t, f.m.Start())
	t.Cleanup(func() { f.hk.g.open(); f.emb.unblock(); _ = f.m.Close() })
	f.ri = createRepo(t, f.m, "kb")
	return f
}

// holdIndex restarts Index as a full rebuild of the agent branch, held at the
// index-job hook.
func (f *mfix) holdIndex(t *testing.T) Reply {
	t.Helper()
	f.hk.armed.Store(true)
	rep, err := f.m.Send(context.Background(), f.ri, Rebuild(f.ri.AgentBranch()))
	require.NoError(t, err)
	f.hk.g.waitArrived(t)
	require.True(t, f.ri.Status().IndexRunning())
	return rep
}

// remote seeds a bare file:// remote carrying the default ontology (the repo's
// own), under the fixture's local-origin root.
func (f *mfix) remote(t *testing.T, name string) string {
	t.Helper()
	return seedBareRemote(t, filepath.Join(f.root, name+".git"))
}

// copyOfOwnDB is a copy of the repo's own database, a valid swap source with
// the same root commit.
func (f *mfix) copyOfOwnDB(t *testing.T) string {
	t.Helper()
	require.NoError(t, f.ri.WithRead(func(s *store.Service) { require.NoError(t, s.Checkpoint()) }))
	tmp := filepath.Join(t.TempDir(), "copy.db")
	copyDB(t, f.m.RepoPath(f.ri.UID()), tmp)
	return tmp
}

// clearVectors empties facts_vec underneath the live store, so the next
// Rebuild re-embeds every fact (through the gated embedder).
func clearVectors(t *testing.T, m *Manager, ri *RepoInstance) {
	t.Helper()
	raw, err := sql.Open("sqlite3", m.RepoPath(ri.UID()))
	require.NoError(t, err)
	_, err = raw.Exec(`DELETE FROM facts_vec`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
}

func send(m *Manager, ri *RepoInstance, e MachineEvent) (Reply, error) {
	return m.Send(context.Background(), ri, e)
}

// ------------------------------------------------------- transition table

// TestMachine_TransitionTable walks the spec's transition table: every state
// a repo can be in, every event it can be sent, the refusal or the actions.
func TestMachine_TransitionTable(t *testing.T) {
	t.Parallel()
	t.Run("store absent (Populate in flight) refuses every change with ErrNotOpen", func(t *testing.T) {
		f := newMFix(t, StagePopulate, "enter", true)
		f.m.Remove("kb") // unmount; the database stays on disk
		f.hk.armed.Store(true)
		ri := f.m.newInstance("kb", f.ri.UID(), false)
		mounted := make(chan error, 1)
		go func() { _, err := send(f.m, ri, Mount(MountSpec{})); mounted <- err }()
		f.hk.g.waitArrived(t)
		require.Equal(t, "populate", ri.Status().Stage)
		for _, e := range []MachineEvent{
			AttachOrigin(OriginSpec{URL: f.remote(t, "r")}), DetachOrigin(),
			SwapStore(SwapSpec{TempDB: "x"}), Rebuild(ri.AgentBranch()), CancelIndex(),
		} {
			_, err := send(f.m, ri, e)
			require.ErrorIs(t, err, ErrNotOpen)
		}
		_, _, aerr := ri.Acquire()
		require.ErrorIs(t, aerr, ErrStoreUnavailable)
		require.ErrorContains(t, aerr, "repo is populating")
		f.hk.g.open()
		require.NoError(t, <-mounted)
		require.Equal(t, "ready", ri.Status().Stage)
		unmount(ri, "test")
	})

	t.Run("ready, index running", func(t *testing.T) {
		f := newMFix(t, StageIndex, "index-job", true)
		url := f.remote(t, "r")
		first := f.holdIndex(t)
		gens := f.ri.Status().gens

		for name, e := range map[string]MachineEvent{
			"attach": AttachOrigin(OriginSpec{URL: url, Branch: "main"}),
			"detach": DetachOrigin(),
			"swap":   SwapStore(SwapSpec{TempDB: f.copyOfOwnDB(t)}),
		} {
			_, err := send(f.m, f.ri, e)
			require.ErrorIs(t, err, ErrIndexing, name)
		}
		require.Equal(t, gens, f.ri.Status().gens, "a refused event touches nothing")

		// An identical rebuild is absorbed: the same job id, nothing restarted.
		rep, err := send(f.m, f.ri, Rebuild(f.ri.AgentBranch()))
		require.NoError(t, err)
		require.True(t, rep.Absorbed)
		require.Equal(t, first.JobID, rep.JobID)
		require.Equal(t, gens, f.ri.Status().gens)

		// A rebuild of another branch replaces the running job.
		rep, err = send(f.m, f.ri, Rebuild("main"))
		require.NoError(t, err)
		require.False(t, rep.Absorbed)
		require.NotEqual(t, first.JobID, rep.JobID)
		after := f.ri.Status()
		require.NotEqual(t, gens[StageIndex], after.gens[StageIndex], "Index restarted")
		require.Equal(t, gens[StageServe], after.gens[StageServe], "Serve untouched")
		require.Equal(t, gens[StageSync], after.gens[StageSync], "Sync untouched")

		// CancelIndex exits Index only; the result is cancelled.
		rep, err = send(f.m, f.ri, CancelIndex())
		require.NoError(t, err)
		require.Equal(t, IndexStateError, rep.Status.Index.State)
		require.Equal(t, "indexing cancelled", rep.Status.Index.Reason)
		require.Zero(t, rep.Status.gens[StageIndex], "Index exited, not re-entered")
		require.Equal(t, gens[StageServe], rep.Status.gens[StageServe])
		require.Equal(t, gens[StageSync], rep.Status.gens[StageSync])
		require.Equal(t, "ready", rep.Status.Stage, "the repo stays usable")
	})

	t.Run("ready, index idle", func(t *testing.T) {
		f := newMFix(t, StageIndex, "index-job", true)
		url := f.remote(t, "r")
		gens := f.ri.Status().gens

		rep, err := send(f.m, f.ri, CancelIndex())
		require.NoError(t, err, "CancelIndex with no job running is a no-op")
		require.Equal(t, gens, rep.Status.gens)
		require.Equal(t, IndexStateReady, rep.Status.Index.State)

		// Attach and Detach restart Sync alone.
		rep, err = send(f.m, f.ri, AttachOrigin(OriginSpec{URL: url, Branch: "main"}))
		require.NoError(t, err)
		require.Equal(t, url, rep.Status.Sync.Origin)
		for _, k := range []StageID{StageOpen, StageIdentify, StageIndex, StageServe} {
			require.Equal(t, gens[k], rep.Status.gens[k], "attach must not touch %s", stageName(k))
		}
		require.NotEqual(t, gens[StageSync], rep.Status.gens[StageSync])
		gens = rep.Status.gens
		rep, err = send(f.m, f.ri, DetachOrigin())
		require.NoError(t, err)
		require.Empty(t, rep.Status.Sync.Origin, "the local loop now")
		require.Equal(t, gens[StageServe], rep.Status.gens[StageServe])
		require.NotEqual(t, gens[StageSync], rep.Status.gens[StageSync])

		// Swap rewinds to Populate: every stage re-entered.
		gens = rep.Status.gens
		rep, err = send(f.m, f.ri, SwapStore(SwapSpec{TempDB: f.copyOfOwnDB(t)}))
		require.NoError(t, err)
		for k := StageOpen; k < StageReady; k++ {
			require.NotEqual(t, gens[k], rep.Status.gens[k], "swap re-enters %s", stageName(k))
		}
		require.Equal(t, "ready", rep.Status.Stage)
	})

	t.Run("unavailable refuses all but Mount, which retries", func(t *testing.T) {
		m, url, _, upstream := subscriptionStore(t, "uid-u")
		require.NoError(t, m.Origins().Set("uid-u", Origin{URL: url, Branch: upstream, Mode: OriginModeSubscribe}))
		_, err := m.Repos().DB().Exec(`UPDATE repo_origins SET branch = '' WHERE repo_uid = ?`, "uid-u")
		require.NoError(t, err)
		ri := m.newInstance("u", "uid-u", true)
		t.Cleanup(func() { unmount(ri, "test") })

		_, err = send(m, ri, Mount(MountSpec{}))
		var se *StageError
		require.ErrorAs(t, err, &se)
		require.Equal(t, StageOpen, se.Stage)
		st := ri.Status()
		require.Equal(t, "unavailable", st.Stage)
		require.Zero(t, st.gens, "a failed walk leaves nothing entered")
		_, err = send(m, ri, Rebuild(upstream))
		require.ErrorIs(t, err, ErrUnavailable)

		require.NoError(t, m.Origins().Set("uid-u", Origin{URL: url, Branch: upstream, Mode: OriginModeSubscribe}))
		_, err = send(m, ri, Mount(MountSpec{}))
		require.NoError(t, err, "Mount retries an unavailable repo")
		require.Equal(t, "ready", ri.Status().Stage)
	})

	t.Run("closed refuses everything; Unmount is idempotent", func(t *testing.T) {
		f := newMFix(t, StageIndex, "index-job", true)
		ri := f.ri
		f.m.Remove("kb")
		require.Equal(t, "closed", ri.Status().Stage)
		for _, e := range []MachineEvent{Mount(MountSpec{}), DetachOrigin(), Rebuild(ri.AgentBranch()), CancelIndex()} {
			_, err := send(f.m, ri, e)
			require.ErrorIs(t, err, ErrClosed)
		}
		_, err := send(f.m, ri, Unmount("again"))
		require.NoError(t, err)
		_, _, aerr := ri.Acquire()
		require.ErrorIs(t, aerr, ErrRepoClosed)
	})

	t.Run("a stale reenter is ignored", func(t *testing.T) {
		f := newMFix(t, StageIndex, "index-job", true)
		gens := f.ri.Status().gens
		f.ri.machine.post(reenter{stage: StageServe, gen: gens[StageServe]})        // no pending restart
		f.ri.machine.post(drained{stage: StageServe, gen: gens[StageServe] + 1000}) // unknown gen
		f.ri.machine.post(crashed{stage: StageServe, gen: gens[StageServe] + 1000}) // stale crash
		// A round trip through the driver: everything posted before it was
		// handled before the reply.
		_, err := send(f.m, f.ri, CancelIndex())
		require.NoError(t, err)
		require.Equal(t, gens, f.ri.Status().gens, "stale internal events change nothing")
		require.True(t, f.ri.Status().Serve.Running)
	})
}

// ------------------------------------------------------------ walkthroughs

// Boot: every registered repo is mounted concurrently; a mount replies at
// Ready within the walk, while its index job still runs; the job's success
// publishes ready.
func TestWalkthrough_Boot(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	g := newGate(StageIndex, "index-job")
	var armed atomic.Bool
	boot := func() *Manager {
		m := New(context.Background(), Deps{
			Cfg:         config.Config{Home: home, OntologyRoot: "kb"},
			AgentBranch: "agent/test",
			KeyPath:     filepath.Join(home, "agent.key"),
			Machine: Options{Synchronous: true, CrashBackoff: testCrashBackoff, Hook: func(s StageID, p string, ctx context.Context) {
				if armed.Load() {
					g.hook(s, p, ctx)
				}
			}},
		})
		require.NoError(t, m.Start())
		return m
	}
	m1 := boot()
	createRepo(t, m1, "alpha")
	createRepo(t, m1, "beta")
	require.NoError(t, m1.Close())

	armed.Store(true)
	m2 := boot() // returns although both index jobs are held
	t.Cleanup(func() { g.open(); _ = m2.Close() })
	for _, name := range []string{"alpha", "beta"} {
		ri := m2.Get(name)
		require.NotNil(t, ri)
		st := ri.Status()
		require.Equal(t, "ready", st.Stage)
		require.True(t, st.IndexRunning(), "%s: the index job is held, the mount replied anyway", name)
		require.True(t, st.Serve.Running)
		require.NotZero(t, st.gens[StageSync])
	}
	g.open()
	for _, name := range []string{"alpha", "beta"} {
		require.Equal(t, IndexStateReady, waitIndexSettled(t, m2.Get(name)).Index.State)
	}
}

// Attach ok: the guard probes the remote, Sync alone restarts on the origin
// loop, the reply comes after the re-entry.
func TestWalkthrough_AttachOK(t *testing.T) {
	t.Parallel()
	f := newMFix(t, StageIndex, "index-job", false)
	url := f.remote(t, "r")
	rep, err := send(f.m, f.ri, AttachOrigin(OriginSpec{URL: url, Branch: "main"}))
	require.NoError(t, err)
	require.True(t, rep.Status.Sync.Running)
	require.Equal(t, url, rep.Status.Sync.Origin)
	o, err := f.m.Origins().Get(f.ri.UID())
	require.NoError(t, err)
	require.Equal(t, url, o.URL, "the origin row is persisted")
}

// Attach to an unreachable remote: the guard's probe fails, ErrOriginUnreachable
// is the reply, NOTHING is persisted and the running loop never stopped.
func TestWalkthrough_AttachUnreachable(t *testing.T) {
	f := newMFix(t, StageIndex, "index-job", false)
	before := f.ri.Status()
	_, err := send(f.m, f.ri, AttachOrigin(OriginSpec{URL: fileuri.New(filepath.Join(f.root, "nowhere.git")), Branch: "main"}))
	require.ErrorIs(t, err, ErrOriginUnreachable)
	after := f.ri.Status()
	require.Equal(t, before.gens, after.gens, "nothing exited or re-entered")
	o, err := f.m.Origins().Get(f.ri.UID())
	require.NoError(t, err)
	require.Nil(t, o, "nothing persisted")
}

// Attach during indexing: refused before any network read.
func TestWalkthrough_AttachDuringIndexing(t *testing.T) {
	f := newMFix(t, StageIndex, "index-job", true)
	f.holdIndex(t)
	_, err := send(f.m, f.ri, AttachOrigin(OriginSpec{URL: fileuri.New(filepath.Join(f.root, "nowhere.git"))}))
	require.ErrorIs(t, err, ErrIndexing, "ErrIndexing, not the probe's verdict: no network read happened")
}

// Detach: Sync restarts with the local loop; a subscription is refused.
func TestWalkthrough_Detach(t *testing.T) {
	t.Parallel()
	f := newMFix(t, StageIndex, "index-job", false)
	url := f.remote(t, "r")
	_, err := send(f.m, f.ri, AttachOrigin(OriginSpec{URL: url, Branch: "main"}))
	require.NoError(t, err)
	rep, err := send(f.m, f.ri, DetachOrigin())
	require.NoError(t, err)
	require.True(t, rep.Status.Sync.Running, "the local loop runs at once")
	require.Empty(t, rep.Status.Sync.Origin)
	o, err := f.m.Origins().Get(f.ri.UID())
	require.NoError(t, err)
	require.Nil(t, o)

	_, sub, _ := buildSubscription(t)
	_, err = send(sub.env.m, sub, DetachOrigin())
	require.ErrorIs(t, err, ErrSubscriptionOrigin)
}

// Swap ok: cancel all, drain newest-first, install + origin, walk up again;
// the heal of the new store is the post-swap index.
func TestWalkthrough_SwapOK(t *testing.T) {
	t.Parallel()
	f := newMFix(t, StageIndex, "index-job", true)
	url := f.remote(t, "r")
	tmp := f.copyOfOwnDB(t)
	rep, err := send(f.m, f.ri, SwapStore(SwapSpec{TempDB: tmp, Origin: OriginSpec{URL: url, Branch: "main"}}))
	require.NoError(t, err)
	require.Equal(t, "ready", rep.Status.Stage)
	require.Equal(t, url, rep.Status.Sync.Origin, "Sync re-entered on the new origin")
	o, err := f.m.Origins().Get(f.ri.UID())
	require.NoError(t, err)
	require.Equal(t, url, o.URL)
	require.Equal(t, IndexStateReady, waitIndexSettled(t, f.ri).Index.State)
	_, statErr := os.Stat(f.m.RepoPath(f.ri.UID()) + ".bak")
	require.True(t, os.IsNotExist(statErr), "the first successful Open after a swap deletes its backup")
}

// A swap persists its origin BEFORE the one irreversible step, the copy. An
// origin row that cannot be saved (here: no consensus branch) aborts the swap
// with nothing changed — the copy is never attempted, the origin row is what it
// was, and the repo comes back on its own store.
func TestSwap_OriginPersistFailureAbortsWithNothingChanged(t *testing.T) {
	f := newMFix(t, StageIndex, "index-job", true)
	url := f.remote(t, "r")
	tmp := f.copyOfOwnDB(t)
	copied := false
	prev := swapCopy
	swapCopy = func(src, dst string) error { copied = true; return prev(src, dst) }
	t.Cleanup(func() { swapCopy = prev })

	_, err := send(f.m, f.ri, SwapStore(SwapSpec{TempDB: tmp, Origin: OriginSpec{URL: url, Branch: ""}}))
	require.ErrorContains(t, err, "save remote config")
	require.ErrorContains(t, err, "nothing changed")
	require.False(t, copied, "the install copy must not run once the origin could not be saved")
	o, gerr := f.m.Origins().Get(f.ri.UID())
	require.NoError(t, gerr)
	require.Nil(t, o, "the origin row is untouched")
	st := f.ri.Status()
	require.Equal(t, "ready", st.Stage)
	require.Empty(t, st.Sync.Origin, "Sync is back on the local loop")
}

// A copy failure (the irreversible step, failing midway) restores BOTH the
// store file from its backup and the previous origin row, so the repo comes
// back on its old store under its old origin.
func TestSwap_CopyFailureRestoresStoreAndOrigin(t *testing.T) {
	f := newMFix(t, StageIndex, "index-job", true)
	previous := f.remote(t, "previous")
	_, err := send(f.m, f.ri, AttachOrigin(OriginSpec{URL: previous, Branch: "main"}))
	require.NoError(t, err)
	idBefore := f.ri.ID()
	incoming := f.remote(t, "incoming")
	tmp := f.copyOfOwnDB(t)
	prev := swapCopy
	swapCopy = func(src, dst string) error {
		// Clobber the destination, then fail: the restore must undo it.
		require.NoError(t, os.WriteFile(dst, []byte("half-written"), 0o600))
		return errors.New("disk full")
	}
	t.Cleanup(func() { swapCopy = prev })

	_, err = send(f.m, f.ri, SwapStore(SwapSpec{TempDB: tmp, Origin: OriginSpec{URL: incoming, Branch: "main"}}))
	require.ErrorContains(t, err, "disk full")
	require.ErrorContains(t, err, "previous store and remote config restored")
	o, gerr := f.m.Origins().Get(f.ri.UID())
	require.NoError(t, gerr)
	require.NotNil(t, o)
	require.Equal(t, previous, o.URL, "the previous origin row is restored")
	st := f.ri.Status()
	require.Equal(t, "ready", st.Stage, "the restored store opens")
	require.Equal(t, previous, st.Sync.Origin)
	require.Equal(t, idBefore, f.ri.ID())
}

// Swap during indexing is refused; cancel-and-continue (CancelIndex, then
// SwapStore) reaches ready with all three workers running on the new store.
func TestWalkthrough_SwapDuringIndexingAndCancelAndContinue(t *testing.T) {
	t.Parallel()
	f := newMFix(t, StageIndex, "index-job", false)
	tmp := f.copyOfOwnDB(t)
	f.holdIndex(t)
	_, err := send(f.m, f.ri, SwapStore(SwapSpec{TempDB: tmp}))
	require.ErrorIs(t, err, ErrIndexing)

	rep, err := send(f.m, f.ri, CancelIndex())
	require.NoError(t, err)
	require.Equal(t, "indexing cancelled", rep.Status.Index.Reason)
	f.hk.armed.Store(false)
	rep, err = send(f.m, f.ri, SwapStore(SwapSpec{TempDB: tmp}))
	require.NoError(t, err)
	require.True(t, rep.Status.Serve.Running)
	require.True(t, rep.Status.Sync.Running)
	require.Equal(t, IndexStateReady, waitIndexSettled(t, f.ri).Index.State)
	require.NotNil(t, f.ri.triggers)
}

// Rebuild with the index ready: 201-shaped (a new job), status indexing,
// Sync and Serve untouched; then ready.
func TestWalkthrough_RebuildReadyAndDuplicate(t *testing.T) {
	f := newMFix(t, StageIndex, "index-job", true)
	gens := f.ri.Status().gens
	rep := f.holdIndex(t)
	require.NotEmpty(t, rep.JobID)
	require.False(t, rep.Absorbed)
	st := f.ri.Status()
	require.Equal(t, IndexStateIndexing, st.Index.State)
	require.Equal(t, gens[StageServe], st.gens[StageServe])
	require.Equal(t, gens[StageSync], st.gens[StageSync])

	dup, err := send(f.m, f.ri, Rebuild(f.ri.AgentBranch()))
	require.NoError(t, err)
	require.True(t, dup.Absorbed, "an identical rebuild already running is absorbed")
	require.Equal(t, rep.JobID, dup.JobID)

	f.hk.g.open()
	require.Equal(t, IndexStateReady, waitIndexSettled(t, f.ri).Index.State)
}

// Rebuild while the heal runs: the heal is replaced (after its batch).
func TestWalkthrough_RebuildWhileIndexing(t *testing.T) {
	t.Parallel()
	f := newMFix(t, StageIndex, "index-job", true)
	writeOn(t, f.ri, trigAgent, "kb/tasks/a.md") // something to re-embed
	clearVectors(t, f.m, f.ri)
	f.emb.armed.Store(true)
	// A full rebuild that re-embeds, parked in the embedder.
	_, err := send(f.m, f.ri, Rebuild(f.ri.AgentBranch()))
	require.NoError(t, err)
	f.emb.waitStarted(t)
	f.emb.armed.Store(false)
	rep, err := send(f.m, f.ri, Rebuild("main"))
	require.NoError(t, err, "a rebuild of another branch replaces the running job; the replaced job's ctx ends its batch")
	require.False(t, rep.Absorbed)
	require.Equal(t, IndexStateReady, waitIndexSettled(t, f.ri).Index.State)
}

// A panic in the index job is its result: error (with a crashdump), published;
// the stage stays entered and the next Rebuild is accepted.
func TestWalkthrough_RebuildPanic(t *testing.T) {
	var panicNext atomic.Bool
	home := t.TempDir()
	m := New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb"},
		AgentBranch: "agent/test",
		KeyPath:     filepath.Join(home, "agent.key"),
		Machine: Options{Synchronous: true, CrashBackoff: testCrashBackoff, Hook: func(_ StageID, p string, _ context.Context) {
			if p == "index-job" && panicNext.CompareAndSwap(true, false) {
				panic("index job blew up")
			}
		}},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	ri := createRepo(t, m, "kb")

	panicNext.Store(true)
	_, err := send(m, ri, Rebuild(ri.AgentBranch()))
	require.NoError(t, err)
	st := waitIndexSettled(t, ri)
	require.Equal(t, IndexStateError, st.Index.State)
	require.Contains(t, st.Index.Reason, "panicked")
	require.NotZero(t, st.gens[StageIndex], "the stage stays entered")
	require.True(t, st.Serve.Running, "a job panic is not a worker crash")

	_, err = send(m, ri, Rebuild(ri.AgentBranch()))
	require.NoError(t, err, "the next Rebuild is accepted")
	require.Equal(t, IndexStateReady, waitIndexSettled(t, ri).Index.State)
}

// CancelIndex: Index exits after the current batch, the result is cancelled,
// the due sweep stays gated, and Rebuild is accepted later.
func TestWalkthrough_CancelIndex(t *testing.T) {
	f := newMFix(t, StageIndex, "index-job", true)
	writeOn(t, f.ri, trigAgent, "kb/tasks/a.md") // something to re-embed
	clearVectors(t, f.m, f.ri)
	f.emb.armed.Store(true)
	_, err := send(f.m, f.ri, Rebuild(f.ri.AgentBranch()))
	require.NoError(t, err)
	f.emb.waitStarted(t) // in a batch, holding lockBranch(agent)

	rep, err := send(f.m, f.ri, CancelIndex())
	require.NoError(t, err, "the reply comes after the job has drained")
	require.Equal(t, IndexStateError, rep.Status.Index.State)
	require.Equal(t, "indexing cancelled", rep.Status.Index.Reason)
	require.False(t, rep.Status.IndexRunning())
	require.False(t, f.ri.triggers.indexIsReady(), "the due sweep stays gated after a cancel")

	f.emb.armed.Store(false)
	_, err = send(f.m, f.ri, Rebuild(f.ri.AgentBranch()))
	require.NoError(t, err)
	require.Equal(t, IndexStateReady, waitIndexSettled(t, f.ri).Index.State)
}

// Worker crash during indexing: a Serve worker is parked on lockBranch(agent)
// — the consensus merger, merging a peer's push into the agent branch —
// behind an index job that holds that lock. The Serve life is reported
// crashed: the driver cancels it, marks it pending and starts a waiter, and
// stays responsive (it never waits on the parked merger). The waiter returns
// once the job releases the lock and the merger exits; after the backoff
// Serve is re-entered.
func TestWalkthrough_WorkerCrashDuringIndexing(t *testing.T) {
	emb := newGatedEmbedder()
	home := t.TempDir()
	cfg := config.Config{Home: home, OntologyRoot: "kb"}
	m := New(context.Background(), Deps{
		Cfg:         cfg,
		AgentBranch: cHostAgent,
		KeyPath:     filepath.Join(home, "agent.key"),
		Embedder:    emb,
		Machine:     Options{Synchronous: true, CrashBackoff: testCrashBackoff},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { emb.unblock(); _ = m.Close() })
	host := &cHost{m: m, ri: createRepo(t, m, "kb")}
	host.setOntology(t, triggerOntology(autoAttrs))
	writeOn(t, host.ri, cHostAgent, "kb/tasks/first.md")
	host.advance(t)
	host.url = serveConsensusGit(t, host.ri)
	peer := newConsensusPeer(t, host.url)
	host.settle(t)

	// The index job: a full rebuild of the agent branch — the merger's merge
	// target — parked in a batch, holding lockBranch(agent).
	clearVectors(t, m, host.ri)
	emb.armed.Store(true)
	_, err := send(m, host.ri, Rebuild(cHostAgent))
	require.NoError(t, err)
	emb.waitStarted(t)

	// A peer push kicks the merger, which parks on lockBranch(agent).
	var parked atomic.Bool
	setConsensusHooks(t, consensusHooks{beforeMerge: func(string) { parked.Store(true) }})
	peer.write(t, "kb/tasks/peer.md", "peer")
	peer.round(t)
	require.Eventually(t, parked.Load, 10*time.Second, 5*time.Millisecond, "the merger reached its merge")

	gen := host.ri.Status().gens[StageServe]
	host.ri.machine.post(crashed{stage: StageServe, gen: gen})
	waitStatus(t, host.ri, "Serve marked pending", func(s Status) bool { return !s.Serve.Running })

	// The driver is not waiting on the parked merger: it answers at once.
	rep, err := send(m, host.ri, Rebuild(cHostAgent))
	require.NoError(t, err)
	require.True(t, rep.Absorbed)
	// The ONE deliberate sleep in this suite, and it is a negative wait: the
	// claim is that something does NOT happen (no re-entry while a Serve
	// worker is still parked), and absence has no event to wait on. It sleeps
	// far past the crash backoff, so a restart driven by the timer alone would
	// have fired; only the waiter (still blocked on the parked worker) can be
	// holding it. Everything else in the suite synchronises on hooks or Watch.
	time.Sleep(50 * testCrashBackoff)
	require.False(t, host.ri.Status().Serve.Running, "no re-entry while a Serve worker is still parked")
	require.Equal(t, gen, host.ri.Status().gens[StageServe])

	// Release the job: the lock frees, the merger finishes and exits, the
	// waiter reports drained, the timer re-enters Serve.
	emb.armed.Store(false)
	emb.unblock()
	st := waitStatus(t, host.ri, "Serve re-entered", func(s Status) bool {
		return s.Serve.Running && s.gens[StageServe] != gen
	})
	require.NotZero(t, st.gens[StageServe])
	require.Equal(t, IndexStateReady, waitIndexSettled(t, host.ri).Index.State)
}

// Archive mid-index with a parked sync round: the index job holds
// lockBranch(main) in a batch, and a sync round is parked behind it. Unmount
// cancels the root context first, from the caller; the job returns at its
// batch boundary and releases the lock; the parked round abandons; the driver
// drains Sync, Serve, Index, Identify, Open newest-first; Closed.
func TestWalkthrough_ArchiveMidIndexWithAParkedSyncRound(t *testing.T) {
	f := newMFix(t, StageIndex, "index-job", false)
	url := seedBareRemoteWithFact(t, filepath.Join(f.root, "r.git")) // main carries a fact to re-embed
	var rounds atomic.Int64
	holdWindows(t, true, func(_ context.Context, repo string) {
		if repo == "kb" {
			rounds.Add(1)
		}
	})
	_, err := send(f.m, f.ri, AttachOrigin(OriginSpec{URL: url, Branch: "main"}))
	require.NoError(t, err)
	require.Eventually(t, func() bool { return rounds.Load() >= 1 }, 20*time.Second, 5*time.Millisecond,
		"the origin loop's first round ran")
	svc := testService(t, f.ri)
	require.Eventually(t, func() bool {
		_, rerr := svc.Facts().ReadFact(context.Background(), "main", "kb/test/f.md", nil)
		return rerr == nil
	}, 20*time.Second, 10*time.Millisecond, "main followed the remote")
	// ReadFact succeeding only means the git ref moved; it says nothing about
	// the index. The SAME round's advanceBranchTo also calls notifyCommit,
	// which runs the index's OWN inline Sync() for main (last=="", so its
	// "full rebuild" fast path indexes and embeds the one fact directly,
	// unarmed, under lockBranch(main)) — a second, independent embed of the
	// exact content the explicit Rebuild below targets. facts_vec is global,
	// not branch-scoped, so if that inline embed lands AFTER clearVectors but
	// BEFORE the explicit Rebuild's phase-2 count below, the explicit job
	// finds nothing left to embed and waitStarted hangs for its full timeout
	// (observed under -race, 6/100 runs). Wait for that inline embed to land
	// first, so clearVectors has something to clear and the explicit,
	// gated Rebuild("main") is guaranteed to be the one doing the embedding.
	require.Eventually(t, func() bool {
		raw, rerr := sql.Open("sqlite3", f.m.RepoPath(f.ri.UID()))
		require.NoError(t, rerr)
		defer raw.Close()
		var n int
		require.NoError(t, raw.QueryRow(`SELECT COUNT(*) FROM facts_vec`).Scan(&n))
		return n > 0
	}, 20*time.Second, 10*time.Millisecond, "main's fact was embedded by the sync round's own inline index")

	// The index job: a full rebuild of main, parked in a batch, holding
	// lockBranch(main).
	clearVectors(t, f.m, f.ri)
	f.emb.armed.Store(true)
	_, err = send(f.m, f.ri, Rebuild("main"))
	require.NoError(t, err)
	f.emb.waitStarted(t)

	// The remote's main advances; a woken round fetches and must fast-forward
	// main under lockBranch(main) — it parks behind the job.
	pushToBareMain(t, bareOf(url), map[string]string{"more.txt": "more"})
	settled := rounds.Load()
	f.ri.wakeSync()
	require.Eventually(t, func() bool { return rounds.Load() > settled }, 20*time.Second, 5*time.Millisecond,
		"the woken round started")

	archived := make(chan error, 1)
	go func() { _, err := f.m.Archive("kb"); archived <- err }()
	select {
	case err := <-archived:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Archive did not return: the drain waited on a round parked behind the index job's lock")
	}
	select {
	case <-f.ri.machine.Done():
	default:
		t.Fatal("Archive returned before the machine was closed")
	}
	require.Equal(t, "closed", f.ri.Status().Stage)
}

// ------------------------------------------------------------- rule checks

// TestMachine_NoStageSends: no stage sends to its own machine (rule 2).
//
// Structurally: the code that runs under a Life — the stages, the sync
// loops, the dispatcher, the merger, the sweep — contains no call to Send. At
// run time: a Send from inside a stage's Enter waits on the driver that is
// running that very Enter, so it can only end on its own ctx — which is why
// the rule is absolute rather than a convention.
func TestMachine_NoStageSends(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	for _, file := range []string{"stages.go", "sync.go", "triggers.go", "consensus.go", "experiment_sweep.go",
		"trigger_script.go", "trigger_recipe.go", "sync_mode.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err, file)
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Send" {
					t.Errorf("%s: a call to Send in code that runs under a stage's life", fset.Position(call.Pos()))
				}
			}
			return true
		})
	}

	// The run-time half, with a timeout guard: a hook inside Serve's Enter
	// sends to its own machine.
	var sendErr atomic.Value
	var m *Manager
	var ri atomic.Pointer[RepoInstance]
	home := t.TempDir()
	m = New(context.Background(), Deps{
		Cfg:         config.Config{Home: home, OntologyRoot: "kb"},
		AgentBranch: "agent/test",
		KeyPath:     filepath.Join(home, "agent.key"),
		Machine: Options{Synchronous: true, CrashBackoff: testCrashBackoff, Hook: func(s StageID, p string, ctx context.Context) {
			self := ri.Load()
			if s != StageServe || p != "enter" || self == nil {
				return
			}
			tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			defer cancel()
			_, err := m.Send(tctx, self, CancelIndex())
			sendErr.Store(err)
		}},
	})
	require.NoError(t, m.Start())
	t.Cleanup(func() { _ = m.Close() })
	first := createRepo(t, m, "kb")
	ri.Store(first)
	_, err := send(m, first, DetachOrigin()) // Sync restart only: Serve's hook does not fire
	require.NoError(t, err)
	restartServe(t, first) // Serve's Enter runs the hook
	require.Eventually(t, func() bool { return sendErr.Load() != nil }, 10*time.Second, 5*time.Millisecond,
		"the hook inside Serve's Enter ran its Send")
	got, _ := sendErr.Load().(error)
	require.True(t, errors.Is(got, context.DeadlineExceeded),
		"a stage's Send to its own machine can only time out (got %v)", got)
}

// TestDispatcher_DueSweepGatedOnIndexReady: the `on: due` sweep reads the
// index (branch_facts ⋈ fact_expires), so it is skipped while the index is not
// ready, and the index job's success kicks the dispatcher to run it. The
// advance (git DiffFacts) runs regardless: a learn trigger fires during
// indexing.
func TestDispatcher_DueSweepGatedOnIndexReady(t *testing.T) {
	clock := newFakeClock()
	dueHooks(t, clock, triggerHooks{})
	g := newGate(StageIndex, "index-job")
	var armed atomic.Bool
	m := newTestManager(t)
	m.deps.Machine.Hook = func(s StageID, p string, ctx context.Context) {
		if armed.Load() {
			g.hook(s, p, ctx)
		}
	}
	ri := bootRepo(t, m)
	setOntology(t, ri, triggerOntology("", trig("due", "due", "", ""), trig("all", "learn", "", "")))
	writeDatedAndWait(t, ri, "kb/tasks/x.md", clock.now().Add(time.Hour))
	clock.add(2 * time.Hour) // overdue now

	armed.Store(true)
	_, err := send(m, ri, Rebuild(ri.AgentBranch()))
	require.NoError(t, err)
	g.waitArrived(t)
	t.Cleanup(g.open)

	kickAndWait(t, ri)
	require.Empty(t, dueFiresOf(t, ri, "due"), "the due sweep is skipped while the index is not ready")
	writeOn(t, ri, trigAgent, "kb/tasks/during.md")
	require.Eventually(t, func() bool {
		for _, f := range firesOf(t, ri, "all") {
			if f.Path == "kb/tasks/during.md" {
				return true
			}
		}
		return false
	}, 20*time.Second, 10*time.Millisecond, "the advance runs during indexing")
	require.Empty(t, dueFiresOf(t, ri, "due"))

	g.open()
	require.Equal(t, IndexStateReady, waitIndexSettled(t, ri).Index.State)
	require.Eventually(t, func() bool { return len(dueFiresOf(t, ri, "due")) == 1 },
		20*time.Second, 10*time.Millisecond, "the index job's success kicks the dispatcher, and the sweep fires")
}
