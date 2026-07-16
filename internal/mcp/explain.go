package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/pmezard/go-difflib/difflib"
)

const explainPageSize = 25
const explainMaxDepth = 10

// explainHistoryDisplay is the number of root revisions surfaced; one extra is
// read as the diff base for the oldest displayed revision.
const explainHistoryDisplay = 3

// explainTool returns the Tool definition for knomit_explain.
func explainTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_explain",
		mcpgo.WithDescription("Explain a fact by walking its versioned provenance graph. The walk is anchored at a commit: pass `commit` to explain the fact AS OF that version (the graph is rewound to how it stood then); omit it to explain at HEAD. The graph is versioned per-edge — every referenced fact is read at the exact version the referrer pointed to, recursively. The root fact is returned in full, with its evolution `history` (recent revisions, each with the confidence/content diff from its predecessor). Every OTHER fact is returned as a lean summary (no body), marked `summary: true` — to read a summary's full body, history, and its own subtree, call knomit_explain again with that fact's `path` AND `commit`. A summary may carry `deleted: true` (the source was retracted since this edge formed) or `superseded: true` (the source is still live but its HEAD revision is newer than the version the referrer reasoned over — re-explain at HEAD to see how it has changed). Call with `file` to start; pass `cursor` to page the walk."),
		mcpgo.WithString("file",
			mcpgo.Required(),
			mcpgo.Description("Path to the fact file (e.g. kb/technology/go/abc123.md)."),
		),
		mcpgo.WithString("commit",
			mcpgo.Description("Anchor commit: explain the fact (and its graph) as of this version. Omit for HEAD. Use a `commit` value returned by a previous explain to drill into a summary node."),
		),
		mcpgo.WithString("cursor",
			mcpgo.Description("Session ID from a previous call. Omit to start."),
		),
	)
}

// explainFactEntry is one node in the walk. The root (depth 0) carries the full
// fact plus its evolution history. Every other node is a lean summary: the
// body and root-only fields are omitted and Summary is true.
type explainFactEntry struct {
	Path       string  `json:"path"`
	Commit     string  `json:"commit"`
	Depth      int     `json:"depth"`
	Title      string  `json:"title"`
	Type       string  `json:"type"`
	Kind       string  `json:"kind"`
	Confidence float64 `json:"confidence"`
	Deleted    bool    `json:"deleted,omitempty"`
	Superseded bool    `json:"superseded,omitempty"`
	Summary    bool    `json:"summary,omitempty"`

	// Root-only fields (omitted on summary nodes).
	Domain         []string        `json:"domain,omitempty"`
	Sources        int             `json:"sources,omitempty"`
	Entities       []string        `json:"entities,omitempty"`
	EvidenceWeight float64         `json:"evidence_weight,omitempty"`
	Body           string          `json:"body,omitempty"`
	Refs           *classifiedRefs `json:"refs,omitempty"`
	History        *explainHistory `json:"history,omitempty"`
}

type classifiedRefs struct {
	Local    []string `json:"local"`
	External []string `json:"external"`
}

// explainHistory is the root fact's bounded evolution. more_available is nested
// here (not a sibling) because it describes the revision list — it is true when
// older revisions exist beyond the ones shown.
type explainHistory struct {
	Revisions     []explainRevision `json:"revisions"`
	MoreAvailable bool              `json:"more_available"`
}

type explainRevision struct {
	Commit  string        `json:"commit"`
	Date    string        `json:"date"`
	Message string        `json:"message"`
	Diff    *revisionDiff `json:"diff"`
}

// revisionDiff is the delta from a revision's predecessor. Each field is
// present only when it changed; a nil revisionDiff means "no tracked change"
// (e.g. the fact's creation, which has no predecessor).
type revisionDiff struct {
	Confidence []float64 `json:"confidence,omitempty"` // [old, new]
	Body       string    `json:"body,omitempty"`       // smaller of unified diff vs "+N/-M"
}

func classifyRefs(refs []string) *classifiedRefs {
	cr := &classifiedRefs{Local: []string{}, External: []string{}}
	for _, ref := range refs {
		if strings.HasSuffix(ref, ".md") {
			cr.Local = append(cr.Local, ref)
		} else {
			cr.External = append(cr.External, ref)
		}
	}
	return cr
}

func kindString(f fact.Fact) string {
	k := f.Kind
	if k == "" {
		k = fact.DefaultKind
	}
	return string(k)
}

// seenKey is the composite (path, commit) identity used by the seen-set: the
// same path at two versions is two distinct nodes in a versioned walk.
func seenKey(path, commit string) string { return path + "@" + commit }

// ExplainHandler returns the handler function for knomit_explain.
func ExplainHandler() func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		ri := repos.RepoFromContext(ctx)
		s := storeIndices(ri)
		agentBranch := boundBranch(ctx, ri)
		ontologyRoot := ri.OntologyRoot()

		file := req.GetString("file", "")
		commit := req.GetString("commit", "")
		cursor := req.GetString("cursor", "")

		if cursor == "" {
			return explainFirstCall(ctx, s, ontologyRoot, agentBranch, file, commit)
		}
		return explainResume(ctx, s, agentBranch, cursor)
	}
}

// readFactVersion reads + parses a fact at a specific commit. When the pinned
// version is unreadable — whether the fact was retracted at the anchor, or the
// pin points at a commit we cannot resolve — it falls back to the most recent
// version before `commit` so the node still surfaces its content. ok is false
// only when no version can be read or parsed.
func readFactVersion(ctx context.Context, s mcpStore, branch, path, commit string) (fact.Fact, bool) {
	res, err := s.facts.ReadFact(ctx, branch, path, &store.ReadFactOpts{AtCommit: commit})
	if err != nil {
		res, err = s.facts.ReadFact(ctx, branch, path, &store.ReadFactOpts{BeforeCommit: commit})
		if err != nil {
			return fact.Fact{}, false
		}
	}
	p, perr := fact.ParseFact(path, res.Content)
	if perr != nil {
		return fact.Fact{}, false
	}
	return p, true
}

// readNode reads a node for the walk: its pinned version plus how that version
// relates to HEAD. Both flags describe the fact AT HEAD, independent of the
// pinned version still being readable:
//   - deleted: the fact is retracted at HEAD (gone since this edge formed).
//   - superseded: the fact is still live but its HEAD revision is newer than the
//     pinned `commit` — the referrer reasoned over an older version. Mutually
//     exclusive with deleted.
//
// ok is false when no version can be read or parsed.
func readNode(ctx context.Context, s mcpStore, branch, path, commit string) (parsed fact.Fact, deleted, superseded, ok bool) {
	parsed, ok = readFactVersion(ctx, s, branch, path, commit)
	if !ok {
		return fact.Fact{}, false, false, false
	}
	headCommit, present := s.search.LastCommitForPath(ctx, branch, path)
	deleted = !present
	superseded = present && headCommit != commit
	return parsed, deleted, superseded, true
}

// bodyDelta returns "" if the bodies are identical, else the smaller of a
// unified diff and a compact "+added/-removed" magnitude.
func bodyDelta(a, b string) string {
	if a == b {
		return ""
	}
	aLines := difflib.SplitLines(a)
	bLines := difflib.SplitLines(b)
	added, removed := 0, 0
	for _, op := range difflib.NewMatcher(aLines, bLines).GetOpCodes() {
		switch op.Tag {
		case 'r':
			removed += op.I2 - op.I1
			added += op.J2 - op.J1
		case 'd':
			removed += op.I2 - op.I1
		case 'i':
			added += op.J2 - op.J1
		}
	}
	magnitude := fmt.Sprintf("+%d/-%d", added, removed)
	unified, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{A: aLines, B: bLines, Context: 1})
	if unified != "" && len(unified) < len(magnitude) {
		return unified
	}
	return magnitude
}

// revisionDelta computes the diff from prev → cur. nil when there is no
// predecessor or nothing tracked changed.
func revisionDelta(prev *fact.Fact, cur fact.Fact) *revisionDiff {
	if prev == nil {
		return nil
	}
	d := &revisionDiff{}
	changed := false
	if prev.Confidence != cur.Confidence {
		d.Confidence = []float64{prev.Confidence, cur.Confidence}
		changed = true
	}
	if body := bodyDelta(prev.Body, cur.Body); body != "" {
		d.Body = body
		changed = true
	}
	if !changed {
		return nil
	}
	return d
}

// buildHistory assembles the root's bounded evolution: up to
// explainHistoryDisplay revisions in the ancestry of anchorCommit, each with
// the diff from its immediately-older predecessor.
func buildHistory(ctx context.Context, s mcpStore, branch, path, anchorCommit string) (*explainHistory, error) {
	revs, err := s.search.RevisionsBefore(ctx, branch, path, anchorCommit, explainHistoryDisplay+1)
	if err != nil {
		return nil, err
	}
	if len(revs) == 0 {
		return nil, nil
	}

	// Parse each revision's fact once, on demand. History only needs the
	// pinned content, so this skips the HEAD-liveness query readNode adds.
	cache := map[string]*fact.Fact{}
	parseAt := func(commit string) *fact.Fact {
		if f, ok := cache[commit]; ok {
			return f
		}
		var out *fact.Fact
		if p, ok := readFactVersion(ctx, s, branch, path, commit); ok {
			out = &p
		}
		cache[commit] = out
		return out
	}

	display := min(len(revs), explainHistoryDisplay)
	h := &explainHistory{
		MoreAvailable: len(revs) > explainHistoryDisplay,
		Revisions:     make([]explainRevision, 0, display),
	}
	for i := range display {
		var diff *revisionDiff
		if cur := parseAt(revs[i].Commit); cur != nil {
			var prev *fact.Fact
			if i+1 < len(revs) {
				prev = parseAt(revs[i+1].Commit)
			}
			diff = revisionDelta(prev, *cur)
		}
		h.Revisions = append(h.Revisions, explainRevision{
			Commit:  revs[i].Commit,
			Date:    time.Unix(revs[i].CommittedAt, 0).UTC().Format(time.RFC3339),
			Message: revs[i].Message,
			Diff:    diff,
		})
	}
	return h, nil
}

func explainFirstCall(ctx context.Context, s mcpStore, ontologyRoot, agentBranch, file, commit string) (*mcpgo.CallToolResult, error) {
	if file == "" {
		return mcpgo.NewToolResultError("file is required"), nil
	}
	file = fact.NormalizePath(ontologyRoot, file)

	// Resolve the anchor: the provided commit, else HEAD.
	anchor := commit
	if anchor == "" {
		head, err := s.branches.HeadCommit(ctx, agentBranch)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("resolve HEAD error: %v", err)), nil
		}
		anchor = head
	}

	// Read the root fact as of the anchor. superseded is a descent-only signal
	// (the root IS the view the caller asked for), so it is discarded here.
	parsed, deleted, _, ok := readNode(ctx, s, agentBranch, file, anchor)
	if !ok {
		return mcpgo.NewToolResultError(fmt.Sprintf("could not read %s at %s", file, anchor)), nil
	}

	history, err := buildHistory(ctx, s, agentBranch, file, anchor)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("history error: %v", err)), nil
	}

	// The root's own commit is its effective revision at the anchor.
	rootCommit := anchor
	if history != nil && len(history.Revisions) > 0 {
		rootCommit = history.Revisions[0].Commit
	}

	refs := classifyRefs(parsed.Refs)
	entry := explainFactEntry{
		Path:           file,
		Commit:         rootCommit,
		Depth:          0,
		Title:          parsed.Title,
		Type:           string(parsed.Type),
		Kind:           kindString(parsed),
		Confidence:     parsed.Confidence,
		Deleted:        deleted,
		Domain:         parsed.Domain,
		Sources:        parsed.Sources,
		Entities:       parsed.Entities,
		EvidenceWeight: parsed.EvidenceWeight,
		Body:           parsed.Body,
		Refs:           refs,
		History:        history,
	}

	// Enqueue children from the VERSIONED edges (each pinned at its target_commit).
	edges, err := s.search.OutgoingAtCommit(ctx, agentBranch, file, anchor)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("outgoing error: %v", err)), nil
	}
	var queueItems []store.QueueItem
	seenSeed := []string{seenKey(file, rootCommit)}
	enqueued := map[string]bool{seenKey(file, rootCommit): true}
	for _, e := range edges {
		k := seenKey(e.Path, e.Commit)
		if enqueued[k] {
			continue
		}
		enqueued[k] = true
		queueItems = append(queueItems, store.QueueItem{Path: e.Path, CommitHash: e.Commit, SortKey: 1})
	}

	session, err := s.toolSession.CreateToolSession(ctx, "explain", agentBranch, file)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("create session error: %v", err)), nil
	}
	if err := s.toolSession.AddSeenPaths(ctx, session.ID, seenSeed); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("add seen paths error: %v", err)), nil
	}
	if len(queueItems) > 0 {
		if err := s.toolSession.EnqueuePaths(ctx, session.ID, queueItems); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("enqueue error: %v", err)), nil
		}
	}

	queueSize, _ := s.toolSession.QueueSize(ctx, session.ID)
	hasMore := queueSize > 0
	if !hasMore {
		_ = s.toolSession.UpdateToolSession(ctx, session.ID, rootCommit, "completed")
	}

	var cursorOut any = session.ID
	if !hasMore {
		cursorOut = nil
	}
	out, err := json.Marshal(map[string]any{
		"facts":    []explainFactEntry{entry},
		"cursor":   cursorOut,
		"has_more": hasMore,
	})
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
	}
	return mcpgo.NewToolResultText(string(out)), nil
}

func explainResume(ctx context.Context, s mcpStore, agentBranch, cursor string) (*mcpgo.CallToolResult, error) {
	session, err := s.toolSession.GetToolSession(ctx, cursor)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("session lookup error: %v", err)), nil
	}
	if session == nil || session.Status != "active" {
		return mcpgo.NewToolResultError("session expired or not found — omit cursor to start a new session"), nil
	}
	// A cursor is a frozen view of the branch it was minted on. Reject a resume
	// bound to a different branch before any dequeue side effect (DequeuePaths
	// mutates the queue) — resuming against another branch would silently leak
	// wrong deleted/superseded flags and truncate the walk (lenses RFC §7.3).
	if session.Branch != agentBranch {
		return mcpgo.NewToolResultError(fmt.Sprintf("cursor was created on branch %q but this request is bound to %q — omit cursor to start a new explain", session.Branch, agentBranch)), nil
	}

	seen, err := s.toolSession.GetSeenPaths(ctx, cursor)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("seen paths error: %v", err)), nil
	}

	var facts []explainFactEntry
	var newSeen []string
	var newQueue []store.QueueItem

	// Retry dequeue up to 3 times if all items in a batch fail.
	for range 3 {
		items, err := s.toolSession.DequeuePaths(ctx, cursor, explainPageSize)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("dequeue error: %v", err)), nil
		}
		if len(items) == 0 {
			break
		}

		for _, item := range items {
			parsed, deleted, superseded, ok := readNode(ctx, s, agentBranch, item.Path, item.CommitHash)
			if !ok {
				continue
			}

			// Surface this node's children from the versioned edges.
			if item.SortKey < explainMaxDepth {
				edges, eerr := s.search.OutgoingAtCommit(ctx, agentBranch, item.Path, item.CommitHash)
				if eerr == nil {
					for _, e := range edges {
						k := seenKey(e.Path, e.Commit)
						if seen[k] {
							continue
						}
						seen[k] = true
						newSeen = append(newSeen, k)
						newQueue = append(newQueue, store.QueueItem{Path: e.Path, CommitHash: e.Commit, SortKey: item.SortKey + 1})
					}
				}
			}

			facts = append(facts, explainFactEntry{
				Path:       item.Path,
				Commit:     item.CommitHash,
				Depth:      item.SortKey,
				Title:      parsed.Title,
				Type:       string(parsed.Type),
				Kind:       kindString(parsed),
				Confidence: parsed.Confidence,
				Deleted:    deleted,
				Superseded: superseded,
				Summary:    true,
			})
		}

		if len(facts) > 0 {
			break
		}
		// All items failed — retry.
	}

	if len(newSeen) > 0 {
		if err := s.toolSession.AddSeenPaths(ctx, cursor, newSeen); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("add seen paths error: %v", err)), nil
		}
	}
	if len(newQueue) > 0 {
		if err := s.toolSession.EnqueuePaths(ctx, cursor, newQueue); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("enqueue error: %v", err)), nil
		}
	}

	queueSize, _ := s.toolSession.QueueSize(ctx, cursor)
	hasMore := queueSize > 0
	if !hasMore {
		_ = s.toolSession.UpdateToolSession(ctx, cursor, "", "completed")
	}

	var cursorOut any = cursor
	if !hasMore {
		cursorOut = nil
	}
	out, err := json.Marshal(map[string]any{
		"facts":    facts,
		"cursor":   cursorOut,
		"has_more": hasMore,
	})
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
	}
	return mcpgo.NewToolResultText(string(out)), nil
}
