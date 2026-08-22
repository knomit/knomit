package store

import (
	"context"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"

	"knomit/internal/embeddings/params"
)

// FactIndex is the interface for fact storage. Implemented by *factIndex.
type FactIndex interface {
	ReadFact(ctx context.Context, branch, path string, opts *ReadFactOpts) (ReadFactResult, error)
	WriteFact(ctx context.Context, branch, path, content, message, operation string) (WriteFactResult, error)
	// WriteRootFile writes a root-level non-fact file (e.g. README.md)
	// PRESERVING CASE. WriteFact lowercases, which is correct for fact paths
	// and wrong for a filename an external reader looks for by exact name.
	WriteRootFile(ctx context.Context, branch, path, content, message, operation string) (WriteFactResult, error)
	// BatchWriteFacts applies writes and deletions as ONE commit. Pass deletes
	// to retract facts atomically with the writes that supersede them — a
	// separate DeleteFact call would be a second commit that can land without
	// its partner (see the learn-subsume path in internal/mcp/learn.go).
	BatchWriteFacts(ctx context.Context, branch string, files map[string]string, deletes []string, message, operation string) (commitHash string, blobHashes map[string]string, err error)
	DeleteFact(ctx context.Context, branch, path, message string) (string, error)
	FactExists(ctx context.Context, branch, path string) (bool, error)
	ListDir(ctx context.Context, branch, path string) ([]DirEntry, error)
	ListAll(ctx context.Context, branch string) ([]string, error)
	ListAllWithHash(ctx context.Context, branch string) (paths []string, blobHashes []string, err error)
	DiffFiles(ctx context.Context, branch, fromCommit string) (added, modified, deleted []string, err error)
}

//go:generate go run go.uber.org/mock/mockgen -destination=../synthesize/mock_search_index_test.go -package=synthesize knomit/internal/store SearchIndex

// The fact search index is decomposed into four cohesive query sub-services —
// FactQuery, GraphStore, HistoryQuery, MethodologyMatcher — each a narrow
// interface a consumer can depend on in isolation (mcp/web never touch
// MethodologyMatcher; synthesize never touches HistoryQuery). SearchIndex below
// composes all four. Each interface has its own {rh *repoHandler} implementation
// in query_services.go — factQuery, graphStore, historyQuery, methodologyMatcher —
// reaching shared state only UP through repoHandler, never sideways through a
// sibling. See .claude/plans/2026-07-21-p1.3-store-decomposition-design.md.

// FactQuery is the interface for fact read/search/existence queries.
type FactQuery interface {
	Search(ctx context.Context, branch string, q SearchOptions) ([]SearchResult, error)
	GetByPath(ctx context.Context, branch, path string) (*FactWithBody, error)
	LastCommitForPath(ctx context.Context, branch, path string) (string, bool)
	Stats(ctx context.Context, branch, pathPrefix, axis string) (StatsResult, error)
	Completions(ctx context.Context, branch, category, prefix string, limit int) ([]string, error)
	RecentFacts(ctx context.Context, branch string, opts SearchOptions) ([]RecentFactEntry, int, error)
	FactsIter(ctx context.Context, branch string) (*FactsIter, error)
	// FactExistsAt reports whether `path` has any valid (added/modified)
	// version in the sparse history reachable from `commit` on `branch`,
	// walking past retractions. Pass commit == "" for a HEAD-anchored check.
	// Used by ref-kind classification: a ref is `fact` (vs `broken`) when
	// the target has any historical version visible at the source's anchor.
	FactExistsAt(ctx context.Context, branch, path, commit string) (bool, error)
	// FactLiveAtCommit reports whether `path` is live (present, not retracted)
	// as of `commit` — the delete-RESPECTING sibling of FactExistsAt. It
	// inspects the most recent commit_log event in the first-parent ancestry
	// and is live only if that event is added/modified. Used as the existence
	// gate for the commit-anchored /incoming and /outgoing sub-resources so a
	// retracted fact 404s in lockstep with the (no-fallback) fact read.
	FactLiveAtCommit(ctx context.Context, branch, path, commit string) (bool, error)
}

// GraphStore is the interface for DERIVED_FROM / SIMILAR_TO graph queries.
type GraphStore interface {
	ExplainFact(ctx context.Context, branch, path string) (ExplainResult, error)
	IncomingAtCommit(ctx context.Context, branch, path, commitHash string) ([]RefSummary, error)
	OutgoingAtCommit(ctx context.Context, branch, path, commitHash string) ([]RefSummary, error)
	// SubgraphEdges returns the undirected SIMILAR_TO adjacency among the given
	// fact paths (one pair per edge whose both endpoints are non-deleted Fact
	// nodes in the set). Scoped clustering runs Louvain over this bounded
	// subgraph in-process instead of clustering the whole repo graph.
	SubgraphEdges(ctx context.Context, paths []string) ([][2]string, error)
	// BlastRadius counts the facts that are live on `branch` at HEAD and
	// transitively derive (DERIVED_FROM, any depth) from any version of
	// `path`. The keystone-impact metric: how much of the live corpus would
	// be invalidated if `path` were false. Returns 0 for leaf facts.
	BlastRadius(ctx context.Context, branch, path string) (int, error)
	// TokenDF returns the count of facts live on branch that carry the given
	// domain/entity tag (kind: "domain"|"entity").
	TokenDF(ctx context.Context, branch, token, kind string) (int, error)
	// SimilarityAdjacency returns the member-restricted SIMILAR_TO graph for
	// the given fact paths. Only edges where both endpoints are in paths are
	// kept. Liveness is enforced via NOT n.deleted = true. An empty or
	// single-element paths slice returns an empty graph with Density == 0.
	SimilarityAdjacency(ctx context.Context, paths []string) (SimilarityGraph, error)
	// ReverseDependentPaths returns all paths transitively DERIVED_FROM any
	// version of `path` (all historical versions seeded), NOT
	// liveness-filtered. Membership among live members is the consumer's job.
	ReverseDependentPaths(ctx context.Context, path string) (map[string]struct{}, error)
}

// HistoryQuery is the interface for commit-log / revision history queries.
type HistoryQuery interface {
	Log(ctx context.Context, branch, path string) ([]LogEntry, error)
	LogPaginated(ctx context.Context, branch, path string, limit int, after, from, before string) ([]LogEntryWithTags, string, string, error)
	// RevisionsBefore returns up to `limit` revisions of `path` in the
	// first-parent ancestry of `anchorCommit`, newest → oldest. Used by
	// knomit_explain to build the root fact's bounded evolution history.
	RevisionsBefore(ctx context.Context, branch, path, anchorCommit string, limit int) ([]RevisionMeta, error)
	CommitDetail(ctx context.Context, commitHash, pathPrefix string) (*CommitDetailResult, error)
	Activity(ctx context.Context, branch, path string) (ActivityResult, error)
}

// MethodologyMatcher is the interface for methodology-fact matching.
type MethodologyMatcher interface {
	RelevantMethodologyForFact(ctx context.Context, branch, factPath string, sourceDomains, sourceEntities []string, k int, minScore float64) ([]MethodologyMatch, error)
}

// SearchIndex composes the four query sub-services. Implemented by *searchFacade
// (see query_services.go); *searchIndex implements IndexManager, not this.
// Prefer depending on the narrowest sub-interface a consumer actually needs;
// this composite exists for callers that genuinely span clusters and for the
// transitional mock.
type SearchIndex interface {
	FactQuery
	GraphStore
	HistoryQuery
	MethodologyMatcher
}

// IndexManager is the interface for search index lifecycle operations. Implemented by *searchIndex.
type IndexManager interface {
	// Sync is lock-FREE: the caller MUST already hold lockBranch(branch). Its
	// only such caller is notifyCommit (the inline write path). Out-of-band
	// callers (commit observer, startup heal) MUST use SyncLocked instead.
	Sync(ctx context.Context, branch string) error
	// SyncLocked runs Sync while acquiring lockBranch(branch), for callers that
	// are NOT already inside the branch lock — so the index mutation can't race
	// an inline write's sync or a concurrent Rebuild on the same branch.
	SyncLocked(ctx context.Context, branch string) error
	// Rebuild acquires lockBranch(branch) for its full duration; it is safe to
	// call out-of-band (no caller holds the branch lock first).
	Rebuild(ctx context.Context, branch string, progress RebuildProgress) error
	SyncWatermark(ctx context.Context, branch string) (string, error)
	// NeedsRebuild reports whether this BRANCH's persisted derived state was
	// written by an older schema version and must be regenerated via Rebuild.
	// Per-branch: Rebuild bumps only the branch it rebuilt, so one branch's
	// answer never speaks for another's.
	NeedsRebuild(ctx context.Context, branch string) (bool, error)
}

// RemoteIndex is the interface for git remote configuration and synchronization.
// Implemented by *remoteIndex, exposed on Service via Remote().
type RemoteIndex interface {
	// GetRemote assembles the INJECTED origin (Service.SetOrigin, sourced from
	// control.db) with this repo's own sync/push status row. There is no writer
	// for connection identity here: control.db's repo_origins owns it.
	GetRemote(name string) (*Remote, error)
	DeleteRemote(name string) error
	Sync(ctx context.Context, localBranch string, auth transport.AuthMethod) (SyncResult, error)
	Push(ctx context.Context, branch string, auth transport.AuthMethod) (PushResult, error)
	// RecordSyncError persists a sync failure (last_status="error",
	// last_error=msg) on the named remote without performing a fetch. Used to
	// surface an auth-resolution failure that never reaches Sync().
	RecordSyncError(name, msg string) error
}

// BranchIndex is the interface for branch lifecycle operations. Implemented by *repoHandler.
type BranchIndex interface {
	EnsureBranch(ctx context.Context, name, gitRef string) (int64, error)
	MergeBranch(ctx context.Context, src, dst string, strategy ConflictStrategy) error
	DropBranch(ctx context.Context, name string) error
	ListBranches(ctx context.Context) ([]Branch, error)
	CreateBranch(ctx context.Context, branch, fromBranch string) error
	DefaultBranch(ctx context.Context) (string, error)
	SetDefaultBranch(branch string) error
	AgentBranchOwner(ctx context.Context) (string, error)
	SetAgentBranchOwner(ctx context.Context, branch string) error
	BranchInfo(localAgent string) (branches, agentBranches []string, matchedAgent string)
	HeadCommit(ctx context.Context, branch string) (string, error)
	HeadCommitInfo(ctx context.Context, branch string) (hash string, committedAt time.Time, err error)
}

// ToolSessionIndex is the interface for tool session persistence. Implemented by *toolIndex.
type ToolSessionIndex interface {
	CreateToolSession(ctx context.Context, tool, branch, pathPrefix, binding, readSet string) (*ToolSession, error)
	GetToolSession(ctx context.Context, id string) (*ToolSession, error)
	UpdateToolSession(ctx context.Context, id, lastCommit, status string) error
	GetSeenPaths(ctx context.Context, sessionID string) (map[string]bool, error)
	AddSeenPaths(ctx context.Context, sessionID string, paths []string) error
	EnqueuePaths(ctx context.Context, sessionID string, items []QueueItem) error
	DequeuePaths(ctx context.Context, sessionID string, limit int) ([]QueueItem, error)
	QueueSize(ctx context.Context, sessionID string) (int, error)
}

// PipelineIndex is the interface for pipeline session management. Implemented by *pipelineIndex.
type PipelineIndex interface {
	CreatePipelineSession(ctx context.Context, tool, branch string) (*PipelineSession, error)
	GetPipelineSession(ctx context.Context, id string) (*PipelineSession, error)
	MarkPipelineSessionScoped(ctx context.Context, id string) error
	AdvancePipelineSessionPhase(ctx context.Context, id, from, to string) (advanced bool, err error)
	CompletePipelineSession(ctx context.Context, id string) error
	InsertPipelineWorkItem(ctx context.Context, item PipelineWorkItem) error
	NextPipelineWorkItem(ctx context.Context, sessionID string) (*PipelineWorkItem, error)
	// AnswerPipelineWorkItem atomically claims and answers an item.
	// claimed=false means another caller already answered it — a benign
	// no-op. Only the claim winner may apply the response's mutations.
	AnswerPipelineWorkItem(ctx context.Context, id int64, response string) (claimed bool, err error)
	// AddPipelineSessionStats accumulates an applied item's corpus-change
	// counts onto the session row, which is where a per-call-stateless
	// engine's running totals have to live.
	AddPipelineSessionStats(ctx context.Context, id string, s PipelineSessionStats) error
	PipelineWorkItemStats(ctx context.Context, sessionID string) (completed, remaining int, err error)
	GetPipelineWatermark(ctx context.Context, tool, branch string) (string, error)
	SetPipelineWatermark(ctx context.Context, tool, branch, hash string) error
}

// TitleTarget is a fact whose title still needs embedding onto the abstraction
// axis.
type TitleTarget struct {
	FactID int64
	Path   string
	Title  string
}

// TitleVector is one fact's title embedding, keyed by fact id — the same
// content-addressed key facts_vec uses.
type TitleVector struct {
	FactID int64
	Vec    []float32
}

// TitleNeighbour is one KNN hit on the abstraction axis.
type TitleNeighbour struct {
	FactID     int64
	Path       string
	Similarity float64
}

// AbstractionIndex is the title-embedding axis ("the abstraction axis") and the
// restatement shortlist built on it. Implemented by *abstractionIndex, exposed
// on Service via Abstraction().
//
// REVIEW PIPELINE ONLY. It is deliberately not part of the SearchIndex
// composite: nothing on the runtime paths (query / explain / learn) may consume
// it, and keeping it off the composite makes that structural rather than a rule
// somebody has to remember.
// MotifIndex is alias resolution over the corpus's motif vocabulary
// (blueprint §3.1). Every method reads or rebuilds DERIVED state: the authored
// strings in facts.motifs remain the claim, and nothing here writes back into
// a fact (MN3).
type MotifIndex interface {
	// RebuildAliases recomputes the mechanical (stem/canonicalize) alias layer
	// for branch from the live corpus, replacing what was there. Judge merges
	// are not preserved — they are the LLM layer's to re-establish.
	RebuildAliases(ctx context.Context, branch string) error
	// CanonicalID resolves one spelling to its cluster's canonical id. An
	// unresolved spelling resolves to ITSELF, so a corpus with no alias table
	// behaves as one where every motif is its own singleton cluster.
	CanonicalID(ctx context.Context, branch, motif string) (string, error)
	// RecordJudgeMerge records that the LLM clustering pass judged two clusters
	// to name the same mechanism. Takes spellings, stores cluster keys. The
	// decision takes effect at the next RebuildAliases.
	//
	// rationale is the judge's own words for the shared mechanism and is
	// REQUIRED: a merge nobody could justify in a sentence is the hallucinated
	// merge the guard exists to stop, and over-merge is invisible downstream.
	RecordJudgeMerge(ctx context.Context, branch, motifA, motifB, rationale string) error
	// RecordJudgeDecline records that the judge saw a pair and said no, so the
	// pair is not re-offered while both clusters still mean what they meant.
	RecordJudgeDecline(ctx context.Context, branch, motifA, motifB string) error
	// AnsweredPairs returns the cluster pairs whose verdict still binds, keyed
	// by pairKey. The selector subtracts these from what it offers.
	AnsweredPairs(ctx context.Context, branch string) (map[string]struct{}, error)
	// Clusters returns the resolved vocabulary — one row per CLUSTER, most
	// frequent first, deterministic on ties.
	Clusters(ctx context.Context, branch string) ([]MotifCluster, error)
	// CarrierTitles returns up to limit titles of live facts carrying the
	// cluster. The judge sees these: string-only clustering keeps
	// adjacent-family false merges (§12-E3), and the titles are what expose it.
	CarrierTitles(ctx context.Context, branch, clusterKey string, limit int) ([]string, error)
	// ClustersNeedingDefinition returns live clusters whose definition is
	// missing or was authored over a different membership. Staleness is a
	// comparison, not a flag — so it catches every cause of drift.
	ClustersNeedingDefinition(ctx context.Context, branch string) ([]DefinitionTarget, error)
	// PutDefinition stores a definition, stamped with the membership it was
	// authored over.
	PutDefinition(ctx context.Context, branch, clusterKey, definition string) error
	// Definition returns a cluster's standing definition, INCLUDING a stale one
	// — a stale sentence is used as interim rather than gapping the cluster.
	Definition(ctx context.Context, branch, clusterKey string) (string, bool, error)
	// AliasRows returns the alias table with its audit columns (method and the
	// merge rationale).
	AliasRows(ctx context.Context, branch string) (map[string]AliasRow, error)
	// ClusterKey returns the STABLE identity of a spelling's cluster. Use this,
	// never CanonicalID, to key state that must survive across sessions:
	// CanonicalID is the highest-df member spelling and flips as usage shifts.
	ClusterKey(ctx context.Context, branch, motif string) (string, error)
	// AliasTable returns the whole spelling -> canonical id mapping.
	AliasTable(ctx context.Context, branch string) (map[string]string, error)
}

type AbstractionIndex interface {
	// LiveFactsMissingTitleVector returns up to limit live epistemic facts on
	// branch that have no title vector yet, lowest fact id first.
	LiveFactsMissingTitleVector(ctx context.Context, branch string, limit int) ([]TitleTarget, error)
	PutTitleVectors(ctx context.Context, vecs []TitleVector) error
	// TitleVectorCoverage reports (embedded, total) over live epistemic facts.
	TitleVectorCoverage(ctx context.Context, branch string) (have, total int, err error)
	// LiveEpistemicFacts is the live set, keyed by fact id with its path.
	LiveEpistemicFacts(ctx context.Context, branch string) (map[int64]string, error)
	// LiveEpistemicFactsOnAxis is the same set restricted to facts that carry a
	// title vector — what the pair cache is diffed against, so a partial
	// backfill cannot mark un-embedded facts as covered.
	LiveEpistemicFactsOnAxis(ctx context.Context, branch string) (map[int64]string, error)
	// TopTitleNeighbours returns up to k live epistemic neighbours of factID on
	// the axis, self excluded, most similar first. A fact with no vector yet
	// returns nothing rather than an error.
	TopTitleNeighbours(ctx context.Context, branch string, factID int64, k int) ([]TitleNeighbour, error)
	// BodyVectorsByFactID returns STORED blended vectors from facts_vec.
	BodyVectorsByFactID(ctx context.Context, ids []int64) (map[int64][]float32, error)

	// CachedPairFactIDs returns the fact ids the standing pair cache covers.
	CachedPairFactIDs(ctx context.Context, branch string) (map[int64]struct{}, error)
	// ReplaceRestatementPairs applies one cache delta atomically: pairs touching
	// dropFactIDs are removed, add is inserted, and coveredNow is recorded as
	// covered.
	ReplaceRestatementPairs(ctx context.Context, branch string, dropFactIDs []int64, add []RestatementPair, coveredNow []int64) error
	// RestatementPairsByRank returns the top `limit` pairs by title cosine.
	RestatementPairsByRank(ctx context.Context, branch string, limit int) ([]RestatementPair, error)
	// RestatementPairStats describes the standing population. Observability
	// only — no branch reads these values.
	RestatementPairStats(ctx context.Context, branch string) (RestatementPairStats, error)

	// RecordRestatementVerdict records what the judge did with one
	// shortlist-originated pair.
	RecordRestatementVerdict(ctx context.Context, branch string, v RestatementVerdict) error
	// RecentRestatementVerdicts returns the last `window` verdicts, newest
	// first — the input to the throttle.
	RecentRestatementVerdicts(ctx context.Context, branch string, window int) ([]RestatementVerdict, error)
	// KeptPairFactIDs returns pairs the judge declined, keyed by FactIDPairKey.
	// Consulted when MINTING pairs, so a declined pair is not re-created by a
	// later neighbour rescan.
	KeptPairFactIDs(ctx context.Context, branch string) (map[string]struct{}, error)
	// FactIDsByPath resolves specific live paths to their current fact ids.
	FactIDsByPath(ctx context.Context, branch string, paths []string) (map[string]int64, error)
	// PartnersOfFacts returns the still-cached partners of the given facts, so
	// an asymmetric KNN discovery is not lost when its owner is re-scanned.
	PartnersOfFacts(ctx context.Context, branch string, factIDs []int64) (map[int64]struct{}, error)
	// DeleteRestatementPair removes one standing pair (the judge declined it).
	DeleteRestatementPair(ctx context.Context, branch string, aFactID, bFactID int64) error
	// ProbeSessionsWaited returns how many sessions this branch has waited
	// since its last throttle probe, and ResetProbeWait / BumpProbeWait move it.
	// The counter is what keeps a defunded corpus recoverable.
	ProbeSessionsWaited(ctx context.Context, branch string) (int, error)
	SetProbeSessionsWaited(ctx context.Context, branch string, n int) error
}

// RestatementVerdict is one judge outcome on a shortlist-originated pair.
//
// Resolved, not Merged: a judge that consolidates a restatement by RETRACTING
// the redundant half has done exactly the work this mechanism exists to buy.
// Counting only merges would defund a corpus that is consolidating
// successfully by another route — which is the failure mode a throttle can
// least afford, since it looks identical to "the shortlist finds nothing".
//
// It carries fact ids as well as paths because ids are content-addressed: a
// "keep" applies to the exact pair of versions that was judged, and editing
// either fact makes the pair eligible again with no staleness rule needed.
type RestatementVerdict struct {
	APath    string
	BPath    string
	AFactID  int64
	BFactID  int64
	Resolved bool
	JudgedAt time.Time
}

// RestatementPair is one standing candidate: two facts whose TITLES are close
// on the abstraction axis while their bodies are not close enough for the
// mechanical dedup gate to have merged them. Canonical order: APath < BPath.
type RestatementPair struct {
	APath    string
	BPath    string
	AFactID  int64
	BFactID  int64
	TitleCos float64
}

// RestatementPairStats describes the standing pair population for health
// output: how many pairs stand, and where the top of the distribution sits.
type RestatementPairStats struct {
	Count int
	P99   float64
	P999  float64
}

// Embedder computes vector embeddings. Roles differ because retrieval models
// embed queries and documents with different prompts.
//
// On ctx and what it does NOT promise: the in-process ONNX implementation runs
// inference through a session handle that exposes no run-termination hook, so
// ctx is observed only at entry to each call — and, in EmbedDocuments, between
// batches. An inference already in flight runs to completion regardless of
// cancellation; cancelling bounds latency to one batch, it does not abort one.
// The parameter is here so the interface is honest about being a cancellation
// checkpoint and so a future remote embedder (which genuinely could abort a
// request) needs no signature change — not because this implementation can
// interrupt itself.
type Embedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
	EmbedDocument(ctx context.Context, title, body string) ([]float32, error)
	Dim() int
	ID() string
	// Thresholds returns the model's calibrated cosine cutoffs (dedup, search
	// recall, SIMILAR_TO, reflect novelty). They are model-dependent, so they
	// travel with the embedder rather than living as hard-coded constants.
	Thresholds() params.Thresholds
}

// EmbedderThresholds returns emb's calibrated cutoffs, or the historical
// nomic-era defaults when no embedder is configured (embeddings disabled), so
// callers get usable values without a nil check at every site.
func EmbedderThresholds(emb Embedder) params.Thresholds {
	if emb == nil {
		return params.Defaults()
	}
	return emb.Thresholds()
}

//go:generate go run go.uber.org/mock/mockgen -destination=mock_batch_embedder_test.go -package=store knomit/internal/store BatchEmbedder
//go:generate go run go.uber.org/mock/mockgen -destination=../mcp/mock_batch_embedder_test.go -package=mcp knomit/internal/store BatchEmbedder

// BatchEmbedder extends Embedder with batched document inference. This is the
// one embed path where ctx buys something material: a full-corpus re-embed
// issues many inferences, and the per-batch checkpoint bounds cancellation
// latency to a single batch rather than the whole corpus.
type BatchEmbedder interface {
	Embedder
	EmbedDocuments(ctx context.Context, titles, bodies []string) ([][]float32, error)
	// EmbedShortStrings embeds bare short strings (fact titles today, motif
	// names later) through the model's short-string template. Separate from
	// EmbedDocuments because the RENDERING differs, not the batching: a few
	// words in the document template embed measurably worse than the same words
	// in the title slot (see embeddings.Model.ShortStringTemplate).
	EmbedShortStrings(ctx context.Context, texts []string) ([][]float32, error)
}
