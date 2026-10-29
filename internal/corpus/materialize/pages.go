package materialize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"knomit/internal/corpus/repos"
	"knomit/internal/engine/store/model"
	"knomit/internal/engine/store/sessions"
)

// The two audiences of a paged materialization. "agent" pages carry the
// document's markdown in Page.Text; "human" pages carry the nodes as data in
// Page.Facts.
const (
	AudienceAgent = "agent"
	AudienceHuman = "human"
)

// HumanDefaultLimit is a human page's default fact count: knomit_materialize
// without `limit`, and every page of the REST pages route, so the two serve
// the same page for the same input.
const HumanDefaultLimit = 3

// toolPrefix prefixes the tool-session `tool` value; the audience follows it,
// so a resume knows its page size before it dequeues anything, and a cursor
// minted by another tool is refused.
const toolPrefix = "materialize:"

// PageCap is the most bytes a page may marshal to: the MCP client's
// tool-result cap, used by every surface that pages (knomit_materialize and
// the REST pages route), so a page means the same thing on both. Pages are
// MEASURED against it (each page is marshalled as it is filled), never
// predicted, so there is no envelope reserve to drift. A single fact larger
// than the cap is still sent, alone; a page never splits a fact.
const PageCap = 32 << 10

// ErrCursorExpired is the one answer for every cursor a call cannot serve:
// expired, unknown, another tool's, another binding's or read set's, another
// owner's, exhausted, or one whose frozen nodes this caller may not read.
// Deliberately indistinguishable (lenses RFC §7.3), and a refusal consumes
// nothing.
var ErrCursorExpired = errors.New("materialize cursor expired or not found")

// PageEnv is what every page repeats about the document, and what each
// snapshot item stores so a later page can repeat it. Its JSON is the wire
// envelope of knomit_materialize AND of the REST pages route: one struct, so
// the two cannot drift.
type PageEnv struct {
	Root                string     `json:"root"`
	Anchor              string     `json:"anchor"`
	Order               string     `json:"order"`
	Audience            string     `json:"audience"`
	Unpinned            bool       `json:"unpinned"`
	CrossKB             bool       `json:"cross_kb"`
	Depth               int        `json:"depth"`
	MaxNodes            int        `json:"max_nodes"`
	Total               int        `json:"total"`
	Truncated           Truncation `json:"truncated"`
	InstructionsVersion string     `json:"instructions_version,omitempty"`
}

// Page is one response of a paged materialization.
type Page struct {
	PageEnv
	Instructions string  `json:"instructions,omitempty"`
	Text         *string `json:"text,omitempty"`
	Facts        []Node  `json:"facts,omitempty"`
	NFrom        int     `json:"n_from"`
	NTo          int     `json:"n_to"`
	Cursor       *string `json:"cursor"`
	HasMore      bool    `json:"has_more"`
}

// pageItem is one snapshot row's state: the envelope and the node, without
// its body (read again at the node's frozen commit).
type pageItem struct {
	Env  PageEnv `json:"env"`
	Node Node    `json:"node"`
}

// CursorScope is everything a cursor is pinned to, supplied by the edge that
// serves the pages: the write mount's session index, and the values a resume
// must match. Every mismatch reads as ErrCursorExpired.
type CursorScope struct {
	// Sessions is the binding's write mount's tool-session index, held under
	// the caller's store acquisition.
	Sessions *sessions.ToolIndex
	// Branch is the binding's write mount branch (Binding.WriteMountBranch).
	Branch string
	// Pin is Binding.PinID; a cursor with an empty pin never resumes.
	Pin string
	// ReadSet is federate.ReadSetFingerprint of the binding.
	ReadSet string
	// Owner is the endpoint plus the caller's principal: a cursor belongs to
	// both (another owner reads it as expired).
	Owner string
}

// NewPageEnv is the envelope of doc for audience.
func NewPageEnv(doc *Document, audience string) PageEnv {
	env := PageEnv{
		Root: doc.Root, Anchor: doc.Anchor, Order: doc.Order, Audience: audience,
		Unpinned: doc.Options.Unpin, CrossKB: doc.Options.CrossKB,
		Depth: doc.Options.Depth, MaxNodes: doc.Options.MaxNodes,
		Total: len(doc.Nodes), Truncated: doc.Truncated,
	}
	if audience == AudienceHuman {
		env.InstructionsVersion = InstructionsVersion
	}
	return env
}

// FirstPage returns page 1 of doc (at most limit nodes, measured against
// PageCap) and, when nodes remain, snapshots them under a new cursor owned by
// sc. The order and every node's version are frozen here: a resume reads
// only the snapshot.
func FirstPage(ctx context.Context, sc CursorScope, doc *Document, audience string, limit int) (Page, error) {
	env := NewPageEnv(doc, audience)
	page, used, err := fillPage(env, doc, doc.Nodes, limit)
	if err != nil {
		return Page{}, err
	}
	rest := doc.Nodes[used:]
	if len(rest) == 0 {
		return page, nil
	}
	session, err := sc.Sessions.CreateToolSession(ctx, toolPrefix+audience, sc.Branch, doc.Root, sc.Pin, sc.ReadSet, sc.Owner)
	if err != nil {
		return Page{}, fmt.Errorf("create session error: %w", err)
	}
	items, err := snapshotItems(env, rest)
	if err != nil {
		return Page{}, err
	}
	if err := sc.Sessions.EnqueuePaths(ctx, session.ID, items); err != nil {
		return Page{}, fmt.Errorf("enqueue error: %w", err)
	}
	page.Cursor = &session.ID
	page.HasMore = true
	return page, nil
}

// NextPage serves the next page of cursor. limitFor gives the page's node
// limit for the cursor's audience (known only once the session is read).
// Every cursor this caller cannot be served — unknown, another tool's,
// another binding's, read set or owner, exhausted, or one whose frozen nodes
// the caller may not read (mayRead is asked again here) — is
// ErrCursorExpired, and leaves the queue exactly as it was.
func NextPage(ctx context.Context, sc CursorScope, b *repos.Binding, mgr *repos.Manager, mayRead func(string) bool, cursor string, limitFor func(audience string) (int, error)) (Page, error) {
	session, err := sc.Sessions.GetToolSession(ctx, cursor)
	if err != nil {
		return Page{}, fmt.Errorf("session lookup error: %w", err)
	}
	// The same refusals as every other cursor (explain.go): a cursor is a
	// frozen view of ONE binding's read set, owned by one endpoint and
	// principal, and every mismatch reads as expiry. A cursor minted by
	// another tool is refused the same way.
	if session == nil || session.Status != "active" || session.Binding == "" ||
		session.Binding != sc.Pin || session.ReadSet != sc.ReadSet ||
		session.Owner != sc.Owner ||
		!strings.HasPrefix(session.Tool, toolPrefix) {
		return Page{}, ErrCursorExpired
	}
	audience := strings.TrimPrefix(session.Tool, toolPrefix)
	limit, err := limitFor(audience)
	if err != nil {
		return Page{}, err
	}

	items, err := sc.Sessions.DequeuePaths(ctx, cursor, limit)
	if err != nil {
		return Page{}, fmt.Errorf("dequeue error: %w", err)
	}
	if len(items) == 0 {
		return Page{}, ErrCursorExpired
	}
	nodes := make([]Node, len(items))
	var env PageEnv
	for i, it := range items {
		var st pageItem
		if err := json.Unmarshal([]byte(it.State), &st); err != nil {
			return Page{}, fmt.Errorf("page decode error: %w", err)
		}
		env, nodes[i] = st.Env, st.Node
	}
	if err := ReadBodies(ctx, b, mgr, mayRead, nodes); err != nil {
		// The frozen view references a repo this caller does not reach (a
		// narrower credential resuming a wider caller's cursor: the binding
		// pin is not per principal), or a repo or version that can no longer
		// be read. Either way this caller gets nothing, and the answer is the
		// expiry one, naming nothing. The page goes back on the queue: the
		// refusal is about THIS caller, not the snapshot.
		if qerr := sc.Sessions.EnqueuePaths(ctx, cursor, items); qerr != nil {
			return Page{}, fmt.Errorf("enqueue error: %w", qerr)
		}
		return Page{}, ErrCursorExpired
	}
	page, used, err := fillPage(env, nil, nodes, limit)
	if err != nil {
		return Page{}, err
	}
	// Nodes that did not fit go back at their own ordinals, ahead of the rest.
	if used < len(items) {
		if err := sc.Sessions.EnqueuePaths(ctx, cursor, items[used:]); err != nil {
			return Page{}, fmt.Errorf("enqueue error: %w", err)
		}
	}
	left, err := sc.Sessions.QueueSize(ctx, cursor)
	if err != nil {
		return Page{}, fmt.Errorf("queue size error: %w", err)
	}
	if left > 0 {
		page.Cursor = &cursor
		page.HasMore = true
	} else {
		_ = sc.Sessions.UpdateToolSession(ctx, cursor, env.Anchor, "completed")
	}
	return page, nil
}

// fillPage builds a page from the head of nodes: as many as fit under
// PageCap, at most limit, at least one. doc is non-nil only on page 1, which
// carries the agent header or the human instructions. used is how many nodes
// the page holds.
func fillPage(env PageEnv, doc *Document, nodes []Node, limit int) (Page, int, error) {
	build := func(k int) Page {
		p := Page{PageEnv: env, NFrom: nodes[0].N, NTo: nodes[k-1].N}
		if env.Audience == AudienceAgent {
			var sb strings.Builder
			if doc != nil {
				sb.WriteString(RenderAgentHeader(doc))
			}
			for _, n := range nodes[:k] {
				sb.WriteString(RenderAgentNode(n))
			}
			text := sb.String()
			p.Text = &text
		} else {
			if doc != nil {
				p.Instructions = HumanInstructions
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
			return Page{}, 0, err
		}
		if len(out) > PageCap {
			break
		}
		used = k
	}
	return build(used), used, nil
}

// snapshotItems turns nodes into snapshot rows: sort_key is the ordinal, the
// state is the envelope and the node without its body.
func snapshotItems(env PageEnv, nodes []Node) ([]model.QueueItem, error) {
	items := make([]model.QueueItem, 0, len(nodes))
	for _, n := range nodes {
		n.Body = ""
		st, err := json.Marshal(pageItem{Env: env, Node: n})
		if err != nil {
			return nil, err
		}
		items = append(items, model.QueueItem{Path: n.Path, CommitHash: n.Commit, SortKey: n.N, State: string(st)})
	}
	return items, nil
}
