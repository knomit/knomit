// Package backupenv is the story harness for REPLICATING knomit instances: it
// boots one the way cmd/serve boots one, and gives a scenario the vocabulary to
// populate a home, destroy it, and watch it come back.
//
// It is a subpackage of testenv rather than part of it because it must import
// internal/app (for Bootstrap, which is the whole point) and internal/app
// imports internal/web — whose own tests import testenv. Putting these helpers
// in testenv itself is therefore an import cycle, not a style choice. Everything
// content-shaped is still reused from the parent package: the FactSpec builder
// and the DeterministicEmbedder come from testenv, not from here.
package backupenv

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"knomit/internal/app"
	"knomit/internal/backup"
	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/test/testenv"
)

// Env is ONE replicating knomit instance, booted the way cmd/serve boots
// one: app.Bootstrap first (it rehydrates KNOMIT_HOME from the replica before
// any database is opened), then the repo manager, then Track for control.db,
// every open repo and every archived database still on the volume.
//
// It is deliberately NOT a Storyboard. Storyboard exists to make a repo's
// branch/fact graph easy to script and auto-verifies it on teardown; the
// scenarios here are about the VOLUME — populate it, destroy it, watch it come
// back — so the unit of the DSL is an instance and its home directory, not a
// branch. What is shared with Storyboard is everything that would otherwise be
// duplicated: the DeterministicEmbedder, the FactSpec builder, and the fact
// content those produce.
//
// app.New is not used, and cannot be: it requires a real ONNX embedder, which
// means downloading a model into <home>/models. Every scenario here replaces
// the home at least once, so that download would be paid again on each boot for
// a test that never needs a real vector. The manager is therefore wired by hand
// with EXACTLY the dependencies app.New passes (signer, agent branch, backup
// tracker, StrictMissing tied to backup being enabled) and the deterministic
// embedder in place of the ONNX one. Everything the recovery story depends on —
// Bootstrap's restore, control.db and its registry, the replica, the agent
// identity — is the production code path.
type Env struct {
	t       *testing.T
	cfg     config.Config
	boot    *app.BootResult
	manager *repos.Manager
	stopped bool
}

// Opts configures one instance. Home and Replica are directories the
// caller owns (t.TempDir()); reusing the same pair across two sequential
// Envs is how a restart is expressed, and keeping Replica while replacing
// Home is how a lost volume is expressed.
type Opts struct {
	// Home is KNOMIT_HOME. It must already contain the injected SSH key —
	// call InjectAgentKey first. A backup-enabled instance REFUSES to boot
	// without one, on purpose: a generated key is a new fingerprint, and the
	// agent branch is derived from it.
	Home string
	// Replica is the directory backing the file:// replica URL. The same
	// litestream code path as S3, with no network.
	Replica string
	// AgentName pins the machine-independent half of the agent branch. Required
	// (Bootstrap refuses an empty one when backup is enabled).
	AgentName string
	// AgentPath is the knomit-backup binary. Replication runs in a child
	// process, so tests must build it — see test/storytests/main_test.go. There
	// is no knomit executable to sit beside under `go test`, so the sibling
	// search would find nothing and Bootstrap would (correctly) refuse.
	AgentPath string
	// Embedder overrides the DeterministicEmbedder. Rarely needed.
	Embedder store.BatchEmbedder
	// MonitorInterval overrides the 50ms replication poll. Raise it when the
	// scenario is about WHICH shutdown step flushed a write: at 50ms a background
	// tick will have carried it long before teardown, so the assertion would hold
	// no matter what the shutdown did.
	MonitorInterval time.Duration
}

// AgentKeyPath is where app.Bootstrap looks for the instance identity when
// cfg.Remote.SSHKey is unset. It mirrors app.keyPathFor, which is unexported;
// if that ever moves, every backup story test fails at boot with "no SSH key
// at ...", which is the loud failure this duplication is worth.
func AgentKeyPath(home string) string { return filepath.Join(home, "id_ed25519") }

// InjectAgentKey writes the SSH key a backup-enabled instance is required to be
// GIVEN. Production mounts it as a secret; from Bootstrap's point of view the
// only thing that matters is that it exists before boot, so the fingerprint —
// and therefore the agent branch — is stable across a restore.
//
// No-op when a key is already present, so it is safe to call before every boot.
func InjectAgentKey(t *testing.T, home string) {
	t.Helper()
	path := AgentKeyPath(home)
	if _, err := os.Stat(path); err == nil {
		return
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("InjectAgentKey: mkdir %s: %v", home, err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("InjectAgentKey: generate: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("InjectAgentKey: marshal: %v", err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("InjectAgentKey: write %s: %v", path, err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("InjectAgentKey: signer: %v", err)
	}
	if err := os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o644); err != nil {
		t.Fatalf("InjectAgentKey: write %s.pub: %v", path, err)
	}
}

// SaveAgentKey reads the key material out of a home so it can be put back after
// the home is destroyed. In production the key is a mounted secret rather than
// volume state; this is the test's way of saying so.
func SaveAgentKey(t *testing.T, home string) []byte {
	t.Helper()
	b, err := os.ReadFile(AgentKeyPath(home))
	if err != nil {
		t.Fatalf("SaveAgentKey(%s): %v", home, err)
	}
	return b
}

// RestoreAgentKey writes saved key material into a home, creating it if needed.
// The counterpart of SaveAgentKey across a wipe.
func RestoreAgentKey(t *testing.T, home string, key []byte) {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("RestoreAgentKey: mkdir %s: %v", home, err)
	}
	if err := os.WriteFile(AgentKeyPath(home), key, 0o600); err != nil {
		t.Fatalf("RestoreAgentKey: write: %v", err)
	}
}

// New boots one replicating instance and registers a Shutdown on test
// cleanup. Any boot failure is fatal — these are acceptance scenarios, so a
// refused boot is never something to carry on past.
func New(t *testing.T, opts Opts) *Env {
	t.Helper()
	switch {
	case opts.Home == "":
		t.Fatal("New: Home is required")
	case opts.Replica == "":
		t.Fatal("New: Replica is required")
	case opts.AgentName == "":
		t.Fatal("New: AgentName is required — a backup-enabled instance refuses to boot without one, " +
			"so an empty name here would test the refusal rather than the scenario")
	case opts.AgentPath == "":
		t.Fatal("New: AgentPath is required — replication runs in the knomit-backup child process and " +
			"there is no installed knomit binary to find it beside under `go test`; build it from TestMain")
	}

	monitor := opts.MonitorInterval
	if monitor == 0 {
		monitor = 50 * time.Millisecond
	}

	cfg := config.Defaults()
	cfg.Home = opts.Home
	cfg.AgentName = opts.AgentName
	cfg.Backup = config.BackupConfig{
		Enabled:           true,
		URL:               "file://" + opts.Replica,
		AgentPath:         opts.AgentPath,
		Instance:          opts.AgentName,
		SnapshotInterval:  time.Hour,
		SnapshotRetention: time.Hour,
		L0Retention:       time.Minute,
		MonitorInterval:   monitor,
		RestoreTimeout:    2 * time.Minute,
		StatusCacheTTL:    10 * time.Millisecond,
	}

	embedder := opts.Embedder
	if embedder == nil {
		embedder = &testenv.DeterministicEmbedder{}
	}

	ctx := context.Background()
	boot, err := app.Bootstrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Bootstrap(%s): %v", opts.Home, err)
	}
	if boot.Backup == nil {
		t.Fatal("Bootstrap returned no backup manager while backup is enabled")
	}

	m := repos.New(ctx, repos.Deps{
		Cfg:         cfg,
		Signer:      boot.Signer,
		AgentBranch: boot.AgentBranch,
		Embedder:    embedder,
		KeyPath:     AgentKeyPath(opts.Home),
		Backup:      boot.Backup,
		// Tied to backup being on, exactly as app.New ties it: with replication
		// running, a registered repo that silently fails to open would have its
		// empty local state replicated over the good backup.
		StrictMissing: true,
		// The index heal runs inline rather than in a goroutine. A heal reading
		// a database through cgo SQLite while litestream's own process works on
		// the same file is a known hazard, and it is not what these tests are
		// asserting about.
		DisableBackgroundSync: true,
	})
	if err := m.Start(); err != nil {
		boot.Backup.Close(context.Background())
		t.Fatalf("repo manager Start(%s): %v", opts.Home, err)
	}

	env := &Env{t: t, cfg: cfg, boot: boot, manager: m}
	t.Cleanup(env.Shutdown)

	// Replicate what actually opened — after Start, which is what reconciles the
	// registry against the disk. Before it there is no truthful answer to "which
	// databases are live". This mirrors cmd/serve exactly.
	if err := boot.Backup.Track("control", filepath.Join(cfg.Home, "control.db")); err != nil {
		t.Fatalf("track control.db: %v", err)
	}
	for name, dbPath := range m.OpenDBPaths() {
		if err := boot.Backup.Track(name, dbPath); err != nil {
			t.Fatalf("track %s: %v", name, err)
		}
	}
	archived, err := m.ArchivedDBPaths()
	if err != nil {
		t.Fatalf("ArchivedDBPaths: %v", err)
	}
	for id, dbPath := range archived {
		if err := boot.Backup.TrackArchived(id, dbPath); err != nil {
			t.Fatalf("track archived %s: %v", id, err)
		}
	}
	return env
}

// Home returns KNOMIT_HOME for this instance.
func (e *Env) Home() string { return e.cfg.Home }

// AgentBranch returns the branch this instance writes to, as Bootstrap resolved
// it from agent_name + the injected key's fingerprint.
func (e *Env) AgentBranch() string { return e.boot.AgentBranch }

// Manager exposes the repo manager for assertions the DSL does not cover.
func (e *Env) Manager() *repos.Manager { return e.manager }

// Backup exposes the replication client.
func (e *Env) Backup() *backup.Manager { return e.boot.Backup }

// CreateRepo creates a repo through the production Manager.Create path, seeded
// with the default ontology preset. knomit has no default repo, so every
// scenario creates the ones it needs.
func (e *Env) CreateRepo(name string) *repos.RepoInstance {
	e.t.Helper()
	ri, err := e.manager.Create(context.Background(), repos.CreateSpec{
		Name: name, Mode: "preset", OntologyPreset: "default",
	}, nil)
	if err != nil {
		e.t.Fatalf("CreateRepo(%q): %v", name, err)
	}
	if ri == nil {
		e.t.Fatalf("CreateRepo(%q): manager returned no instance", name)
	}
	return ri
}

// Repo returns the named repo, failing loudly if this instance does not have
// it. After a restore that failure is the interesting one — it means the
// registry came back but the database did not — so it is never silent.
func (e *Env) Repo(name string) *repos.RepoInstance {
	e.t.Helper()
	ri := e.manager.Get(name)
	if ri == nil {
		e.t.Fatalf("repo %q is not registered on this instance (registered: %v)", name, e.manager.Names())
	}
	return ri
}

// Learn writes a fact to the instance's agent branch through the production
// WriteFact path and returns the commit hash.
func (e *Env) Learn(repo, path string, spec testenv.FactSpec, message string) string {
	e.t.Helper()
	var res store.WriteFactResult
	var err error
	e.Repo(repo).WithRead(func(svc *store.Service) {
		res, err = svc.Facts().WriteFact(
			context.Background(), e.AgentBranch(), path, spec.Build(), message, "test")
	})
	if err != nil {
		e.t.Fatalf("Learn(%s/%s): %v", repo, path, err)
	}
	return res.CommitHash
}

// FactContent reads a fact at the agent branch head and returns its raw serialized
// text — the thing that must come back byte-identical after a recovery.
// Fails the test if the fact cannot be read.
func (e *Env) FactContent(repo, path string) string {
	e.t.Helper()
	var res store.ReadFactResult
	var err error
	e.Repo(repo).WithRead(func(svc *store.Service) {
		res, err = svc.Facts().ReadFact(context.Background(), e.AgentBranch(), path, nil)
	})
	if err != nil {
		e.t.Fatalf("FactContent(%s/%s on %s): %v", repo, path, e.AgentBranch(), err)
	}
	return res.Content
}

// HeadCommit returns the agent branch's head commit in the named repo. Comparing
// it across a recovery is how a test asserts that the restored history is on the
// branch this instance actually writes to, rather than stranded beside it.
func (e *Env) HeadCommit(repo string) string {
	e.t.Helper()
	var head string
	var err error
	e.Repo(repo).WithRead(func(svc *store.Service) {
		head, err = svc.Branches().HeadCommit(context.Background(), e.AgentBranch())
	})
	if err != nil {
		e.t.Fatalf("HeadCommit(%s on %s): %v", repo, e.AgentBranch(), err)
	}
	return head
}

// SearchPaths runs a text query against the repo's search index on the agent
// branch and returns the matching fact paths. The index lives inside the same
// SQLite database as the facts, so this is how a test asserts the knowledge base
// came back USABLE and not merely present.
func (e *Env) SearchPaths(repo, query string) []string {
	e.t.Helper()
	var results []store.SearchResult
	var err error
	e.Repo(repo).WithRead(func(svc *store.Service) {
		results, err = svc.Search().Search(context.Background(), e.AgentBranch(), store.SearchOptions{
			Text: query, Limit: 50,
		})
	})
	if err != nil {
		e.t.Fatalf("SearchPaths(%s, %q): %v", repo, query, err)
	}
	paths := make([]string, 0, len(results))
	for _, r := range results {
		paths = append(paths, r.Path)
	}
	return paths
}

// WaitReplicated blocks until each named database reports a non-zero remote
// position. It is a liveness check, not a durability one: Shutdown is what
// guarantees the replica holds everything, because Untrack's close performs a
// synchronous final sync. This exists so a scenario fails fast and loudly when
// replication never started at all, rather than at a confusing assertion three
// phases later.
func (e *Env) WaitReplicated(names ...string) {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		synced := map[string]bool{}
		for _, st := range e.boot.Backup.Status(context.Background()) {
			if st.InSync && st.RemoteTXID > 0 {
				synced[st.Name] = true
			}
		}
		missing := []string{}
		for _, n := range names {
			if !synced[n] {
				missing = append(missing, n)
			}
		}
		if len(missing) == 0 {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %v to replicate (still unsynced: %v)", names, missing)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// Shutdown tears the instance down so the replica ends up holding everything it
// had. It UNTRACKS every database before closing the manager, which is the
// opposite of cmd/serve's order.
//
// The reason is what this fixture needs from teardown, not any doubt about the
// shipped one. Untrack closes ONE litestream database and performs its final
// sync synchronously, returning that database's own error — so this fixture can
// name every database that had to be flushed and fail with the name of any that
// was not (the `want` check below). A whole-manager close reports one aggregate,
// and polling for "InSync && RemoteTXID > 0" is weaker still: a stale position
// satisfies it, so a poll can return before the row a scenario depends on was
// uploaded at all.
//
// cmd/serve's order — knomit's SQLite handles close first, then the agent's
// final sync — is exercised by
// TestRecovery_ProductionShutdownOrderFlushesTheLastWrite, through
// ShutdownInServerOrder below. It is NOT a defect: an earlier comment here said
// knomit's close checkpoints and removes each -wal out from under the agent, so
// the final sync fails. That was true only while litestream ran in-process, where
// POSIX advisory locks do not conflict between descriptors in the same process.
// With litestream in a child process the kernel arbitrates, knomit's close cannot
// take the exclusive lock the checkpoint-and-delete needs, the -wal survives, and
// the agent's final sync reads it.
//
// Idempotent: registered as a t.Cleanup and also safe to call explicitly, which
// is what a scenario does before destroying the home.
func (e *Env) Shutdown() {
	if e.stopped {
		return
	}
	e.stopped = true
	ctx := context.Background()

	// The set that MUST be flushed, computed independently of the tracker. The
	// untrack loop below is driven by Status, and an empty Status would make it
	// a silent no-op: the fixture would quietly degrade to relying on the 50ms
	// monitor having happened to sync, and a scenario that then wipes the volume
	// would be asserting against whatever the timing gave it. Checking the live
	// set against Status turns that into a failure with a name in it.
	want := map[string]bool{"control": true}
	for name := range e.manager.OpenDBPaths() {
		want[name] = true
	}

	flushed := map[string]bool{}
	for _, st := range e.boot.Backup.Status(ctx) {
		if err := e.boot.Backup.Untrack(st.Name); err != nil {
			e.t.Errorf("final flush of %q: %v", st.Name, err)
			continue
		}
		flushed[st.Name] = true
	}
	for name := range want {
		if !flushed[name] {
			e.t.Errorf("Shutdown did not flush %q — the replica is not guaranteed to hold this "+
				"database's last writes, so anything asserted after a wipe would depend on "+
				"whether the background monitor happened to run", name)
		}
	}
	if err := e.manager.Close(); err != nil {
		e.t.Errorf("close repo manager: %v", err)
	}
	if err := e.boot.Backup.Close(ctx); err != nil {
		e.t.Errorf("close backup: %v", err)
	}
}

// ShutdownInServerOrder tears the instance down the way the SHIPPED server does:
// repos.Manager.Close() — which closes every SQLite handle — and only then
// backup.Manager.Close(), whose final sync is the last chance for anything
// written since the previous monitor tick to reach the replica. Nothing is
// untracked first.
//
// That is exactly cmd/serve's teardown: its `defer boot.Backup.Close` is
// registered before app.New's `defer a.Close`, so the repo manager unwinds first
// and the agent's sync runs last.
//
// It exists because Shutdown deliberately does the opposite (see there) and
// therefore leaves the shipped path with no coverage at all. Pair it with a
// MonitorInterval long enough that no background tick can be what carried the
// write, or the scenario passes without exercising the shutdown.
//
// The Close error is asserted rather than logged: a final sync that fails is the
// difference between a clean stop and losing every write since the last tick,
// and it is precisely what this teardown is here to prove does not happen.
func (e *Env) ShutdownInServerOrder() {
	e.t.Helper()
	if e.stopped {
		return
	}
	e.stopped = true

	if err := e.manager.Close(); err != nil {
		e.t.Fatalf("close repo manager: %v", err)
	}
	if err := e.boot.Backup.Close(context.Background()); err != nil {
		e.t.Fatalf("close backup after the repo manager: %v — the final sync is the only thing "+
			"carrying writes made since the last monitor tick", err)
	}
}

// DestroyHome deletes KNOMIT_HOME in full and puts back ONLY the SSH key — the
// exact loss this feature exists to survive. Everything else (control.db, every
// repo database, every litestream shadow directory, the models cache) is gone.
//
// The instance MUST already be shut down. Removing SQLite database files beneath
// a live mapping raises SIGBUS and takes the test binary down, so this refuses
// to run against an instance that is still up rather than producing a crash
// whose cause is three frames away from its symptom.
//
// The key survives because in production it is a mounted secret, not volume
// state. The hostname deliberately does NOT survive, which is what agent_name
// exists to absorb.
func (e *Env) DestroyHome() {
	e.t.Helper()
	if !e.stopped {
		e.t.Fatal("DestroyHome: the instance is still running — shut it down first, " +
			"or removing its mapped database files will raise SIGBUS")
	}
	key := SaveAgentKey(e.t, e.cfg.Home)
	if err := os.RemoveAll(e.cfg.Home); err != nil {
		e.t.Fatalf("DestroyHome(%s): %v", e.cfg.Home, err)
	}
	if err := os.MkdirAll(e.cfg.Home, 0o755); err != nil {
		e.t.Fatalf("DestroyHome(%s): recreate: %v", e.cfg.Home, err)
	}
	RestoreAgentKey(e.t, e.cfg.Home, key)

	// The fixture is only meaningful if the volume really is empty. A leftover
	// control.db would make the next boot's restore a silent no-op, and the
	// scenario would pass while proving nothing.
	if _, err := os.Stat(filepath.Join(e.cfg.Home, "control.db")); !os.IsNotExist(err) {
		e.t.Fatalf("DestroyHome: control.db survived the wipe (%v); the next boot would restore nothing", err)
	}
	if entries, err := os.ReadDir(filepath.Join(e.cfg.Home, "repos")); err == nil {
		e.t.Fatalf("DestroyHome: repos/ survived the wipe with %d entries; the next boot would restore nothing", len(entries))
	}
}

// MachineDerivedBranch returns the agent branch this machine would produce for
// the SAME key with NO agent_name configured — i.e. the hostname-derived branch
// knomit used before agent_name existed.
//
// It exists to make the identity assertions discriminating. A test that only
// checked "the branch is the same before and after" would pass against the very
// bug this feature was built for: on one machine, a hostname-derived branch is
// also stable. Comparing against this value pins the opposite property — that
// the branch is NOT the machine's — so a regression to hostname derivation
// fails rather than passing quietly.
//
// Derived through the real code path (app.Bootstrap with backup disabled, so the
// agent_name requirement does not apply) in a throwaway home holding a copy of
// the key.
func MachineDerivedBranch(t *testing.T, key []byte) string {
	t.Helper()
	home := t.TempDir()
	RestoreAgentKey(t, home, key)
	cfg := config.Defaults()
	cfg.Home = home
	cfg.AgentName = "" // fall back to the hostname, as knomit did before agent_name
	boot, err := app.Bootstrap(context.Background(), cfg)
	if err != nil {
		t.Fatalf("MachineDerivedBranch: Bootstrap: %v", err)
	}
	if boot.Backup != nil {
		t.Fatal("MachineDerivedBranch: backup must stay disabled here")
	}
	return boot.AgentBranch
}

// SplitAgentBranch breaks an agent branch into its two halves: the configured
// (or hostname) name and the SSH key fingerprint. The fingerprint is the last
// hyphen-separated component and is hex, so the split is unambiguous even when
// the name contains hyphens.
func SplitAgentBranch(t *testing.T, branch string) (name, fingerprint string) {
	t.Helper()
	const prefix = "agent/"
	if len(branch) <= len(prefix) || branch[:len(prefix)] != prefix {
		t.Fatalf("SplitAgentBranch(%q): not an agent branch", branch)
	}
	rest := branch[len(prefix):]
	i := -1
	for j := len(rest) - 1; j >= 0; j-- {
		if rest[j] == '-' {
			i = j
			break
		}
	}
	if i < 0 {
		t.Fatalf("SplitAgentBranch(%q): no fingerprint component", branch)
	}
	return rest[:i], rest[i+1:]
}
