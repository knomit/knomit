package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
	"strings"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// sort constants for knomit_query.
const (
	sortRelevance = "relevance"
	sortRecent    = "recent"
)

// Paging / size constants for knomit_query.
const (
	// defaultPageSize / maxPageSize bound the per-call page in snippet mode.
	defaultPageSize = 20
	maxPageSize     = 100

	// include_body pages are kept small: full bodies are heavy, and a page of
	// them must stay under the MCP tool-result size cap. For many full bodies,
	// callers should fetch per-fact via knomit_explain instead.
	includeBodyDefaultPage = 3
	includeBodyMaxPage     = 5

	// maxCandidates is the DEFAULT and CEILING for the max_results argument:
	// the total result set materialised into a snapshot for one query.
	// Paging walks within this set; it is NOT the page size.
	maxCandidates = 500

	// snippetMaxRunes is the snippet body length (in runes) returned by default
	// when include_body is false. Cut to a word/line boundary; "…" appended.
	snippetMaxRunes = 400
)

// pageSizeFor normalises the caller-supplied page size for the active mode.
func pageSizeFor(limit int, includeBody bool) int {
	def, max := defaultPageSize, maxPageSize
	if includeBody {
		def, max = includeBodyDefaultPage, includeBodyMaxPage
	}
	if limit <= 0 {
		return def
	}
	if limit > max {
		return max
	}
	return limit
}

// queryTool returns the Tool definition for knomit_query.
func queryTool() mcpgo.Tool {
	return mcpgo.NewTool("knomit_query",
		mcpgo.WithDescription("Search the knowledge base. Returns lightweight result rows (title, type, domain, score, and a ~400-char body SNIPPET with body_truncated=true) — NOT full bodies — so a large result set never floods. Results are paginated: when more remain, the response carries a `cursor`; pass it back (with no other filters) to get the next page. For the full body of a fact, set include_body=true (small pages only) or, better for a single fact, call knomit_explain. At least one of text, entities, domain, applies_to, path, type, origin, or min_confidence is required (not needed when paging with cursor). Set sort=recent to browse most-recently-updated facts (optionally filtered by type/domain/path); sort=recent needs no other filter."),
		mcpgo.WithString("text",
			mcpgo.Description("Full-text search query."),
		),
		mcpgo.WithArray("entities",
			mcpgo.Description("Filter by entities (all must be present)."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithArray("domain",
			mcpgo.Description("Filter by domain tags."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithArray("applies_to",
			mcpgo.Description("Filter by ancestor-or-equal domain match. Use when you want facts whose declared scope INCLUDES one of these areas (e.g. 'store/resolver' surfaces facts scoped to 'store' or 'store/resolver')."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithString("path",
			mcpgo.Description("Filter by path prefix."),
		),
		mcpgo.WithNumber("min_confidence",
			mcpgo.Description("Minimum confidence threshold (0–1)."),
		),
		mcpgo.WithNumber("min_similarity",
			mcpgo.Description("Minimum cosine similarity for text search (0–1); 0 uses the active embedding model's calibrated recall floor."),
		),
		mcpgo.WithNumber("limit",
			mcpgo.Description("Page size (results per call). Default 20, max 100 in snippet mode; default 3, max 5 when include_body=true."),
		),
		mcpgo.WithBoolean("include_body",
			mcpgo.Description("Return full fact bodies instead of snippets. Page size is capped low (default 3, max 5). For a single fact's full body, prefer knomit_explain."),
		),
		mcpgo.WithString("sort",
			mcpgo.Description("Result ordering: \"relevance\" (default) ranks by match score; \"recent\" orders by most-recently-committed. sort=recent may be called with no filter to browse the whole base, most-recent-first."),
		),
		mcpgo.WithString("cursor",
			mcpgo.Description("Opaque page token from a previous response's `cursor`. When set, all filter arguments are ignored (the result set is frozen); only `limit` and `include_body` still apply."),
		),
		mcpgo.WithNumber("max_results",
			mcpgo.Description("Maximum total results materialised for this query across all pages (snapshot depth). Default 500; values above 500 are clamped. Page size is controlled by `limit`, not this."),
		),
		mcpgo.WithArray("type",
			mcpgo.Description("Filter to these epistemic types (e.g. observation, policy, principle, hypothesis)."),
			mcpgo.WithStringItems(),
		),
		mcpgo.WithBoolean("domain_exact",
			mcpgo.Description("Match `domain` by exact canonical tag only (no token containment / hierarchy). Default false: 'ai' also matches 'ai governance', etc."),
		),
		mcpgo.WithArray("origin",
			mcpgo.Description("Filter by fact origin: authored (hand-written), distilled (synthesis-pipeline output), or discovered (emergent — surfaced by the discovery engine). Accepts a single value or a list."),
			mcpgo.WithStringItems(),
		),
	)
}

// factOutput is one result row. It is also the unit stored (JSON-encoded) in a
// session snapshot, so it must round-trip cleanly — hence Frontmatter is a
// concrete type, not interface{}.
type factOutput struct {
	File          string            `json:"file"`
	Title         string            `json:"title"`
	Kind          string            `json:"kind,omitempty"` // omitted when epistemic (the default)
	Type          string            `json:"type"`
	Score         float64           `json:"score"` // relevance score in [0,100]; 100 for filter-only queries
	Body          string            `json:"body"`
	BodyTruncated bool              `json:"body_truncated,omitempty"` // true only when body is a snippet
	Commit        string            `json:"commit"`
	Frontmatter   frontmatterOutput `json:"frontmatter"`
}

type frontmatterOutput struct {
	Domain         []string `json:"domain"`
	Confidence     float64  `json:"confidence"`
	Sources        int      `json:"sources"`
	Entities       []string `json:"entities"`
	Refs           []string `json:"refs"`
	EvidenceWeight float64  `json:"evidence_weight,omitempty"`
	CommittedAt    int64    `json:"committed_at,omitempty"`
}

// queryResponse is the knomit_query envelope. Cursor is non-nil only while more
// pages remain.
type queryResponse struct {
	Facts   []factOutput `json:"facts"`
	Cursor  *string      `json:"cursor"`
	HasMore bool         `json:"has_more"`
}

// pagedRowState is the minimal per-row state persisted in a session snapshot.
// Only the search score is stored — it is the one field a resumed page cannot
// re-derive from the fact. Everything else (title, type, body, frontmatter) is
// re-read lazily from the fact at its frozen commit when the page is served, so
// the snapshot stays tiny regardless of body size.
type pagedRowState struct {
	Score       float64 `json:"score"`
	CommittedAt int64   `json:"committed_at"`
}

// QueryHandler returns the handler function for knomit_query.
// The repo is resolved from the request context at call time via RepoMiddleware.
func QueryHandler() func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		// A binding federates one write repo and N read mounts. Sessions and
		// snapshots always live in the WRITE repo's store (sWrite); relevance
		// reads fan out across every mount (queryFirstCall).
		b := repos.BindingFromContext(ctx)
		sWrite := storeIndices(b.Write())

		includeBody := req.GetBool("include_body", false)
		pageSize := pageSizeFor(req.GetInt("limit", 0), includeBody)

		maxResults := req.GetInt("max_results", maxCandidates)
		if maxResults <= 0 {
			return mcpgo.NewToolResultError("max_results must be a positive integer"), nil
		}
		if maxResults > maxCandidates {
			maxResults = maxCandidates
		}

		sort := req.GetString("sort", sortRelevance)
		if sort != sortRelevance && sort != sortRecent {
			return mcpgo.NewToolResultError("sort must be \"relevance\" or \"recent\""), nil
		}

		// Resume path: a cursor pages a frozen snapshot; filters and sort are ignored.
		if cursor := req.GetString("cursor", ""); cursor != "" {
			return queryResume(ctx, b, sWrite, cursor, pageSize, includeBody)
		}

		if sort == sortRecent {
			return queryRecent(ctx, b, sWrite, req, pageSize, maxResults, includeBody)
		}
		return queryFirstCall(ctx, b, sWrite, req, pageSize, maxResults, includeBody)
	}
}

// recoverFanout converts a panic inside a fan-out goroutine into the mount's
// error slot. net/http only recovers panics raised on the REQUEST goroutine, so
// a panic in one of these bare per-mount goroutines would otherwise crash the
// WHOLE process — not merely the offending connection (pre-lens, the same store
// call ran on the request goroutine, so its blast radius was one connection;
// federation widened it to process death). A concrete source: an
// archive/shutdown race can leave a mount's svc == nil, so storeIndices returns
// a zero mcpStore whose index fields are nil interfaces and the first index call
// panics. Routing the panic into *slot lets it flow through the existing "any
// mount error fails the whole query" path (RFC §9.1) — a lens must never
// silently shrink its read set — instead of taking the server down. mount names
// the offending mount so the failure stays diagnosable.
func recoverFanout(mount string, slot *error) {
	if p := recover(); p != nil {
		*slot = fmt.Errorf("mount %s panicked: %v", mount, p)
	}
}

// mountLabel is a human-legible mount identity for fan-out error messages: the
// mount's repo name and its pinned branch. Name() never touches the store, so it
// is safe to call even on a mount whose svc is nil.
func mountLabel(rt repos.ReadTarget) string {
	return rt.RI.Name() + "@" + rt.Branch
}

// queryRecent serves the recency-ordered browse (sort=recent). It draws the
// ordered candidate set from RecentFacts (already filtered + committed_at
// DESC), snapshots it into a session, and serves the first page through the
// shared resume path so body hydration and pagination match relevance mode.
func queryRecent(ctx context.Context, b *repos.Binding, sWrite mcpStore, req mcpgo.CallToolRequest, pageSize, maxResults int, includeBody bool) (*mcpgo.CallToolResult, error) {
	q := parseQueryFilters(req)
	targets, err := federate.ReadTargetsFor(b, q.Path)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	q.Limit = maxResults // per-mount snapshot depth (RFC §7.1: no overscan factor)

	// Fan out in parallel; any mount error fails the whole query — a lens must
	// never silently shrink its read set (RFC §9.1).
	lists := make([][]store.RecentFactEntry, len(targets))
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t federate.Target) {
			defer wg.Done()
			// A panic here (e.g. nil-svc mount) must become this mount's error, not
			// crash the process — net/http recovers only the request goroutine.
			defer recoverFanout(mountLabel(t.RT), &errs[i])
			mq := q
			mq.Path = t.Path
			sm := storeIndices(t.RT.RI)
			lists[i], _, errs[i] = sm.search.RecentFacts(ctx, t.RT.Branch, mq)
		}(i, t)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("recent error: %v", e)), nil
		}
	}

	// RecentFacts orders its per-mount list by committed_at DESC WITHOUT a text
	// query, but by RELEVANCE score WITH one (store.recentFactsSearch — a
	// deliberate store-level fix pinned by
	// TestRecentFacts_WithQuery_SortsByRelevanceNotDate). The federated merge
	// must honour whichever key each mount ordered by: commit timestamps are
	// directly comparable across mounts (a k-way timestamp merge is correct), but
	// per-mount relevance ranks are NOT comparable across mounts (RFC §7.1) —
	// they must be fused by reciprocal rank fusion, exactly as the relevance
	// path does. federate.FuseRRF's N=1 identity also restores lens-of-one byte-identity,
	// which a global timestamp re-sort would silently break. So: text query →
	// RRF; text-less recency → committed_at merge.
	var order []federate.MountRef
	if q.Text != "" {
		order = federate.FuseRRF(listLens(lists))
		if len(order) > maxResults {
			order = order[:maxResults]
		}
	} else {
		// Text-less recency: each mount's list is already committed_at-DESC
		// (RecentFacts), the precondition federate.MergeRecent relies on.
		stamps := make([][]int64, len(targets))
		for i, list := range lists {
			stamps[i] = make([]int64, len(list))
			for j, e := range list {
				stamps[i][j] = e.CommittedAt
			}
		}
		order = federate.MergeRecent(stamps, maxResults)
	}
	if len(order) == 0 {
		return marshalQueryResponse(queryResponse{Facts: []factOutput{}, Cursor: nil, HasMore: false})
	}

	// Recent mode snapshots ALL merged rows into the session and serves page 1
	// through the shared resume path, so body hydration + paging match relevance
	// mode. Each row's WIRE path (bare for the write mount, kb://-qualified for a
	// foreign mount — RFC §6.2 uniformity) carries the mount identity on resume.
	sess, err := sWrite.toolSession.CreateToolSession(ctx, "query", b.WriteMountBranch(), "", b.Name(), federate.ReadSetFingerprint(b))
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("session error: %v", err)), nil
	}
	items := make([]store.QueueItem, len(order))
	for i, ref := range order {
		e := lists[ref.Mount][ref.Rank]
		state, mErr := json.Marshal(pagedRowState{Score: e.Score, CommittedAt: e.CommittedAt})
		if mErr != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("snapshot error: %v", mErr)), nil
		}
		items[i] = store.QueueItem{
			Path:       wirePath(b, targets[ref.Mount].RT, e.Path),
			CommitHash: e.CommitHash,
			SortKey:    i,
			State:      string(state),
		}
	}
	if err := sWrite.toolSession.EnqueuePaths(ctx, sess.ID, items); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("snapshot enqueue error: %v", err)), nil
	}
	// Serve the first page (and compute cursor/has_more) through the shared
	// resume path — every recent row is hydrated from its pinned commit.
	return queryResume(ctx, b, sWrite, sess.ID, pageSize, includeBody)
}

// parseQueryFilters reads the shared filter arguments into SearchOptions.
// Limit is set by the caller per mode.
func parseQueryFilters(req mcpgo.CallToolRequest) store.SearchOptions {
	return store.SearchOptions{
		Text:           req.GetString("text", ""),
		Entities:       req.GetStringSlice("entities", nil),
		Domain:         req.GetStringSlice("domain", nil),
		DomainAncestor: req.GetStringSlice("applies_to", nil),
		Path:           req.GetString("path", ""),
		MinConfidence:  req.GetFloat("min_confidence", 0),
		MinSimilarity:  req.GetFloat("min_similarity", 0),
		IncludeTypes:   req.GetStringSlice("type", nil),
		IncludeOrigins: stringOrSlice(req, "origin"),
		DomainExact:    req.GetBool("domain_exact", false),
	}
}

// stringOrSlice reads an argument that may be either a single string or a
// list of strings — clients (and hand-written test inputs) routinely supply
// a single value where the schema declares an array. Returns nil for an
// absent or empty argument.
func stringOrSlice(req mcpgo.CallToolRequest, key string) []string {
	if v := req.GetString(key, ""); v != "" {
		return []string{v}
	}
	return req.GetStringSlice(key, nil)
}

// hasAnyFilter reports whether any selecting filter was supplied.
func hasAnyFilter(q store.SearchOptions) bool {
	return q.Text != "" || len(q.Entities) > 0 || len(q.Domain) > 0 ||
		len(q.DomainAncestor) > 0 || q.Path != "" || q.MinConfidence > 0 ||
		len(q.IncludeTypes) > 0 || len(q.IncludeOrigins) > 0
}

// queryFirstCall fans a relevance query out across every read mount in
// parallel, fuses the per-mount ranked lists with reciprocal rank fusion,
// returns the first page, and (only when the fused set exceeds one page)
// snapshots the remainder into the write repo's session DB with WIRE paths.
func queryFirstCall(ctx context.Context, b *repos.Binding, sWrite mcpStore, req mcpgo.CallToolRequest, pageSize, maxResults int, includeBody bool) (*mcpgo.CallToolResult, error) {
	q := parseQueryFilters(req)
	if !hasAnyFilter(q) {
		return mcpgo.NewToolResultError("at least one of text, entities, domain, applies_to, path, type, origin, or min_confidence is required"), nil
	}
	targets, err := federate.ReadTargetsFor(b, q.Path)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	q.Limit = maxResults // per-mount snapshot depth (RFC §7.1: no overscan factor)

	// Fan out in parallel; any mount error fails the whole query — a lens must
	// never silently shrink its read set (RFC §9.1).
	lists := make([][]store.SearchResult, len(targets))
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t federate.Target) {
			defer wg.Done()
			// A panic here (e.g. nil-svc mount) must become this mount's error, not
			// crash the process — net/http recovers only the request goroutine.
			defer recoverFanout(mountLabel(t.RT), &errs[i])
			mq := q
			mq.Path = t.Path
			sm := storeIndices(t.RT.RI)
			lists[i], errs[i] = sm.search.Search(ctx, t.RT.Branch, mq)
		}(i, t)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("search error: %v", e)), nil
		}
	}

	order := federate.FuseRRF(listLens(lists))
	if len(order) > maxResults {
		order = order[:maxResults]
	}
	if len(order) == 0 {
		return marshalQueryResponse(queryResponse{Facts: []factOutput{}, Cursor: nil, HasMore: false})
	}

	// renderRow builds one output row from its fused reference. The displayed
	// Score stays the mount's NATIVE relevance score — RRF only orders, it does
	// not rescore — so across a fused (multi-mount) order the displayed scores
	// may be non-monotonic; that is the faithful representation of per-repo
	// relevance. File is the wire path: bare for the write repo, kb://-qualified
	// for a foreign mount (RFC §6.2 uniformity).
	renderRow := func(ref federate.MountRef) factOutput {
		r := lists[ref.Mount][ref.Rank]
		out := buildFactOutput(r, includeBody)
		out.File = wirePath(b, targets[ref.Mount].RT, r.Path)
		return out
	}

	// Fast path: the whole fused set fits in one page — no session needed.
	// Bodies are already in hand (search returns full bodies), so include_body
	// needs no re-fetch here.
	if len(order) <= pageSize {
		page := make([]factOutput, len(order))
		for i, ref := range order {
			page[i] = renderRow(ref)
		}
		return marshalQueryResponse(queryResponse{Facts: page, Cursor: nil, HasMore: false})
	}

	// Snapshot the remainder ([pageSize:]) into the write repo's session DB; the
	// first page is rendered directly (full bodies available, no re-fetch).
	sess, err := sWrite.toolSession.CreateToolSession(ctx, "query", b.WriteMountBranch(), "", b.Name(), federate.ReadSetFingerprint(b))
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("session error: %v", err)), nil
	}
	items := make([]store.QueueItem, 0, len(order)-pageSize)
	for i := pageSize; i < len(order); i++ {
		ref := order[i]
		r := lists[ref.Mount][ref.Rank]
		// Snapshot only what a resumed page can't re-derive: the rank score.
		// The WIRE path + commit pin the version; title/body/frontmatter are
		// re-read from the fact on resume, so the snapshot carries no heavy body.
		state, mErr := json.Marshal(pagedRowState{Score: r.Score, CommittedAt: r.CommittedAt})
		if mErr != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("snapshot error: %v", mErr)), nil
		}
		items = append(items, store.QueueItem{
			Path:       wirePath(b, targets[ref.Mount].RT, r.Path),
			CommitHash: r.CommitHash,
			SortKey:    i,
			State:      string(state),
		})
	}
	if err := sWrite.toolSession.EnqueuePaths(ctx, sess.ID, items); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("snapshot enqueue error: %v", err)), nil
	}

	page := make([]factOutput, pageSize)
	for i := range pageSize {
		page[i] = renderRow(order[i])
	}
	cursor := sess.ID
	return marshalQueryResponse(queryResponse{Facts: page, Cursor: &cursor, HasMore: true})
}

// listLens returns each list's length, the shape federate.FuseRRF consumes.
func listLens[T any](lists [][]T) []int {
	ns := make([]int, len(lists))
	for i, l := range lists {
		ns[i] = len(l)
	}
	return ns
}

// wirePath renders a result path as addressed on the wire: qualified iff the
// mount is not the binding's write repo (RFC §6.2 uniformity invariant).
func wirePath(b *repos.Binding, rt repos.ReadTarget, rel string) string {
	if rt.RI == b.Write() {
		return rel
	}
	return federate.QualifyPath(federate.ID12(rt.RI.ID()), rel)
}

// queryResume serves the next page from a frozen session snapshot, routing each
// item back to its mount (RFC §7.3): the item's wire path carries the mount
// identity, the current binding supplies the mount's instance and pinned branch.
// Session state (dequeue, size, status) lives in the WRITE repo's session DB.
func queryResume(ctx context.Context, b *repos.Binding, sWrite mcpStore, cursor string, pageSize int, includeBody bool) (*mcpgo.CallToolResult, error) {
	sess, err := sWrite.toolSession.GetToolSession(ctx, cursor)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("session error: %v", err)), nil
	}
	if sess == nil || sess.Status != "active" {
		return mcpgo.NewToolResultError("session expired or not found — omit cursor to start a new query"), nil
	}
	// A cursor is a frozen view of ONE binding's read set (lenses RFC §7.3).
	// A different binding — even one sharing the write repo — must not see it;
	// the error is indistinguishable from expiry by design.
	if sess.Binding != b.Name() {
		return mcpgo.NewToolResultError("session expired or not found — omit cursor to start a new query"), nil
	}
	// A cursor is a frozen view of the binding's READ SET at mint time — and the
	// write mount's branch (WriteMountBranch) is one term of that fingerprint, so
	// a resume bound to a different branch, a read mount re-pinned to a different
	// branch, or a changed mount set all diverge the fingerprint here. Reject it
	// before any dequeue side effect: resuming against another branch's state
	// would silently leak the wrong deleted/superseded flags. The error is
	// indistinguishable from expiry BY DESIGN (lenses RFC §7.3): a caller must not
	// be able to tell a re-pinned read set — or a branch change — from an expired
	// cursor, or it could probe how a shared name's read mounts changed.
	if sess.ReadSet != federate.ReadSetFingerprint(b) {
		return mcpgo.NewToolResultError("session expired or not found — omit cursor to start a new query"), nil
	}

	// Per-mount store handles, resolved once per resume.
	stores := map[*repos.RepoInstance]mcpStore{b.Write(): sWrite}
	// Non-nil so a fully-unreadable resume still marshals facts:[] (never null).
	page := []factOutput{}

	// A dequeued window can be entirely unreadable — every row pinned at a commit
	// that no longer resolves. Skipping those rows alone would return an empty
	// page while has_more stays true (a spurious empty page). Mirror
	// explainResume: dequeue the next window, bounded at the same attempt count,
	// until a window yields a readable row or the queue drains.
	for range 3 {
		items, err := sWrite.toolSession.DequeuePaths(ctx, cursor, pageSize)
		if err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("page error: %v", err)), nil
		}
		if len(items) == 0 {
			break
		}
		for _, it := range items {
			var st pagedRowState
			if err := json.Unmarshal([]byte(it.State), &st); err != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("page decode error: %v", err)), nil
			}
			id, rel, qualified, perr := federate.ParseQualifiedPath(it.Path)
			if perr != nil {
				return mcpgo.NewToolResultError(fmt.Sprintf("page decode error: %v", perr)), nil
			}
			// Unqualified rows route to the write mount; qualified rows route to the
			// mount their kb:// id names in the current binding.
			rt := repos.ReadTarget{RI: b.Write(), Branch: b.WriteMountBranch()}
			if qualified {
				var ok bool
				if rt, ok = b.ByID(id); !ok {
					// A mount this snapshot referenced is gone from the binding —
					// the frozen view no longer exists (RFC §7.3).
					return mcpgo.NewToolResultError("session expired or not found — omit cursor to start a new query"), nil
				}
			}
			sm, ok := stores[rt.RI]
			if !ok {
				sm = storeIndices(rt.RI)
				stores[rt.RI] = sm
			}
			// Re-read at the frozen commit on the mount's pinned branch; a row
			// unreadable at its pin is skipped, not fatal (as today). The wire path
			// (it.Path) is carried through untouched so qualified rows stay qualified.
			parsed, _, _, okRead := readNode(ctx, sm, rt.Branch, rel, it.CommitHash)
			if !okRead {
				continue
			}
			page = append(page, buildFactOutputFromFact(parsed, it.Path, it.CommitHash, st.Score, st.CommittedAt, includeBody))
		}
		if len(page) > 0 {
			break
		}
		// Whole window unreadable — try the next.
	}

	remaining, err := sWrite.toolSession.QueueSize(ctx, cursor)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("page size error: %v", err)), nil
	}
	resp := queryResponse{Facts: page, HasMore: remaining > 0}
	if remaining > 0 {
		resp.Cursor = &cursor
	} else {
		// Drained: mark completed so it is obvious in the session table; the
		// idle reaper removes the empty row in due course.
		_ = sWrite.toolSession.UpdateToolSession(ctx, cursor, sess.LastCommit, "completed")
	}
	return marshalQueryResponse(resp)
}

// buildFactOutput renders a search result (first-page rows, whose full body is
// already in hand). When includeBody is false the body is truncated to a
// snippet (body_truncated set); otherwise the full body is returned as-is.
func buildFactOutput(r store.SearchResult, includeBody bool) factOutput {
	body, truncated := bodyView(r.Body, includeBody)
	return factOutput{
		File:          r.Path,
		Title:         r.Title,
		Kind:          wireKind(r.Kind),
		Type:          r.Type,
		Score:         r.Score,
		Body:          body,
		BodyTruncated: truncated,
		Commit:        r.CommitHash,
		Frontmatter: frontmatterOutput{
			Domain:         orEmpty(r.Domain),
			Confidence:     r.Confidence,
			Sources:        r.Sources,
			Entities:       orEmpty(r.Entities),
			Refs:           orEmpty(r.Refs),
			EvidenceWeight: r.EvidenceWeight,
			CommittedAt:    r.CommittedAt,
		},
	}
}

// buildFactOutputFromFact renders a resumed-page row from a fact re-read at its
// frozen commit, carrying the search score the snapshot preserved (the only
// field not re-derivable from the fact file itself).
func buildFactOutputFromFact(f fact.Fact, path, commit string, score float64, committedAt int64, includeBody bool) factOutput {
	body, truncated := bodyView(f.Body, includeBody)
	return factOutput{
		File:          path,
		Title:         f.Title,
		Kind:          wireKind(string(f.Kind)),
		Type:          string(f.Type),
		Score:         score,
		Body:          body,
		BodyTruncated: truncated,
		Commit:        commit,
		Frontmatter: frontmatterOutput{
			Domain:         orEmpty(f.Domain),
			Confidence:     f.Confidence,
			Sources:        f.Sources,
			Entities:       orEmpty(f.Entities),
			Refs:           orEmpty(f.Refs),
			EvidenceWeight: f.EvidenceWeight,
			CommittedAt:    committedAt,
		},
	}
}

// wireKind elides the default (epistemic) kind so it is omitted on the wire,
// mirroring fact.Fact.MarshalJSON.
func wireKind(kind string) string {
	if fact.Kind(kind) == fact.DefaultKind {
		return ""
	}
	return kind
}

// bodyView returns the body to emit and whether it was truncated: the full body
// when includeBody, else a bounded snippet.
func bodyView(full string, includeBody bool) (string, bool) {
	if includeBody {
		return full, false
	}
	return snippetBody(full, snippetMaxRunes)
}

// snippetBody truncates body to at most maxRunes runes, cutting back to a
// word/line boundary when one is reasonably close to the end, and appends an
// ellipsis. Returns (snippet, true) when truncation happened.
func snippetBody(body string, maxRunes int) (string, bool) {
	r := []rune(body)
	if len(r) <= maxRunes {
		return body, false
	}
	cut := string(r[:maxRunes])
	if i := strings.LastIndexAny(cut, " \n\t"); i > maxRunes/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " \n\t") + "…", true
}

// marshalQueryResponse encodes the envelope, mapping JSON errors to a tool error.
func marshalQueryResponse(resp queryResponse) (*mcpgo.CallToolResult, error) {
	out, err := json.Marshal(resp)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
	}
	return mcpgo.NewToolResultText(string(out)), nil
}

// orEmpty returns the slice or an empty slice if nil.
func orEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
