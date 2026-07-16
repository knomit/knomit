package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
	"strings"
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

	// maxCandidates caps the total result set materialised into a snapshot for
	// one query (mirrors the REST search handler's cap). Paging walks within
	// this set; it is NOT the page size.
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

		ri := repos.RepoFromContext(ctx)
		s := storeIndices(ri)
		agentBranch := boundBranch(ctx, ri)

		includeBody := req.GetBool("include_body", false)
		pageSize := pageSizeFor(req.GetInt("limit", 0), includeBody)

		sort := req.GetString("sort", sortRelevance)
		if sort != sortRelevance && sort != sortRecent {
			return mcpgo.NewToolResultError("sort must be \"relevance\" or \"recent\""), nil
		}

		// Resume path: a cursor pages a frozen snapshot; filters and sort are ignored.
		if cursor := req.GetString("cursor", ""); cursor != "" {
			return queryResume(ctx, s, agentBranch, cursor, pageSize, includeBody)
		}

		if sort == sortRecent {
			return queryRecent(ctx, s, agentBranch, req, pageSize, includeBody)
		}
		return queryFirstCall(ctx, s, agentBranch, req, pageSize, includeBody)
	}
}

// queryRecent serves the recency-ordered browse (sort=recent). It draws the
// ordered candidate set from RecentFacts (already filtered + committed_at
// DESC), snapshots it into a session, and serves the first page through the
// shared resume path so body hydration and pagination match relevance mode.
func queryRecent(ctx context.Context, s mcpStore, agentBranch string, req mcpgo.CallToolRequest, pageSize int, includeBody bool) (*mcpgo.CallToolResult, error) {
	q := parseQueryFilters(req)
	q.Limit = maxCandidates

	entries, _, err := s.search.RecentFacts(ctx, agentBranch, q)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("recent error: %v", err)), nil
	}
	if len(entries) == 0 {
		return marshalQueryResponse(queryResponse{Facts: []factOutput{}, Cursor: nil, HasMore: false})
	}

	sess, err := s.toolSession.CreateToolSession(ctx, "query", agentBranch, "")
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("session error: %v", err)), nil
	}
	items := make([]store.QueueItem, len(entries))
	for i, e := range entries {
		state, mErr := json.Marshal(pagedRowState{Score: e.Score, CommittedAt: e.CommittedAt})
		if mErr != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("snapshot error: %v", mErr)), nil
		}
		items[i] = store.QueueItem{
			Path:       e.Path,
			CommitHash: e.CommitHash,
			SortKey:    i,
			State:      string(state),
		}
	}
	if err := s.toolSession.EnqueuePaths(ctx, sess.ID, items); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("snapshot enqueue error: %v", err)), nil
	}
	// Serve the first page (and compute cursor/has_more) through the shared
	// resume path — every recent row is hydrated from its pinned commit.
	return queryResume(ctx, s, agentBranch, sess.ID, pageSize, includeBody)
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

// queryFirstCall runs the search, returns the first page, and (only when the
// result set exceeds one page) creates a session snapshot for the remainder.
func queryFirstCall(ctx context.Context, s mcpStore, agentBranch string, req mcpgo.CallToolRequest, pageSize int, includeBody bool) (*mcpgo.CallToolResult, error) {
	q := parseQueryFilters(req)
	if !hasAnyFilter(q) {
		return mcpgo.NewToolResultError("at least one of text, entities, domain, applies_to, path, type, origin, or min_confidence is required"), nil
	}
	q.Limit = maxCandidates

	results, err := s.search.Search(ctx, agentBranch, q)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("search error: %v", err)), nil
	}

	if len(results) == 0 {
		return marshalQueryResponse(queryResponse{Facts: []factOutput{}, Cursor: nil, HasMore: false})
	}

	// Fast path: the whole result set fits in one page — no session needed.
	// Bodies are already in hand (search returns full bodies), so include_body
	// needs no re-fetch here.
	if len(results) <= pageSize {
		page := make([]factOutput, len(results))
		for i, r := range results {
			page[i] = buildFactOutput(r, includeBody)
		}
		return marshalQueryResponse(queryResponse{Facts: page, Cursor: nil, HasMore: false})
	}

	// Snapshot the remainder ([pageSize:]) into a session; the first page is
	// rendered directly from results (full bodies available, no re-fetch).
	sess, err := s.toolSession.CreateToolSession(ctx, "query", agentBranch, "")
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("session error: %v", err)), nil
	}
	items := make([]store.QueueItem, 0, len(results)-pageSize)
	for i := pageSize; i < len(results); i++ {
		// Snapshot only what a resumed page can't re-derive: the rank score.
		// path+commit pin the version; title/body/frontmatter are re-read from
		// the fact on resume, so the snapshot carries no heavy body text.
		state, mErr := json.Marshal(pagedRowState{Score: results[i].Score, CommittedAt: results[i].CommittedAt})
		if mErr != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("snapshot error: %v", mErr)), nil
		}
		items = append(items, store.QueueItem{
			Path:       results[i].Path,
			CommitHash: results[i].CommitHash,
			SortKey:    i,
			State:      string(state),
		})
	}
	if err := s.toolSession.EnqueuePaths(ctx, sess.ID, items); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("snapshot enqueue error: %v", err)), nil
	}

	page := make([]factOutput, pageSize)
	for i := range pageSize {
		page[i] = buildFactOutput(results[i], includeBody)
	}
	cursor := sess.ID
	return marshalQueryResponse(queryResponse{Facts: page, Cursor: &cursor, HasMore: true})
}

// queryResume serves the next page from a frozen session snapshot.
func queryResume(ctx context.Context, s mcpStore, agentBranch, cursor string, pageSize int, includeBody bool) (*mcpgo.CallToolResult, error) {
	sess, err := s.toolSession.GetToolSession(ctx, cursor)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("session error: %v", err)), nil
	}
	if sess == nil {
		return mcpgo.NewToolResultError("session expired or not found — omit cursor to start a new query"), nil
	}

	items, err := s.toolSession.DequeuePaths(ctx, cursor, pageSize)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("page error: %v", err)), nil
	}

	page := make([]factOutput, 0, len(items))
	for _, it := range items {
		var st pagedRowState
		if err := json.Unmarshal([]byte(it.State), &st); err != nil {
			return mcpgo.NewToolResultError(fmt.Sprintf("page decode error: %v", err)), nil
		}
		// Re-read the fact at its frozen commit (version-pinned, no drift) and
		// render snippet or full body per include_body. If it can't be read at
		// that commit, skip the row rather than failing the whole page.
		parsed, _, _, ok := readNode(ctx, s, agentBranch, it.Path, it.CommitHash)
		if !ok {
			continue
		}
		page = append(page, buildFactOutputFromFact(parsed, it.Path, it.CommitHash, st.Score, st.CommittedAt, includeBody))
	}

	remaining, err := s.toolSession.QueueSize(ctx, cursor)
	if err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("page size error: %v", err)), nil
	}
	resp := queryResponse{Facts: page, HasMore: remaining > 0}
	if remaining > 0 {
		resp.Cursor = &cursor
	} else {
		// Drained: mark completed so it is obvious in the session table; the
		// idle reaper removes the empty row in due course.
		_ = s.toolSession.UpdateToolSession(ctx, cursor, sess.LastCommit, "completed")
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
