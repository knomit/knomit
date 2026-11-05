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

	"knomit/internal/corpus/repos/control"

	"knomit/internal/engine/store/index"
	"knomit/internal/engine/store/model"

	"github.com/rs/zerolog/log"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/remotesync"
	"knomit/internal/platform/crashdump"
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
		svc.Remote().SetOrigin(&remotesync.Origin{URL: origin.URL, Branch: origin.Branch, AuthMethod: origin.AuthMethod, AuthToken: origin.AuthToken, SecretID: origin.SecretID})
	}
	if env.embedder != nil {
		svc.SetEmbedder(env.embedder)
	}
	svc.SetSigner(env.signer)
	svc.SetAgentID(env.agentID)
	if r.fleetDoor {
		// A copy that is not the master on main never re-submits its own
		// claim to the role, or its stand-down (user ruling 2026-10-16).
		svc.SetReplaySkipPaths(fact.FleetMasterPath)
	}
	if m := env.m; m != nil {
		svc.SetAcceptList(m.VerifyAccepts().For(r.uid))
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
		if err := ConfigureGitRemote(svc, origin.URL, origin.AuthMethod, r.upstreamMain, r.agentBranch); err != nil {
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
		// The live ontology reload's catch-all trigger (#429): every ref
		// advance the store makes passes NotifyCommit and lands here, so a
		// merge into the read branch, a receive-pack or any other way the
		// branch moves applies a changed ontology within one debounce. An
		// unchanged ontology blob costs a ref read and a tree lookup.
		r.reloadOntology(context.Background(), cur)
		if r.pub != nil {
			r.pub.BroadcastStatus(hash)
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
	if r.pub != nil {
		if head, herr := svc.HeadCommit(context.Background(), read); herr == nil {
			r.pub.BroadcastStatus(head)
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
	if r.pub != nil {
		r.pub.CancelTasks()
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
func (r *RepoInstance) storedOrigin() (*control.Origin, error) {
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
	// ontMu across the read AND the store: a live reload (reloadOntology)
	// that ran in between would see the repo not identified yet and do
	// nothing, but one that stored a newer tip first would then be
	// overwritten by this older read. Holding it is safe: loadOntology's
	// preset refresh takes a branch lock, and nothing holding a branch lock
	// ever waits on ontMu.
	r.ontMu.Lock()
	ont, src, ontErr := r.loadOntology(ctx, svc)
	// A symlinked ontology REFUSES the repo (user ruling 2026-10-08): the walk
	// fails here, the machine is Unavailable, and the boot path lists the repo
	// as "unopenable" with this error as its detail — the way an identity
	// conflict does. A missing, empty or unparseable ontology is not refused:
	// that repo mounts readable but unwritable, with the reason in
	// OntologyError.
	if errors.Is(ontErr, fact.ErrSymlinkNotFollowed) {
		r.ontMu.Unlock()
		return ontErr
	}
	// The reserved fleet id (fleet_chokepoint.go): a fleet repository is
	// mounted only through the fleet code's door or at boot; every other
	// mount — clone, subscribe, restore, a swapped store — is refused here.
	if err := admitFleetOntology(r, ont); err != nil {
		r.ontMu.Unlock()
		return err
	}
	r.setIdentity(svc, &identity{ontology: ont, ontologyErr: ontErr, src: src})
	r.ontMu.Unlock()
	r.seedWatermarks(ctx, svc)

	r.idMu.Lock()
	r.id = ""
	r.idMu.Unlock()
	id := r.ID()
	if id == "" {
		log.Warn().Str("repo", r.Name()).Msg("root commit unresolved; registry identity not recorded")
		return nil
	}
	return r.recordIdentity(id)
}

// recordIdentity records id, the root commit of r's read branch, as r's
// knowledge base in the registry: the one RecordRepoID call for a mounted
// repo. Identify calls it on every mount, and reidentifyAfterRewind after a
// sync round that replaced the consensus branch with another history.
// Another ACTIVE repo already holding id is identityConflict; any other
// failure is logged and leaves the record as it was, for the next call.
func (r *RepoInstance) recordIdentity(id string) error {
	reg := r.env.m.Repos()
	if reg == nil || r.uid == "" {
		return nil
	}
	if err := reg.RecordRepoID(r.uid, id); err != nil {
		// Another ACTIVE repo already holds this knowledge base: two local
		// copies would both write agent/<host> and clobber each other on push.
		if errors.Is(err, control.ErrRepoAlreadyRegistered) {
			short := id
			if len(short) > 12 {
				short = short[:12]
			}
			return identityConflict{short: short, err: err}
		}
		// A transient failure leaves repo_id as it was; the next call retries.
		log.Warn().Err(err).Str("repo", r.Name()).Msg("recording repo identity failed")
	}
	return nil
}

// reidentifyAfterRewind runs after a sync round that force-updated the
// consensus branch (remotesync ModeRewound): an attach that admitted an
// unrelated history (replace_main, PATCH /origin/upstream with replace_main)
// has just replaced it, and the agent branch was replayed onto it, so the
// root of the read branch, the repo's knowledge base, may now be the
// origin's. It is recorded at once, with Identify's rule (recordIdentity on
// RootCommit of the read branch), so HeldByAnotherActiveRepo names this repo
// for that knowledge base from this round on rather than from its next
// mount; a second local copy is then refused at create. A round whose agent
// branch kept its root records the same id again, which changes nothing.
//
// The attach guard has already refused a root another active repo held at
// the time of the attach. A conflict here means one was registered between
// that check and this round: it is logged, and the next mount's Identify
// turns this repo Unavailable as it always has. The in-memory ID (kb://
// addressing) is not touched; the next Identify re-reads it.
func (r *RepoInstance) reidentifyAfterRewind(ctx context.Context, svc *store.Service) {
	root, err := svc.RootCommit(ctx, r.ReadBranch())
	if err != nil || root == "" {
		log.Warn().Err(err).Str("repo", r.Name()).Msg("re-identify after a rewound consensus branch: root commit unresolved")
		return
	}
	if err := r.recordIdentity(root); err != nil {
		log.Error().Err(err).Str("repo", r.Name()).
			Msg("the replaced consensus branch holds a knowledge base another active repo holds; this repo will be Unavailable at its next mount")
	}
}

// afterFetch is what a successful fetch round of the origin loop runs when it
// returns (runReconcileLoop's fetched): the rewound branch's re-identify when
// the round force-updated the consensus branch, then the live ontology
// reload, on EVERY successful round. The reload is here as well as on the
// commit observer so a round's ontology is in force when the round returns —
// not one debounce later — which is what a caller that syncs and then
// writes (and a Synchronous test) relies on.
func (r *RepoInstance) afterFetch(ctx context.Context, svc *store.Service, rewound bool) {
	if rewound {
		r.reidentifyAfterRewind(ctx, svc)
	}
	r.reloadOntology(ctx, svc)
}

// reloadOntology applies the ontology at the tip of the read branch when it
// is not the file in force: knomit/knomit#429, "ontology changes that arrive
// through git are not loaded until the server restarts".
//
// TRIGGERS. It runs after every successful fetch round (afterFetch) and from
// the commit observer, which fires (debounced) after every ref advance the
// store makes on any branch: a sync merge or fast-forward, a consensus merge
// into the agent branch, a receive-pack, a local write. So whatever moves
// the read branch — sync, an origin attach or upstream re-point (its first
// round), a merge, a raw push — is covered by one of the two, and a store
// swap re-runs Identify itself. An index rebuild moves no ref.
//
// THE COMPARISON. The tip's first rung present is compared with the file the
// repo's state came from by path and blob id (identity.src) — usable or not.
// Equal: nothing is read or parsed — almost every commit leaves the ontology
// alone, and an unusable file already judged stays judged. Only a new blob
// is read and parsed.
//
// CONSENSUS IS HONOURED IN FULL (user ruling on #429,
// kb/decisions/repos/ontology/consensus-reload-honors-all). The main branch
// is consensus: what is on it was admitted at the consensus gate (signing,
// review, merge), and a usable ontology there REPLACES the one in force
// as-is — added topics and categories, every attribute changed or added
// (verify_signatures, learn_dedup and the rest), removed or renamed topics
// and validations. There is deliberately NO subset check, no additive-only
// filter and no attribute guard here: security is the gate's job, not the
// job of a consumer of main, and a filter here would be "fixing the security
// in the wrong place". DO NOT import refreshDivergence or SubsetDivergence:
// those rules belong to the boot preset refresh (knomit's release
// overwriting the stored file in an instance-signed commit), a different
// path with a different author. Nor does a reload run either refresh: it
// writes nothing.
//
// UNUSABLE MAKES THE REPO UNWRITABLE (user decision 1 on #429's review). A
// tip whose ontology cannot be used at all — missing at every rung, an empty
// first rung, a fatal parse error, or a symlink: exactly what readOntology
// refuses — is the state a restart would give: the repo has NO ontology in
// force (OntologyError), WritableBranch refuses every authored write, and
// the store refuses every fact write by any writer (SetOntologyUnusable ->
// validateWrite), so nothing is written with its validations skipped. The
// previous ontology is NOT kept: the repo cannot run on content it cannot
// read, and a stale ontology would silently diverge from consensus. Status
// reports it (OntologyStatus.Error; GET /repos ontology_error, GET
// /repos/{name} ontology.error). When consensus fixes the file — a new blob
// — the next reload applies it and the repo is writable again, no restart.
// A symlink makes the live repo unwritable, not Unavailable: Identify's
// refusal of it still applies at the next mount.
//
// A READ error is not unusable: the file could not be read at all, which
// says nothing about it (ontologyReadError). It changes and records nothing,
// and the next round or commit tries again.
//
// THE FLEET ID on a repo that is not the fleet repository (user decision 2):
// the repo becomes UNAVAILABLE with cause fleet_conflict, exactly as a mount
// of it is (admitFleetOntology at Identify). It is never applied as a fleet
// repository (Manager.fleetRepo finds the fleet by ontology id) and never
// kept writable. The reload marks it unwritable at once and hands the
// unmount to the Manager on another goroutine (retireFleetConflict): the
// unmount drains the Sync stage this reload may be running in. Its way back
// is the mount's: archive and purge, or a store whose read branch no longer
// carries the fleet id. A fleet repository (the fleet door) whose consensus
// ontology changes — to a newer fleet ontology, or away from the fleet id —
// has it applied like any repo's, with a Warn naming the transition: that is
// what its next mount would load.
//
// EVERYTHING DOWNSTREAM FOLLOWS because it reads ri.Ontology() (or the git
// file itself) per operation, never a copy: write placement, context and
// learn_dedup in the MCP and REST writers, WritableBranch and IsFleet (and
// with it fleet admission and Manager.fleetRepo), federated topic filtering,
// resolutions, the review pipeline, the MCP instructions of every session
// initialised after the swap (a session's instructions are sent once, at
// initialize), and artifact rules. The store's JS validations, the trigger
// dispatcher, verify_signatures, consensus, conflicts, sync and guidance read
// the ontology from git at their own tip and never used this snapshot. The
// swap is one atomic pointer store, so a writer that loads the ontology once
// validates against exactly one.
func (r *RepoInstance) reloadOntology(ctx context.Context, svc *store.Service) {
	if svc == nil {
		return
	}
	r.ontMu.Lock()
	defer r.ontMu.Unlock()
	cur := r.ident.Load()
	// Not identified (before Identify, or between an Exit and the next
	// Enter during a swap): Identify decides, not the reload.
	if cur == nil || errors.Is(cur.ontologyErr, errNotIdentified) {
		return
	}
	c, p, b, berr := svc.OntologyBlobAt(ctx, r.ReadBranch())
	tip := ontologySource{Commit: c, Path: p, Blob: b}
	if berr != nil && !errors.Is(berr, fact.ErrSymlinkNotFollowed) {
		// The tip cannot be read at all (a store closing under a swap, a
		// transient storage error): that says nothing about the ontology.
		// The next round or commit asks again.
		log.Debug().Err(berr).Str("repo", r.Name()).Msg("ontology reload: read branch tip unreadable; nothing changed")
		return
	}
	if tip.sameFile(cur.src) {
		return
	}
	ont, src, err := r.readOntology(ctx, svc)
	var unread ontologyReadError
	if errors.As(err, &unread) {
		// Unknown, not unusable: record NOTHING, so src never names a blob
		// that was not judged and the next round or commit reads it again.
		// A read error must never keep usable consensus content out, nor
		// make a working repo unwritable.
		log.Warn().Err(err).Str("repo", r.Name()).Str("commit", tip.Commit).Str("blob", tip.Blob).
			Msg("ontology reload: the read branch's ontology could not be read; nothing changed, retrying on the next round or commit")
		return
	}
	if err != nil {
		log.Error().Err(err).Str("repo", r.Name()).Str("commit", src.Commit).Str("path", src.Path).Str("blob", src.Blob).
			Str("previous_blob", cur.src.Blob).
			Msg("ontology reload: the read branch's ontology is unusable; the repository is UNWRITABLE until consensus fixes it")
		r.setIdentity(svc, &identity{ontologyErr: err, src: src})
		return
	}
	if aerr := admitFleetOntology(r, ont); aerr != nil {
		ferr := fmt.Errorf("ontology at %s on %s: %w", src.Path, r.ReadBranch(), aerr)
		log.Error().Err(ferr).Str("repo", r.Name()).Str("commit", src.Commit).Str("blob", src.Blob).
			Msg("ontology reload: the read branch's ontology took the reserved fleet id on a repository that is not the fleet repository; it is unavailable (fleet_conflict), as a mount of it would be")
		r.setIdentity(svc, &identity{ontologyErr: ferr, src: src})
		if m := r.env.m; m != nil && r.fleetConflictRetired.CompareAndSwap(false, true) {
			go m.retireFleetConflict(r, ferr)
		}
		return
	}
	// A FLEET TRANSITION is applied like any consensus change (that is what
	// the next mount would load), but it changes what the repo IS, so it is
	// a Warn that names the edge. Dropping the fleet id on the fleet
	// repository makes IsFleet false at once: WritableBranch opens authored
	// writes on it, and Manager.fleetRepo no longer finds a fleet (nil), so
	// every fleet duty stops. Gaining it (only possible through the fleet
	// door; admitFleetOntology refused it otherwise) makes the repo
	// read-only for authored writes and the instance's fleet repository.
	wasFleet, isFleet := cur.ontology != nil && fact.IsFleetOntology(cur.ontology), fact.IsFleetOntology(ont)
	switch {
	case wasFleet && !isFleet:
		log.Warn().Str("repo", r.Name()).Str("commit", src.Commit).Str("blob", src.Blob).Str("ontology_id", ont.ID).
			Msg("ontology reload: FLEET -> NOT FLEET: the fleet repository's consensus ontology dropped the fleet id; it is now an ordinary repository — authored writes are open on it and this instance no longer has a fleet repository")
	case !wasFleet && isFleet:
		log.Warn().Str("repo", r.Name()).Str("commit", src.Commit).Str("blob", src.Blob).Str("previous_id", idOf(cur.ontology)).
			Msg("ontology reload: NOT FLEET -> FLEET: the repository's consensus ontology took the fleet id; it is now the fleet repository and read-only for authored writes")
	}
	log.Info().Str("repo", r.Name()).Str("commit", src.Commit).Str("path", src.Path).Str("blob", src.Blob).
		Str("previous_blob", cur.src.Blob).Str("ontology_id", ont.ID).
		Msg("ontology reload: the read branch holds a different ontology; it is now in force")
	r.setIdentity(svc, &identity{ontology: ont, src: src})
}

// setIdentity installs id as the repo's identity and tells the store whether
// fact writes are possible: an identity with no usable ontology refuses every
// fact write on the store (svc.SetOntologyUnusable), whoever the writer, so
// the advertised flag (WritableBranch) and the store's write seam can never
// disagree. The caller holds ontMu.
func (r *RepoInstance) setIdentity(svc *store.Service, id *identity) {
	r.ident.Store(id)
	if svc != nil {
		svc.SetOntologyUnusable(id.ontologyErr)
	}
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
	r.ontMu.Lock()
	r.ident.Store(&identity{ontologyErr: errNotIdentified})
	r.ontMu.Unlock()
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
	if err := svc.CreateBranch(ctx, r.agentBranch, r.seedSourceForAgentBranch(ctx, svc)); err != nil {
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
	if _, err := svc.HeadCommit(ctx, r.agentBranch); err == nil {
		return r.agentBranch
	}
	owner, err := svc.AgentBranchOwner(ctx)
	if err != nil {
		log.Warn().Err(err).Str("repo", r.Name()).
			Msg("could not read the recorded agent branch owner; not taking over")
		return r.agentBranch
	}
	if owner == "" || owner == r.agentBranch {
		return r.agentBranch
	}
	if _, err := svc.HeadCommit(ctx, owner); err != nil {
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
	if owner, err := svc.AgentBranchOwner(ctx); err == nil && owner == r.agentBranch {
		return
	}
	if err := svc.SetAgentBranchOwner(ctx, r.agentBranch); err != nil {
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
// (RepoInstance.WritableBranch) — except a symlinked ontology, whose error
// (fact.ErrSymlinkNotFollowed) makes Identify refuse the repo outright.
//
// A preset-derived ontology that is a strict subset of its embedded preset is
// refreshed in place (refreshDivergence); a diverged one is left alone with a
// warning. A subscription never writes, so it never refreshes. The refreshes
// (this one and refreshFleetOntology) are KNOMIT'S RELEASE overwriting the
// stored file, and they run here, at Identify, only — never on the live
// reload, which applies what consensus delivered and writes nothing.
//
// src names the file the returned ontology (or error) came from; after a
// refresh that wrote, it is the refreshed file at the new tip.
func (r *RepoInstance) loadOntology(ctx context.Context, svc *store.Service) (*fact.Ontology, ontologySource, error) {
	read := r.ReadBranch()
	ont, src, err := r.readOntology(ctx, svc)
	var unread ontologyReadError
	if errors.As(err, &unread) {
		// At Identify a read failure leaves nothing usable, as it always
		// has: the repo mounts unwritable with the "no ontology" reason it
		// has always given, now carrying the cause. src stays EMPTY, so the
		// live reload does not take this tip's file as already judged: the
		// first trigger that can read it applies it.
		paths := strings.Join(fact.OntologyPathsNewestFirst(), ", ")
		log.Error().Err(unread.err).Str("repo", r.Name()).Str("branch", read).
			Msgf("no ontology at %s: this repository is not a knowledge base and will not accept writes", paths)
		return nil, ontologySource{}, fmt.Errorf("no ontology at %s on %s: %w", paths, read, err)
	}
	if err != nil {
		return nil, src, err
	}
	var refreshed *fact.Ontology
	if fact.IsFleetOntology(ont) {
		refreshed = r.refreshFleetOntology(ctx, svc, read, src.Path, ont)
	} else {
		refreshed = r.refreshPreset(ctx, svc, read, src.Path, ont)
	}
	if refreshed != ont {
		// The refresh committed a new file: name THAT one, or the next
		// reload would find "a different blob" and re-parse what is already
		// in force. A failed read keeps the old source, which costs one
		// harmless re-parse.
		if c, p, b, berr := svc.OntologyBlobAt(ctx, read); berr == nil {
			src = ontologySource{Commit: c, Path: p, Blob: b}
		}
	}
	return refreshed, src, nil
}

// ontologyReadError is readOntology's "the ontology could not be READ" —
// the tip, its tree or the file's blob was unreadable (a store closing under
// a swap, a transient storage error, a branch not there yet). It says
// nothing about what the file holds, so it is never a verdict: the live
// reload records nothing for it and retries on its next trigger, and
// Identify reports it under the "no ontology" reason with an empty source.
type ontologyReadError struct {
	branch string
	err    error
}

func (e ontologyReadError) Error() string {
	return fmt.Sprintf("the ontology on %s could not be read: %v", e.branch, e.err)
}

func (e ontologyReadError) Unwrap() error { return e.err }

// readOntology reads and parses the ontology at the tip of the repo's read
// branch, with the rules loadOntology documents (first rung present; a
// symlink, a missing, an empty or an unparseable file is an error, never a
// substitute). It writes nothing and refreshes nothing: it is the half of
// loadOntology the live reload shares. src names the file read — commit,
// rung, blob — on success and on every error the tip could be resolved for.
// The symlink message names the mount's refusal: on a live reload the same
// file keeps the ontology in force instead, and the next mount refuses it.
func (r *RepoInstance) readOntology(ctx context.Context, svc *store.Service) (*fact.Ontology, ontologySource, error) {
	read := r.ReadBranch()
	paths := fact.OntologyPathsNewestFirst()
	// The first rung PRESENT is the ontology. A symlink there is never
	// followed nor read as "no ontology": the error names the path (user
	// ruling 2026-10-08), and Identify refuses the repo on it.
	commit, srcPath, blob, berr := svc.OntologyBlobAt(ctx, read)
	src := ontologySource{Commit: commit, Path: srcPath, Blob: blob}
	if errors.Is(berr, fact.ErrSymlinkNotFollowed) {
		log.Error().Err(berr).Str("repo", r.Name()).Str("branch", read).
			Msg("ontology is a symlink: this repository is refused until the ontology is a file")
		return nil, src, fmt.Errorf("ontology on %s: %w", read, berr)
	}
	// A READ failure — the tip, its tree or the blob could not be read — is
	// not an answer about the ontology: it is "unknown, ask again". It is
	// returned as ontologyReadError with NO source, never as "no ontology
	// at …" naming the tip's file. Naming that file would let the reload
	// record a transient storage error as the verdict on a usable consensus
	// file and skip that blob for good (identity.reloadSrc, or src on a repo
	// with nothing in force), keeping consensus content out until a restart.
	if berr != nil {
		return nil, ontologySource{}, ontologyReadError{branch: read, err: berr}
	}
	content := ""
	if srcPath != "" {
		if h := r.ontologyReadHook.Load(); h != nil {
			if herr := (*h)(); herr != nil {
				return nil, ontologySource{}, ontologyReadError{branch: read, err: herr}
			}
		}
		// The bytes at the very commit whose blob was named, not at the
		// branch's tip again: the tip may have moved in between.
		p, data, rerr := svc.OntologyFileAtCommit(ctx, commit)
		if rerr != nil {
			return nil, ontologySource{}, ontologyReadError{branch: read, err: rerr}
		}
		srcPath, content = p, string(data)
	}
	if srcPath != "" && content == "" {
		// The first rung present is empty: that IS the ontology, and a later
		// rung does not stand in for it. Name the file, not every rung.
		log.Error().Str("repo", r.Name()).Str("branch", read).Str("path", srcPath).
			Msg("ontology file is empty: this repository will not accept writes until it holds an ontology")
		return nil, src, fmt.Errorf("ontology at %s on %s is empty", srcPath, read)
	}
	if content == "" {
		log.Error().Str("repo", r.Name()).Str("branch", read).
			Msgf("no ontology at %s: this repository is not a knowledge base and will not accept writes", strings.Join(paths, ", "))
		return nil, src, fmt.Errorf("no ontology at %s on %s", strings.Join(paths, ", "), read)
	}
	if srcPath != OntologyPath {
		log.Info().Str("repo", r.Name()).
			Msgf("ontology loaded from the legacy path %s; rename it to %s", srcPath, OntologyPath)
	}
	ont, err := fact.ParseOntology([]byte(content))
	if err != nil {
		log.Error().Err(err).Str("repo", r.Name()).Str("path", srcPath).
			Msg("ontology does not parse: this repository will not accept writes until it does")
		return nil, src, fmt.Errorf("ontology at %s does not parse: %w", srcPath, err)
	}
	return ont, src, nil
}

// refreshPreset is the boot-time preset refresh: a stored ontology whose id
// is an embedded preset's, and which is a strict subset of it with identical
// root attributes (refreshDivergence), is overwritten by the preset. It
// returns the ontology in force: the preset after a successful write, ont
// otherwise.
func (r *RepoInstance) refreshPreset(ctx context.Context, svc *store.Service, read, srcPath string, ont *fact.Ontology) *fact.Ontology {
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
	return ont
}

// refreshFleetOntology is the fleet repository's ontology refresh: the fleet
// ontology is knomit's own and changes only with a release, so on the MASTER
// (this fleet's root key is on this machine, isFleetMaster) a stored ontology
// that differs from the built-in one is replaced by it wholesale — not the
// presets' subset rule. Every other instance leaves it alone: a member never
// writes a dot path on the fleet repository (the master's push gate refuses
// one, and the member's branch would be stuck behind the refused commit). A
// mount that is not admitted (admitFleetOntology) writes nothing either. It
// returns the ontology in force.
func (r *RepoInstance) refreshFleetOntology(ctx context.Context, svc *store.Service, read, srcPath string, ont *fact.Ontology) *fact.Ontology {
	if r.subscribed || !r.fleetDoor || !r.env.m.isFleetMaster(svc) || r.env.m.fleetMasterPause(r) != "" {
		return ont
	}
	builtIn := fact.FleetOntology()
	storedY, sErr := ont.Serialize()
	builtInY, bErr := builtIn.Serialize()
	if sErr != nil || bErr != nil || bytes.Equal(storedY, builtInY) {
		return ont
	}
	log.Info().Str("repo", r.Name()).Str("path", srcPath).
		Msg("fleet ontology refresh: this instance is the master; replacing the stored fleet ontology with the built-in one")
	if _, err := svc.Facts().WriteFact(ctx, read, srcPath, string(builtInY),
		"ontology: refresh to the built-in fleet ontology", "updated"); err != nil {
		log.Warn().Err(err).Str("repo", r.Name()).Msg("fleet ontology refresh: write failed, keeping stored")
		return ont
	}
	return builtIn
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
			if head, err := svc.HeadCommit(ctx, r.agentBranch); err == nil {
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
			if _, err := svc.HeadCommit(ctx, name); err != nil {
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
func healIndexBranches(ctx context.Context, im *index.Index, repo string, branches []healBranch, progress model.RebuildProgress) (ok bool) {
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
	// ref_edges (migration 000035) mirrors the DERIVED_FROM edges in the
	// author's refs order; a store indexed before it existed is filled once
	// (meta sentinel), from the edges and the blobs in the object table. It
	// runs AFTER the branches above, so a store below GraphSchemaVersion 8,
	// whose v8 rebuild resets the edges and rewrites them with their rows, is
	// not backfilled from the old edges first. The heal is the index job, run
	// in the background while the repo already serves (the index status reads
	// "indexing"): until the sentinel is set, outgoing reads (OutgoingAtCommit,
	// RefEdgesOfBlobs) derive the rows from the edges and the blobs instead, so
	// they answer the same before and after. Not fatal: Verify reports the
	// drift, and the next heal retries.
	if len(branches) > 0 {
		if n, err := im.BackfillRefEdges(ctx, branches[0].name); err != nil {
			log.Warn().Err(err).Str("repo", repo).Msg("ref_edges backfill failed; the next heal retries")
		} else if n > 0 {
			log.Info().Str("repo", repo).Int("blobs", n).Msg("ref_edges backfilled")
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
	if run := r.runTriggers; run != nil {
		life.Go("triggers", run)
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
			r.triggers.Kick, r.syncWake, mode(), once, r.fleetHostRound, r.fleetMainAdvanced)
	}
	if hasOrigin {
		if remote != nil {
			r.syncOrigin = remote.URL
		}
		authFn := makeRemoteAuthFn(cfg.Remote, env.keyPath, env.secrets)
		name = "reconcile"
		run = func(ctx context.Context) {
			runReconcileLoop(ctx, svc, r.pub, r.remoteEdgeSink(), r.Name(), r.agentBranch, authFn, cfg.LocalOriginRoot, cfg.ReadOnly,
				env.onPush, r.triggers.Kick, r.syncWake, r.breakers, mode(), r.peers, r.afterFetch, once)
		}
	}
	// The local-upstream repair runs on the life's goroutine, never on the
	// driver: EnsureLocalUpstream takes lockBranch(upstream), which the Index
	// job holds for a whole Rebuild of that branch, and a driver parked there
	// stops the create from narrating the index and every queued event from
	// being handled (kb/invariants/repos/create-job/index-before-sync). It
	// still runs before the loop's first round, which is the ordering
	// TestOpen_BootstrapsAMissingLocalUpstreamFromOrigin (Synchronous, inline)
	// and its _Background variant (the life's goroutine) pin.
	work := func(ctx context.Context) {
		r.ensureLocalUpstream(ctx, svc)
		run(ctx)
	}
	if once {
		defer release()
		work(ctx)
		return nil
	}
	life.Go(name, func(ctx context.Context) {
		defer release()
		work(ctx)
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
	created, err := svc.Remote().EnsureLocalUpstream(ctx, r.upstreamMain)
	if err != nil {
		log.Warn().Err(err).Str("repo", r.Name()).
			Msg("ensureLocalUpstream: bootstrap failed; the reconcile loop will repair it")
		return
	}
	if created {
		// Created from origin OR completed at an existing ref; the store's
		// own log line says which.
		log.Info().Str("repo", r.Name()).Str("upstream", r.upstreamMain).
			Msg("ensureLocalUpstream: local upstream created or completed")
	}
}

// idOf is o's id, "" for a nil ontology (a repo with none in force).
func idOf(o *fact.Ontology) string {
	if o == nil {
		return ""
	}
	return o.ID
}
