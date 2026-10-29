package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/foundation/auth"
)

// The two audiences of knomit_materialize.
const (
	audienceAgent = materialize.AudienceAgent
	audienceHuman = materialize.AudienceHuman
)

// Page sizes, in facts. A human page carries full bodies for a model to
// write from, so it is as small as knomit_query's include_body pages; an
// agent page is text the caller reads straight through. Every page is also
// measured against materialize.PageCap.
const (
	materializeHumanDefaultLimit = materialize.HumanDefaultLimit
	materializeHumanMaxLimit     = materialize.HumanMaxLimit
	materializeAgentDefaultLimit = 25
	materializeAgentMaxLimit     = 100
)

// materializeTimeout bounds one call. A first call walks up to 500 facts.
const materializeTimeout = 60 * time.Second

// errMaterializeExpired is the one answer for every cursor this call cannot
// serve: expired, unknown, another tool's, another binding's, or a read set
// that changed. Deliberately indistinguishable (lenses RFC §7.3).
const errMaterializeExpired = "session expired or not found — call knomit_materialize again without cursor to start over"

// errSessionBusy is the answer of every cursor resume (knomit_materialize,
// knomit_query, knomit_explain) whose dequeue found the session store locked
// past its busy timeout: the dequeue was rolled back, so the same call with
// the same cursor is safe to retry.
const errSessionBusy = "the session store is busy; nothing was consumed — retry the same call with the same cursor"

func materializeTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_materialize",
		mcpgo.WithDescription("Materialize ONE fact together with the facts it stands on, as one document in reading order, every fact in full. Use it to hand a fact and its foundations to someone at once (an agent's brief, a frame's context, a human-readable write-up); use knomit_explain instead to inspect a fact's provenance and history, and knomit_query to find facts. The content is the fact's refs walk: every fact it cites, recursively, to `depth` (default 4, max 10), at most `max_nodes` facts (default 200, max 500); a fact reached twice is written once and later citations link back to it; a cited fact that was not written is listed as \"not included\" with the reason. By default every ref is read at the version the citing fact cited (historical: the output depends only on the fact, the anchor `commit` and the options); `unpin: true` reads every ref at its latest version instead and silently leaves out a cited fact that is gone. A frame (type frame) is ordered differently: its ancestors (the frames it refs, recursively, whatever their distance, each with its own content only) come first, then the frame, then what it stands on. Refs into another knowledge base are followed only with `cross_kb: true`, at that knowledge base's latest version. `audience: \"agent\"` returns the document as markdown text in `text`; `audience: \"human\"` returns the facts as data in `facts`, plus `instructions` on the first page for writing them up as an article. Results are paged: while `has_more` is true, call again with `cursor` (and nothing else but `binding` and optionally `limit`). Read-only; never writes."),
		bindingArg(true),
		mcpgo.WithString("file",
			mcpgo.Description("The fact to materialize: a path under kb/ (e.g. kb/decisions/x/abc123.md), or kb://<id>/… for a fact in another mount of this binding. Required unless `cursor` is passed."),
		),
		mcpgo.WithString("audience",
			mcpgo.Enum(audienceAgent, audienceHuman),
			mcpgo.Description("\"agent\": the document as markdown `text`. \"human\": the facts as data plus writing `instructions`. Required unless `cursor` is passed."),
		),
		mcpgo.WithString("commit",
			mcpgo.Description("Anchor: materialize as of this commit. Omit for the branch tip, resolved once on the first call."),
		),
		mcpgo.WithNumber("depth",
			mcpgo.Description("How many ref hops to expand from the fact, 0 to 10; 0 is the fact alone. Default 4. A frame's ancestors are not bounded by it."),
		),
		mcpgo.WithNumber("max_nodes",
			mcpgo.Description("The most facts to write, 1 to 500, the fact itself and a frame's ancestors included. Default 200."),
		),
		mcpgo.WithBoolean("unpin",
			mcpgo.Description("true: read every ref at its target's latest version instead of the version that was cited; a target that is gone is left out without mention. Default false."),
		),
		mcpgo.WithBoolean("cross_kb",
			mcpgo.Description("true: follow kb://<other-id>/… refs into another knowledge base registered on this server that your credential can read, at its latest version; any other is listed as not available. Default false: such refs are listed as not included."),
		),
		mcpgo.WithString("cursor",
			mcpgo.Description("The `cursor` from the previous page. With it, every other argument except `binding` and `limit` is refused: the document was fixed by the first call."),
		),
		mcpgo.WithNumber("limit",
			mcpgo.Description("Most facts per page: agent default 25, max 100; human default 3, max 5. The first call's limit is kept for every later page of its cursor; passing limit with cursor changes it for that page only (not for a book cursor, whose page size is fixed). A page also stops before it would exceed the transport's size cap."),
		),
		mcpgo.WithObject("search",
			mcpgo.Description("BOOK MODE, instead of `file`: materialize a SEARCH as a book. The search runs on the server (the same filters as knomit_query and the UI's results list; sort relevance or recent) and every result, up to `facts`, becomes one unit: that result's own materialization under `depth` and `max_nodes`, in search order. A fact is written once in the book (a search result in its own unit, any other repeated fact where its walk reaches the most sections); every other appearance is a heading-only node with a `link` (unit, number, anchor, forward or back). Each node carries `unit`, `number` (\"5\", \"5.1.2\") and `tree_depth`; each page carries `title`, `search_line` and `units`. Human pages only (audience omitted or \"human\"), byte-identical to the REST GET …/search/book pages. Page with `cursor`; add `unit` to continue from another unit."),
			mcpgo.Properties(bookSearchKeys),
		),
		mcpgo.WithNumber("facts",
			mcpgo.Description("Book mode: the most search results (units), 1 to 500. Default: every result, up to 500. Fewer keeps the best matches."),
		),
		mcpgo.WithNumber("unit",
			mcpgo.Description("Book mode, with `cursor` only: continue from this unit's first section (1 to the book's `units`), forward or back."),
		),
	)
}

// MaterializeHandler serves knomit_materialize. mgr reaches every repo
// registered on the server, for cross_kb.
func MaterializeHandler(mgr *repos.Manager) func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		if err := rejectUnknownArguments(req, materializeTool()); err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		ctx, cancel := context.WithTimeout(ctx, materializeTimeout)
		defer cancel()

		b, err := repos.RequireBinding(ctx)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		sWrite, releaseWrite, err := storeIndices(b.Write())
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		defer releaseWrite()

		if cursor := req.GetString("cursor", ""); cursor != "" {
			book, err := materialize.IsBookCursor(ctx, materializeScope(ctx, b, sWrite), cursor)
			if err != nil {
				return mcpgo.NewToolResultError(err.Error()), nil
			}
			if book {
				return materializeBookResume(ctx, req, b, mgr, sWrite, cursor)
			}
			if _, ok := req.GetArguments()["unit"]; ok {
				return mcpgo.NewToolResultError("unit is for a book cursor only"), nil
			}
			return materializeResume(ctx, req, b, mgr, sWrite, cursor)
		}
		if isBookFirst(req) {
			return materializeBookFirst(ctx, req, b, mgr, sWrite)
		}
		for _, k := range append(bookOnlyArgs, "unit") {
			if _, ok := req.GetArguments()[k]; ok {
				return mcpgo.NewToolResultError(k + " is for book mode only: pass `search` instead of `file`"), nil
			}
		}
		return materializeFirst(ctx, req, b, mgr, sWrite)
	}
}

// materializeFirst plans the whole document, returns page 1, and snapshots
// the rest of the nodes for the cursor.
func materializeFirst(ctx context.Context, req mcpgo.CallToolRequest, b *repos.Binding, mgr *repos.Manager, sWrite mcpStore) (*mcpgo.CallToolResult, error) {
	audience := req.GetString("audience", "")
	if audience != audienceAgent && audience != audienceHuman {
		return mcpgo.NewToolResultError(`audience is required: "agent" or "human"`), nil
	}
	file := req.GetString("file", "")
	if file == "" {
		return mcpgo.NewToolResultError("file is required"), nil
	}
	opts := materialize.DefaultOptions()
	for _, a := range []struct {
		name string
		dst  *int
	}{{"depth", &opts.Depth}, {"max_nodes", &opts.MaxNodes}} {
		v, present, err := intArgument(req, a.name)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if present {
			*a.dst = v
		}
	}
	opts.Unpin = req.GetBool("unpin", false)
	opts.CrossKB = req.GetBool("cross_kb", false)
	limit, err := materializeLimit(req, audience, materializeDefaultLimit(audience))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	doc, err := materialize.Plan(ctx, materialize.Request{
		Binding: b, Manager: mgr, MayRead: callerMayRead(ctx),
		File: file, Commit: req.GetString("commit", ""), Options: opts,
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	page, err := materialize.FirstPage(ctx, materializeScope(ctx, b, sWrite), doc, audience, limit)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return marshalMaterializePage(page)
}

// materializeResume serves the next page of a snapshot.
func materializeResume(ctx context.Context, req mcpgo.CallToolRequest, b *repos.Binding, mgr *repos.Manager, sWrite mcpStore, cursor string) (*mcpgo.CallToolResult, error) {
	for _, k := range []string{"file", "audience", "commit", "depth", "max_nodes", "unpin", "cross_kb"} {
		if _, ok := req.GetArguments()[k]; ok {
			return mcpgo.NewToolResultError(fmt.Sprintf("%s cannot be passed with cursor: the document was fixed by the first call — pass only cursor (and binding, limit)", k)), nil
		}
	}
	page, err := materialize.NextPage(ctx, materializeScope(ctx, b, sWrite), b, mgr, callerMayRead(ctx), cursor,
		func(audience string, frozen int) (int, error) { return materializeLimit(req, audience, frozen) })
	switch {
	case errors.Is(err, materialize.ErrCursorExpired):
		return mcpgo.NewToolResultError(errMaterializeExpired), nil
	case errors.Is(err, materialize.ErrCursorBusy):
		return mcpgo.NewToolResultError(errSessionBusy), nil
	case err != nil:
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	return marshalMaterializePage(page)
}

// materializeScope pins a cursor to the binding (pin and read set) and to
// the caller (endpoint + principal, callerOwner).
func materializeScope(ctx context.Context, b *repos.Binding, sWrite mcpStore) materialize.CursorScope {
	return materialize.CursorScope{
		Sessions: sWrite.toolSession, Branch: b.WriteMountBranch(), Pin: b.PinID(),
		ReadSet: federate.ReadSetFingerprint(b), Owner: callerOwner(ctx),
	}
}

// callerMayRead is the cross-KB read check for a repo the binding does not
// mount: the caller's credential must reach that repo's own routes
// (auth.ReachesRepo). Read permission itself was already checked when the
// call reached this tool.
func callerMayRead(ctx context.Context) func(string) bool {
	return func(repo string) bool { return auth.ReachesRepo(ctx, repo) }
}

func marshalMaterializePage(p materialize.Page) (*mcpgo.CallToolResult, error) {
	out, err := json.Marshal(p)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
	}
	return mcpgo.NewToolResultText(string(out)), nil
}

// materializeLimit is the page's fact limit: the caller's, clamped to the
// audience's maximum, or else def. A first call passes the audience's
// default (materializeDefaultLimit); a resume passes the limit its first
// call froze in the cursor, so a cursor paged without `limit` keeps the
// first call's page size (the REST pages route, which refuses `limit` on a
// resume, serves the same pages).
func materializeLimit(req mcpgo.CallToolRequest, audience string, def int) (int, error) {
	max := materializeAgentMaxLimit
	if audience == audienceHuman {
		max = materializeHumanMaxLimit
	}
	v, present, err := intArgument(req, "limit")
	if err != nil {
		return 0, err
	}
	if !present {
		return def, nil
	}
	if v < 1 {
		return 0, errors.New("limit must be at least 1")
	}
	return min(v, max), nil
}

// materializeDefaultLimit is the audience's page size when the first call
// names no `limit`.
func materializeDefaultLimit(audience string) int {
	if audience == audienceHuman {
		return materializeHumanDefaultLimit
	}
	return materializeAgentDefaultLimit
}

// intArgument reads an integer argument. present is false when the key is
// absent; a value that is not a whole number is an error naming the key.
func intArgument(req mcpgo.CallToolRequest, key string) (v int, present bool, err error) {
	raw, ok := req.GetArguments()[key]
	if !ok {
		return 0, false, nil
	}
	f, isNum := raw.(float64)
	if !isNum {
		if i, isInt := raw.(int); isInt {
			return i, true, nil
		}
		return 0, true, fmt.Errorf("%s must be a whole number", key)
	}
	if f != math.Trunc(f) || math.IsInf(f, 0) {
		return 0, true, fmt.Errorf("%s must be a whole number", key)
	}
	return int(f), true, nil
}
