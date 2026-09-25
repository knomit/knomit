package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/rs/zerolog/log"

	"knomit/internal/repos"
	"knomit/internal/synthesize"
)

// reviewTool returns the Tool definition for knomit_review.
//
// Starting a new session can take 60-120s on a large knowledge base
// (clustering + dedup of dirty facts), so the tool advertises optional task
// support. Clients implementing the MCP tasks capability will run the call
// asynchronously and poll tasks/get for completion, avoiding their tool-call
// timeout. Clients without task support get the original synchronous behavior.
func reviewTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_review",
		mcpgo.WithDescription("Maintain the existing knowledge base: prune redundant facts, distill clusters into higher-order synthesis facts, and reflect on hypothesis transitions to record methodology. Does NOT generate new hypotheses — that is a separate explicit operation via knomit_hypothesize. When a user asks for a 'review', they want only this tool; do not chain to knomit_hypothesize unless the user explicitly requests hypothesis generation. Call with no arguments to start: this opens a review session, or resumes the one already in progress on this knowledge base (the result then has resumed: true). Continue by passing session_id, item_id and response: session_id is required on every call after the first, item_id with every response, and session_id alone returns the current item, and may advance the session when nothing is outstanding, and the result's `next` line names it. The binding selects the knowledge base; it does not identify the review session. If a start is refused because a session with a different scope or effort is in progress, continue that session with its session_id, or start again with takeover: true to abandon it. STOP when the result has done: true — the session is finished and that session_id can no longer be continued; calling with it again is an error. A start call may return done: true immediately when there is nothing to review; that is a normal, complete outcome, not a reason to retry. PAGED ITEMS: a work item whose facts exceed one tool result arrives split across pages. If a result has more_available: true, you have NOT seen the whole item — do not answer it. Call again with session_id, item_id and page = <next page> until more_available is false, accumulating every page, then answer once using the completion_token from the final page. Answering a paged item early is rejected."),
		bindingArg(true),
		mcpgo.WithString("session_id", mcpgo.Description("Session ID from the result you are answering. Required on every call after the first; omit it only to start.")),
		mcpgo.WithString("response", mcpgo.Description("Your JSON decisions for the previous work item.")),
		mcpgo.WithNumber("page", mcpgo.Description("Fetch another page of the CURRENT work item, without answering it. Large items are delivered across several pages: when a result carries more_available: true, call again with session_id, item_id and page = <the next page number> until more_available is false, THEN answer. Paging does not answer or advance anything, so pages may be re-fetched freely. Omit when submitting a response.")),
		mcpgo.WithString("completion_token", mcpgo.Description("Echo back the completion_token from the FINAL page of a multi-page work item. Required to answer such an item: it is the server's proof you read every page, and a response without it is rejected (the item stays available, so you can page properly and resubmit). Not needed for items delivered in a single page — those carry no token.")),
		mcpgo.WithNumber("item_id", mcpgo.Description("Echo back item.id from the work item you are answering. Required with every response, so an answer can only ever apply to the item it was written for.")),
		mcpgo.WithBoolean("takeover", mcpgo.Description("Start only: abandon the review session already in progress on this branch and start a new one. Needed only when a start is refused because a live session was opened with a different scope or effort.")),
		mcpgo.WithString("effort", mcpgo.Description("Discovery effort dial: 'normal' (default — pre-discovery behaviour), 'medium', or 'high'. Medium/high engage the structural-bridge engine to surface emergent synthesis facts from cross-cluster bridges.")),
		mcpgo.WithArray("domain", mcpgo.Description("Optional scope filter: restrict the seed pool to facts in these domains. Empty = whole corpus.")),
		mcpgo.WithArray("entities", mcpgo.Description("Optional scope filter: restrict the seed pool to facts tagged with these entities. Empty = whole corpus.")),
		mcpgo.WithTaskSupport(mcpgo.TaskSupportOptional),
	)
}

// ReviewHandler returns the handler function for knomit_review.
// A fresh synthesize.Reviewer is constructed per call from the repo in ctx.
//
// The handler signature is unchanged whether the call arrives synchronously
// or wrapped as a task — mcp-go dispatches it appropriately based on the
// client's request shape.
//
// When invoked as a task, mcp-go runs the handler in a goroutine but passes
// the HTTP request context, which Go's net/http cancels as soon as the
// initial CreateTaskResult response is sent. Without detaching, our work
// would see context.Canceled on the first SQL query. context.WithoutCancel
// keeps the values (notably the repo) but suppresses the cancellation that
// comes from the request lifecycle ending; client-initiated cancellation via
// tasks/cancel still works because mcp-go uses a separate cancel func.
func ReviewHandler() func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if req.Params.Task != nil {
			ctx = context.WithoutCancel(ctx)
		}
		// FIRST, before the binding gate and before any argument is read
		// (knomit#122). A malformed call is malformed wherever it is pointed,
		// and the harm this prevents is a call that RUNS: an unrecognised
		// scope key made every session whole-corpus, and an unscoped
		// completion advances the watermark. Rejecting after StartSession
		// would be too late by exactly the write that matters.
		if err := rejectUnknownArguments(req, reviewTool()); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		b, err := repos.RequireBinding(ctx)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if !b.WriteOK() {
			return mcpgo.NewToolResultError(readOnlyViewMessage(b)), nil
		}
		ri := b.Write()

		// Pin the write mount's store for the whole review call: the Reviewer
		// resolves store indices from ri per operation and uses them across a
		// long LLM-driven session step, so without the pin a concurrent
		// SwapStore/Archive could close the SQLite handle mid-review. The pin
		// makes those drains wait for this call instead.
		unpin, err := ri.Pin()
		if err != nil {
			return mcpgo.NewToolResultError(errStoreUnavailable.Error()), nil
		}
		defer unpin()

		effort, scope, err := parseEffortAndScope(req, ri)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		// Attribute whatever session this call opens. Harmless on a continue
		// call, which creates none — the handle is read only by StartSession
		// (knomit#123).
		ctx = withActor(ctx, req)
		// The session is opened against the BINDING's write branch: a review
		// inside an experiment reviews the experiment and advances the
		// experiment's watermark, not the agent branch's. The engine still
		// reads it exactly once, in StartSession, onto the session row
		// (invariants/synthesize/session-branch-binding).
		reviewer := synthesize.NewReviewerOnBranch(ri, logProgress, effort, scope, b.WriteBranch())

		sessionID := req.GetString("session_id", "")
		response := req.GetString("response", "")
		page := int(req.GetFloat("page", 0))
		itemID := int64(req.GetFloat("item_id", 0))
		completionToken := req.GetString("completion_token", "")
		takeover := req.GetBool("takeover", false)

		// An answer or a page fetch belongs to a session, so without one it is
		// refused rather than read as a start. Treating it as a start abandoned
		// the caller's own session and re-served the same first item, so an
		// agent that dropped session_id looped with no error and no progress.
		if sessionID == "" && (response != "" || itemID != 0 || completionToken != "" || page > 0) {
			return mcpgo.NewToolResultError(errContinuationWithoutSession), nil
		}

		if sessionID != "" && takeover {
			return mcpgo.NewToolResultError("takeover applies only to a start: omit session_id to start a new session with takeover:true, or omit takeover to continue this one"), nil
		}

		var result *synthesize.ReviewResult

		switch {
		case sessionID == "":
			result, err = reviewer.StartOrResumeSession(ctx, synthesize.StartOptions{
				Takeover:     takeover,
				ResumeWindow: ri.PipelineResumeWindow(),
			})
		case page > 0 && response == "":
			// A page fetch, not an answer. Ordered before the response guard
			// below because paging is the one continue-call that legitimately
			// carries no decisions — it is a pure read of an item the agent is
			// still assembling.
			result, err = reviewer.PageItem(ctx, sessionID, itemID, page)
		case response == "":
			result, err = reviewer.Current(ctx, sessionID)
		case itemID == 0:
			return mcpgo.NewToolResultError(errAnswerWithoutItem), nil
		default:
			result, err = reviewer.ContinueSessionForItemPaged(ctx, sessionID, response, itemID, completionToken)
		}

		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("review error: %v", err)), nil
		}

		if result != nil {
			result.Next = synthesize.ReviewNext(result, repos.SessionScoped(ctx))
		}
		resultJSON, _ := json.MarshalIndent(result, "", "  ")
		return mcpgo.NewToolResultText(string(resultJSON)), nil
	}
}

// errAnswerWithoutItem is the refusal for a response sent without item_id.
const errAnswerWithoutItem = "item_id is required with response: echo item.id from the item you are answering. " +
	"Nothing was applied. Call knomit_review with session_id and no response to get the current item."

// errContinuationWithoutSession is the refusal for a response, item_id,
// completion_token or page sent without session_id.
const errContinuationWithoutSession = "session_id is required with response, item_id, completion_token or page: " +
	"pass the session_id from the result you are answering. Nothing was applied, and no session was started or abandoned. " +
	"Call knomit_review with none of these arguments only to start a session."

// parseEffortAndScope resolves the shared 'effort' + 'domain'/'entities'
// arguments that both knomit_review and knomit_hypothesize accept. An empty
// effort falls back to the repo's configured default; the result is validated
// and normalized so callers always receive a well-known Effort. A returned
// error is a caller-facing validation message.
func parseEffortAndScope(req mcpgo.CallToolRequest, ri *repos.RepoInstance) (synthesize.Effort, synthesize.ScopeFilter, error) {
	effort := synthesize.Effort(req.GetString("effort", ""))
	if effort == "" {
		effort = synthesize.Effort(ri.DiscoveryEffortDefault())
	}
	if err := effort.Validate(); err != nil {
		return "", synthesize.ScopeFilter{}, err
	}
	effort = synthesize.NormalizeEffort(effort)
	scope := synthesize.ScopeFilter{
		Domain:   req.GetStringSlice("domain", nil),
		Entities: req.GetStringSlice("entities", nil),
	}
	return effort, scope, nil
}

// logProgress surfaces synthesize.ProgressEvent emissions to the server log.
// "warn" phases (e.g. validation rejections from ApplyDistillDecisions) go
// out at WARN; everything else at DEBUG so the server log stays usable
// during long review sessions.
func logProgress(e synthesize.ProgressEvent) {
	if e.Phase == "warn" {
		log.Warn().Str("phase", e.Phase).Msg(e.Message)
		return
	}
	log.Debug().Str("phase", e.Phase).Msg(e.Message)
}
