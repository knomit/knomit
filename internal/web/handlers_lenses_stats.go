package web

import (
	"net/http"
	"sort"

	"knomit/internal/federate"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// round2 rounds to 2 decimal places, byte-identical to the store's per-mount
// AvgConfidence rounding (store/index.go), so the union mean is emitted at the
// same precision as the repo /stats endpoint.
func round2(x float64) float64 { return float64(int(x*100+0.5)) / 100 }

// lensRepoStats is one per-mount row of the lens union stats: the mount's
// identity (id12/name/source/branch/is_write) plus its own aggregate stats
// and commit activity.
type lensRepoStats struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Source        string         `json:"source"`
	Branch        string         `json:"branch"`
	IsWrite       bool           `json:"is_write"`
	Total         int            `json:"total"`
	AvgConfidence float64        `json:"avg_confidence"`
	Domains       map[string]int `json:"domains"`
	Entities      map[string]int `json:"entities"`
	LastCommit    string         `json:"last_commit"`
	Changes7d     int            `json:"changes_7d"`
	Changes30d    int            `json:"changes_30d"`
	Changes90d    int            `json:"changes_90d"`
}

// lensStatsResponse is the union stats envelope. Flat (no _links), consistent
// with the other lens union reads.
//
// On dedup: total/domains/entities/types are per-mount SUMS, not deduped counts.
// Mounts are distinct repos (replica mounts are rejected at lens create), but
// distinct is not disjoint: a re-rooted fork mounted alongside its upstream
// has a different root-commit ID yet shares server-generated fact UUIDs, so
// the same kb/<topic>/<category>/<uuid>.md path can exist on two mounts and
// is counted once per mount here — the exact collision the facts/search/
// topics/highlights unions dedup via federate.WriteFirstWinners. We accept
// the over-count on the SUMS deliberately: aggregate StatsResults
// (store/index.go) carry no per-fact paths to dedup on, and rebuilding the
// histograms from RecentFacts would be capped at the maxLensFactCandidates
// (500) per-mount snapshot — trading a rare fork over-count for a systematic
// under-count on any repo with >500 facts (and RecentFactEntry carries no
// per-fact confidence, so avg_confidence could not be rebuilt at all). The
// per-repo Repos breakdown is exact per mount; consumers needing
// collision-free numbers should read it. Tracked with the other
// lens-browsing accepted gaps (kb/gotchas/lens/browsing-ui-accepted-gaps).
// Highlights DO carry paths, so unlike the sums they dedupe exactly — see
// handleHALLensStats.
type lensStatsResponse struct {
	Total         int               `json:"total"`
	RepoCount     int               `json:"repo_count"`
	LastCommit    string            `json:"last_commit"`
	AvgConfidence float64           `json:"avg_confidence"`
	Domains       map[string]int    `json:"domains"`
	Entities      map[string]int    `json:"entities"`
	Types         map[string]int    `json:"types"`
	Highlights    []store.Highlight `json:"highlights"`
	DefaultAxis   string            `json:"default_axis"`
	Repos         []lensRepoStats   `json:"repos"`
}

// handleHALLensStats serves GET /lenses/{lens}/stats — the union stats +
// activity roll-up of a lens's write repo + N read mounts, with a per-repo
// breakdown. It fans out over federate.ReadTargetsFor exactly like the other
// lens reads and reuses the repo statsProvider/activityProvider per mount —
// no new store logic. Any mount error fails the whole request (RFC §9.1 — a
// lens never silently shrinks its read set).
//
// highlights is the deduped (federate.WriteFirstWinners, write mount wins)
// GLOBAL top-N across mounts, not a per-mount list: each mount already
// returns its own top store.MaxHighlights, so merging then truncating to the
// same N is correct (see sortHighlights) — PROVIDED every mount was cut by
// the same axis; when the caller left axis unresolved and mounts disagree on
// their own default, a corrective second fan-out pins them all to one axis
// first (see the comment above that call).
//
// default_axis is POOLED, not an AND-of-mounts vote: the per-mount top-layer
// and observation out-degree counters (store.StatsResult.TopLayerFacts etc.)
// are summed across every mount, and store.AxisFromSeparation is applied ONCE
// to the pooled totals — the same rule store.Stats applies repo-side. This
// matters in practice, not just in theory: an AND rule means one mount voting
// confidence vetoes impact for the WHOLE lens, and a mount with zero facts
// (an empty repo) is a trivial way to trigger that veto even though it
// contributes nothing to either side of the ratio. Pooling instead means a
// lens spanning a distilled and an authored mount resolves however the
// COMBINED evidence points, and an empty mount is a no-op (it sums (0,0) into
// both sides, changing nothing).
func handleHALLensStats(statsP statsProvider, actP activityProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b := repos.BindingFromContext(r.Context())
		qp := r.URL.Query()

		// Ontology-aware fan-out target selection — the same seam the facts/
		// search twins use. A kb://-qualified path restricts to one mount; an
		// unqualified path applies per mount.
		targets, err := federate.ReadTargetsFor(b, qp.Get("path"))
		if err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
				err.Error(), r.URL.Path)
			return
		}

		// Optional repeatable `repo=<mount name>` narrows the fan-out (422 on
		// an unknown mount name) — the shared lens union-read filter.
		targets, ok := narrowByRepo(w, r, b, targets, qp["repo"])
		if !ok {
			return
		}

		axis := qp.Get("axis")

		resp := lensStatsResponse{
			Domains:    map[string]int{},
			Entities:   map[string]int{},
			Types:      map[string]int{},
			Highlights: []store.Highlight{},
			Repos:      make([]lensRepoStats, 0, len(targets)),
		}
		// highlights[i] is mount targets[i]'s own top-N, indexed in parallel
		// with targets so federate.WriteFirstWinners can attribute a winning
		// path back to its mount.
		highlights := make([][]store.Highlight, len(targets))
		// mountAxes[i] is mount targets[i]'s OWN default_axis recommendation —
		// what its per-mount store.Stats call used to cut its top-N when the
		// caller left axis unresolved. Used below only to decide whether the
		// corrective re-fan-out is needed, never to derive resp.DefaultAxis
		// itself (that's the pooled counters, not a vote).
		mountAxes := make([]string, len(targets))
		// fallback[i] reports that mount targets[i] had NO eligible highlights of
		// its own and answered with excluded types instead (store's per-scope
		// fallback — see factQuery.highlights). Tracked because that decision is
		// only correct for the scope that made it, and this handler is building a
		// bigger one; see the suppression pass below the fan-out.
		fallback := make([]bool, len(targets))
		var topFacts, topEdges, obsFacts, obsEdges int // pooled AxisFromSeparation inputs
		var confWeight float64                         // Σ(avg_i · total_i); divided by Σ(total_i) below
		for i, t := range targets {
			st, err := statsP.Stats(r.Context(), t.RT.RI, t.RT.Branch, t.Path, axis)
			if err != nil {
				writeStoreError(w, r, err, "Failed to load stats", t.RT.Branch)
				return
			}
			act, err := actP.Activity(r.Context(), t.RT.RI, t.RT.Branch, t.Path)
			if err != nil {
				writeStoreError(w, r, err, "Failed to load activity", t.RT.Branch)
				return
			}
			// Mirror handleHALStats: JSON must carry {} for empty maps, never
			// null — an empty StatsResult has nil Domains/Entities.
			domains := st.Domains
			if domains == nil {
				domains = map[string]int{}
			}
			entities := st.Entities
			if entities == nil {
				entities = map[string]int{}
			}
			types := st.Types
			if types == nil {
				types = map[string]int{}
			}
			resp.Total += st.Total
			confWeight += st.AvgConfidence * float64(st.Total)
			for d, n := range domains {
				resp.Domains[d] += n
			}
			for e, n := range entities {
				resp.Entities[e] += n
			}
			for ty, n := range types {
				resp.Types[ty] += n
			}
			highlights[i] = st.Highlights
			fallback[i] = st.HighlightsFallback
			mountAxes[i] = st.DefaultAxis
			topFacts += st.TopLayerFacts
			topEdges += st.TopLayerEdges
			obsFacts += st.ObservationFacts
			obsEdges += st.ObservationEdges
			// last_commit is the MAX timestamp across mounts. The store emits a
			// fixed-format UTC RFC3339 stamp, so lexicographic comparison IS
			// chronological comparison; "" loses to any real stamp.
			if act.LastCommit > resp.LastCommit {
				resp.LastCommit = act.LastCommit
			}
			resp.Repos = append(resp.Repos, lensRepoStats{
				ID:            federate.ID12(t.RT.RI.ID()),
				Name:          t.RT.RI.Name(),
				Source:        t.RT.Source,
				Branch:        t.RT.Branch,
				IsWrite:       t.RT.RI == b.Write(),
				Total:         st.Total,
				AvgConfidence: st.AvgConfidence,
				Domains:       domains,
				Entities:      entities,
				LastCommit:    act.LastCommit,
				Changes7d:     act.Changes7d,
				Changes30d:    act.Changes30d,
				Changes90d:    act.Changes90d,
			})
		}

		// The pooled verdict: sum every mount's raw top-layer/observation
		// counters and apply store.AxisFromSeparation ONCE — the single
		// definition of the separation rule, shared with the repo-scoped
		// store.Stats. NOT an AND-of-mounts vote (see the doc comment above
		// this handler) — a mount with zero facts sums (0,0) into both sides
		// and cannot veto the others.
		resp.DefaultAxis = store.AxisFromSeparation(topFacts, topEdges, obsFacts, obsEdges)
		rankAxis := store.NormalizeAxis(axis, resp.DefaultAxis)

		// store.Stats cuts each mount's SQL LIMIT to
		// NormalizeAxis(<requested axis>, <that mount's own default>)
		// (store/index.go). When the caller gave no usable axis (empty or
		// unrecognized — store.NormalizeAxis(axis, "") == "" captures both)
		// AND the resolved POOLED axis differs from what some mount used for
		// its own cut, that per-mount cut used the WRONG axis relative to the
		// union: e.g. a distilled mount (own default impact) was limited to
		// its top-N BY IMPACT while the pooled union resolves to confidence,
		// so the distilled mount's true top-N-by-confidence facts outside its
		// impact top-N were never fetched, and merging would silently
		// under-report. Re-fan-out once, pinning every mount to the SAME
		// rankAxis, so every mount's top-N is comparable before the merge.
		// This is NOT redundant with the loop above: that loop lets each
		// mount fall back to its OWN default; this pins them all to ONE
		// shared axis. Skipped whenever the caller passed an explicit,
		// recognized axis (already forwarded identically to every mount
		// above) or every mount's own default already equals the resolved
		// rankAxis (re-fetching would return the same rows). A single flat
		// pass over mountAxes — no recursion, no repeated fan-out — so this
		// cannot loop or double-count regardless of how many mounts disagree.
		mountsDisagreeWithResolved := false
		for _, ma := range mountAxes {
			if ma != resp.DefaultAxis {
				mountsDisagreeWithResolved = true
				break
			}
		}
		if store.NormalizeAxis(axis, "") == "" && mountsDisagreeWithResolved {
			for i, t := range targets {
				st, err := statsP.Stats(r.Context(), t.RT.RI, t.RT.Branch, t.Path, rankAxis)
				if err != nil {
					writeStoreError(w, r, err, "Failed to load stats", t.RT.Branch)
					return
				}
				highlights[i] = st.Highlights
				fallback[i] = st.HighlightsFallback
			}
		}

		// Apply the exclusion's own rule at UNION scope.
		//
		// store's fallback answers "does THIS scope have a distilled layer to
		// bury?" — and a mount cannot see the union it is about to be merged
		// into. A lens over core (1,186 observations against 128 syntheses) plus
		// a small mount that is pure observation would take that mount's
		// observations into the merge, where a distilled layer very much does
		// exist. Under AxisConfidence, which orders on confidence alone with no
		// impact term, a confident observation then outranks the syntheses the
		// panel exists to surface — the exact burying the exclusion was written
		// to prevent, arriving through the door built for the case where it
		// cannot happen.
		//
		// So the fallback lists are dropped whenever ANY mount contributed a real
		// one. If none did, the union as a whole is the pure-observation scope
		// and they all stay — which is the single-repo behaviour, applied to the
		// scope the reader is actually looking at.
		anyEligible := false
		for i := range highlights {
			if len(highlights[i]) > 0 && !fallback[i] {
				anyEligible = true
				break
			}
		}
		if anyEligible {
			for i := range highlights {
				if fallback[i] {
					highlights[i] = nil
				}
			}
		}

		// Dedupe by path via the shared lens union-read helper (write mount
		// wins, then binding order) — the same collision the facts/search/
		// topics unions handle: a re-rooted fork mounted beside its upstream
		// shares server-generated fact UUIDs, so the same kb/<...>/<uuid>.md
		// path can arrive from two mounts. The aggregate sums above accept
		// that over-count deliberately (no per-fact paths to dedupe on);
		// highlights carry paths, so here it would be a visible duplicate
		// row. federate.WriteFirstWinners is the ONE definition every union
		// surface must use — hand-rolling a second one here would let this
		// endpoint's dedupe silently diverge from the others'.
		winner := federate.WriteFirstWinners(targets, b.Write(), highlights,
			func(h store.Highlight) string { return h.Path })
		deduped := make([]store.Highlight, 0, len(targets)*store.MaxHighlights)
		for i, hs := range highlights {
			for _, h := range hs {
				if winner[h.Path] == i {
					// Qualify HERE, and not one line earlier: the store hands
					// back mount-RELATIVE paths, and a bare path on the wire
					// resolves against the write repo — so every highlight owned
					// by a read mount 404'd when opened. /facts and /search have
					// always qualified; this was the one lens read that did not.
					//
					// After the dedup, never before. Dedup exists because one
					// path can live on two mounts (a re-rooted fork shares fact
					// UUIDs); keying on qualified paths would give the copies
					// different keys, so both would survive and the write mount
					// would stop winning.
					h.Path = lensWirePath(b, targets[i].RT, h.Path)
					deduped = append(deduped, h)
				}
			}
		}

		// Rank the deduped merge by rankAxis. Each mount's list is now cut
		// by that same axis (either from the start, or via the re-fetch
		// above), so a merge-and-sort is a correct global top-N.
		sortHighlights(deduped, rankAxis)
		if len(deduped) > store.MaxHighlights {
			deduped = deduped[:store.MaxHighlights]
		}
		resp.Highlights = deduped

		resp.RepoCount = len(targets)
		if resp.Total > 0 {
			resp.AvgConfidence = round2(confWeight / float64(resp.Total))
		}
		hal.WriteHAL(w, http.StatusOK, resp)
	}
}

// sortHighlights orders the merged union list by the chosen axis. Each mount
// already returned its own top-N (store.MaxHighlights), so taking N from the
// merge is a correct global top-N: a mount's (N+1)th can never displace the
// global Nth.
func sortHighlights(hs []store.Highlight, axis string) {
	sort.SliceStable(hs, func(i, j int) bool {
		switch axis {
		case store.AxisConfidence:
			if hs[i].Confidence != hs[j].Confidence {
				return hs[i].Confidence > hs[j].Confidence
			}
			return hs[i].CommittedAt > hs[j].CommittedAt
		case store.AxisRecent:
			if hs[i].CommittedAt != hs[j].CommittedAt {
				return hs[i].CommittedAt > hs[j].CommittedAt
			}
			return hs[i].Confidence > hs[j].Confidence
		}
		if hs[i].Impact != hs[j].Impact {
			return hs[i].Impact > hs[j].Impact
		}
		return hs[i].Confidence > hs[j].Confidence
	})
}
