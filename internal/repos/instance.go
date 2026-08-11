package repos

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/store"
)

var (
	// ErrRepoClosed is returned by Acquire/WithRead after the instance began
	// teardown (Archive, Manager.Close). The store is gone for good; callers
	// should treat the repo as unregistered.
	ErrRepoClosed = errors.New("repo is closed")
	// ErrStoreUnavailable is returned by Acquire/WithRead while no store is
	// attached — the on-disk database is being replaced (SwapStore) or a test
	// instance carries no service. Transient; callers may retry.
	ErrStoreUnavailable = errors.New("repo store is unavailable")
)

// storeHandle pairs one generation of the store service with a refcount of
// in-flight users. Teardown (closeFn) and replacement (SwapStore) detach the
// handle under the write lock — making it unreachable for new Acquires — then
// wait for wg to drain before closing svc. This is the single mechanism that
// makes "snapshot the service, use it after releasing the lock" safe: a caller
// that went through Acquire keeps its generation alive until it releases.
//
// WaitGroup discipline: every wg.Add(1) happens under ri.mu.RLock while the
// handle is still attached. Detaching requires ri.mu.Lock, which drains all
// RLock holders first — so by the time a detacher calls wg.Wait, no new Add
// can race it (the counter only decreases).
type storeHandle struct {
	svc *store.Service
	wg  sync.WaitGroup
}

// newStoreHandle wraps svc in a handle; nil svc yields a nil handle so
// Acquire reports ErrStoreUnavailable rather than handing out a nil service.
func newStoreHandle(svc *store.Service) *storeHandle {
	if svc == nil {
		return nil
	}
	return &storeHandle{svc: svc}
}

// Index readiness states for a RepoInstance. The store is live for reads in
// every state; "indexing" means a background (re)build is populating the
// derived index, so reads may return partial results until it reaches "ready".
const (
	indexReady int32 = iota
	indexIndexing
	indexFailed
)

// RepoInstance holds all runtime state for a single repository.
type RepoInstance struct {
	mu          sync.RWMutex
	name        string
	dbPath      string
	agentBranch string
	// uid is the registry identity: stable for the repo's whole life, minted at
	// create, and unchanged by rename or by a store swap. Distinct from id (the
	// root-commit hash), which is MUTABLE — a disjoint-history connect replaces
	// the store and with it that value.
	uid string
	// id is the repo's stable identity: the root commit hash (lenses RFC
	// decision 11). Resolved lazily; "" when unresolvable.
	// idMu guards id. ID() caches only successful resolution so a transient
	// failure (e.g. during a store swap) is retried on the next call.
	idMu                sync.Mutex
	id                  string
	ontology            *fact.Ontology
	embedder            store.BatchEmbedder
	ontologyRoot        string
	methodologyMinScore float64
	clusterResolution   float64
	clusterMinCommunity int
	// Discovery dial + verification thresholds (emergent-fact discovery).
	// See [config.DiscoveryConfig] for vocabulary.
	discoveryEffortDefault        string
	discoveryConfidenceThreshold  float64
	discoveryBlastRadiusThreshold int
	discoveryBridge               string
	// Bridge quality knobs (Task 12). See config.DiscoveryConfig for vocabulary.
	discoveryCohFloor     float64
	discoveryMaxMembers   int
	discoveryQualityFloor float64
	discoveryWCoh         float64
	discoveryWGap         float64
	discoveryWSpec        float64
	onCommit              func(string, string) // re-applied to new svc after SwapStore
	// handle is the current store generation; nil while no store is attached
	// (mid-SwapStore, or a test instance without a service). closed marks the
	// beginning of permanent teardown. Both are guarded by mu; all store access
	// goes through Acquire so close/swap can drain in-flight users before
	// closing the underlying service.
	handle     *storeHandle
	closed     bool
	hub        *TaskHub
	syncCancel context.CancelFunc
	syncWg     *sync.WaitGroup
	// indexCancel/indexWg own the background index-heal lifecycle, SEPARATE from
	// syncCancel/syncWg (the reconcile loop). Only real teardown cancels/waits
	// these; startSync's loop-restart must not touch them. See repoBuilder.build.
	indexCancel context.CancelFunc
	indexWg     *sync.WaitGroup
	startSync   func(url string) error
	closeFn     func()

	indexState atomic.Int32 // indexReady | indexIndexing | indexFailed
	indexDone  atomic.Int64
	indexTotal atomic.Int64
}

// IndexStatus reports the repo's background-index readiness for the API/UI.
// state is "ready" | "indexing" | "error"; done/total are populated while
// indexing (0/0 when unknown).
func (ri *RepoInstance) IndexStatus() (state string, done, total int) {
	switch ri.indexState.Load() {
	case indexIndexing:
		state = "indexing"
	case indexFailed:
		state = "error"
	default:
		state = "ready"
	}
	return state, int(ri.indexDone.Load()), int(ri.indexTotal.Load())
}

// markIndexing flips the repo into the indexing state (progress reset).
func (ri *RepoInstance) markIndexing() {
	ri.indexDone.Store(0)
	ri.indexTotal.Store(0)
	ri.indexState.Store(indexIndexing)
}

func (ri *RepoInstance) setIndexProgress(done, total int) {
	ri.indexDone.Store(int64(done))
	ri.indexTotal.Store(int64(total))
}

func (ri *RepoInstance) markIndexReady()  { ri.indexState.Store(indexReady) }
func (ri *RepoInstance) markIndexFailed() { ri.indexState.Store(indexFailed) }

// Acquire returns the current store service together with a release func the
// caller MUST invoke when it is done with the service (idempotent). Between
// Acquire and release the service is guaranteed to stay open: teardown
// (Archive/Close) and replacement (SwapStore) wait for every outstanding
// release before closing the underlying handle. This — not lock scope — is
// what makes it safe to keep using the service after Acquire returns, so
// long-running operations (MCP tool calls, SSE flows, background rebuilds,
// synthesis sessions) hold one Acquire for their full duration instead of
// snapshotting the pointer out of a lock.
//
// Returns ErrRepoClosed once teardown has begun, or ErrStoreUnavailable while
// no store is attached (mid-swap / test instance without a service).
//
// Do NOT hold an Acquire across a call that replaces or closes this repo's
// store (Manager.SwapStore, Manager.Archive) — the drain would wait on the
// caller's own reference and deadlock.
func (ri *RepoInstance) Acquire() (*store.Service, func(), error) {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	if ri.closed {
		return nil, nil, ErrRepoClosed
	}
	h := ri.handle
	if h == nil {
		return nil, nil, ErrStoreUnavailable
	}
	h.wg.Add(1)
	var once sync.Once
	return h.svc, func() { once.Do(h.wg.Done) }, nil
}

// Pin holds the current store generation open without exposing the service —
// for callers that delegate store access to a component taking the whole
// RepoInstance (e.g. a synthesize.Reviewer) but must guarantee the store is
// not closed out from under it. Release with the returned func (idempotent).
func (ri *RepoInstance) Pin() (func(), error) {
	_, release, err := ri.Acquire()
	return release, err
}

// WithRead calls fn with the store service, holding a store reference (via
// Acquire) for the duration of fn. This is the scoped convenience form; fn is
// NOT called when no service is available — the error reports why
// (ErrRepoClosed / ErrStoreUnavailable). Callers that need the service beyond
// fn's scope must use Acquire directly instead of copying the pointer out.
func (ri *RepoInstance) WithRead(fn func(*store.Service)) error {
	svc, release, err := ri.Acquire()
	if err != nil {
		return err
	}
	defer release()
	fn(svc)
	return nil
}

// detachStore atomically takes the current handle out of the instance so no
// new Acquire can reach it, optionally marking the instance permanently
// closed. The caller owns the returned handle and must drain it
// (h.wg.Wait()) before closing h.svc. Returns nil when no store was attached.
func (ri *RepoInstance) detachStore(markClosed bool) *storeHandle {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	if markClosed {
		ri.closed = true
	}
	h := ri.handle
	ri.handle = nil
	return h
}

// attachStore installs a new store generation. No-op (returns false) when the
// instance is already closed — the caller must close svc itself in that case.
func (ri *RepoInstance) attachStore(svc *store.Service) bool {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	if ri.closed {
		return false
	}
	ri.handle = newStoreHandle(svc)
	return true
}

// Name returns the repository name.
func (ri *RepoInstance) Name() string { return ri.name }

// UID returns the repo's registry identity — the control.db primary key and
// the .db filename stem. Never empty for a registered repo.
func (ri *RepoInstance) UID() string { return ri.uid }

// AgentBranch returns the agent branch this repo writes to.
func (ri *RepoInstance) AgentBranch() string { return ri.agentBranch }

// ID returns the repo's stable identity — the root commit hash, identical in
// every clone and unaffected by renames (lenses RFC decision 11). Caches only
// successful resolution; failures are retried on the next call and return ""
// meanwhile. Returns "" when the store is unavailable (bare test instances);
// callers treat an empty ID as "identity unknown".
//
// Lock ordering: idMu is taken BEFORE WithRead's ri.mu.RLock. ID() is the only
// user of idMu, so no caller path takes them in the opposite order.
func (ri *RepoInstance) ID() string {
	ri.idMu.Lock()
	defer ri.idMu.Unlock()
	if ri.id != "" {
		return ri.id
	}
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		root, err := svc.RootCommit(context.Background(), ri.agentBranch)
		if err != nil {
			log.Warn().Err(err).Str("repo", ri.name).Msg("repo id: root commit unresolved")
			return
		}
		ri.id = root
	})
	return ri.id
}

// ShortID returns the 12-hex wire form of the repo's stable id (the root
// commit hash, RFC §6.2 decision 11) — the exact identifier that appears in
// kb://<id>/… result paths, refs, and the knomit_repos mount table. Returns
// "" when the id is unresolvable, and the full id unchanged if it is somehow
// shorter than 12 chars.
func (ri *RepoInstance) ShortID() string {
	id := ri.ID()
	if len(id) < 12 {
		return id
	}
	return id[:12]
}

// WritableBranch reports whether facts may be authored on branch through
// this repo. This is the branch write-eligibility classification of the
// lenses RFC (decision 19): in v1 only the repo's own agent branch is
// writable. The consensus branch (authoring there bypasses reconciliation
// and the watermark model) and other machines' agent/* branches (authoring
// there corrupts their watermarks) are never writable. Future experiment
// branches widen this classification — extend HERE, not at call sites.
func (ri *RepoInstance) WritableBranch(branch string) bool {
	return branch != "" && branch == ri.agentBranch
}

// Ontology returns the ontology loaded from this repo's git store at open time.
func (ri *RepoInstance) Ontology() *fact.Ontology { return ri.ontology }

// Embedder returns the batch embedder for this repo, or nil if unavailable.
func (ri *RepoInstance) Embedder() store.BatchEmbedder { return ri.embedder }

// OntologyRoot returns the root path under which facts live for this repo (e.g. "kb").
func (ri *RepoInstance) OntologyRoot() string { return ri.ontologyRoot }

// MethodologyMinScore returns the minimum composite score below which
// methodology candidates are dropped from prompt injection.
func (ri *RepoInstance) MethodologyMinScore() float64 { return ri.methodologyMinScore }

// ClusterResolution returns the Louvain γ the review/cluster read path must use.
// It mirrors the value the background checker warms (config [cluster_cache]
// resolution, default 2.0) so both hit the same cluster_cache key.
func (ri *RepoInstance) ClusterResolution() float64 { return ri.clusterResolution }

// ClusterMinCommunitySize returns the min community size paired with the resolution.
func (ri *RepoInstance) ClusterMinCommunitySize() int { return ri.clusterMinCommunity }

// DiscoveryEffortDefault returns the default effort dial used when an MCP
// caller omits 'effort'. Empty string falls back to "normal".
func (ri *RepoInstance) DiscoveryEffortDefault() string {
	if ri.discoveryEffortDefault == "" {
		return "normal"
	}
	return ri.discoveryEffortDefault
}

// DiscoveryConfidenceThreshold is the minimum confidence a discovered
// proposal must carry to land. An explicit 0 disables the gate (matching the
// config contract); negative values likewise disable it. The 0.5 default is
// supplied by config.Defaults() and NOT re-defaulted here, because doing so
// would make 0 (the only value an operator can set to disable the gate)
// indistinguishable from "unset" and silently re-enable a gate the operator
// turned off. Constructors that bypass config.Load() (e.g.
// NewTestInstanceWithDeps) seed this field with the same default.
func (ri *RepoInstance) DiscoveryConfidenceThreshold() float64 {
	return ri.discoveryConfidenceThreshold
}

// DiscoveryBlastRadiusThreshold is the minimum BlastRadius required for a
// backward (keystone) discovery to land. An explicit 0 disables the gate
// (matching the documented config contract); negative values likewise
// disable it. The "unconfigured" default of 1 is supplied by
// config.Defaults() — NOT re-defaulted here, because doing so would make 0
// (the only value an operator can set to disable the gate) indistinguishable
// from "unset" and silently re-enable a gate the operator turned off.
// Constructors that bypass config.Load() (e.g. NewTestInstanceWithDeps) seed
// this field with the same default.
func (ri *RepoInstance) DiscoveryBlastRadiusThreshold() int {
	return ri.discoveryBlastRadiusThreshold
}

// DiscoveryBridge returns the structural-token policy: "domain", "entity",
// "both" (default), "keyword" (YAKE body keywords only), or "all" (domain +
// entity + keyword).
func (ri *RepoInstance) DiscoveryBridge() string {
	if ri.discoveryBridge == "" {
		return "both"
	}
	return ri.discoveryBridge
}

// DiscoveryCohFloor returns the minimum intra-cluster cohesion a bridge seed
// set must have to pass the quality gate. Default 0.5 (from config.Defaults).
// Like the other discovery/cluster accessors, the field is set once at
// construction and never mutated, so no lock is taken.
func (ri *RepoInstance) DiscoveryCohFloor() float64 { return ri.discoveryCohFloor }

// DiscoveryMaxMembers returns the maximum number of members in a bridge seed
// set that will be scored; larger sets are gated out. Default 5 (from config.Defaults).
func (ri *RepoInstance) DiscoveryMaxMembers() int { return ri.discoveryMaxMembers }

// DiscoveryQualityFloor returns the minimum weighted quality score Q a bridge
// seed set must achieve to be kept. 0.0 disables the floor. Default 0.0.
func (ri *RepoInstance) DiscoveryQualityFloor() float64 { return ri.discoveryQualityFloor }

// DiscoveryWCoh returns the weight applied to the cohesion component in Q.
// Default 1.0 (from config.Defaults).
func (ri *RepoInstance) DiscoveryWCoh() float64 { return ri.discoveryWCoh }

// DiscoveryWGap returns the weight applied to the derivation-gap component in
// Q. Default 1.0 (from config.Defaults).
func (ri *RepoInstance) DiscoveryWGap() float64 { return ri.discoveryWGap }

// DiscoveryWSpec returns the weight applied to the specificity component in Q.
// Default 1.0 (from config.Defaults).
func (ri *RepoInstance) DiscoveryWSpec() float64 { return ri.discoveryWSpec }

// TaskHub returns the hub for broadcasting task status events.
func (ri *RepoInstance) TaskHub() *TaskHub { return ri.hub }

// ActivateSync starts sync and push loops for the given remote URL.
// Returns an error if the remote cannot be configured.
func (ri *RepoInstance) ActivateSync(url string) error {
	if ri.startSync == nil {
		return nil
	}
	return ri.startSync(url)
}

// DeactivateSync cancels the running sync/push loops so the repo stops talking
// to a remote (used when the remote is disconnected). Safe to call when no
// loop is running; a later ActivateSync starts a fresh loop.
func (ri *RepoInstance) DeactivateSync() {
	ri.mu.Lock()
	cancel := ri.syncCancel
	ri.syncCancel = func() {}
	ri.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Close stops the observer and closes the store.
func (ri *RepoInstance) Close() {
	if ri.closeFn != nil {
		ri.closeFn()
	}
}

// shutdown performs the full teardown sequence for a single instance:
// cancel the background index heal and the sync loop, wait for both to wind
// down (heal first), shut the task hub, then release store/observer resources.
// Used by Manager.Close (bulk) and the lifecycle Archive path (single).
func (ri *RepoInstance) shutdown() {
	ri.mu.RLock()
	cancel := ri.syncCancel
	indexCancel := ri.indexCancel
	ri.mu.RUnlock()
	if indexCancel != nil {
		indexCancel()
	}
	if cancel != nil {
		cancel()
	}
	// Wait the index heal BEFORE the loop: the heal's activate() does
	// syncWg.Add(1) for the reconcile loop, so indexWg.Wait() must complete
	// before syncWg.Wait() to avoid the loop's Add racing past a syncWg that
	// transiently read zero. Both must finish before closeFn closes the store.
	if ri.indexWg != nil {
		ri.indexWg.Wait()
	}
	if ri.syncWg != nil {
		ri.syncWg.Wait()
	}
	if ri.hub != nil {
		ri.hub.Shutdown()
	}
	if ri.closeFn != nil {
		ri.closeFn()
	}
}

// Verify runs the integrity check against the current store while holding a
// store reference (Acquire), so a concurrent SwapStore/Archive drains this
// call before closing the service instead of closing it mid-check. Delegates
// to store.Service.Verify and stamps the report with this repo's name.
func (ri *RepoInstance) Verify(ctx context.Context, opts store.VerifyOpts) (store.IntegrityReport, error) {
	svc, release, err := ri.Acquire()
	if err != nil {
		return store.IntegrityReport{Repo: ri.name}, err
	}
	defer release()
	report, err := svc.Verify(ctx, opts)
	report.Repo = ri.name
	return report, err
}

// NewTestInstance creates a minimal RepoInstance for use in tests that
// exercise Manager operations (Set, Get, Replace, ForEach, Names, context).
// Production code must use Manager.openOne instead.
func NewTestInstance(name string) *RepoInstance {
	return &RepoInstance{
		name:        name,
		syncCancel:  func() {},
		syncWg:      &sync.WaitGroup{},
		indexCancel: func() {},
		indexWg:     &sync.WaitGroup{},
	}
}

// TestInstanceConfig holds optional fields for NewTestInstanceWithDeps.
// Zero values are safe — nil fields are treated as "not configured".
type TestInstanceConfig struct {
	Name string
	// UID is the instance's control.db identity. Leave empty for a bare
	// instance that no registry row backs; set it (to the uid of a row the
	// test inserted) when the code under test writes through Manager.Repos()
	// or Manager.Origins(), both of which key on it.
	UID                 string
	AgentBranch         string
	Svc                 *store.Service
	Ontology            *fact.Ontology
	Hub                 *TaskHub
	Embedder            store.BatchEmbedder
	OntologyRoot        string
	MethodologyMinScore float64
	StartSync           func(url string) error
}

// NewTestInstanceWithDeps creates a RepoInstance pre-populated with the given
// dependencies. Intended for handler/integration tests in sibling packages.
// Production code must use Manager.openOne instead.
func NewTestInstanceWithDeps(cfg TestInstanceConfig) *RepoInstance {
	return &RepoInstance{
		name:                cfg.Name,
		uid:                 cfg.UID,
		agentBranch:         cfg.AgentBranch,
		handle:              newStoreHandle(cfg.Svc),
		ontology:            cfg.Ontology,
		embedder:            cfg.Embedder,
		ontologyRoot:        cfg.OntologyRoot,
		methodologyMinScore: cfg.MethodologyMinScore,
		clusterResolution:   defaultClusterResolution,
		clusterMinCommunity: defaultClusterMinCommunitySize,
		// Mirror config.Defaults(): neither blast-radius nor confidence
		// accessors re-default 0 (explicit 0 means "gate disabled"), so test
		// instances must carry the production defaults explicitly.
		discoveryConfidenceThreshold:  0.5,
		discoveryBlastRadiusThreshold: 1,
		hub:                           cfg.Hub,
		startSync:                     cfg.StartSync,
		syncCancel:                    func() {},
		syncWg:                        &sync.WaitGroup{},
		indexCancel:                   func() {},
		indexWg:                       &sync.WaitGroup{},
	}
}
