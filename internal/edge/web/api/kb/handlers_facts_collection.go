package kb

import (
	"context"
	"knomit/internal/edge/web/httpx"
	"net/http"
	"strconv"
	"strings"

	"knomit/internal/engine/store/model"

	"knomit/internal/engine/store/index"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	knomitfact "knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
)

// factsTopicPrefix is the ontology root under which `?topic=X` translates to
// `path=kb/X/`. Kept in sync with internal/domain/fact.NewFact's lowercased root.
const factsTopicPrefix = "kb/"

// recentFacts reads one repo's recency-ordered (or, with Text, relevance
// ordered) fact page and its total. A store that cannot be reached is
// WithRead's error, returned as-is (httpx.WriteStoreError answers it 503).
// The repo facts collection, the motif cluster carriers and every mount of the
// lens facts fan-out read through it.
func recentFacts(
	ctx context.Context,
	ri *repos.RepoInstance, branch string, opts index.SearchOptions,
) ([]index.RecentFactEntry, int, error) {
	var (
		out   []index.RecentFactEntry
		total int
		err   error
	)
	if aerr := ri.WithRead(func(svc *store.Service) {
		out, total, err = svc.FactQuery().RecentFacts(ctx, branch, opts)
	}); aerr != nil {
		return out, total, aerr
	}
	return out, total, err
}

// recentFactItem is one item in the facts collection response.
type recentFactItem struct {
	Path        string         `json:"path"`
	Title       string         `json:"title"`
	Kind        string         `json:"kind,omitempty"` // omitted when epistemic (the default)
	Type        string         `json:"type,omitempty"`
	Domain      []string       `json:"domain,omitempty"`
	Entities    []string       `json:"entities,omitempty"`
	Motifs      []string       `json:"motifs,omitempty"`
	CommittedAt int64          `json:"committed_at,omitempty"`
	Operation   string         `json:"operation,omitempty"`
	Expires     string         `json:"expires,omitempty"`
	Expired     bool           `json:"expired,omitempty"`
	Context     map[string]any `json:"context,omitempty"`
	Links       hal.LinkMap    `json:"_links"`
}

// handleHALFactsCollection serves GET /repos/{repo}/branches/{branch}/facts
// (exact path, no wildcard).
func handleHALFactsCollection(b hal.URLBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		branch := httpx.BranchFromContext(r.Context())
		a := hal.Anchor{Branch: branch}
		qp := r.URL.Query()

		limit, ok := listLimitParam(w, r)
		if !ok {
			return
		}
		offset, ok := offsetParam(w, r)
		if !ok {
			return
		}
		motifs, motifMatch, ok := motifParams(w, r)
		if !ok {
			return
		}

		// `entity` (singular) is the canonical name advertised by the HAL
		// template and matches the data model column name. `entities` (plural)
		// is accepted as a back-compat alias. CSV values merge.
		entities := append(splitCSV(qp.Get("entity")), splitCSV(qp.Get("entities"))...)

		// `topic` is shorthand for filtering by ontology-root subdirectory:
		// `?topic=invariants` → `path=kb/invariants/`. An explicit `?path=`
		// always wins so callers can override the convention.
		path := qp.Get("path")
		if path == "" {
			if topic := strings.TrimSpace(qp.Get("topic")); topic != "" {
				path = factsTopicPrefix + topic + "/"
			}
		}

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

		opts := index.SearchOptions{
			Path:           path,
			Text:           qp.Get("q"),
			Limit:          limit,
			Offset:         offset,
			MinConfidence:  minConfidence,
			Domain:         splitCSV(qp.Get("domain")),
			Entities:       entities,
			IncludeTypes:   splitCSV(qp.Get("type")),
			ExcludeTypes:   splitCSV(qp.Get("exclude_type")),
			IncludeKinds:   splitCSV(qp.Get("kind")),
			ExcludeKinds:   splitCSV(qp.Get("exclude_kind")),
			IncludeOrigins: splitCSV(qp.Get("origin")),
			EpisodeOps:     splitCSV(qp.Get("ep")),
			Motifs:         motifs,
			MotifMatch:     motifMatch,
		}
		now := timeNow()
		if !applyExpiryParams(w, r, &opts, now) {
			return
		}
		if !applyContextParams(w, r, &opts) {
			return
		}

		// `since_fork` narrows the list to what THIS EXPERIMENT changed.
		//
		// It is a git diff from the recorded fork commit, never a timestamp
		// comparison: syncing an experiment merges the agent branch's later
		// commits into it, so "committed after the fork" would report the
		// parent's work as the experiment's — the exact reading the filter
		// exists to prevent.
		var empty bool
		if qp.Get("since_fork") == "true" || qp.Get("since_fork") == "1" {
			paths, ok := experimentChangedPaths(w, r, ri, branch)
			if !ok {
				return
			}
			// An experiment that has changed nothing yet must render as an
			// EMPTY list, not as every fact on the branch, so the store is
			// not asked at all rather than asked with an inert filter.
			if len(paths) == 0 {
				empty = true
			}
			opts.PathsIn = paths
		}

		var (
			entries []index.RecentFactEntry
			total   int
			err     error
		)
		if !empty {
			entries, total, err = recentFacts(r.Context(), ri, branch, opts)
			if err != nil {
				httpx.WriteStoreError(w, r, err, "Failed to list facts", branch)
				return
			}
		}
		if entries == nil {
			entries = []index.RecentFactEntry{}
		}

		factsBase := b.Branch(repoName, a) + "/facts"
		selfURL := httpx.SelfWithQuery(factsBase, r)

		links := hal.LinkMap{"self": {Href: selfURL}}
		// Add next/prev pagination links when applicable.
		if offset+len(entries) < total {
			nextQ := r.URL.Query()
			nextQ.Set("offset", strconv.Itoa(offset+limit))
			links["next"] = hal.Link{Href: factsBase + "?" + nextQ.Encode()}
		}
		if offset > 0 {
			prevOffset := offset - limit
			if prevOffset < 0 {
				prevOffset = 0
			}
			prevQ := r.URL.Query()
			prevQ.Set("offset", strconv.Itoa(prevOffset))
			links["prev"] = hal.Link{Href: factsBase + "?" + prevQ.Encode()}
		}

		items := make([]recentFactItem, 0, len(entries))
		for _, e := range entries {
			// Mirror fact.Fact.MarshalJSON: elide Kind when it equals the
			// default (epistemic) so the field is omitted on the wire.
			kind := e.Kind
			if knomitfact.Kind(kind) == knomitfact.DefaultKind {
				kind = ""
			}
			items = append(items, recentFactItem{
				Path:        e.Path,
				Title:       e.Title,
				Kind:        kind,
				Type:        e.Type,
				Domain:      e.Domain,
				Entities:    e.Entities,
				Motifs:      e.Motifs,
				CommittedAt: e.CommittedAt,
				Operation:   e.Operation,
				Expires:     e.Expires,
				Context:     e.Context,
				Expired:     expiredAt(e.Expires, now),
				Links:       hal.LinkMap{"self": {Href: b.Fact(repoName, a, e.Path)}},
			})
		}

		view := hal.CollectionView[recentFactItem]{
			Count: total,
			Links: links,
			Embedded: map[string][]recentFactItem{
				"facts": items,
			},
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// experimentChangedPaths returns the fact paths this experiment branch has
// added, modified or deleted since its fork commit. It writes a problem
// document and returns ok=false when the branch is not an experiment this
// instance knows — asking for "since the fork" off an experiment is a client
// error, not an empty answer, because an empty answer reads as "nothing has
// changed" and would be believed.
//
// Deleted paths are included in the set even though a retracted fact is no
// longer on the branch to be listed: the set is what CHANGED, and filtering
// deletes out here would make the caller's filter quietly differ from the
// diff it names.
func experimentChangedPaths(w http.ResponseWriter, r *http.Request, ri *repos.RepoInstance, branch string) ([]string, bool) {
	name, isExp := model.ExperimentNameOf(branch)
	if !isExp {
		hal.WriteProblem(w, http.StatusBadRequest, "Not an experiment",
			"since_fork narrows a list to what an experiment changed since it forked; branch "+
				strconv.Quote(branch)+" is not an experiment",
			r.URL.Path)
		return nil, false
	}

	var (
		exp   model.Experiment
		found bool
		paths []string
		err   error
	)
	if aerr := ri.WithRead(func(svc *store.Service) {
		exp, found, err = svc.GetExperiment(r.Context(), name)
		if err != nil || !found {
			return
		}
		var added, modified, deleted []string
		added, modified, deleted, err = svc.Facts().DiffFiles(r.Context(), branch, exp.ForkCommit)
		paths = append(append(append(paths, added...), modified...), deleted...)
	}); aerr != nil {
		httpx.WriteStoreUnavailable(w, r)
		return nil, false
	}
	if err != nil {
		hal.WriteProblem(w, http.StatusInternalServerError, "Failed to diff the experiment",
			err.Error(), r.URL.Path)
		return nil, false
	}
	if !found {
		hal.WriteProblem(w, http.StatusNotFound, "Experiment not found",
			"branch "+strconv.Quote(branch)+" has no experiment record, so there is no fork point to compare against",
			r.URL.Path)
		return nil, false
	}
	return paths, true
}
