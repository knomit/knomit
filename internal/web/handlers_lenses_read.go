package web

import (
	"net/http"
	"strconv"
	"strings"

	knomitfact "knomit/internal/fact"
	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// maxLensFactCandidates bounds the per-mount fetch depth for a lens union facts
// collection: each mount contributes at most this many rows to the merged,
// deduped union. It mirrors the MCP query snapshot-depth model (RFC §7.1) —
// paging over the union walks WITHIN this materialised set.
const maxLensFactCandidates = 500

// lensFactSource identifies which mount a union row came from.
type lensFactSource struct {
	Repo   string `json:"repo"`
	ID     string `json:"id"`
	Branch string `json:"branch"`
}

// lensFactItem is one row of the lens union facts collection.
type lensFactItem struct {
	Path        string         `json:"path"`
	Title       string         `json:"title"`
	Kind        string         `json:"kind,omitempty"` // omitted when epistemic (the default)
	Type        string         `json:"type,omitempty"`
	CommittedAt int64          `json:"committed_at,omitempty"`
	Operation   string         `json:"operation,omitempty"`
	Score       float64        `json:"score"`
	Source      lensFactSource `json:"source"`
}

// lensFactsResponse is the union facts collection envelope. It is flat
// (facts + total), mirroring the MCP query response shape rather than the HAL
// collection envelope — later lens read tasks (search, completions, single
// fact) depend on this shape verbatim.
type lensFactsResponse struct {
	Facts []lensFactItem `json:"facts"`
	Total int            `json:"total"`
}

// handleHALLensFacts serves GET /lenses/{lens}/facts — the recency-ordered
// union of a lens's write repo + N read mounts. It is the REST twin of the MCP
// queryRecent fan-out (internal/mcp/query.go): select targets via
// federate.ReadTargetsFor, fetch each mount's RecentFacts at its pinned branch,
// dedupe by repo-relative path (the write mount's copy wins), merge by
// committed_at across mounts, and qualify read-mount paths (kb://<id12>/…).
func handleHALLensFacts(provider factsCollectionProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b := repos.BindingFromContext(r.Context())
		qp := r.URL.Query()

		limit := 50
		if v := qp.Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		if limit > 500 {
			limit = 500
		}
		offset := 0
		if v := qp.Get("offset"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				offset = n
			}
		}

		// `query=` is the text filter (accept `q=` as the repo-collection alias).
		// Present it as SearchOptions.Text; RecentFacts applies it per mount.
		text := qp.Get("query")
		if text == "" {
			text = qp.Get("q")
		}

		// Ontology-aware fan-out target selection — the same seam MCP queryRecent
		// uses. A kb://-qualified path restricts to a single mount (with the
		// filter made repo-relative); an unqualified path applies per mount,
		// skipping mounts whose ontology lacks the topic.
		targets, err := federate.ReadTargetsFor(b, qp.Get("path"))
		if err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
				err.Error(), r.URL.Path)
			return
		}

		// Optional repeatable `repo=<mount name>` narrows the fan-out. An unknown
		// name is a well-formed request naming a nonexistent mount → 422.
		if sel := qp["repo"]; len(sel) > 0 {
			known := make(map[string]bool, len(b.Reads()))
			for _, rt := range b.Reads() {
				known[rt.RI.Name()] = true
			}
			want := make(map[string]bool, len(sel))
			for _, name := range sel {
				if !known[name] {
					hal.WriteProblem(w, http.StatusUnprocessableEntity, "Unknown repo",
						`no mount named "`+name+`" in lens "`+b.Name()+`"`, r.URL.Path)
					return
				}
				want[name] = true
			}
			kept := make([]federate.Target, 0, len(targets))
			for _, t := range targets {
				if want[t.RT.RI.Name()] {
					kept = append(kept, t)
				}
			}
			targets = kept
		}

		// Fan out to every selected mount at its Binding-resolved branch. Any
		// mount error fails the whole request — a lens must never silently shrink
		// its read set (RFC §9.1).
		lists := make([][]store.RecentFactEntry, len(targets))
		for i, t := range targets {
			entries, _, err := provider.RecentFacts(t.RT.RI, t.RT.Branch, store.SearchOptions{
				Path:   t.Path,
				Text:   text,
				Limit:  maxLensFactCandidates,
				Offset: 0,
			})
			if err != nil {
				writeStoreError(w, r, err, "Failed to list facts", t.RT.Branch)
				return
			}
			lists[i] = entries
		}

		// Dedupe by repo-relative path. The WRITE mount's copy always wins — its
		// facts are the lens's editable, canonical rows — so it is recorded first;
		// remaining collisions resolve in binding order. (Reads() is sorted by
		// repo name, so the write mount is not positionally "first" in general;
		// prioritise it explicitly.) winner maps a rel path to its target index.
		winner := make(map[string]int)
		record := func(isWrite bool) {
			for i, t := range targets {
				if (t.RT.RI == b.Write()) != isWrite {
					continue
				}
				for _, e := range lists[i] {
					if _, seen := winner[e.Path]; !seen {
						winner[e.Path] = i
					}
				}
			}
		}
		record(true)  // write mount first
		record(false) // then read mounts in binding order

		// Merge by committed_at across mounts (k-way timestamp merge). Each
		// mount's list is committed_at-DESC (the text-less RecentFacts order);
		// MergeRecent re-sorts globally, so the union is recency-ordered, not
		// mount-grouped.
		totalEntries := 0
		for _, l := range lists {
			totalEntries += len(l)
		}
		stamps := make([][]int64, len(targets))
		for i, list := range lists {
			stamps[i] = make([]int64, len(list))
			for j, e := range list {
				stamps[i][j] = e.CommittedAt
			}
		}
		order := federate.MergeRecent(stamps, totalEntries)

		// Emit deduped rows in recency order: keep a row only when its mount is
		// the winner for that rel path (each rel path is unique within a mount,
		// so the winner's copy appears exactly once in the merged order).
		rows := make([]lensFactItem, 0, len(winner))
		for _, ref := range order {
			e := lists[ref.Mount][ref.Rank]
			if winner[e.Path] != ref.Mount {
				continue
			}
			t := targets[ref.Mount]
			// Mirror fact.Fact.MarshalJSON: elide Kind when it equals the default
			// (epistemic) so the field is omitted on the wire.
			kind := e.Kind
			if knomitfact.Kind(kind) == knomitfact.DefaultKind {
				kind = ""
			}
			rows = append(rows, lensFactItem{
				Path:        lensWirePath(b, t.RT, e.Path),
				Title:       e.Title,
				Kind:        kind,
				Type:        e.Type,
				CommittedAt: e.CommittedAt,
				Operation:   e.Operation,
				Score:       e.Score,
				Source: lensFactSource{
					Repo:   t.RT.RI.Name(),
					ID:     federate.ID12(t.RT.RI.ID()),
					Branch: t.RT.Branch,
				},
			})
		}

		// total is the post-dedupe union size; offset/limit page WITHIN it.
		total := len(rows)
		if offset > len(rows) {
			offset = len(rows)
		}
		end := offset + limit
		if end > len(rows) {
			end = len(rows)
		}
		page := rows[offset:end]
		if page == nil {
			page = []lensFactItem{}
		}

		hal.WriteHAL(w, http.StatusOK, lensFactsResponse{Facts: page, Total: total})
	}
}

// lensWirePath renders a union row's path as addressed on the wire: bare for the
// binding's write repo, kb://<id12>/… for any read mount (RFC §6.2 uniformity),
// mirroring internal/mcp.wirePath.
func lensWirePath(b *repos.Binding, rt repos.ReadTarget, rel string) string {
	if rt.RI == b.Write() {
		return rel
	}
	return federate.QualifyPath(federate.ID12(rt.RI.ID()), rel)
}

// lensSearchItem is one row of the lens union search collection. It carries the
// repo SearchResult's exposed fields (mirroring the repo /search item) plus the
// canonical qualified path and the row's source mount (same source shape as the
// lens facts collection). No shadow metadata and no _links (flat envelope).
type lensSearchItem struct {
	Path       string         `json:"path"`
	Title      string         `json:"title"`
	Score      float64        `json:"score"`
	Kind       string         `json:"kind,omitempty"` // omitted when epistemic (the default)
	Type       string         `json:"type,omitempty"`
	Domain     []string       `json:"domain,omitempty"`
	Entities   []string       `json:"entities,omitempty"`
	Confidence float64        `json:"confidence,omitempty"`
	Source     lensFactSource `json:"source"`
}

// lensSearchResponse is the union search collection envelope. Flat
// (results + total), consistent with the lens facts collection and accepted by
// the web client's search reader (data.results fallback).
type lensSearchResponse struct {
	Results []lensSearchItem `json:"results"`
	Total   int              `json:"total"`
}

// handleHALLensSearch serves GET /lenses/{lens}/search — the RRF-fused union
// relevance search of a lens's write repo + N read mounts. It is the REST twin
// of the MCP knomit_query relevance path (internal/mcp/query.go queryFirstCall)
// and MUST produce the SAME fused ordering for the same lens+query: fan out over
// federate.ReadTargetsFor, collect each mount's ranked SearchResult list, and
// order the union with federate.FuseRRF (per-mount rank, not native score).
//
// After fusion, rows are deduped by repo-relative path with the WRITE mount's
// copy winning (its facts are the lens's editable, canonical rows), exactly as
// the facts collection dedupes. A shadowed copy is dropped even when it ranks
// higher in the fused order than the winner. Read-mount paths are qualified
// (kb://<id12>/…); each row carries its source {repo,id,branch}.
func handleHALLensSearch(provider searchProvider, emb store.Embedder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b := repos.BindingFromContext(r.Context())
		qp := r.URL.Query()

		splitCSV := func(s string) []string {
			if s == "" {
				return nil
			}
			var out []string
			for _, part := range strings.Split(s, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
			return out
		}

		// Numeric params mirror the repo /search handler, including its 400s.
		var minConfidence float64
		if v := qp.Get("min_confidence"); v != "" {
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
					"invalid min_confidence value", r.URL.Path)
				return
			}
			minConfidence = n
		}
		var minSimilarity float64
		if v := qp.Get("min_similarity"); v != "" {
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
					"invalid min_similarity value", r.URL.Path)
				return
			}
			minSimilarity = n
		}
		limit := 50
		if v := qp.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
					"invalid limit value", r.URL.Path)
				return
			}
			limit = n
		}
		if limit > 500 {
			limit = 500
		}
		if limit < 0 {
			limit = 0
		}

		// Ontology-aware fan-out target selection — the same seam MCP queryFirstCall
		// uses. A kb://-qualified path restricts to one mount (filter made
		// repo-relative); an unqualified path applies per mount.
		targets, err := federate.ReadTargetsFor(b, qp.Get("path"))
		if err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
				err.Error(), r.URL.Path)
			return
		}

		// Optional repeatable `repo=<mount name>` narrows the fan-out. An unknown
		// name is a well-formed request naming a nonexistent mount → 422.
		if sel := qp["repo"]; len(sel) > 0 {
			known := make(map[string]bool, len(b.Reads()))
			for _, rt := range b.Reads() {
				known[rt.RI.Name()] = true
			}
			want := make(map[string]bool, len(sel))
			for _, name := range sel {
				if !known[name] {
					hal.WriteProblem(w, http.StatusUnprocessableEntity, "Unknown repo",
						`no mount named "`+name+`" in lens "`+b.Name()+`"`, r.URL.Path)
					return
				}
				want[name] = true
			}
			kept := make([]federate.Target, 0, len(targets))
			for _, t := range targets {
				if want[t.RT.RI.Name()] {
					kept = append(kept, t)
				}
			}
			targets = kept
		}

		// Shared query across mounts (per-mount Path is set below). Depth is the
		// per-mount candidate cap; the fused union is truncated to `limit` last.
		base := store.SearchOptions{
			Text:           qp.Get("q"),
			Entities:       splitCSV(qp.Get("entities")),
			Domain:         splitCSV(qp.Get("domain")),
			DomainExact:    qp.Get("domain_exact") == "true" || qp.Get("domain_exact") == "1",
			IncludeTypes:   splitCSV(qp.Get("type")),
			ExcludeTypes:   splitCSV(qp.Get("exclude_type")),
			IncludeKinds:   splitCSV(qp.Get("kind")),
			ExcludeKinds:   splitCSV(qp.Get("exclude_kind")),
			IncludeOrigins: splitCSV(qp.Get("origin")),
			EpisodeOps:     splitCSV(qp.Get("ep")),
			MinConfidence:  minConfidence,
			MinSimilarity:  minSimilarity,
			Limit:          maxLensFactCandidates,
		}

		// Fan out to every selected mount at its Binding-resolved branch. Any mount
		// error fails the whole request — a lens must never silently shrink its read
		// set (RFC §9.1). The embedder (possibly nil when embeddings are disabled)
		// is forwarded exactly as the repo /search handler forwards it.
		lists := make([][]store.SearchResult, len(targets))
		for i, t := range targets {
			q := base
			q.Path = t.Path
			res, err := provider.Search(t.RT.RI, emb, t.RT.Branch, q)
			if err != nil {
				writeStoreError(w, r, err, "Search failed", t.RT.Branch)
				return
			}
			lists[i] = res
		}

		// Fuse the per-mount ranked lists by reciprocal rank fusion — the SAME
		// ordering MCP knomit_query produces (RFC §7.1). RRF orders by per-mount
		// rank, never by native score, so a fused order is not a naive interleave.
		order := federate.FuseRRF(lensListLens(lists))

		// Dedupe by repo-relative path. The WRITE mount's copy always wins (its
		// facts are the lens's editable, canonical rows), so it is recorded first;
		// remaining collisions resolve in binding order. winner maps a rel path to
		// its target index.
		winner := make(map[string]int)
		record := func(isWrite bool) {
			for i, t := range targets {
				if (t.RT.RI == b.Write()) != isWrite {
					continue
				}
				for _, res := range lists[i] {
					if _, seen := winner[res.Path]; !seen {
						winner[res.Path] = i
					}
				}
			}
		}
		record(true)  // write mount first
		record(false) // then read mounts in binding order

		// Emit deduped rows in fused order: keep a row only when its mount is the
		// winner for that rel path. A shadowed copy is dropped even when it ranks
		// higher than the winner (each rel path is unique within a mount, so the
		// winner's copy appears exactly once in the fused order).
		rows := make([]lensSearchItem, 0, len(winner))
		for _, ref := range order {
			res := lists[ref.Mount][ref.Rank]
			if winner[res.Path] != ref.Mount {
				continue
			}
			t := targets[ref.Mount]
			// Mirror fact.Fact.MarshalJSON: elide Kind when it equals the default
			// (epistemic) so the field is omitted on the wire.
			kind := res.Kind
			if knomitfact.Kind(kind) == knomitfact.DefaultKind {
				kind = ""
			}
			rows = append(rows, lensSearchItem{
				Path:       lensWirePath(b, t.RT, res.Path),
				Title:      res.Title,
				Score:      res.Score,
				Kind:       kind,
				Type:       res.Type,
				Domain:     res.Domain,
				Entities:   res.Entities,
				Confidence: res.Confidence,
				Source: lensFactSource{
					Repo:   t.RT.RI.Name(),
					ID:     federate.ID12(t.RT.RI.ID()),
					Branch: t.RT.Branch,
				},
			})
		}

		// total is the full deduped union size; `limit` caps the returned page.
		total := len(rows)
		if limit < len(rows) {
			rows = rows[:limit]
		}
		if rows == nil {
			rows = []lensSearchItem{}
		}

		hal.WriteHAL(w, http.StatusOK, lensSearchResponse{Results: rows, Total: total})
	}
}

// lensListLens returns each list's length, the shape federate.FuseRRF consumes.
func lensListLens[T any](lists [][]T) []int {
	ns := make([]int, len(lists))
	for i, l := range lists {
		ns[i] = len(l)
	}
	return ns
}
