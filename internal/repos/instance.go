package repos

import (
	"context"
	"sync"
	"sync/atomic"

	"knomit/internal/fact"
	"knomit/internal/store"
)

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
	mu                  sync.RWMutex
	name                string
	dbPath              string
	agentBranch         string
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
	onCommit                      func(string, string) // re-applied to new svc after SwapStore
	svc                           *store.Service
	hub                           *TaskHub
	syncCancel                    context.CancelFunc
	syncWg                        *sync.WaitGroup
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

// WithRead calls fn with the store service under a read lock.
// This is the only way external code may access svc.
func (ri *RepoInstance) WithRead(fn func(*store.Service)) {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	fn(ri.svc)
}

// withWrite calls fn under a write lock. Only used within the repos package
// (SwapStore, StartSync closure).
func (ri *RepoInstance) withWrite(fn func()) {
	ri.mu.Lock()
	defer ri.mu.Unlock()
	fn()
}

// Name returns the repository name.
func (ri *RepoInstance) Name() string { return ri.name }

// AgentBranch returns the agent branch this repo writes to.
func (ri *RepoInstance) AgentBranch() string { return ri.agentBranch }

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
// or "both" (default).
func (ri *RepoInstance) DiscoveryBridge() string {
	if ri.discoveryBridge == "" {
		return "both"
	}
	return ri.discoveryBridge
}

// DiscoveryCohFloor returns the minimum intra-cluster cohesion a bridge seed
// set must have to pass the quality gate. Default 0.5 (from config.Defaults).
func (ri *RepoInstance) DiscoveryCohFloor() float64 {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	return ri.discoveryCohFloor
}

// DiscoveryMaxMembers returns the maximum number of members in a bridge seed
// set that will be scored; larger sets are gated out. Default 5 (from config.Defaults).
func (ri *RepoInstance) DiscoveryMaxMembers() int {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	return ri.discoveryMaxMembers
}

// DiscoveryQualityFloor returns the minimum weighted quality score Q a bridge
// seed set must achieve to be kept. 0.0 disables the floor. Default 0.0.
func (ri *RepoInstance) DiscoveryQualityFloor() float64 {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	return ri.discoveryQualityFloor
}

// DiscoveryWCoh returns the weight applied to the cohesion component in Q.
// Default 1.0 (from config.Defaults).
func (ri *RepoInstance) DiscoveryWCoh() float64 {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	return ri.discoveryWCoh
}

// DiscoveryWGap returns the weight applied to the derivation-gap component in
// Q. Default 1.0 (from config.Defaults).
func (ri *RepoInstance) DiscoveryWGap() float64 {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	return ri.discoveryWGap
}

// DiscoveryWSpec returns the weight applied to the specificity component in Q.
// Default 1.0 (from config.Defaults).
func (ri *RepoInstance) DiscoveryWSpec() float64 {
	ri.mu.RLock()
	defer ri.mu.RUnlock()
	return ri.discoveryWSpec
}

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

// Verify runs the integrity check against the current store under the read
// lock so that a concurrent SwapStore cannot move the rug. Delegates to
// store.Service.Verify and stamps the report with this repo's name.
func (ri *RepoInstance) Verify(ctx context.Context, opts store.VerifyOpts) (store.IntegrityReport, error) {
	ri.mu.RLock()
	svc := ri.svc
	ri.mu.RUnlock()
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
	Name                string
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
		agentBranch:         cfg.AgentBranch,
		svc:                 cfg.Svc,
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
