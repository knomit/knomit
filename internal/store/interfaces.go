package store

import (
	"context"
	"time"

	"knomit/internal/store/model"
)

// BranchIndex is the interface for branch lifecycle operations. Implemented by *repoHandler.
type BranchIndex interface {
	EnsureBranch(ctx context.Context, name, gitRef string) (int64, error)
	DropBranch(ctx context.Context, name string) error
	ListBranches(ctx context.Context) ([]model.Branch, error)
	CreateBranch(ctx context.Context, branch, fromBranch string) error
	DefaultBranch(ctx context.Context) (string, error)
	SetDefaultBranch(branch string) error
	AgentBranchOwner(ctx context.Context) (string, error)
	SetAgentBranchOwner(ctx context.Context, branch string) error
	BranchInfo(localAgent string) (branches, agentBranches []string, matchedAgent string)
	HeadCommit(ctx context.Context, branch string) (string, error)
	HeadCommitInfo(ctx context.Context, branch string) (hash string, committedAt time.Time, err error)
	// GetExperiment reads one experiment record (a vcs read; the lifecycle
	// itself is merge.Repo, reached as Service.Merge). The bool separates "no
	// such experiment" from a read failure: eligibility must not treat the
	// latter as an answer.
	GetExperiment(ctx context.Context, name string) (model.Experiment, bool, error)
}

// ToolSessionIndex is the interface for tool session persistence. Implemented by *toolIndex.
type ToolSessionIndex interface {
	CreateToolSession(ctx context.Context, tool, branch, pathPrefix, binding, readSet string) (*model.ToolSession, error)
	GetToolSession(ctx context.Context, id string) (*model.ToolSession, error)
	UpdateToolSession(ctx context.Context, id, lastCommit, status string) error
	GetSeenPaths(ctx context.Context, sessionID string) (map[string]bool, error)
	AddSeenPaths(ctx context.Context, sessionID string, paths []string) error
	EnqueuePaths(ctx context.Context, sessionID string, items []model.QueueItem) error
	DequeuePaths(ctx context.Context, sessionID string, limit int) ([]model.QueueItem, error)
	QueueSize(ctx context.Context, sessionID string) (int, error)
}

// PipelineIndex is the interface for pipeline session management. Implemented by *pipelineIndex.
type PipelineIndex interface {
	CreatePipelineSession(ctx context.Context, tool, branch, createdBy string) (*model.PipelineSession, error)
	// CreatePipelineSessionReplacing is CreatePipelineSession that abandons
	// only the named active session ("" meaning none), and otherwise fails
	// with ErrPipelineSlotChanged without writing.
	CreatePipelineSessionReplacing(ctx context.Context, tool, branch, createdBy, startKey, replace string, idleBefore time.Time) (*model.PipelineSession, error)
	GetPipelineSession(ctx context.Context, id string) (*model.PipelineSession, error)
	ActivePipelineSession(ctx context.Context, tool, branch string) (*model.PipelineSession, error)
	// ResumePipelineSession bumps an active, planned session's heartbeat for a
	// resuming caller; false when it is no longer resumable.
	ResumePipelineSession(ctx context.Context, id string) (resumed bool, err error)
	// MarkPipelineSessionPlanned clears the planning mark and bumps the
	// heartbeat once a session's work is queued; false when the session was
	// no longer active (displaced while it planned).
	MarkPipelineSessionPlanned(ctx context.Context, id string) (marked bool, err error)
	// AbandonPlanningPipelineSession abandons one session still planning.
	AbandonPlanningPipelineSession(ctx context.Context, id string) error
	MarkPipelineSessionScoped(ctx context.Context, id string) error
	// AdvancePipelineSessionPhase moves from→to only while no item of the
	// session is unanswered or still being applied and no other advance's hook
	// is running; a won advance marks the session advancing.
	AdvancePipelineSessionPhase(ctx context.Context, id, from, to string) (advanced bool, err error)
	// CompletePipelineSession completes an ACTIVE session that is not mid
	// advance; false otherwise, in which case nothing of the completion may
	// follow.
	CompletePipelineSession(ctx context.Context, id string) (completed bool, err error)
	InsertPipelineWorkItem(ctx context.Context, item model.PipelineWorkItem) error
	NextPipelineWorkItem(ctx context.Context, sessionID string) (*model.PipelineWorkItem, error)
	// AnswerPipelineWorkItem atomically claims and answers an item.
	// claimed=false means another caller already answered it — a benign
	// no-op. Only the claim winner may apply the response's mutations.
	AnswerPipelineWorkItem(ctx context.Context, id int64, response string) (claimed bool, err error)
	// FinishPipelineWorkItem ends a claimed item's applying state, after its
	// apply returns. Until then the item holds the session's phase.
	FinishPipelineWorkItem(ctx context.Context, id int64) error
	// FinishPipelineSessionAdvance clears the advancing mark once the phase
	// hook has returned. Until then the next advance and completion wait.
	FinishPipelineSessionAdvance(ctx context.Context, id string) error
	// ApplyingPipelineWorkItem returns a claimed item still being applied, or 0.
	ApplyingPipelineWorkItem(ctx context.Context, sessionID string) (int64, error)
	// AddPipelineSessionStats accumulates an applied item's corpus-change
	// counts onto the session row, which is where a per-call-stateless
	// engine's running totals have to live.
	AddPipelineSessionStats(ctx context.Context, id string, s model.PipelineSessionStats) error
	PipelineWorkItemStats(ctx context.Context, sessionID string) (completed, remaining int, err error)
	// PendingPipelineWorkItems returns this session's UNANSWERED items, in
	// queue order. It exists for the mid-session refresh: an item's payload is
	// materialised when the session is planned, so a merge applied later in the
	// same session leaves every still-queued item describing a corpus that no
	// longer exists.
	PendingPipelineWorkItems(ctx context.Context, sessionID string) ([]model.PipelineWorkItem, error)

	// AnsweredDistillResponses returns the raw stored responses of this
	// session's ANSWERED distill items — the complement of the method above,
	// and the only read path back out of an answer. Used at session completion
	// to tally declined_reason from the durable record rather than from state
	// kept in memory, which does not survive the per-call engine rebuild.
	AnsweredDistillResponses(ctx context.Context, sessionID string) ([]string, error)
	// UpdatePipelineWorkItemFacts rewrites an unanswered item's payload. The
	// response IS NULL guard is the same CAS the claim protocol uses: an item
	// answered between the read and the rewrite must not be edited underneath
	// the answer that is already being applied.
	UpdatePipelineWorkItemFacts(ctx context.Context, id int64, factsJSON string) (updated bool, err error)
	// DeletePipelineWorkItem removes an unanswered item, for the case where a
	// refresh leaves it with too little left to judge. Same CAS guard.
	DeletePipelineWorkItem(ctx context.Context, id int64) (deleted bool, err error)
	GetPipelineWatermark(ctx context.Context, tool, branch string) (string, error)
	SetPipelineWatermark(ctx context.Context, tool, branch, hash string) error
}

//go:generate go run go.uber.org/mock/mockgen -destination=mock_batch_embedder_test.go -package=store knomit/internal/store BatchEmbedder
//go:generate go run go.uber.org/mock/mockgen -destination=../mcp/mock_batch_embedder_test.go -package=mcp knomit/internal/store BatchEmbedder
