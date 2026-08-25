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
		mcpgo.WithDescription("Maintain the existing knowledge base: prune redundant facts, distill clusters into higher-order synthesis facts, and reflect on hypothesis transitions to record methodology. Does NOT generate new hypotheses — that is a separate explicit operation via knomit_hypothesize. When a user asks for a 'review', they want only this tool; do not chain to knomit_hypothesize unless the user explicitly requests hypothesis generation. Call with no arguments to start a new review session. Call with session_id + response to continue. STOP when the result has done: true — the session is finished and that session_id can no longer be continued; calling with it again is an error. A start call may return done: true immediately when there is nothing to review; that is a normal, complete outcome, not a reason to retry. PAGED ITEMS: a work item whose facts exceed one tool result arrives split across pages. If a result has more_available: true, you have NOT seen the whole item — do not answer it. Call again with session_id, item_id and page = <next page> until more_available is false, accumulating every page, then answer once using the completion_token from the final page. Answering a paged item early is rejected."),
		mcpgo.WithString("session_id", mcpgo.Description("Session ID from a previous call. Omit to start a new session.")),
		mcpgo.WithString("response", mcpgo.Description("Your JSON decisions for the previous work item.")),
		mcpgo.WithNumber("page", mcpgo.Description("Fetch another page of the CURRENT work item, without answering it. Large items are delivered across several pages: when a result carries more_available: true, call again with session_id, item_id and page = <the next page number> until more_available is false, THEN answer. Paging does not answer or advance anything, so pages may be re-fetched freely. Omit when submitting a response.")),
		mcpgo.WithString("completion_token", mcpgo.Description("Echo back the completion_token from the FINAL page of a multi-page work item. Required to answer such an item: it is the server's proof you read every page, and a response without it is rejected (the item stays available, so you can page properly and resubmit). Not needed for items delivered in a single page — those carry no token.")),
		mcpgo.WithNumber("item_id", mcpgo.Description("Echo back item.id from the work item you are answering. Optional but strongly recommended: applying a distill item enqueues follow-up items, so the current item can change between calls — echoing the id lets the server reject a stale answer instead of applying it to a different item.")),
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
		b := repos.BindingFromContext(ctx)
		if !b.WriteOK() {
			return mcpgo.NewToolResultError(fmt.Sprintf(
				"read-only view: branch %q is not writable; facts are authored on %q",
				b.WriteMountBranch(), b.Write().AgentBranch())), nil
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
		reviewer := synthesize.NewReviewerWithOptions(ri, logProgress, effort, scope)

		sessionID := req.GetString("session_id", "")
		response := req.GetString("response", "")
		page := int(req.GetFloat("page", 0))
		itemID := int64(req.GetFloat("item_id", 0))
		completionToken := req.GetString("completion_token", "")

		var result *synthesize.ReviewResult

		switch {
		case sessionID == "":
			result, err = reviewer.StartSession(ctx)
		case page > 0 && response == "":
			// A page fetch, not an answer. Ordered before the response guard
			// below because paging is the one continue-call that legitimately
			// carries no decisions — it is a pure read of an item the agent is
			// still assembling.
			result, err = reviewer.PageItem(ctx, sessionID, itemID, page)
		case response == "":
			return mcpgo.NewToolResultError("response is required when continuing a session (or pass page=N to read another page of the current item)"), nil
		default:
			result, err = reviewer.ContinueSessionForItemPaged(ctx, sessionID, response, itemID, completionToken)
		}

		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("review error: %v", err)), nil
		}

		resultJSON, _ := json.MarshalIndent(result, "", "  ")
		return mcpgo.NewToolResultText(string(resultJSON)), nil
	}
}

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
