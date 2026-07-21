package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

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

		limit, ok := limitParam(w, r)
		if !ok {
			return
		}
		offset, ok := offsetParam(w, r)
		if !ok {
			return
		}

		// `query=` is the text filter (accept `q=` as the repo-collection alias).
		// Present it as SearchOptions.Text; RecentFacts applies it per mount.
		text := qp.Get("query")
		if text == "" {
			text = qp.Get("q")
		}

		// Numeric filters mirror the lens /search handler, including its 400s.
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

		// `topic` is shorthand for an ontology-root subdirectory filter
		// (`?topic=invariants` → `path=kb/invariants/`), mirroring the repo facts
		// collection (handlers_facts_collection.go); an explicit `?path=` always
		// wins. factsTopicPrefix is shared with that twin.
		path := qp.Get("path")
		if path == "" {
			if topic := strings.TrimSpace(qp.Get("topic")); topic != "" {
				path = factsTopicPrefix + topic + "/"
			}
		}

		// Ontology-aware fan-out target selection — the same seam MCP queryRecent
		// uses. A kb://-qualified path restricts to a single mount (with the
		// filter made repo-relative); an unqualified path applies per mount,
		// skipping mounts whose ontology lacks the topic.
		targets, err := federate.ReadTargetsFor(b, path)
		if err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
				err.Error(), r.URL.Path)
			return
		}

		// Optional repeatable `repo=<mount name>` narrows the fan-out (422 on an
		// unknown mount name).
		targets, ok = narrowByRepo(w, r, b, targets, qp["repo"])
		if !ok {
			return
		}

		// Shared filter set across mounts (per-mount Path is set below). This
		// mirrors the lens /search handler and the repo facts collection so a lens
		// answers a filtered browse EXACTLY like the repos it federates — every
		// selecting filter is forwarded, not just path+text. Forwarding only
		// path/text silently drops filters the caller sent (wrong data, not merely
		// wrong order), diverging from both the repo and MCP twins.
		base := store.SearchOptions{
			Text: text,
			// `entity` (singular) is the canonical name advertised by the HAL
			// template and matches the data-model column; `entities` (plural) is a
			// back-compat alias. Merge both, exactly as the repo facts collection
			// does — forwarding only the plural silently drops a caller's canonical
			// `entity=` filter (wrong data, not merely wrong order).
			Entities:       append(splitCSV(qp.Get("entity")), splitCSV(qp.Get("entities"))...),
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
			Offset:         0,
		}

		// Fan out to every selected mount at its Binding-resolved branch. Any
		// mount error fails the whole request — a lens must never silently shrink
		// its read set (RFC §9.1).
		lists := make([][]store.RecentFactEntry, len(targets))
		for i, t := range targets {
			q := base
			q.Path = t.Path
			entries, _, err := provider.RecentFacts(r.Context(), t.RT.RI, t.RT.Branch, q)
			if err != nil {
				writeStoreError(w, r, err, "Failed to list facts", t.RT.Branch)
				return
			}
			lists[i] = entries
		}

		// Dedupe by repo-relative path (write mount wins, then binding order).
		winner := federate.WriteFirstWinners(targets, b.Write(), lists,
			func(e store.RecentFactEntry) string { return e.Path })

		// Order the union honouring whichever key each mount ordered by — exactly
		// as MCP queryRecent does. RecentFacts returns each mount's list
		// committed_at-DESC for a text-LESS query, but RELEVANCE-ranked WITH a text
		// query (the store's recentFactsSearch behaviour). Commit timestamps are
		// comparable across mounts (k-way timestamp merge), but per-mount relevance
		// ranks are NOT (RFC §7.1), so a text query fuses by reciprocal rank.
		// FuseRRF's N=1 identity also preserves lens-of-one byte-identity with the
		// repo facts endpoint, which a global timestamp re-sort would silently break.
		var order []federate.MountRef
		if text != "" {
			order = federate.FuseRRF(lensListLens(lists))
		} else {
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
			order = federate.MergeRecent(stamps, totalEntries)
		}

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

// lensFactView is the single-fact wire body served through a lens: the repo
// FactView (path/title/body/refs/_links) exactly as the repo single-fact
// handler produces it, plus the lens-level source {repo,id,branch}. Because
// FactView has a custom MarshalJSON (it hoists _links), the source cannot be
// added by struct embedding — a promoted MarshalJSON would drop the extra
// field — so the two objects are spliced here, preserving the repo body
// verbatim (RFC: the lens single-fact body IS the repo body + source).
type lensFactView struct {
	FactView
	Source lensFactSource
}

func (v lensFactView) MarshalJSON() ([]byte, error) {
	inner, err := v.FactView.MarshalJSON()
	if err != nil {
		return nil, err
	}
	src, err := json.Marshal(v.Source)
	if err != nil {
		return nil, err
	}
	// inner is a non-empty JSON object; splice `,"source":<src>` before its
	// closing brace to keep the repo body's field order intact.
	out := make([]byte, 0, len(inner)+len(src)+len(`,"source":`))
	out = append(out, inner[:len(inner)-1]...)
	out = append(out, `,"source":`...)
	out = append(out, src...)
	out = append(out, '}')
	return out, nil
}

// handleHALLensFact serves GET /lenses/{lens}/facts/{path...} — a single fact
// read through a lens. It is the lens twin of handleHALFact: the SAME repo
// single-fact body plus a source {repo,id,branch} block.
//
// Addressing (RFC §6.2): a bare kb/… path reads from the WRITE repo at its
// Binding-resolved branch — there is NO dedupe scan, bare means the write repo,
// period, even when a read mount shadows the same repo-relative path. A
// kb://<id12>/kb/… path (URL-encoded by clients; decoded then split with
// federate.ParseQualifiedPath) reads from that mount at its resolved branch.
//
// An unmounted id12 returns 404 with the SAME "Fact not found" shape as a
// genuinely-missing fact — a caller must not be able to tell an unknown mount
// from an absent fact (no mount-topology leak). A missing/retracted fact is a
// 404 in parity with the repo handler; a real backend error surfaces as 500.
func handleHALLensFact(b hal.URLBuilder, reader FactReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bind := repos.BindingFromContext(r.Context())

		// chi routes the wildcard on the ESCAPED path, so a kb://-qualified path
		// arrives percent-encoded; PathUnescape is idempotent for a bare path
		// (no % escapes) and recovers the qualified form. On a decode error, fall
		// back to the raw capture rather than 500 — ParseQualifiedPath will judge
		// it.
		raw := chi.URLParam(r, "*")
		wire := raw
		if dec, derr := url.PathUnescape(raw); derr == nil {
			wire = dec
		}
		if wire == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Missing fact path",
				"fact path is required", r.URL.Path)
			return
		}

		// Resolve the target mount + repo-relative path from the addressing.
		id, rel, qualified, err := federate.ParseQualifiedPath(wire)
		var (
			ri     *repos.RepoInstance
			branch string
		)
		if qualified {
			// A malformed kb:// path or an unmounted id is indistinguishable, on
			// the wire, from a fact that simply isn't there — same 404.
			if err != nil {
				lensFactNotFound(w, r, wire)
				return
			}
			rt, ok := bind.ByID(id)
			if !ok {
				lensFactNotFound(w, r, wire)
				return
			}
			ri, branch = rt.RI, rt.Branch
		} else {
			// Bare path: the write repo at its read-mount branch. No fan-out.
			// rel is already the bare path from ParseQualifiedPath.
			ri, branch = bind.Write(), bind.WriteMountBranch()
		}

		a := hal.Anchor{Branch: branch}
		f, head, err := reader.Read(r.Context(), ri, a, rel, false)
		if err != nil {
			if errors.Is(err, errFactNotFound) {
				lensFactNotFound(w, r, wire)
				return
			}
			writeStoreError(w, r, err, "Failed to read fact", branch)
			return
		}

		resolver := readerRefResolver{ctx: r.Context(), reader: reader, ri: ri, branch: branch, commit: ""}
		view := BuildFactView(b, ri.Name(), a, head, f, resolver)
		// The top-level `path` echoes the canonical wire address (RFC §6.2): bare
		// for the write repo, kb://<id12>/… for a read mount — so a client can
		// round-trip it into another lens request and land on the SAME fact. The
		// `_links` stay repo-scoped (repo-relative), pointing at the owning mount's
		// own endpoints. lensWirePath renders the write repo bare (ByID includes
		// the write repo, but rt.RI == b.Write() there → bare).
		view.Path = lensWirePath(bind, repos.ReadTarget{RI: ri, Branch: branch}, rel)
		hal.WriteHAL(w, http.StatusOK, lensFactView{
			FactView: view,
			Source: lensFactSource{
				Repo:   ri.Name(),
				ID:     federate.ID12(ri.ID()),
				Branch: branch,
			},
		})
	}
}

// lensFactNotFound writes the uniform lens single-fact 404. It is used for every
// not-found case — unmounted id, malformed kb:// path, missing/retracted fact —
// so the four are byte-identical for a given requested path and none reveals
// mount topology. The detail echoes only the path the client asked for; it
// names no branch or mount (the repo handler names both, but doing so here
// would leak whether an id resolved to a mount).
func lensFactNotFound(w http.ResponseWriter, r *http.Request, wirePath string) {
	hal.WriteProblem(w, http.StatusNotFound, "Fact not found",
		`no fact at path "`+wirePath+`"`, r.URL.Path)
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

// narrowByRepo applies the optional repeatable `repo=<mount name>` filter shared
// by every lens union-read handler: it restricts the fan-out targets to the
// named mounts. An unknown name is a well-formed request naming a nonexistent
// mount → 422 (the write repo is itself a mount via Binding self-mount, so it is
// selectable by name). When sel is empty the targets pass through unchanged.
//
// On the 422 path it writes the problem response and returns ok=false; the
// caller must return immediately without writing again.
func narrowByRepo(w http.ResponseWriter, r *http.Request, b *repos.Binding, targets []federate.Target, sel []string) ([]federate.Target, bool) {
	if len(sel) == 0 {
		return targets, true
	}
	known := make(map[string]bool, len(b.Reads()))
	for _, rt := range b.Reads() {
		known[rt.RI.Name()] = true
	}
	want := make(map[string]bool, len(sel))
	for _, name := range sel {
		if !known[name] {
			hal.WriteProblem(w, http.StatusUnprocessableEntity, "Unknown repo",
				`no mount named "`+name+`" in lens "`+b.Name()+`"`, r.URL.Path)
			return nil, false
		}
		want[name] = true
	}
	kept := make([]federate.Target, 0, len(targets))
	for _, t := range targets {
		if want[t.RT.RI.Name()] {
			kept = append(kept, t)
		}
	}
	return kept, true
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
		limit, ok := limitParam(w, r)
		if !ok {
			return
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

		// Optional repeatable `repo=<mount name>` narrows the fan-out (422 on an
		// unknown mount name).
		targets, ok = narrowByRepo(w, r, b, targets, qp["repo"])
		if !ok {
			return
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
			res, err := provider.Search(r.Context(), t.RT.RI, emb, t.RT.Branch, q)
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

		// Dedupe by repo-relative path (write mount wins, then binding order).
		winner := federate.WriteFirstWinners(targets, b.Write(), lists,
			func(res store.SearchResult) string { return res.Path })

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

// lensCompletionsResponse is the union completions envelope. Flat (values),
// consistent with the other lens union-read collections (which drop the repo
// handler's _links.self — a lens read collection has no per-branch anchor).
type lensCompletionsResponse struct {
	Values []string `json:"values"`
}

// handleHALLensCompletions serves GET /lenses/{lens}/completions — the union of
// per-mount completion values across a lens's write repo + N read mounts, plus a
// lens-only category=repo that lists the lens's mount NAMES in binding order. It
// is the lens twin of handleHALCompletions (handlers_completions.go): the SAME
// category set, the SAME completionsProvider seam, the SAME case-insensitive
// prefix matching, the SAME per-category value ordering, and — critically — the
// SAME unknown-category error, since a bad category is forwarded to the store
// per mount and its error flows through writeStoreError exactly as the repo
// handler's does (500 problem+json).
//
// For the existing categories it fans out over federate.ReadTargetsFor and
// unions each mount's values, de-duplicated across mounts with first-seen order
// preserved (so the repo handler's per-category ordering survives for the first
// mount that supplies a given value). The lens-only category=repo is served
// entirely from the Binding without touching the store.
func handleHALLensCompletions(provider completionsProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b := repos.BindingFromContext(r.Context())
		qp := r.URL.Query()
		category := qp.Get("category")
		prefix := qp.Get("prefix")

		// category=repo is lens-only: the lens's mount names in binding order
		// (write repo first), filtered case-insensitively by prefix to mirror the
		// store's LIKE behaviour for the value categories.
		if category == "repo" {
			values := filterByPrefixFold(lensMountNames(b), prefix)
			hal.WriteHAL(w, http.StatusOK, lensCompletionsResponse{Values: values})
			return
		}

		// Fan out over every mount (no path filter — completions are not
		// path-scoped). Reads() always includes the write repo's self-mount, so
		// targets is non-empty and an unknown category is guaranteed to reach a
		// store call and surface its error, matching the repo handler.
		targets, err := federate.ReadTargetsFor(b, "")
		if err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
				err.Error(), r.URL.Path)
			return
		}

		seen := make(map[string]bool)
		values := []string{}
		for _, t := range targets {
			// Each mount fetches its own top-20 (mirroring the repo handler's store
			// limit); the union below dedupes across mounts.
			vals, err := provider.Completions(r.Context(), t.RT.RI, t.RT.Branch, category, prefix, 20)
			if err != nil {
				writeStoreError(w, r, err, "Failed to load completions", t.RT.Branch)
				return
			}
			for _, v := range vals {
				if !seen[v] {
					seen[v] = true
					values = append(values, v)
				}
			}
		}
		hal.WriteHAL(w, http.StatusOK, lensCompletionsResponse{Values: values})
	}
}

// lensMountNames returns the lens's distinct mount names in binding order: the
// write repo first, then the read mounts in Reads() order (sorted by repo name).
// Reads() carries the write repo as a self-mount, so it is de-duplicated. This
// is the value list for the lens-only category=repo completion.
func lensMountNames(b *repos.Binding) []string {
	seen := make(map[string]bool)
	out := make([]string, 0, len(b.Reads()))
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	add(b.Write().Name())
	for _, rt := range b.Reads() {
		add(rt.RI.Name())
	}
	return out
}

// filterByPrefixFold keeps values whose lower-cased form starts with the
// lower-cased prefix, mirroring the store's case-insensitive LIKE prefix% for
// value completions. An empty prefix passes everything through unchanged.
func filterByPrefixFold(values []string, prefix string) []string {
	if prefix == "" {
		return values
	}
	lp := strings.ToLower(prefix)
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.HasPrefix(strings.ToLower(v), lp) {
			out = append(out, v)
		}
	}
	return out
}

// lensListLens returns each list's length, the shape federate.FuseRRF consumes.
func lensListLens[T any](lists [][]T) []int {
	ns := make([]int, len(lists))
	for i, l := range lists {
		ns[i] = len(l)
	}
	return ns
}
