package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"knomit/internal/repos"
	"knomit/internal/store"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// changesPageSize is the default and maximum page size: the same cap as
// knomit_query's snippet pages. Rows are tiny (a path and one word), so the
// cap is about bounding one response, not its weight.
const changesPageSize = 100

// ChangesDescription is the plain-English contract, shared by the MCP tool and
// (restated) by the REST route's OpenAPI entry.
const ChangesDescription = "What is different under a folder since a commit you saw. " +
	"Compares the folder at commit `since` with the folder now and returns each fact path under it as " +
	"added, modified or deleted, plus `head` — the commit it compared against. " +
	"No timestamp is read anywhere: the answer depends only on the two commits, so two synced machines " +
	"get byte-identical answers. " +
	"KEEP THE BOOKMARK YOURSELF: store `head` in your own state and pass it as `since` next time; knomit " +
	"writes nothing for you. Omit `since` to get the whole folder as it is now (all rows `added`) — that " +
	"is also the recovery when you have lost your bookmark. " +
	"It is the NET difference: something added and removed between two reads is not reported, because it " +
	"is no longer there. A rename or move is a `deleted` row plus an `added` row (no rename detection). " +
	"If `since` is not behind the current head (a bookmark from a machine that was further ahead, or from " +
	"another branch) the call refuses with a named error instead of reporting false deletions: retry after " +
	"sync, or omit `since`. " +
	"Reads the repository's main (consensus) branch, so your own writes appear once they reach main. " +
	"Rows are sorted by path; when `has_more` is true pass the returned `cursor` (alone) for the next page, " +
	"which compares the SAME two commits."

// changesTool returns the Tool definition for knomit_changes.
func changesTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_changes",
		mcpgo.WithDescription(ChangesDescription),
		bindingArg(true),
		mcpgo.WithString("prefix",
			mcpgo.Description("The folder, relative to the ontology root, e.g. \"tasks/research\" for kb/tasks/research. "+
				"Case-insensitive; leading/trailing \"/\" ignored; no .md. Omit for the whole ontology root. "+
				"A folder that did not exist at `since`, or no longer exists now, reads as empty."),
		),
		mcpgo.WithString("since",
			mcpgo.Description("Your bookmark: the full 40-hex `head` a previous call returned. Omit to list the folder as it is now."),
		),
		mcpgo.WithNumber("limit",
			mcpgo.Description(fmt.Sprintf("Page size (default and max %d).", changesPageSize)),
		),
		mcpgo.WithString("cursor",
			mcpgo.Description("Page token from a previous response's `cursor`. When set, prefix/since are ignored: the page continues the same comparison."),
		),
	)
}

// changesResponse is the knomit_changes envelope. Cursor is non-nil only while
// more pages remain.
type changesResponse struct {
	Changes []store.PathChange `json:"changes"`
	Head    string             `json:"head"`
	Branch  string             `json:"branch"`
	HasMore bool               `json:"has_more"`
	Cursor  *string            `json:"cursor"`
}

// ChangesHandler returns the handler for knomit_changes. It only READS: two
// trees of the write repo's upstream branch. It never calls a store write.
func ChangesHandler() func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		b, err := repos.RequireBinding(ctx)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		// The write repo, read at its UPSTREAM — deliberately not the
		// binding's read mount (the agent branch) and not an experiment pin:
		// every machine's agent branch differs, main is the one history two
		// machines share.
		svc, release, err := b.Write().Acquire()
		if err != nil {
			return mcpgo.NewToolResultError(errStoreUnavailable.Error()), nil
		}
		defer release()
		branch := svc.UpstreamBranch()

		limit := req.GetInt("limit", changesPageSize)
		if limit <= 0 || limit > changesPageSize {
			limit = changesPageSize
		}

		var q store.ChangesQuery
		if c := req.GetString("cursor", ""); c != "" {
			if q, err = store.DecodeChangesCursor(c); err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
		} else {
			q = store.ChangesQuery{Since: req.GetString("since", ""), Prefix: req.GetString("prefix", "")}
		}
		q.Limit = limit

		res, err := svc.Facts().ChangesUnder(ctx, branch, q)
		if err != nil {
			return mcpgo.NewToolResultError(changesErrorText(err, branch)), nil
		}

		out := changesResponse{Changes: res.Changes, Head: res.Head, Branch: branch, HasMore: res.HasMore}
		if res.HasMore {
			cur := store.EncodeChangesCursor(q.Since, res.Head, q.Prefix, res.Changes[len(res.Changes)-1].Path)
			out.Cursor = &cur
		}
		data, err := json.Marshal(out)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
		}
		return mcpgo.NewToolResultText(string(data)), nil
	}
}

// changesErrorText words a store error for the caller. The client errors
// carry their remedy already; an unknown branch here means the repo has no
// local main yet.
func changesErrorText(err error, branch string) string {
	if errors.Is(err, store.ErrBranchNotFound) {
		return fmt.Sprintf("this repository has no local %q branch yet (it appears after the first sync); nothing to compare", branch)
	}
	return err.Error()
}
