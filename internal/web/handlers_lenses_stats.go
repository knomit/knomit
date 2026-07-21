package web

import (
	"net/http"

	"knomit/internal/federate"
	"knomit/internal/repos"
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
// On dedup: total/domains/entities are EXACT sums, not approximations. Fact
// paths are kb/<topic>/<category>/<server-generated-uuid>.md and mounts are
// distinct repos (replica mounts are rejected at lens create), so cross-mount
// path collisions cannot occur and the facts-union's winning-path dedup is a
// no-op for aggregates. Aggregate StatsResults carry no per-fact paths to
// dedup on anyway; a truly-deduped count would require fetching the fact sets
// (up to 500 rows/mount). So we sum — cheap, and exact.
type lensStatsResponse struct {
	Total         int             `json:"total"`
	RepoCount     int             `json:"repo_count"`
	LastCommit    string          `json:"last_commit"`
	AvgConfidence float64         `json:"avg_confidence"`
	Domains       map[string]int  `json:"domains"`
	Entities      map[string]int  `json:"entities"`
	Repos         []lensRepoStats `json:"repos"`
}

// handleHALLensStats serves GET /lenses/{lens}/stats — the union stats +
// activity roll-up of a lens's write repo + N read mounts, with a per-repo
// breakdown. It fans out over federate.ReadTargetsFor exactly like the other
// lens reads and reuses the repo statsProvider/activityProvider per mount —
// no new store logic. Any mount error fails the whole request (RFC §9.1 — a
// lens never silently shrinks its read set).
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

		resp := lensStatsResponse{
			Domains:  map[string]int{},
			Entities: map[string]int{},
			Repos:    make([]lensRepoStats, 0, len(targets)),
		}
		var confWeight float64 // Σ(avg_i · total_i); divided by Σ(total_i) below
		for _, t := range targets {
			st, err := statsP.Stats(t.RT.RI, t.RT.Branch, t.Path)
			if err != nil {
				writeStoreError(w, r, err, "Failed to load stats", t.RT.Branch)
				return
			}
			act, err := actP.Activity(t.RT.RI, t.RT.Branch, t.Path)
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
			resp.Total += st.Total
			confWeight += st.AvgConfidence * float64(st.Total)
			for d, n := range domains {
				resp.Domains[d] += n
			}
			for e, n := range entities {
				resp.Entities[e] += n
			}
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
		resp.RepoCount = len(targets)
		if resp.Total > 0 {
			resp.AvgConfidence = round2(confWeight / float64(resp.Total))
		}
		hal.WriteHAL(w, http.StatusOK, resp)
	}
}
