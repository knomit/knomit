package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store/model"
)

// The two audiences of knomit_materialize.
const (
	audienceAgent = "agent"
	audienceHuman = "human"
)

// materializeToolPrefix prefixes the tool-session `tool` value; the audience
// follows it, so a resume knows its page size before it dequeues anything,
// and a cursor minted by another tool is refused.
const materializeToolPrefix = "materialize:"

// Page sizes, in facts. A human page carries full bodies for a model to
// write from, so it is as small as knomit_query's include_body pages; an
// agent page is text the caller reads straight through.
const (
	materializeHumanDefaultLimit = 3
	materializeHumanMaxLimit     = 5
	materializeAgentDefaultLimit = 25
	materializeAgentMaxLimit     = 100
)

// materializePageCap is the most bytes a page may marshal to: the MCP
// client's tool-result cap. Pages are MEASURED against it (each page is
// marshalled as it is filled), never predicted, so there is no envelope
// reserve to drift. A single fact larger than the cap is still sent, alone.
const materializePageCap = 32 << 10

// materializeTimeout bounds one call. A first call walks up to 500 facts.
const materializeTimeout = 60 * time.Second

// errMaterializeExpired is the one answer for every cursor this call cannot
// serve: expired, unknown, another tool's, another binding's, or a read set
// that changed. Deliberately indistinguishable (lenses RFC §7.3).
const errMaterializeExpired = "session expired or not found — call knomit_materialize again without cursor to start over"

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
			mcpgo.Description("true: follow kb://<other-id>/… refs into another knowledge base registered on this server, read at its latest version. Default false: such refs are listed as not included."),
		),
		mcpgo.WithString("cursor",
			mcpgo.Description("The `cursor` from the previous page. With it, every other argument except `binding` and `limit` is refused: the document was fixed by the first call."),
		),
		mcpgo.WithNumber("limit",
			mcpgo.Description("Most facts per page: agent default 25, max 100; human default 3, max 5. A page also stops before it would exceed the transport's size cap."),
		),
	)
}

// materializeEnv is what every page repeats about the document, and what
// each snapshot item stores so a later page can repeat it.
type materializeEnv struct {
	Root                string                 `json:"root"`
	Anchor              string                 `json:"anchor"`
	Order               string                 `json:"order"`
	Audience            string                 `json:"audience"`
	Unpinned            bool                   `json:"unpinned"`
	CrossKB             bool                   `json:"cross_kb"`
	Depth               int                    `json:"depth"`
	MaxNodes            int                    `json:"max_nodes"`
	Total               int                    `json:"total"`
	Truncated           materialize.Truncation `json:"truncated"`
	InstructionsVersion string                 `json:"instructions_version,omitempty"`
}

// materializePage is one response.
type materializePage struct {
	materializeEnv
	Instructions string             `json:"instructions,omitempty"`
	Text         *string            `json:"text,omitempty"`
	Facts        []materialize.Node `json:"facts,omitempty"`
	NFrom        int                `json:"n_from"`
	NTo          int                `json:"n_to"`
	Cursor       *string            `json:"cursor"`
	HasMore      bool               `json:"has_more"`
}

// materializeItem is one snapshot row's state: the envelope and the node,
// without its body (read again at the node's frozen commit).
type materializeItem struct {
	Env  materializeEnv   `json:"env"`
	Node materialize.Node `json:"node"`
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
			return materializeResume(ctx, req, b, mgr, sWrite, cursor)
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
	limit, err := materializeLimit(req, audience)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	doc, err := materialize.Plan(ctx, materialize.Request{
		Binding: b, Manager: mgr, File: file, Commit: req.GetString("commit", ""), Options: opts,
	})
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	env := materializeEnv{
		Root: doc.Root, Anchor: doc.Anchor, Order: doc.Order, Audience: audience,
		Unpinned: opts.Unpin, CrossKB: opts.CrossKB, Depth: opts.Depth, MaxNodes: opts.MaxNodes,
		Total: len(doc.Nodes), Truncated: doc.Truncated,
	}
	if audience == audienceHuman {
		env.InstructionsVersion = materialize.InstructionsVersion
	}
	page, used, err := fillMaterializePage(env, doc, doc.Nodes, limit)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	rest := doc.Nodes[used:]
	if len(rest) > 0 {
		session, err := sWrite.toolSession.CreateToolSession(ctx, materializeToolPrefix+audience, b.WriteMountBranch(), doc.Root, b.PinID(), federate.ReadSetFingerprint(b))
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("create session error: %v", err)), nil
		}
		items, err := materializeItems(env, rest)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		if err := sWrite.toolSession.EnqueuePaths(ctx, session.ID, items); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("enqueue error: %v", err)), nil
		}
		page.Cursor = &session.ID
		page.HasMore = true
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
	session, err := sWrite.toolSession.GetToolSession(ctx, cursor)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("session lookup error: %v", err)), nil
	}
	// The same refusals as every other cursor (explain.go): a cursor is a
	// frozen view of ONE binding's read set, and every mismatch reads as
	// expiry. A cursor minted by another tool is refused the same way.
	if session == nil || session.Status != "active" || session.Binding == "" ||
		session.Binding != b.PinID() || session.ReadSet != federate.ReadSetFingerprint(b) ||
		!strings.HasPrefix(session.Tool, materializeToolPrefix) {
		return mcpgo.NewToolResultError(errMaterializeExpired), nil
	}
	audience := strings.TrimPrefix(session.Tool, materializeToolPrefix)
	limit, err := materializeLimit(req, audience)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	items, err := sWrite.toolSession.DequeuePaths(ctx, cursor, limit)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("dequeue error: %v", err)), nil
	}
	if len(items) == 0 {
		return mcpgo.NewToolResultError(errMaterializeExpired), nil
	}
	nodes := make([]materialize.Node, len(items))
	var env materializeEnv
	for i, it := range items {
		var st materializeItem
		if err := json.Unmarshal([]byte(it.State), &st); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("page decode error: %v", err)), nil
		}
		env, nodes[i] = st.Env, st.Node
	}
	if err := materialize.ReadBodies(ctx, b, mgr, nodes); err != nil {
		// The frozen view references a repo or version that can no longer be
		// read: it cannot be served consistently.
		return mcpgo.NewToolResultError(errMaterializeExpired + " (" + err.Error() + ")"), nil
	}
	page, used, err := fillMaterializePage(env, nil, nodes, limit)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	// Nodes that did not fit go back at their own ordinals, ahead of the rest.
	if used < len(items) {
		if err := sWrite.toolSession.EnqueuePaths(ctx, cursor, items[used:]); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("enqueue error: %v", err)), nil
		}
	}
	left, err := sWrite.toolSession.QueueSize(ctx, cursor)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("queue size error: %v", err)), nil
	}
	if left > 0 {
		page.Cursor = &cursor
		page.HasMore = true
	} else {
		_ = sWrite.toolSession.UpdateToolSession(ctx, cursor, env.Anchor, "completed")
	}
	return marshalMaterializePage(page)
}

// fillMaterializePage builds a page from the head of nodes: as many as fit
// under materializePageCap, at most limit, at least one. doc is non-nil only
// on page 1, which carries the agent header or the human instructions. used
// is how many nodes the page holds.
func fillMaterializePage(env materializeEnv, doc *materialize.Document, nodes []materialize.Node, limit int) (materializePage, int, error) {
	build := func(k int) materializePage {
		p := materializePage{materializeEnv: env, NFrom: nodes[0].N, NTo: nodes[k-1].N}
		if env.Audience == audienceAgent {
			var sb strings.Builder
			if doc != nil {
				sb.WriteString(materialize.RenderAgentHeader(doc))
			}
			for _, n := range nodes[:k] {
				sb.WriteString(materialize.RenderAgentNode(n))
			}
			text := sb.String()
			p.Text = &text
		} else {
			if doc != nil {
				p.Instructions = materialize.HumanInstructions
			}
			p.Facts = nodes[:k]
		}
		return p
	}
	// Measure with a cursor present: the largest form the page can take.
	placeholder := strings.Repeat("0", 36)
	used := 1
	for k := 2; k <= min(limit, len(nodes)); k++ {
		p := build(k)
		p.Cursor, p.HasMore = &placeholder, true
		out, err := json.Marshal(p)
		if err != nil {
			return materializePage{}, 0, err
		}
		if len(out) > materializePageCap {
			break
		}
		used = k
	}
	return build(used), used, nil
}

// materializeItems turns nodes into snapshot rows: sort_key is the ordinal,
// the state is the envelope and the node without its body.
func materializeItems(env materializeEnv, nodes []materialize.Node) ([]model.QueueItem, error) {
	items := make([]model.QueueItem, 0, len(nodes))
	for _, n := range nodes {
		n.Body = ""
		st, err := json.Marshal(materializeItem{Env: env, Node: n})
		if err != nil {
			return nil, err
		}
		items = append(items, model.QueueItem{Path: n.Path, CommitHash: n.Commit, SortKey: n.N, State: string(st)})
	}
	return items, nil
}

func marshalMaterializePage(p materializePage) (*mcpgo.CallToolResult, error) {
	out, err := json.Marshal(p)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
	}
	return mcpgo.NewToolResultText(string(out)), nil
}

// materializeLimit is the page's fact limit: the caller's, clamped to the
// audience's maximum, or the audience's default.
func materializeLimit(req mcpgo.CallToolRequest, audience string) (int, error) {
	def, max := materializeAgentDefaultLimit, materializeAgentMaxLimit
	if audience == audienceHuman {
		def, max = materializeHumanDefaultLimit, materializeHumanMaxLimit
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
