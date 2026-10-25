package kb

import (
	"context"
	"errors"
	"fmt"
	"knomit/internal/edge/web/httpx"
	"net/http"
	"strconv"

	"knomit/internal/engine/store/model"

	"knomit/internal/engine/store/index"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/repos"
	knomitfact "knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
)

// searchRepo runs a search against one repo instance. It calls through
// ri.WithRead to access the real store, and uses the server embedder for
// query vector generation. Both the repo /search handler and every mount of the
// lens /search fan-out call it.
func searchRepo(ctx context.Context, ri *repos.RepoInstance, emb model.Embedder, branch string, q index.SearchOptions) ([]index.SearchResult, error) {
	// Generate query vector if text is provided and an embedder is available.
	if q.Text != "" && emb != nil && len(q.QueryVec) == 0 {
		vec, err := emb.EmbedQuery(ctx, q.Text)
		if err != nil {
			// Text search is vector-only: an unembeddable query is a failure, not
			// an empty result.
			log.Error().Err(err).Msg("search: embed query failed")
			return nil, fmt.Errorf("search: embed query: %w", err)
		}
		if len(vec) == 0 {
			// An empty vector with a nil error is as unusable as an error:
			// leaving QueryVec empty would make the store embed the same text
			// again (a second inference) and fail on its own.
			log.Error().Msg("search: embedder returned an empty query vector")
			return nil, errors.New("search: embed query: embedder returned an empty vector")
		}
		q.QueryVec = vec
	}

	var (
		out []index.SearchResult
		err error
	)
	if aerr := ri.WithRead(func(svc *store.Service) {
		out, err = svc.FactQuery().Search(ctx, branch, q)
	}); aerr != nil {
		return out, aerr
	}
	return out, err
}

// searchResultItem is one item in the search results collection.
// Body is intentionally absent — collection items omit body.
type searchResultItem struct {
	Path       string         `json:"path"`
	Title      string         `json:"title"`
	Score      float64        `json:"score"`
	Kind       string         `json:"kind,omitempty"` // omitted when epistemic (the default)
	Type       string         `json:"type,omitempty"`
	Domain     []string       `json:"domain,omitempty"`
	Entities   []string       `json:"entities,omitempty"`
	Motifs     []string       `json:"motifs,omitempty"`
	Confidence float64        `json:"confidence,omitempty"`
	Expires    string         `json:"expires,omitempty"`
	Expired    bool           `json:"expired,omitempty"`
	Context    map[string]any `json:"context,omitempty"`
	Links      hal.LinkMap    `json:"_links"`
}

// searchView is the search collection plus an optional notice. Notice is
// omitted unless the caller's min_similarity removed every candidate.
type searchView struct {
	hal.CollectionView[searchResultItem]
	Notice string `json:"notice,omitempty"`
}

// handleSearch serves GET /repos/{repo}/branches/{branch}/search.
func handleSearch(b hal.URLBuilder, emb model.Embedder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		branch := httpx.BranchFromContext(r.Context())
		a := hal.Anchor{Branch: branch}

		qp := r.URL.Query()
		text := qp.Get("q")
		entitiesStr := qp.Get("entities")
		domainStr := qp.Get("domain")
		path := qp.Get("path")
		minConfidenceStr := qp.Get("min_confidence")
		minSimilarityStr := qp.Get("min_similarity")
		typeStr := qp.Get("type")
		excludeTypeStr := qp.Get("exclude_type")
		kindStr := qp.Get("kind")
		excludeKindStr := qp.Get("exclude_kind")
		originStr := qp.Get("origin")
		epStr := qp.Get("ep")
		domainExact := qp.Get("domain_exact") == "true" || qp.Get("domain_exact") == "1"

		var minConfidence float64
		if minConfidenceStr != "" {
			v, err := strconv.ParseFloat(minConfidenceStr, 64)
			if err != nil {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
					"invalid min_confidence value", r.URL.Path)
				return
			}
			minConfidence = v
		}

		var minSimilarity float64
		if minSimilarityStr != "" {
			v, err := strconv.ParseFloat(minSimilarityStr, 64)
			if err != nil {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
					"invalid min_similarity value", r.URL.Path)
				return
			}
			if err := index.ValidateMinSimilarity(v); err != nil {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", err.Error(), r.URL.Path)
				return
			}
			minSimilarity = v
		}

		limit, ok := limitParam(w, r)
		if !ok {
			return
		}
		motifs, motifMatch, ok := motifParams(w, r)
		if !ok {
			return
		}

		q := index.SearchOptions{
			Text:           text,
			Path:           path,
			Entities:       splitCSV(entitiesStr),
			Domain:         splitCSV(domainStr),
			DomainExact:    domainExact,
			IncludeTypes:   splitCSV(typeStr),
			ExcludeTypes:   splitCSV(excludeTypeStr),
			IncludeKinds:   splitCSV(kindStr),
			ExcludeKinds:   splitCSV(excludeKindStr),
			IncludeOrigins: splitCSV(originStr),
			EpisodeOps:     splitCSV(epStr),
			Motifs:         motifs,
			MotifMatch:     motifMatch,
			MinConfidence:  minConfidence,
			MinSimilarity:  minSimilarity,
			Limit:          limit,
		}
		now := timeNow()
		if !applyExpiryParams(w, r, &q, now) {
			return
		}
		if !applyContextParams(w, r, &q) {
			return
		}

		// since_fork narrows the search to what this experiment changed. It is
		// honoured HERE as well as on the facts collection because the Library
		// switches between the two endpoints on its sort order alone: a filter
		// that survived "recent" and silently vanished under "relevance" would
		// widen the result set without saying so.
		var empty bool
		if qp.Get("since_fork") == "true" || qp.Get("since_fork") == "1" {
			paths, ok := experimentChangedPaths(w, r, ri, branch)
			if !ok {
				return
			}
			if len(paths) == 0 {
				empty = true
			}
			q.PathsIn = paths
		}

		log.Debug().Str("q", text).Str("branch", branch).Int("limit", limit).Msg("hal search")

		var (
			results []index.SearchResult
			err     error
		)
		var diag index.SearchDiag
		q.Diag = &diag
		if !empty {
			results, err = searchRepo(r.Context(), ri, emb, branch, q)
		}
		log.Info().
			Bool("text", text != "").
			Float64("min_similarity", minSimilarity).
			Float64("cutoff", diag.Cutoff).
			Str("repo", repoName).
			Str("branch", branch).
			Int("vec_hits", diag.VecHits).
			Int("after_cutoff", diag.Candidates).
			Int("returned", len(results)).
			Msg("hal search")
		if err != nil {
			log.Debug().Err(err).Msg("hal search failed")
			httpx.WriteStoreError(w, r, err, "Search failed", branch)
			return
		}

		selfURL := httpx.SelfWithQuery(b.Branch(repoName, a)+"/search", r)

		items := make([]searchResultItem, 0, len(results))
		for _, res := range results {
			// Mirror fact.Fact.MarshalJSON: elide Kind when it equals the
			// default (epistemic) so the field is omitted on the wire.
			kind := res.Kind
			if knomitfact.Kind(kind) == knomitfact.DefaultKind {
				kind = ""
			}
			item := searchResultItem{
				Path:       res.Path,
				Title:      res.Title,
				Score:      res.Score,
				Kind:       kind,
				Type:       res.Type,
				Domain:     res.Domain,
				Entities:   res.Entities,
				Motifs:     res.Motifs,
				Confidence: res.Confidence,
				Expires:    res.Expires,
				Context:    res.Context,
				Expired:    expiredAt(res.Expires, now),
				Links:      hal.LinkMap{"self": {Href: b.Fact(repoName, a, res.Path)}},
			}
			items = append(items, item)
		}

		view := searchView{CollectionView: hal.CollectionView[searchResultItem]{
			Count: len(items),
			Links: hal.LinkMap{"self": {Href: selfURL}},
			Embedded: map[string][]searchResultItem{
				"results": items,
			},
		}}
		// An empty text result caused by the caller's own cutoff says so, rather
		// than looking like "nothing matched".
		view.Notice = index.CutoffNotice(text != "", minSimilarity, diag)
		hal.WriteHAL(w, http.StatusOK, view)
	}
}
