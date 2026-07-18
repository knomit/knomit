package web

import (
	"net/http"
	"strconv"

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
