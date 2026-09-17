package repos

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/platform/fileuri"
	"knomit/internal/store"
)

// pathRecorder wraps a handler and records every request path, so a test can
// assert that a refusal cost NOTHING but the advertisement.
type pathRecorder struct {
	inner http.Handler
	mu    sync.Mutex
	paths []string
}

func (p *pathRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.paths = append(p.paths, r.URL.Path)
	p.mu.Unlock()
	p.inner.ServeHTTP(w, r)
}

func (p *pathRecorder) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.paths...)
}

func (p *pathRecorder) reset() {
	p.mu.Lock()
	p.paths = nil
	p.mu.Unlock()
}

func (p *pathRecorder) sawUploadPack() bool {
	for _, path := range p.seen() {
		if path == "/git-upload-pack" {
			return true
		}
	}
	return false
}

// identityFixture is a manager holding ONE active repo, "kept", subscribed to
// a knomit-served origin. Every layer test asks the same question of it: what
// happens when a SECOND create is pointed at the same knowledge base?
type identityFixture struct {
	m          *Manager
	kept       *RepoInstance
	origin     *store.Service
	url        string
	altURL     string
	recorder   *pathRecorder
	rootCommit string
	// originRoot is the LocalOriginRoot the manager is configured with, so a
	// test can put a file:// remote under it and clear the local-origin gate.
	originRoot string
}

func newIdentityFixture(t *testing.T) *identityFixture {
	t.Helper()
	ctx := context.Background()
	f := &identityFixture{originRoot: t.TempDir()}

	svc, err := store.Open(filepath.Join(t.TempDir(), "origin.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	ont, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	require.NoError(t, svc.InitRepo(map[string]string{OntologyPath: string(ont)}, "main"))
	for i := range 3 {
		_, err := svc.Facts().WriteFact(ctx, "main",
			fmt.Sprintf("kb/gotchas/seed-%d.md", i), testFactBodyFor(i), "seed", "")
		require.NoError(t, err)
	}
	f.origin = svc
	f.rootCommit, err = svc.RootCommit(ctx, "main")
	require.NoError(t, err)
	require.Len(t, f.rootCommit, 40)

	f.recorder = &pathRecorder{inner: svc.Handler()}
	srv := httptest.NewServer(f.recorder)
	t.Cleanup(srv.Close)
	f.url = srv.URL

	// The SAME store on a SECOND address. Pointing the duplicate create at
	// f.url would be answered by ErrOriginInUse — an older, cheaper check on
	// the URL string — long before any identity layer ran, so the test would
	// pass without exercising the thing it names. A mirror of a repository
	// genuinely does have a different URL, which is exactly why the
	// origin-string check was never the real uniqueness guard
	// (Manager.ActiveRepoWithOrigin says so in as many words).
	alt := httptest.NewServer(f.recorder)
	t.Cleanup(alt.Close)
	f.altURL = alt.URL

	f.m = New(ctx, Deps{
		Cfg: config.Config{
			Home:            t.TempDir(),
			OntologyRoot:    "kb",
			LocalOriginRoot: f.originRoot,
		},
		AgentBranch:           "machine/test",
		DisableBackgroundSync: true,
	})
	require.NoError(t, f.m.Start())
	t.Cleanup(func() { _ = f.m.Close() })

	kept, err := f.m.Create(ctx, CreateSpec{Name: "kept", Mode: "subscribe", Origin: &OriginSpec{URL: f.url}}, nil)
	require.NoError(t, err)
	require.NotNil(t, kept)
	f.kept = kept

	// The fixture only discriminates if "kept" is REGISTERED under the shared
	// identity: that registry row is what layers 1 and 3 read, and a fixture
	// where it is missing would let every refusal below pass for the wrong
	// reason (or fail for one).
	require.Equal(t, f.rootCommit, kept.ID(), "the local copy must hold the origin's root commit")
	requireRegisteredRepoID(t, f.m, kept.UID(), f.rootCommit)

	f.recorder.reset()
	return f
}

func requireRegisteredRepoID(t *testing.T, m *Manager, uid, want string) {
	t.Helper()
	reg := m.Repos()
	require.NotNil(t, reg)
	active, err := reg.List(StateActive)
	require.NoError(t, err)
	for _, rec := range active {
		if rec.UID == uid {
			require.Equal(t, want, rec.RepoID, "the fixture repo must be registered under the shared identity")
			return
		}
	}
	t.Fatalf("uid %s is not an active repo", uid)
}

// requireNoRepoNamed asserts that nothing about name survives a refused
// create: no live instance, no registry row in any state, and no database
// file left on disk beyond the ones wantDBs other repos legitimately have.
func requireNoRepoNamed(t *testing.T, m *Manager, name string, wantDBs int) {
	t.Helper()
	require.Nil(t, m.Get(name), "a refused create must leave no live repo")
	reg := m.Repos()
	require.NotNil(t, reg)
	for _, state := range []RepoState{StateActive, StateArchived} {
		rows, err := reg.List(state)
		require.NoError(t, err)
		for _, rec := range rows {
			require.NotEqual(t, name, rec.Name, "a refused create must leave no %s registry row", state)
		}
	}
	// The partial .db is what cleanup() removes, and counting is the only way
	// to see it: the refused create's uid is minted inside Create and never
	// escapes, so there is no path to name the file directly.
	found, err := filepath.Glob(filepath.Join(filepath.Dir(m.RepoPath("x")), "*.db"))
	require.NoError(t, err)
	var dbs []string
	for _, path := range found {
		// Service.Open makes an ephemeral <uid>.sessions.db alongside each real
		// store and removes it on Close; it is not a repo database and must not
		// be counted as one.
		if !strings.HasSuffix(path, ".sessions.db") {
			dbs = append(dbs, path)
		}
	}
	require.Len(t, dbs, wantDBs, "a refused create left its partial database behind: %v", dbs)
}

// LAYER 1 — a knomit origin ADVERTISES its identity, so a duplicate subscribe
// is refused from the advertisement alone: named holder, no upload-pack
// request, nothing transferred.
func TestPreflight_Layer1_KnomitOriginAdvertisesItsIdentity(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	// MOVE THE ORIGIN AHEAD of the local copy, so no advertised tip is local.
	// Without this, "kept" holds the origin's tip and LAYER 2 answers — the
	// test would pass with layer 1 deleted, which is exactly what per-layer
	// sabotage is for. The root commit does not move, so layer 1 still can.
	_, err := f.origin.Facts().WriteFact(ctx, "main", "kb/gotchas/after-kept.md",
		testFactBodyFor(42), "written after the local copy was made", "")
	require.NoError(t, err)

	rp, err := f.m.beginRemoteProbe(ctx, OriginSpec{URL: f.altURL})
	require.NoError(t, err)
	defer rp.close()
	id := rp.identity()
	require.Equal(t, f.rootCommit, id.RepoID, "the origin must advertise knomit-repo-id")
	require.Empty(t, tipHeldLocally(f.m, id.Tips),
		"no advertised tip may be local, or layer 2 would be what fires here")
	f.recorder.reset()

	err = f.m.CreatePreflight(ctx, CreateSpec{
		Name: "second", Mode: "subscribe", Origin: &OriginSpec{URL: f.altURL},
	})
	require.ErrorIs(t, err, ErrKnowledgeBaseAlreadyLocal)
	require.Contains(t, err.Error(), `"kept"`, "the refusal must name the repo that already holds it")
	require.False(t, f.recorder.sawUploadPack(),
		"the refusal must cost nothing but the advertisement; the server saw %v", f.recorder.seen())
}

// LAYER 2 — a remote that says NOTHING about itself (an ordinary bare git
// repo, no knomit capability) is still refused when a local store already
// holds one of its advertised tips. A commit is unique to a history, so a tip
// is proof, and it needs no cooperation from the server.
func TestPreflight_Layer2_AdvertisedTipIsAlreadyLocal(t *testing.T) {
	f := newIdentityFixture(t)
	mirror := mirrorOfOrigin(t, f, "mirror.git", false)

	rp, err := f.m.beginRemoteProbe(context.Background(), OriginSpec{URL: mirror})
	require.NoError(t, err)
	defer rp.close()
	id := rp.identity()
	require.Empty(t, id.RepoID,
		"a bare git remote advertises no knomit identity, so layer 1 cannot be what fires here")
	require.NotEmpty(t, id.Tips)

	err = f.m.CreatePreflight(context.Background(), CreateSpec{
		Name: "second", Mode: "subscribe", Origin: &OriginSpec{URL: mirror},
	})
	require.ErrorIs(t, err, ErrKnowledgeBaseAlreadyLocal)
	require.Contains(t, err.Error(), `"kept"`)
}

// LAYER 3 — the local copy is BEHIND the remote, so no advertised tip is local
// and the remote claims nothing: layers 1 and 2 both pass, CORRECTLY. The
// create clones and is then refused on the shared ROOT commit, before
// registration.
//
// This is the reported incident's own shape, and the assertions are about what
// the refusal COSTS: no registry row, no live repo, no database file, and no
// index phase — because reaching m.Add is what opens the store, backfills the
// commit graph and starts the heal that the old refusal then cancelled.
func TestCreate_Layer3_RefusesBeforeRegistrationWhenLocalCopyIsBehind(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()
	ahead := mirrorOfOrigin(t, f, "ahead.git", true)

	// Layers 1 and 2 must PASS here, or this test is measuring one of them.
	rp, err := f.m.beginRemoteProbe(ctx, OriginSpec{URL: ahead})
	require.NoError(t, err)
	id := rp.identity()
	rp.close()
	require.Empty(t, id.RepoID, "the bare mirror advertises no knomit identity")
	require.NotEmpty(t, id.Tips)
	require.Empty(t, tipHeldLocally(f.m, id.Tips),
		"the local copy must NOT hold any advertised tip, or layer 2 would fire instead")
	require.NoError(t, f.m.CreatePreflight(ctx, CreateSpec{
		Name: "second", Mode: "subscribe", Origin: &OriginSpec{URL: ahead},
	}), "the cheap layers must not be able to refuse this one")

	var events []Event
	ri, err := f.m.Create(ctx, CreateSpec{Name: "second", Mode: "subscribe", Origin: &OriginSpec{URL: ahead}},
		func(e Event) { events = append(events, e) })

	require.ErrorIs(t, err, ErrKnowledgeBaseAlreadyLocal)
	require.Contains(t, err.Error(), `"kept"`)
	require.Nil(t, ri)
	requireNoRepoNamed(t, f.m, "second", 1) // only "kept"

	// It got as far as the TRANSFER (this is the layer that must clone to
	// answer) and no further: reaching "register" or "index" would mean the
	// refusal happened after m.Add, which is the failure this layer exists to
	// move.
	var phases []string
	for _, e := range events {
		phases = append(phases, e.Phase+"/"+e.Step)
	}
	require.Contains(t, phases, PhaseTransfer+"/subscribe", "got %v", phases)
	for _, e := range events {
		require.NotEqual(t, PhaseIndex, e.Phase, "the refusal must precede m.Add; got %v", phases)
		require.NotEqual(t, "register", e.Step, "the refusal must precede m.Add; got %v", phases)
	}

	// And "kept" is untouched — refusing the second create must not disturb the
	// repo it was refused in favour of.
	require.NotNil(t, f.m.Get("kept"))
	require.Equal(t, f.rootCommit, f.m.Get("kept").ID())
}

// A remote holding a DIFFERENT knowledge base passes all three layers. The
// discriminating counterpart to the three above: without it they would be
// satisfied by a check that refuses everything.
func TestCreate_UnrelatedRemoteIsNotRefused(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	other, err := store.Open(filepath.Join(t.TempDir(), "other.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	ont, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	require.NoError(t, other.InitRepo(map[string]string{OntologyPath: string(ont)}, "main"))
	_, err = other.Facts().WriteFact(ctx, "main", "kb/gotchas/other.md", testFactBodyFor(99), "other", "")
	require.NoError(t, err)
	otherRoot, err := other.RootCommit(ctx, "main")
	require.NoError(t, err)
	require.NotEqual(t, f.rootCommit, otherRoot, "the two fixtures must be different knowledge bases")

	srv := httptest.NewServer(other.Handler())
	t.Cleanup(srv.Close)

	require.NoError(t, f.m.CreatePreflight(ctx, CreateSpec{
		Name: "second", Mode: "subscribe", Origin: &OriginSpec{URL: srv.URL},
	}))
	ri, err := f.m.Create(ctx, CreateSpec{Name: "second", Mode: "subscribe", Origin: &OriginSpec{URL: srv.URL}}, nil)
	require.NoError(t, err)
	require.NotNil(t, ri)
	require.Equal(t, otherRoot, ri.ID())
}

// mirrorOfOrigin makes a BARE git mirror of the fixture origin's main branch
// under the manager's LocalOriginRoot — a remote with no knomit handler, so no
// knomit-repo-id capability anywhere. With ahead=true it carries one extra
// commit, so the local copy holds the shared ROOT but not the advertised TIP.
func mirrorOfOrigin(t *testing.T, f *identityFixture, name string, ahead bool) string {
	t.Helper()
	bare := filepath.Join(f.originRoot, name)
	require.NoError(t, os.MkdirAll(bare, 0o755))
	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)

	work := filepath.Join(t.TempDir(), "work")
	runGit(t, "", "clone", f.url, work)
	runGit(t, work, "remote", "add", "mirror", fileuri.New(bare))
	if ahead {
		require.NoError(t, os.WriteFile(filepath.Join(work, "ahead.txt"), []byte("ahead of the local copy"), 0o644))
		runGit(t, work, "add", "-A")
		runGit(t, work, "commit", "-m", "ahead of the local copy")
	}
	runGit(t, work, "push", "mirror", "main")
	return fileuri.New(bare)
}

// The wizard's BRANCH step learns it too. probe-initialized reuses the same
// advertisement, so the user is told "already local, as <name>" before
// pressing Create rather than only by the POST's 409.
//
// AlreadyLocal is ORTHOGONAL to Initialized: this remote IS a knowledge base
// (initialized=yes) AND is already here, and collapsing the second answer into
// the first would make every client that switches on `initialized` wrong.
func TestProbeInitialized_ReportsAlreadyLocal(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	res, err := f.m.ProbeInitializedOn(ctx, OriginSpec{URL: f.altURL}, "main")
	require.NoError(t, err)
	require.Equal(t, InitializedYes, res.Initialized, "the remote really is a knowledge base")
	require.Equal(t, "kept", res.AlreadyLocal)
}

// And a remote holding a DIFFERENT knowledge base reports nothing there — the
// discriminating counterpart, without which the assertion above is satisfied
// by a field that is always set.
func TestProbeInitialized_UnrelatedRemoteIsNotAlreadyLocal(t *testing.T) {
	f := newIdentityFixture(t)
	ctx := context.Background()

	other, err := store.Open(filepath.Join(t.TempDir(), "other.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = other.Close() })
	ont, err := fact.DefaultOntology().Serialize()
	require.NoError(t, err)
	require.NoError(t, other.InitRepo(map[string]string{OntologyPath: string(ont)}, "main"))
	srv := httptest.NewServer(other.Handler())
	t.Cleanup(srv.Close)

	res, err := f.m.ProbeInitializedOn(ctx, OriginSpec{URL: srv.URL}, "main")
	require.NoError(t, err)
	require.Equal(t, InitializedYes, res.Initialized)
	require.Empty(t, res.AlreadyLocal)
}
