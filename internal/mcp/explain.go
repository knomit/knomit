package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

const explainPageSize = 25
const explainMaxDepth = 10

// explainHistoryDisplay is the number of root revisions shown inline on the
// first call, next to the body and graph.
const explainHistoryDisplay = 3

// explainHistoryPageSize is the number of revisions on a history-only page (a
// call with history_cursor). Larger than the inline page: such a page carries
// no body or graph, so revisions are all it costs.
const explainHistoryPageSize = 20

// explainTool returns the Tool definition for knomit_explain.
func explainTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_explain",
		mcpgo.WithDescription("Explain a fact by walking its versioned provenance graph. The walk is anchored at a commit: pass `commit` to explain the fact AS OF that version (the graph is rewound to how it stood then); omit it to explain at HEAD. The graph is versioned per-edge — every referenced fact is read at the exact version the referrer pointed to, recursively. The root fact is returned in full, with its `history`: the commits that changed its content, merge-delivered writes included, newest first. Each revision carries its `action` (added/modified) and its confidence/content diff against the content it was edited from. The first call returns the newest few. To read more, call again with the same `file` and `history_cursor` set to `history.history_cursor`; repeat while a `history_cursor` is returned. Those calls return history only. Do NOT page history by passing an older revision as `commit`. On a \"history changed\" error, call again without `history_cursor`. Every OTHER fact is returned as a lean summary (no body), marked `summary: true` — to read a summary's full body, history, and its own subtree, call knomit_explain again with that fact's `path` AND `commit`. A summary may carry `deleted: true` (the source was retracted since this edge formed) or `superseded: true` (the source is still live but its HEAD revision is newer than the version the referrer reasoned over — re-explain at HEAD to see how it has changed). Call with `file` to start; pass `cursor` to page the walk."),
		bindingArg(true),
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
		mcpgo.WithString("history_cursor",
			mcpgo.Description("Pass `history.history_cursor` from your previous explain of this `file` to get the next page of its history (history only). Pass it with the same `file`, without `commit` or `cursor`, and unchanged."),
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
	// Sources rides with Confidence on every node, summaries included: the two
	// together are the epistemic weight a caller needs to judge a node it did
	// not open, and instructions.go requires both for each fact cited in a
	// hypothesis evidence chain. No omitempty — sources: 0 is the claim "no
	// independent sources recorded", not an absence, and fact.Fact serializes it
	// unconditionally, so dropping it here would make explain and query disagree.
	Sources    int  `json:"sources"`
	Deleted    bool `json:"deleted,omitempty"`
	Superseded bool `json:"superseded,omitempty"`
	Summary    bool `json:"summary,omitempty"`
	// Expires/Expired ride on every node, summaries included: an expired
	// hypothesis cited deep in an evidence chain is exactly what a reader must
	// see. Expired is judged against the server clock at the time of THIS call
	// (a by-path read has no query clock to share). Knomit never acts on it.
	Expires string `json:"expires,omitempty"`
	Expired bool   `json:"expired,omitempty"`

	// Root-only fields (omitted on summary nodes).
	Domain   []string `json:"domain,omitempty"`
	Entities []string `json:"entities,omitempty"`
	// Motifs is the §6 explain surface: each of the root fact's motifs with its
	// document frequency and the other facts carrying it. This is where a
	// reader asks "what else instantiates this mechanism?" and gets an answer
	// without composing a second query.
	Motifs         []explainMotif  `json:"motifs,omitempty"`
	EvidenceWeight float64         `json:"evidence_weight,omitempty"`
	Body           string          `json:"body,omitempty"`
	Refs           *classifiedRefs `json:"refs,omitempty"`
	History        *explainHistory `json:"history,omitempty"`
}

// explainMotif is one motif on the root fact, resolved.
type explainMotif struct {
	// Motif is the spelling THIS fact carries — what its author wrote (MN3).
	Motif string `json:"motif"`
	// Canonical is the cluster's representative spelling, present only when it
	// differs from Motif. Its absence therefore means "this fact spells it the
	// way the corpus mostly does", which is the common and uninteresting case.
	Canonical string `json:"canonical,omitempty"`
	// Definition is the cluster's glossary sentence, when one has been
	// authored. Absent rather than empty on an undefined cluster.
	Definition string `json:"definition,omitempty"`
	// DF counts live facts carrying ANY spelling in the cluster, this one
	// included. 1 means nothing else instantiates it yet.
	DF int `json:"df"`
	// ClusterKey is the cluster's STABLE identity — what GET /motifs/{key}
	// accepts and what state may be keyed on. Canonical flips with usage;
	// this does not.
	ClusterKey string `json:"cluster_key,omitempty"`
	// MemberCount is how many spellings resolve to this cluster (this one
	// included). 1 means no aliasing — the common case.
	MemberCount int `json:"member_count,omitempty"`
	// Siblings are other facts carrying the cluster, most relevant first and
	// bounded. Empty at df 1.
	Siblings []string `json:"siblings,omitempty"`
}

type classifiedRefs struct {
	Local    []string `json:"local"`
	External []string `json:"external"`
}

// explainHistory is one page of the root fact's change list (store.PathHistory).
// more_available is nested here (not a sibling) because it describes the
// revision list — it is true when older revisions exist beyond the ones shown,
// and then HistoryCursor fetches the next page.
type explainHistory struct {
	Revisions     []explainRevision `json:"revisions"`
	MoreAvailable bool              `json:"more_available"`
	HistoryCursor string            `json:"history_cursor,omitempty"`
}

type explainRevision struct {
	Commit  string        `json:"commit"`
	Date    string        `json:"date"`
	Message string        `json:"message"`
	Action  string        `json:"action"`
	Diff    *revisionDiff `json:"diff"`
}

// historyCursor is the opaque history_cursor token: a keyset position in the
// root's change list (store.PathHistoryCursor) plus what it is valid for. It
// grants nothing beyond explain(file, commit) itself, and is refused unless
// every binding field matches the call that presents it:
//   - Pin and ReadSet: the binding it was issued to (PinID + read-set
//     fingerprint), exactly as explain's session cursor is bound.
//   - Mount and Path: the mount and NORMALIZED fact path it pages.
//   - Anchor: the commit the history is as of; the fact must be readable there.
type historyCursor struct {
	Pin            string   `json:"b"`
	ReadSet        string   `json:"r"`
	Mount          string   `json:"m"`
	Path           string   `json:"p"`
	Anchor         string   `json:"a"`
	Frontier       []string `json:"f"`
	AnchorOnBranch bool     `json:"o"`
}

func encodeHistoryCursor(c historyCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// errUnknownHistoryCursor is returned for any history_cursor this call cannot
// accept as its own; deliberately one message, like an expired session.
const errUnknownHistoryCursor = "unknown history_cursor — call knomit_explain without history_cursor and use the history_cursor it returns"

// errHistoryChanged is returned when the history a valid cursor was paging is
// gone (the branch was rewound, or its index is being rebuilt).
const errHistoryChanged = "history changed since this history_cursor was issued — call knomit_explain again without history_cursor to restart"

func decodeHistoryCursor(s string) (historyCursor, bool) {
	var c historyCursor
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || json.Unmarshal(b, &c) != nil || c.Anchor == "" || c.Path == "" || len(c.Frontier) == 0 {
		return historyCursor{}, false
	}
	return c, true
}

// revisionDiff is a revision's delta from the content it was edited from,
// precomputed by the store (see store.RevisionDiff).
type revisionDiff = store.RevisionDiff

// classifyRefs splits a fact's refs into the local fact edges the provenance
// walk descends into, and everything else. It consumes fact.ClassifyRef — the
// single ref-classification authority — so explain cannot drift from the edge
// builder, replay, the fact API, or the web client.
//
// localRepoID is the id of the mount owning the fact being explained, not the
// binding's write repo: under a lens, a read-mount fact's kb://<that-mount>/…
// refs are local to IT.
//
// The old rule sent every kb:// ref to External "despite ending in .md", which
// misfiled a self-qualified ref — the documented canonical form — as a
// cross-repo pointer. Only a FOREIGN kb:// ref is external.
//
// There is no "unresolved" bucket: knomit_learn and knomit_update reject a
// local ref that will not resolve, so the state cannot exist. Both slices stay
// non-nil so they serialize as [] rather than null.
func classifyRefs(refs []string, localRepoID string) *classifiedRefs {
	cr := &classifiedRefs{Local: []string{}, External: []string{}}
	for _, raw := range refs {
		if r := fact.ClassifyRef(raw, localRepoID); r.Kind == fact.RefLocalFact {
			cr.Local = append(cr.Local, r.Path)
			continue
		}
		cr.External = append(cr.External, raw)
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

		// A binding federates one write repo and N read mounts. Sessions and
		// snapshots always live in the WRITE repo's session DB (sWrite); explain
		// never fans out — the input fact fixes the mount, and the ENTIRE
		// provenance walk lives inside that mount at its pinned branch (RFC §6.2).
		b, err := repos.RequireBinding(ctx)
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		sWrite, releaseWrite, err := storeIndices(b.Write())
		if err != nil {
			return mcpgo.NewToolResultError(err.Error()), nil
		}
		defer releaseWrite()

		file := req.GetString("file", "")
		commit := req.GetString("commit", "")
		cursor := req.GetString("cursor", "")
		historyCur := req.GetString("history_cursor", "")

		if historyCur != "" {
			if cursor != "" || commit != "" {
				return mcpgo.NewToolResultError("history_cursor is passed with `file` only — drop `commit` and `cursor`"), nil
			}
			return explainHistoryPage(ctx, b, file, historyCur)
		}
		if cursor == "" {
			return explainFirstCall(ctx, b, sWrite, file, commit)
		}
		return explainResume(ctx, b, sWrite, cursor)
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
	headCommit, present := s.factQuery.LastCommitForPath(ctx, branch, path)
	deleted = !present
	superseded = present && headCommit != commit
	return parsed, deleted, superseded, true
}

// buildHistory assembles one page of the root's change list
// (store.PathHistory, precomputed: indexed lookups only, diffs included — no
// blob is read). pos is nil for the first page. Each revision's diff is against
// the content it was edited from.
// When older revisions remain, HistoryCursor carries the keyset position, bound
// to the binding, mount and path by bind.
func buildHistory(ctx context.Context, s mcpStore, branch, path, anchor string, pos *store.PathHistoryCursor, size int, bind historyCursor) (*explainHistory, error) {
	revs, next, err := s.history.PathHistory(ctx, branch, path, anchor, pos, size)
	if err != nil {
		return nil, err
	}
	if len(revs) == 0 && pos == nil {
		return nil, nil
	}

	h := &explainHistory{
		MoreAvailable: next != nil,
		Revisions:     make([]explainRevision, 0, len(revs)),
	}
	if next != nil {
		bind.Anchor = anchor
		bind.Frontier = next.Frontier
		bind.AnchorOnBranch = next.AnchorOnBranch
		h.HistoryCursor = encodeHistoryCursor(bind)
	}
	for _, r := range revs {
		h.Revisions = append(h.Revisions, explainRevision{
			Commit:  r.Commit,
			Date:    time.Unix(r.AuthoredAt, 0).UTC().Format(time.RFC3339),
			Message: r.Message,
			Action:  r.Action,
			Diff:    r.Diff,
		})
	}
	return h, nil
}

// explainTarget is the mount an explain call's `file` routes to.
type explainTarget struct {
	rt      repos.ReadTarget
	s       mcpStore
	release func()
	rel     string // repo-relative, normalized
	mount   string // "" for the write repo, else the mount's 12-hex id
}

// resolveExplainTarget routes the input fact to its mount: a kb://-qualified
// file names a specific mount; a bare file is the write repo. explain never
// fans out — the whole provenance walk lives inside this single mount (RFC
// §6.2). The caller must call release on success.
func resolveExplainTarget(b *repos.Binding, file string) (explainTarget, error) {
	if file == "" {
		return explainTarget{}, fmt.Errorf("file is required")
	}
	id, rel, qualified, err := federate.ParseQualifiedPath(file)
	if err != nil {
		return explainTarget{}, err
	}
	rt := repos.ReadTarget{RI: b.Write(), Branch: b.WriteMountBranch()}
	if qualified {
		var ok bool
		if rt, ok = b.ByID(id); !ok {
			return explainTarget{}, fmt.Errorf("repo %s is not mounted in this binding", id)
		}
	}
	rel = fact.NormalizePath(rt.RI.OntologyRoot(), rel)
	if !fact.IsFactFilePath(rt.RI.OntologyRoot(), rel) {
		return explainTarget{}, fmt.Errorf("%s is not a fact path: explain takes a fact under %s/ or a job slot under %s/<area>/", file, rt.RI.OntologyRoot(), fact.PrivateRoot)
	}
	s, release, err := storeIndices(rt.RI)
	if err != nil {
		return explainTarget{}, err
	}
	t := explainTarget{rt: rt, s: s, release: release, rel: rel}
	if rt.RI != b.Write() {
		t.mount = federate.ID12(rt.RI.ID())
	}
	return t, nil
}


// explainHistoryPage serves a history_cursor call: the next page of the root's
// change list from the cursor's keyset position. No body, no graph walk. The
// cursor must belong to this binding, mount and (normalized) path, and the
// fact must be readable at its anchor.
func explainHistoryPage(ctx context.Context, b *repos.Binding, file, token string) (*mcpgo.CallToolResult, error) {
	hc, ok := decodeHistoryCursor(token)
	if !ok || hc.Pin == "" || hc.Pin != b.PinID() || hc.ReadSet != federate.ReadSetFingerprint(b) {
		return mcpgo.NewToolResultError(errUnknownHistoryCursor), nil
	}
	t, err := resolveExplainTarget(b, file)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	defer t.release()
	if hc.Mount != t.mount || hc.Path != t.rel {
		return mcpgo.NewToolResultError(fmt.Sprintf("history_cursor belongs to another fact — pass it with the file it came from (%s)", hc.Path)), nil
	}
	if _, _, _, ok := readNode(ctx, t.s, t.rt.Branch, t.rel, hc.Anchor); !ok {
		return mcpgo.NewToolResultError(errUnknownHistoryCursor), nil
	}

	pos := &store.PathHistoryCursor{Frontier: hc.Frontier, AnchorOnBranch: hc.AnchorOnBranch}
	history, err := buildHistory(ctx, t.s, t.rt.Branch, t.rel, hc.Anchor, pos, explainHistoryPageSize, hc)
	if errors.Is(err, store.ErrHistoryChanged) {
		return mcpgo.NewToolResultError(errHistoryChanged), nil
	}
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("history error: %v", err)), nil
	}
	out, err := json.Marshal(map[string]any{
		"path":    file,
		"history": history,
	})
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
	}
	return mcpgo.NewToolResultText(string(out)), nil
}

func explainFirstCall(ctx context.Context, b *repos.Binding, sWrite mcpStore, file, commit string) (*mcpgo.CallToolResult, error) {
	t, err := resolveExplainTarget(b, file)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	defer t.release()
	rt, s, rel := t.rt, t.s, t.rel
	branch := rt.Branch

	// wire renders a repo-relative path as addressed on the wire: qualified iff
	// the mount is not the binding's write repo (RFC §6.2 uniformity). Seen-keys
	// stay repo-relative (derived from rel), never wired.
	qualify := rt.RI != b.Write()
	prefix := ""
	if qualify {
		prefix = federate.KBScheme + federate.ID12(rt.RI.ID()) + "/"
	}
	wire := func(p string) string {
		if qualify {
			return prefix + p
		}
		return p
	}

	// Resolve the anchor: the provided commit, else HEAD.
	anchor := commit
	if anchor == "" {
		head, err := s.branches.HeadCommit(ctx, branch)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("resolve HEAD error: %v", err)), nil
		}
		anchor = head
	}

	// Read the root fact as of the anchor. superseded is a descent-only signal
	// (the root IS the view the caller asked for), so it is discarded here.
	parsed, deleted, _, ok := readNode(ctx, s, branch, rel, anchor)
	if !ok {
		return mcpgo.NewToolResultError(fmt.Sprintf("could not read %s at %s", wire(rel), anchor)), nil
	}

	history, err := buildHistory(ctx, s, branch, rel, anchor, nil, explainHistoryDisplay, historyCursor{
		Pin:     b.PinID(),
		ReadSet: federate.ReadSetFingerprint(b),
		Mount:   t.mount,
		Path:    rel,
	})
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("history error: %v", err)), nil
	}

	// The root's own commit is its effective revision at the anchor: the
	// first-parent resolution (RevisionsBefore), NOT the newest entry of the
	// change list. Seen-keys and edge pins are first-parent commits, and the
	// two differ when the newest change arrived through a merge.
	rootCommit := anchor
	live, err := s.history.RevisionsBefore(ctx, branch, rel, anchor, 1)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("history error: %v", err)), nil
	}
	if len(live) > 0 {
		rootCommit = live[0].Commit
	}

	refs := classifyRefs(parsed.Refs, fact.ID12(rt.RI.ID()))
	entry := explainFactEntry{
		Path:           wire(rel),
		Commit:         rootCommit,
		Depth:          0,
		Title:          parsed.Title,
		Type:           string(parsed.Type),
		Kind:           kindString(parsed),
		Confidence:     parsed.Confidence,
		Deleted:        deleted,
		Expires:        parsed.Expires,
		Expired:        parsed.IsExpired(timeNow()),
		Domain:         parsed.Domain,
		Sources:        parsed.Sources,
		Entities:       parsed.Entities,
		Motifs:         explainMotifs(ctx, rt, s, parsed, rel),
		EvidenceWeight: parsed.EvidenceWeight,
		Body:           parsed.Body,
		Refs:           refs,
		History:        history,
	}

	// Enqueue children from the VERSIONED edges (each pinned at its target_commit).
	// Queue items carry the WIRE path (uniform with query's snapshot contract);
	// seen-keys stay repo-relative.
	edges, err := s.graph.OutgoingAtCommit(ctx, branch, rel, anchor)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("outgoing error: %v", err)), nil
	}
	var queueItems []store.QueueItem
	seenSeed := []string{seenKey(rel, rootCommit)}
	enqueued := map[string]bool{seenKey(rel, rootCommit): true}
	for _, e := range edges {
		k := seenKey(e.Path, e.Commit)
		if enqueued[k] {
			continue
		}
		enqueued[k] = true
		// Mark the child seen at ENQUEUE time, symmetric with resume: seen gates
		// enqueue only (never emission), so this stops resume re-enqueuing a node
		// that was already queued at mint (e.g. a diamond root→A, root→B, A→B).
		seenSeed = append(seenSeed, k)
		queueItems = append(queueItems, store.QueueItem{Path: wire(e.Path), CommitHash: e.Commit, SortKey: 1})
	}

	session, err := sWrite.toolSession.CreateToolSession(ctx, "explain", b.WriteMountBranch(), rel, b.PinID(), federate.ReadSetFingerprint(b))
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("create session error: %v", err)), nil
	}
	if err := sWrite.toolSession.AddSeenPaths(ctx, session.ID, seenSeed); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("add seen paths error: %v", err)), nil
	}
	if len(queueItems) > 0 {
		if err := sWrite.toolSession.EnqueuePaths(ctx, session.ID, queueItems); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("enqueue error: %v", err)), nil
		}
	}

	queueSize, _ := sWrite.toolSession.QueueSize(ctx, session.ID)
	hasMore := queueSize > 0
	if !hasMore {
		_ = sWrite.toolSession.UpdateToolSession(ctx, session.ID, rootCommit, "completed")
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

func explainResume(ctx context.Context, b *repos.Binding, sWrite mcpStore, cursor string) (*mcpgo.CallToolResult, error) {
	session, err := sWrite.toolSession.GetToolSession(ctx, cursor)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("session lookup error: %v", err)), nil
	}
	if session == nil || session.Status != "active" {
		return mcpgo.NewToolResultError("session expired or not found — omit cursor to start a new session"), nil
	}
	// A cursor is a frozen view of ONE binding's read set (lenses RFC §7.3).
	// A different binding — even one sharing the write repo — must not see it;
	// the error is indistinguishable from expiry by design. session.Binding
	// holds PinID(), the binding's stable id (repo:<uid> / lens:<uid>), not
	// Name() — a repo or lens rename changes Name but never moves the pin, so
	// a rename cannot orphan an in-flight cursor.
	//
	// PinID() (via pinOf) fails CLOSED on an empty uid: it returns "", never a
	// bare "repo:"/"lens:" prefix. "" must never match here even though a
	// stored "" and a computed "" are byte-equal — a uid-less binding must be
	// able to MINT a session but never RESUME one, or two uid-less bindings
	// would positively match instead of merely colliding on a shared prefix.
	if session.Binding == "" || session.Binding != b.PinID() {
		return mcpgo.NewToolResultError("session expired or not found — omit cursor to start a new session"), nil
	}
	// A cursor is a frozen view of the binding's READ SET at mint time — and the
	// write mount's branch (WriteMountBranch) is one term of that fingerprint, so
	// a resume bound to a different branch, a read mount re-pinned to a different
	// branch, or a changed mount set all diverge the fingerprint here. Reject
	// before any dequeue side effect (DequeuePaths mutates the queue): resuming
	// against another branch's state would silently leak wrong deleted/superseded
	// flags and truncate the walk. The error is indistinguishable from expiry BY
	// DESIGN (lenses RFC §7.3): a caller must not be able to tell a re-pinned read
	// set — or a branch change — from an expired cursor.
	if session.ReadSet != federate.ReadSetFingerprint(b) {
		return mcpgo.NewToolResultError("session expired or not found — omit cursor to start a new session"), nil
	}

	seen, err := sWrite.toolSession.GetSeenPaths(ctx, cursor)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("seen paths error: %v", err)), nil
	}

	var facts []explainFactEntry
	var newSeen []string
	var newQueue []store.QueueItem

	// Per-mount store handles, resolved once per resume. Explain never leaves the
	// input fact's mount, but each dequeued item carries its own wire path, so it
	// is routed the same way as query (RFC §7.3): unqualified → write mount,
	// qualified → the mount its kb:// id names in the current binding.
	stores := map[*repos.RepoInstance]mcpStore{b.Write(): sWrite}
	// Non-write mounts are acquired on first use and released when the resume
	// returns; the write mount's acquisition is owned by the calling handler.
	var mountReleases []func()
	defer func() {
		for _, r := range mountReleases {
			r()
		}
	}()

	// Retry dequeue up to 3 times if all items in a batch fail.
	for range 3 {
		items, err := sWrite.toolSession.DequeuePaths(ctx, cursor, explainPageSize)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("dequeue error: %v", err)), nil
		}
		if len(items) == 0 {
			break
		}

		for _, item := range items {
			id, rel, qualified, perr := federate.ParseQualifiedPath(item.Path)
			if perr != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("page decode error: %v", perr)), nil
			}
			rt := repos.ReadTarget{RI: b.Write(), Branch: b.WriteMountBranch()}
			if qualified {
				var ok bool
				if rt, ok = b.ByID(id); !ok {
					// A mount this snapshot referenced is gone from the binding —
					// the frozen view no longer exists (RFC §7.3).
					return mcpgo.NewToolResultError("session expired or not found — omit cursor to start a new session"), nil
				}
			}
			sm, ok := stores[rt.RI]
			if !ok {
				var release func()
				var serr error
				sm, release, serr = storeIndices(rt.RI)
				if serr != nil {
					// This item's mount is closing or replacing its store; the
					// frozen view cannot be served consistently right now.
					return mcpgo.NewToolResultError(serr.Error()), nil
				}
				mountReleases = append(mountReleases, release)
				stores[rt.RI] = sm
			}
			// wire re-derives the item's own mount prefix (the walk never leaves the
			// mount), so child edges stay qualified iff the item was.
			wire := func(p string) string {
				if qualified {
					return federate.KBScheme + id + "/" + p
				}
				return p
			}

			parsed, deleted, superseded, okRead := readNode(ctx, sm, rt.Branch, rel, item.CommitHash)
			if !okRead {
				continue
			}

			// Surface this node's children from the versioned edges. Seen-keys stay
			// repo-relative; queued children carry the item's wire prefix.
			if item.SortKey < explainMaxDepth {
				edges, eerr := sm.graph.OutgoingAtCommit(ctx, rt.Branch, rel, item.CommitHash)
				if eerr == nil {
					for _, e := range edges {
						k := seenKey(e.Path, e.Commit)
						if seen[k] {
							continue
						}
						seen[k] = true
						newSeen = append(newSeen, k)
						newQueue = append(newQueue, store.QueueItem{Path: wire(e.Path), CommitHash: e.Commit, SortKey: item.SortKey + 1})
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
				Sources:    parsed.Sources,
				Deleted:    deleted,
				Superseded: superseded,
				Summary:    true,
				Expires:    parsed.Expires,
				Expired:    parsed.IsExpired(timeNow()),
			})
		}

		if len(facts) > 0 {
			break
		}
		// All items failed — retry.
	}

	if len(newSeen) > 0 {
		if err := sWrite.toolSession.AddSeenPaths(ctx, cursor, newSeen); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("add seen paths error: %v", err)), nil
		}
	}
	if len(newQueue) > 0 {
		if err := sWrite.toolSession.EnqueuePaths(ctx, cursor, newQueue); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("enqueue error: %v", err)), nil
		}
	}

	queueSize, _ := sWrite.toolSession.QueueSize(ctx, cursor)
	hasMore := queueSize > 0
	if !hasMore {
		_ = sWrite.toolSession.UpdateToolSession(ctx, cursor, "", "completed")
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

// explainMotifsShown bounds the sibling list per motif. A PROMPT/RESPONSE-SIZE
// BUDGET: explain answers about one fact, and an unbounded sibling list on a
// popular motif would bury it. Raised 5→8 with the REST vocabulary surface:
// member_count tells a reader the list is a preview, and the full cluster is
// one GET /motifs/{cluster_key} away.
const explainMotifsShown = 8

// explainMotifs resolves a fact's motifs for the §6 explain surface.
//
// Degrades to nothing rather than failing the call: explain's job is the fact
// and its provenance, and a corpus whose vocabulary cannot be read should still
// answer that. Every field is read-only derived state (MN6 permits unrestricted
// reading; this consults no mechanical decision and spawns no work).
func explainMotifs(ctx context.Context, rt repos.ReadTarget, s mcpStore, parsed fact.Fact, self string) []explainMotif {
	if len(parsed.Motifs) == 0 || s.motifs == nil {
		return nil
	}
	branch := rt.Branch
	// Read ONCE for the whole fact, not per motif: the alias table is the same
	// table for every entry, and a fact carrying five motifs would otherwise
	// read it five times.
	aliasRows, aliasErr := s.motifs.AliasRows(ctx, branch)
	out := make([]explainMotif, 0, len(parsed.Motifs))
	for _, m := range parsed.Motifs {
		entry := explainMotif{Motif: m}

		if canonical, err := s.motifs.CanonicalID(ctx, branch, m); err == nil && canonical != m {
			// Shown only when it DIFFERS: on the common path the fact spells
			// the motif the way the corpus mostly does, and repeating it would
			// be noise in every row.
			entry.Canonical = canonical
		}
		key, err := s.motifs.ClusterKey(ctx, branch, m)
		if err != nil {
			out = append(out, entry)
			continue
		}
		entry.ClusterKey = key
		// Member count from the alias table; an unresolved cluster has no
		// rows and is the singleton it degrades to everywhere else.
		entry.MemberCount = 1
		if aliasErr == nil {
			n := 0
			for _, row := range aliasRows {
				if row.ClusterKey == key {
					n++
				}
			}
			if n > 0 {
				entry.MemberCount = n
			}
		}
		if def, ok, dErr := s.motifs.Definition(ctx, branch, key); dErr == nil && ok {
			entry.Definition = def
		}
		// df over the CLUSTER, matching TokenDF and the health metrics — a
		// corpus that spells one mechanism three ways must not read as three
		// lonely motifs here either.
		canonical := entry.Canonical
		if canonical == "" {
			canonical = m
		}
		if df, dfErr := s.graph.TokenDF(ctx, branch, canonical, "motif"); dfErr == nil {
			entry.DF = df
		}
		if titles, tErr := s.motifs.CarrierTitles(ctx, branch, key, explainMotifsShown+1); tErr == nil {
			for _, t := range titles {
				if t == parsed.Title {
					continue // the fact being explained is not its own sibling
				}
				if len(entry.Siblings) == explainMotifsShown {
					break
				}
				entry.Siblings = append(entry.Siblings, t)
			}
		}
		out = append(out, entry)
	}
	return out
}
