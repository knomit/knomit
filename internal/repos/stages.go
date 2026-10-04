package repos

// The six stages of a mounted repo, in order. A repo is a cursor over them.
//
// FOUNDATION stages (Populate, Open, Identify) build the repo in the
// foreground; an Enter error is fatal and the machine becomes Unavailable.
// WORKER stages (Index, Serve, Sync) each start background work through their
// Life and return at once; they cannot fail, and nothing depends on them, so
// restarting one touches nothing else.
//
// Every Enter and Exit runs on the machine's driver goroutine, so a field a
// stage writes from Enter/Exit and reads from Enter/Exit only (upstreamMain,
// syncOrigin, closing, pendingBak) needs no lock. Background work started
// through Life.Go must not touch those fields.
//
// RULE 2: no stage sends to its own machine. A stage receives the
// *RepoInstance and its *Life, and neither exposes Send; background work may
// only Life.post an internal event or use the repo's 1-slot kick and wake
// channels (triggerKick, syncWake, consensusKick). A Send from here would wait
// on the driver that is running this very code (TestMachine_NoStageSends).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/platform/crashdump"
	"knomit/internal/store"
)

// StageID indexes the stage list. StageReady is the cursor past the last
// stage.
type StageID int

const (
	StagePopulate StageID = iota
	StageOpen
	StageIdentify
	StageIndex
	StageServe
	StageSync
	StageReady
)

// Stage is one step of a mounted repo's lifecycle.
type Stage interface {
	Name() string
	// Enter runs in the foreground. A worker launches its background work via
	// life.Go and returns at once.
	Enter(ctx context.Context, life *Life, r *RepoInstance) error
	// Exit runs after the stage's life was cancelled and drained. Idempotent.
	Exit(r *RepoInstance)
}

type stageEntry struct {
	Stage
	worker bool
}

// stages is the whole lifecycle. Adding a stage is one row here.
var stages = [StageReady]stageEntry{
	{populateStage{}, false},
	{openStage{}, false},
	{identifyStage{}, false},
	{indexStage{}, true},
	{serveStage{}, true},
	{syncStage{}, true},
}

// stageName names a cursor position, StageReady included.
func stageName(k StageID) string {
	if k >= StageReady {
		return "ready"
	}
	return stages[k].Name()
}

// Life owns everything one Enter of one stage started: a context derived from
// the machine's root (so Unmount reaches it), a WaitGroup of its goroutines,
// and the generation that stamps the internal events they post. Stale
// generations are ignored by the driver.
type Life struct {
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	gen    uint64
	stage  StageID
	post   func(internalEvent)
}

// Go runs f under this life. A panic is recovered, written as a crashdump and
// reported to the driver as crashed{stage}, which restarts the stage after the
// crash backoff (the driver never waits for it).
func (l *Life) Go(name string, f func(ctx context.Context)) {
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer func() {
			if rec := recover(); rec != nil {
				crashdump.ReportRecovered("lifecycle:"+name, rec)
				log.Error().Str("stage", stageName(l.stage)).Str("worker", name).Interface("panic", rec).
					Msg("lifecycle: a background worker panicked; the stage restarts after the crash backoff")
				l.post(crashed{stage: l.stage, gen: l.gen})
			}
		}()
		f(l.ctx)
	}()
}

// ---------------------------------------------------------------- Populate

type populateStage struct{}

func (populateStage) Name() string { return "populate" }

// Enter brings the repo's database into existence according to the mount
// spec: nothing for MountExisting, the create's init path otherwise — run
// under the life ctx, so an Unmount aborts a clone in flight.
func (populateStage) Enter(ctx context.Context, _ *Life, r *RepoInstance) error {
	spec := r.machine.mountSpec
	if spec.Mode != MountCreate {
		return nil
	}
	return r.env.m.populate(ctx, r, spec)
}

func (populateStage) Exit(*RepoInstance) {}

// -------------------------------------------------------------------- Open

type openStage struct{}

func (openStage) Name() string { return "open" }

// Enter opens the store and applies EVERY piece of per-store process wiring,
// for the first open and every reopen alike — there is no second list to
// mirror (kb/invariants/store/lifecycle/reopen-rewire). It never creates a
// repository: a .db with no git data is a broken repo, surfaced as an error.
func (openStage) Enter(_ context.Context, _ *Life, r *RepoInstance) error {
	env := r.env
	origin, err := r.storedOrigin()
	if err != nil {
		return fmt.Errorf("read origin: %w", err)
	}
	svc, err := store.Open(r.dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			svc.Close()
		}
	}()
	// Bound every remote git network op, and tell the store where facts live;
	// store.Open restores neither.
	svc.SetNetworkTimeout(env.cfg.Git.NetworkTimeout)
	svc.SetOntologyRoot(env.cfg.OntologyRoot)
	// A subscription accepts no authored commits on any branch; the flag does
	// not survive store.Open (kb/invariants/repos/subscription/readonly-survives-swap).
	svc.SetReadOnly(r.subscribed)
	// The origin before OpenRepo: rehydrating the upstream and the fetch
	// refspec read it. control.db owns it; the store holds no credential.
	if origin != nil {
		svc.SetOrigin(&store.Origin{URL: origin.URL, Branch: origin.Branch, AuthMethod: origin.AuthMethod, AuthToken: origin.AuthToken})
	}
	if env.embedder != nil {
		svc.SetEmbedder(env.embedder)
	}
	svc.SetSigner(env.signer)
	if m := env.m; m != nil {
		svc.SetAcceptList(m.acceptListFor(r.uid))
		svc.SetOwnKeys(m.ownFleetKeys)
	}
	if err := svc.OpenRepo(); err != nil {
		return fmt.Errorf("open git: %w", err)
	}
	r.upstreamMain = rehydrateUpstreamMain(svc, r.Name())
	read := r.agentBranch
	if read == "" {
		read = r.upstreamMain
	}
	// A subscription with no upstream would read from "" — no ontology, no
	// identity, no index. Create persists the RESOLVED upstream, so this
	// means a corrupted or hand-edited origin row.
	if r.subscribed && read == "" {
		return fmt.Errorf("subscription has no upstream branch recorded in its origin")
	}
	// The git remote is a DERIVED CACHE of control.db, rewritten at every open.
	if origin != nil && origin.URL != "" {
		if err := svc.ConfigureRemote(origin.URL, r.upstreamMain, r.agentBranch); err != nil {
			log.Warn().Err(err).Str("repo", r.Name()).
				Msg("configure git remote from control.db failed; sync may not reach the origin")
		}
	}
	r.setReadBranch(read)

	// The commit observer: index sync + SSE status on every commit, debounced.
	// SyncLocked, never bare Sync: it can fire while the index job rebuilds
	// this branch (kb/invariants/store/index/branch-locking).
	r.observer.Store(newCommitObserver(time.Second, func(hash string) {
		cur, release, aerr := r.Acquire()
		if aerr != nil {
			return
		}
		defer release()
		if serr := cur.IndexManager().SyncLocked(context.Background(), r.ReadBranch()); serr != nil {
			log.Warn().Err(serr).Str("repo", r.Name()).Msg("observer sync failed")
		}
		if r.hub != nil {
			r.hub.broadcastStatus(hash)
		}
	}))
	svc.SetOnCommit(r.onCommit)
	if !r.attachStore(svc) {
		return ErrRepoClosed
	}
	ok = true
	if bak := r.machine.pendingBak; bak != "" {
		os.Remove(bak)
		r.machine.pendingBak = ""
	}
	if r.hub != nil {
		if head, herr := svc.Branches().HeadCommit(context.Background(), read); herr == nil {
			r.hub.broadcastStatus(head)
		}
	}
	return nil
}

// Exit cancels the hub's tasks (each holds an Acquire, so the drain below
// would otherwise wait on work nobody told to stop), stops the observer, takes
// the store out of the instance, drains every in-flight Acquire and only then
// closes the service (kb/invariants/repos/store-lifetime). On Unmount the
// instance is marked closed, so later Acquires fail with ErrRepoClosed rather
// than ErrStoreUnavailable.
func (openStage) Exit(r *RepoInstance) {
	if r.hub != nil {
		r.hub.cancelTasks()
	}
	if obs := r.observer.Swap(nil); obs != nil {
		obs.Stop()
	}
	h := r.detachStore(r.machine.closing)
	if h == nil {
		return
	}
	h.wg.Wait()
	h.svc.Close()
}

// rehydrateUpstreamMain reads back the consensus branch this repo's origin
// tracks (persisted in control.db at clone time and injected above). EMPTY
// means this repo has no origin — the index plan relies on that, so it is
// never defaulted to "main".
func rehydrateUpstreamMain(svc *store.Service, repo string) string {
	remote, err := svc.Remote().GetRemote("origin")
	if err != nil {
		log.Warn().Err(err).Str("repo", repo).Msg("read stored remote failed; upstream branch unknown")
		return ""
	}
	if remote != nil {
		return remote.Branch
	}
	return ""
}

// storedOrigin reads this repo's origin from control.db (nil when it has none,
// or when the Manager has no control.db yet).
func (r *RepoInstance) storedOrigin() (*Origin, error) {
	if r.env.m == nil {
		return nil, nil
	}
	origins := r.env.m.Origins()
	if origins == nil {
		return nil, nil
	}
	return origins.Get(r.uid)
}

// ---------------------------------------------------------------- Identify

type identifyStage struct{}

func (identifyStage) Name() string { return "identify" }

// Enter establishes what this repo IS: its agent branch (adopted or taken over
// from the recorded owner), its ontology, its pipeline watermarks, its root
// commit, and the registry record of that root — the ONE RecordRepoID site.
// ensureBranch runs before loadOntology: on a restored home the agent branch
// is absent until ensureBranch adopts it, and loadOntology reads that branch.
func (identifyStage) Enter(ctx context.Context, _ *Life, r *RepoInstance) error {
	svc, release, err := r.Acquire()
	if err != nil {
		return err
	}
	defer release()
	r.ensureBranch(ctx, svc)
	ont, ontErr := r.loadOntology(ctx, svc)
	r.ident.Store(&identity{ontology: ont, ontologyErr: ontErr})
	r.seedWatermarks(ctx, svc)

	r.idMu.Lock()
	r.id = ""
	r.idMu.Unlock()
	id := r.ID()
	if id == "" {
		log.Warn().Str("repo", r.Name()).Msg("root commit unresolved; registry identity not recorded")
		return nil
	}
	reg := r.env.m.Repos()
	if reg == nil || r.uid == "" {
		return nil
	}
	if err := reg.RecordRepoID(r.uid, id); err != nil {
		// Another ACTIVE repo already holds this knowledge base: two local
		// copies would both write agent/<host> and clobber each other on push.
		if errors.Is(err, ErrRepoAlreadyRegistered) {
			return identityConflict{short: r.ShortID(), err: err}
		}
		// A transient failure leaves repo_id unset; the next Identify retries.
		log.Warn().Err(err).Str("repo", r.Name()).Msg("recording repo identity failed")
	}
	return nil
}

// identityConflict is Identify's fatal error: the knowledge base is already
// held by another active repo. It unwraps to ErrRepoAlreadyRegistered.
type identityConflict struct {
	short string
	err   error
}

func (e identityConflict) Error() string {
	return fmt.Sprintf("knowledge base %s is already held by another active repo", e.short)
}

func (e identityConflict) Unwrap() error { return e.err }

// Exit forgets the identity: the next Identify reads it from whatever store
// the walk opens.
func (identifyStage) Exit(r *RepoInstance) {
	r.ident.Store(&identity{ontologyErr: errNotIdentified})
	r.idMu.Lock()
	r.id = ""
	r.idMu.Unlock()
}

// ensureBranch creates the agent branch if it doesn't already exist. It does
// NOT touch the origin record: that is written once, by whichever path
// attached the origin, and re-seeding it on every open is how the upstream
// used to get silently rewritten to "main".
func (r *RepoInstance) ensureBranch(ctx context.Context, svc *store.Service) {
	if r.agentBranch == "" {
		return // a subscription has no agent branch
	}
	if err := svc.Branches().CreateBranch(ctx, r.agentBranch, r.seedSourceForAgentBranch(ctx, svc)); err != nil {
		log.Warn().Err(err).Str("repo", r.Name()).Msg("branch create/ensure failed")
		return
	}
	r.recordAgentBranchOwner(ctx, svc)
}

// seedSourceForAgentBranch returns the branch CreateBranch should seed this
// instance's agent branch from: the agent branch itself when it exists (a
// no-op), else the RECORDED owner of this database, which carries the
// accumulated knowledge when the SSH key or host name changed. Falls back to
// the agent branch whenever no usable record exists, so CreateBranch fails
// loudly rather than seeding from a broken source.
func (r *RepoInstance) seedSourceForAgentBranch(ctx context.Context, svc *store.Service) string {
	if _, err := svc.Branches().HeadCommit(ctx, r.agentBranch); err == nil {
		return r.agentBranch
	}
	owner, err := svc.Branches().AgentBranchOwner(ctx)
	if err != nil {
		log.Warn().Err(err).Str("repo", r.Name()).
			Msg("could not read the recorded agent branch owner; not taking over")
		return r.agentBranch
	}
	if owner == "" || owner == r.agentBranch {
		return r.agentBranch
	}
	if _, err := svc.Branches().HeadCommit(ctx, owner); err != nil {
		log.Warn().Err(err).Str("repo", r.Name()).Str("recorded_owner", owner).
			Msg("recorded agent branch owner no longer exists; not taking over")
		return r.agentBranch
	}
	log.Info().Str("repo", r.Name()).Str("agent_branch", r.agentBranch).Str("from", owner).
		Msg("agent branch absent; taking over the repo from the recorded owner")
	return owner
}

// recordAgentBranchOwner claims this repo database for this instance's agent
// branch, once CreateBranch has reported success.
func (r *RepoInstance) recordAgentBranchOwner(ctx context.Context, svc *store.Service) {
	if owner, err := svc.Branches().AgentBranchOwner(ctx); err == nil && owner == r.agentBranch {
		return
	}
	if err := svc.Branches().SetAgentBranchOwner(ctx, r.agentBranch); err != nil {
		log.Warn().Err(err).Str("repo", r.Name()).Str("branch", r.agentBranch).
			Msg("could not record the agent branch owner; a later takeover would have nothing to seed from")
	}
}

// loadOntology reads the ontology from the repo's read branch, walking
// fact.OntologyPathsNewestFirst so a legacy path still opens.
//
// IT NEVER SUBSTITUTES. A repository is a knomit knowledge base if and only if
// it has an ontology, fixed at create time; when it cannot be established the
// error is returned instead and the repo opens readable but unwritable
// (RepoInstance.WritableBranch).
//
// A preset-derived ontology that is a strict subset of its embedded preset is
// refreshed in place (refreshDivergence); a diverged one is left alone with a
// warning. A subscription never writes, so it never refreshes.
func (r *RepoInstance) loadOntology(ctx context.Context, svc *store.Service) (*fact.Ontology, error) {
	read := r.ReadBranch()
	paths := fact.OntologyPathsNewestFirst()
	var srcPath, content string
	for _, p := range paths {
		result, rerr := svc.Facts().ReadFact(ctx, read, p, nil)
		if rerr == nil && result.Content != "" {
			srcPath, content = p, result.Content
			break
		}
	}
	if content == "" {
		log.Error().Str("repo", r.Name()).Str("branch", read).
			Msgf("no ontology at %s: this repository is not a knowledge base and will not accept writes", strings.Join(paths, ", "))
		return nil, fmt.Errorf("no ontology at %s on %s", strings.Join(paths, ", "), read)
	}
	if srcPath != OntologyPath {
		log.Info().Str("repo", r.Name()).
			Msgf("ontology loaded from the legacy path %s; rename it to %s", srcPath, OntologyPath)
	}
	ont, err := fact.ParseOntology([]byte(content))
	if err != nil {
		log.Error().Err(err).Str("repo", r.Name()).Str("path", srcPath).
			Msg("ontology does not parse: this repository will not accept writes until it does")
		return nil, fmt.Errorf("ontology at %s does not parse: %w", srcPath, err)
	}
	if preset := fact.EmbeddedPresetByID(ont.ID); !r.subscribed && preset != nil {
		if divergence := refreshDivergence(ont, preset); divergence == "" {
			storedY, sErr := ont.Serialize()
			presetY, pErr := preset.Serialize()
			if sErr == nil && pErr == nil && !bytes.Equal(storedY, presetY) {
				log.Info().Str("repo", r.Name()).Str("preset_id", ont.ID).Str("path", srcPath).
					Msg("ontology refresh: stored is subset of embedded preset; upgrading to latest")
				if _, werr := svc.Facts().WriteFact(ctx, read, srcPath, string(presetY),
					fmt.Sprintf("ontology: refresh to embedded %s preset", ont.ID), "updated"); werr != nil {
					log.Warn().Err(werr).Str("repo", r.Name()).Msg("ontology refresh: write failed, keeping stored")
				} else {
					ont = preset
				}
			}
		} else {
			log.Warn().Str("repo", r.Name()).Str("preset_id", ont.ID).Str("reason", divergence).
				Msg("ontology refresh: stored has diverged from embedded preset; upgrade skipped")
		}
	}
	return ont, nil
}

// refreshDivergence is SubsetDivergence plus one rule of its own: the refresh
// never ADDS or CHANGES a repository-level (root) attribute. The refresh
// commit is signed by this instance, and a change to verify_signatures is a
// policy decision for whoever merges to the repo's main, never an upgrade's.
func refreshDivergence(stored, preset *fact.Ontology) string {
	if d := stored.SubsetDivergence(preset); d != "" {
		return d
	}
	if !fact.RootAttributesEqual(stored, preset) {
		return fact.DivergenceAttributes
	}
	return ""
}

// seedWatermarks sets the pipeline watermark to HEAD for any tool that has
// none on the agent branch, so the first pipeline run only processes facts
// written after this point. A subscription runs no pipelines.
func (r *RepoInstance) seedWatermarks(ctx context.Context, svc *store.Service) {
	if r.agentBranch == "" {
		return
	}
	for _, tool := range []string{"review", "hypothesize"} {
		if wm, _ := svc.Pipeline().GetPipelineWatermark(ctx, tool, r.agentBranch); wm == "" {
			if head, err := svc.Branches().HeadCommit(ctx, r.agentBranch); err == nil {
				if err := svc.Pipeline().SetPipelineWatermark(ctx, tool, r.agentBranch, head); err != nil {
					log.Warn().Err(err).Str("tool", tool).Msg("pipeline watermark: initial set failed")
				}
			}
		}
	}
}

// ------------------------------------------------------------------- Index

type indexStage struct{}

func (indexStage) Name() string { return "index" }

// indexSpec is what the next Index Enter runs: the startup heal, or a full
// rebuild of one branch (Rebuild). Driver-owned.
type indexSpec struct {
	full   bool
	branch string
}

// Enter plans the index work and starts the job. The plan is cheap (per-branch
// schema-version reads); the job is the heavy part and runs under the life,
// posting indexDone{gen, err} when it returns, however it returns. A panic in
// the job is its result (an error), not a crash: the stage stays entered and
// the next Rebuild is accepted.
func (indexStage) Enter(ctx context.Context, life *Life, r *RepoInstance) error {
	m := r.machine
	spec := m.indexSpec
	gen := life.gen
	svc, release, err := r.Acquire()
	if err != nil {
		// No store: nothing to index. That is the job's result, reported the
		// way every result is, so the status stops reading "indexing".
		life.post(indexDone{gen: gen, err: fmt.Errorf("index: %w", err)})
		return nil
	}
	var branches []healBranch
	if spec.full {
		branches = []healBranch{{name: spec.branch, stale: true}}
	} else {
		branches = r.planIndex(ctx, svc)
	}
	hook := m.opts.Hook
	progress := func(phase string, done, total int) {
		r.indexProgress.Store(&indexProgress{phase: phase, done: done, total: total})
		m.postProgress(gen)
	}
	life.Go("index", func(ctx context.Context) {
		defer release()
		var jobErr error
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					crashdump.ReportRecovered("index:"+r.Name(), rec)
					jobErr = fmt.Errorf("index job panicked: %v", rec)
				}
			}()
			if hook != nil {
				hook(StageIndex, "index-job", ctx)
			}
			if spec.full {
				jobErr = svc.IndexManager().Rebuild(ctx, spec.branch, progress)
				return
			}
			if !healIndexBranches(ctx, svc.IndexManager(), r.Name(), branches, progress) {
				jobErr = errHealIncomplete
			}
		}()
		if jobErr == nil && ctx.Err() != nil {
			jobErr = ctx.Err()
		}
		life.post(indexDone{gen: gen, err: jobErr})
	})
	return nil
}

func (indexStage) Exit(*RepoInstance) {}

// errHealIncomplete is the heal's verdict when the branch local reads depend
// on could not be indexed.
var errHealIncomplete = errors.New("the index heal did not complete; see the server log")

// planIndex collects the branches whose index the heal maintains: the read
// branch, plus the upstream for a writable repo with an origin
// (kb/invariants/repos/initial-upstream-index-sync), each with its own
// full-rebuild-or-incremental verdict. upstreamMain is non-empty exactly when
// this repo has a stored origin, so it doubles as the has-origin test.
func (r *RepoInstance) planIndex(ctx context.Context, svc *store.Service) []healBranch {
	names := []string{r.ReadBranch()}
	if r.upstreamMain != "" && r.upstreamMain != names[0] {
		names = append(names, r.upstreamMain)
	}
	names = r.dropUnresolvableBranches(ctx, svc, names)
	// Per branch, not once for the repo: the schema version is keyed per
	// branch so a branch that could not be rebuilt last time is the only one
	// retried.
	branches := make([]healBranch, 0, len(names))
	for _, name := range names {
		stale, err := svc.IndexManager().NeedsRebuild(ctx, name)
		if err != nil {
			log.Warn().Err(err).Str("repo", r.Name()).Str("branch", name).
				Msg("index schema version check failed; assuming current")
		}
		branches = append(branches, healBranch{name: name, stale: stale})
	}
	return branches
}

// dropUnresolvableBranches removes upstream branches whose ref does not
// resolve, so the heal never schedules work that cannot succeed (a stored
// upstream naming a branch this repo does not have is a configuration
// disagreement, not an index problem). The read branch is never dropped: a
// failing heal is the right way to surface it.
func (r *RepoInstance) dropUnresolvableBranches(ctx context.Context, svc *store.Service, names []string) []string {
	out := make([]string, 0, len(names))
	for i, name := range names {
		if i > 0 {
			if _, err := svc.Branches().HeadCommit(ctx, name); err != nil {
				log.Warn().Err(err).Str("repo", r.Name()).Str("branch", name).
					Msg("stored upstream branch does not exist in this repo; skipping its index heal " +
						"(check the origin's default branch against the remotes row)")
				continue
			}
		}
		out = append(out, name)
	}
	return out
}

// healBranch is one unit of index heal work: a branch, plus whether ITS
// persisted schema version is behind.
type healBranch struct {
	name  string
	stale bool
}

// healIndexBranches brings each maintained branch's search index up to date:
// a stale branch is full-Rebuilt, the rest are incrementally synced under
// lockBranch (SyncLocked). It returns false when the read branch (index 0)
// failed; an upstream-only failure is logged but not fatal — the sync loop
// owns upstream convergence. Nothing here re-arms a retry: Rebuild drops a
// failed branch's schema version key, so it reports stale again next time.
func healIndexBranches(ctx context.Context, im store.IndexManager, repo string, branches []healBranch, progress store.RebuildProgress) (ok bool) {
	healFailed := false
	for i, branch := range branches {
		if branch.stale {
			if err := im.Rebuild(ctx, branch.name, progress); err != nil {
				level := log.Warn().Err(err).Str("repo", repo).Str("branch", branch.name)
				if i == 0 {
					level.Msg("schema-mismatch rebuild failed")
					healFailed = true
				} else {
					level.Msg("schema-mismatch rebuild (upstream) failed; the next heal retries this branch")
				}
			}
			continue
		}
		if err := im.SyncLocked(ctx, branch.name); err != nil {
			level := log.Warn().Err(err).Str("repo", repo).Str("branch", branch.name)
			if i == 0 {
				level.Msg("initial index sync failed")
				healFailed = true
			} else {
				level.Msg("initial index sync (upstream) failed; reconcile loop will retry")
			}
		}
	}
	return !healFailed
}

// ------------------------------------------------------------------- Serve

type serveStage struct{}

func (serveStage) Name() string { return "serve" }

// Enter starts the repo's lifetime workers: the trigger dispatcher and the
// consensus merger (built for a writable repo with an agent branch), and the
// experiment sweep (not for a subscription, not when expiry_days is 0, not
// under Synchronous). Each reaches the store through Acquire per run, and each
// is drained before Open closes the store.
func (serveStage) Enter(_ context.Context, life *Life, r *RepoInstance) error {
	if d := r.triggers; d != nil {
		life.Go("triggers", d.run)
	}
	if c := r.consensus; c != nil {
		life.Go("consensus", c.run)
	}
	if s := r.sweep; s != nil && !r.machine.opts.Synchronous {
		life.Go("experiment-sweep", s.run)
	}
	return nil
}

func (serveStage) Exit(*RepoInstance) {}

// -------------------------------------------------------------------- Sync

type syncStage struct{}

func (syncStage) Name() string { return "sync" }

// Enter starts the reconcile loop the origin selects: runReconcileLoop with an
// origin, runLocalReconcileLoop without. The loop's immediate first round is
// the startup reconcile. One loop per Sync life is what keeps the two
// mutually exclusive; an origin appearing or disappearing while it runs is a
// skipped tick, never an exit (kb/invariants/store/local-reconcile).
//
// Under Synchronous it runs ONE round inline and starts no loop.
//
// The store is acquired for the whole Sync life, released only when the loop
// returns (or the inline round ends), the same as the index job: the loop's
// svc stays valid by construction (kb/invariants/repos/store-lifetime), not
// by stage order. Open.Exit's drain cannot wait on it forever, because every
// exit drains Sync (newest-first) before Open, and the loop returns on its
// life's ctx.
func (syncStage) Enter(ctx context.Context, life *Life, r *RepoInstance) error {
	svc, release, err := r.Acquire()
	if err != nil {
		return nil
	}
	r.ensureLocalUpstream(ctx, svc)
	env := r.env
	cfg := env.cfg
	mode := func() *syncMode {
		return newSyncMode(svc, r.Name(), r.agentBranch, cfg.ReadOnly, cfg.Git.RealtimePullInterval, &r.realtimePush)
	}
	// GetRemote reports (nil, nil) exactly when no origin is injected; an
	// error can only come from reading the status row of an origin that IS
	// injected.
	remote, rerr := svc.Remote().GetRemote("origin")
	hasOrigin := remote != nil || rerr != nil
	once := r.machine.opts.Synchronous
	r.syncOrigin = ""
	name := "local-reconcile"
	run := func(ctx context.Context) {
		runLocalReconcileLoop(ctx, svc, r.Name(), r.agentBranch, cfg.Git.LocalReconcileInterval,
			r.triggerKick, r.syncWake, mode(), once)
	}
	if hasOrigin {
		if remote != nil {
			r.syncOrigin = remote.URL
		}
		authFn := makeRemoteAuthFn(cfg.Remote, env.keyPath)
		name = "reconcile"
		run = func(ctx context.Context) {
			runReconcileLoop(ctx, svc, r.hub, r.Name(), r.agentBranch, authFn, cfg.LocalOriginRoot, cfg.ReadOnly,
				env.onPush, r.triggerKick, r.syncWake, r.breakers, mode(), once)
		}
	}
	if once {
		defer release()
		run(ctx)
		return nil
	}
	life.Go(name, func(ctx context.Context) {
		defer release()
		run(ctx)
	})
	return nil
}

func (syncStage) Exit(r *RepoInstance) { r.syncOrigin = "" }

// ensureLocalUpstream repairs a store that holds refs/remotes/origin/<upstream>
// but no local <upstream>: the served advertisement takes HEAD from the LOCAL
// upstream ref, so without this a peer cloning before the first successful
// fetch gets an advertisement with no HEAD. reconcileMain remains the
// authoritative repair; a failure here is logged and the loop retries.
func (r *RepoInstance) ensureLocalUpstream(ctx context.Context, svc *store.Service) {
	created, err := svc.EnsureLocalUpstream(ctx, r.upstreamMain)
	if err != nil {
		log.Warn().Err(err).Str("repo", r.Name()).
			Msg("ensureLocalUpstream: bootstrap failed; the reconcile loop will repair it")
		return
	}
	if created {
		log.Info().Str("repo", r.Name()).Str("upstream", r.upstreamMain).
			Msg("ensureLocalUpstream: bootstrapped local upstream from origin")
	}
}
