package web

import (
	"context"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	knomitfact "knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// searchProvider is the narrow interface the search HAL handler depends on.
// Production wires it to defaultSearchProvider; tests inject stubs.
type searchProvider interface {
	Search(ctx context.Context, ri *repos.RepoInstance, emb store.Embedder, branch string, q store.SearchOptions) ([]store.SearchResult, error)
}

// defaultSearchProvider is the production searchProvider that calls through
// ri.WithRead to access the real store, and uses the server embedder for
// query vector generation.
type defaultSearchProvider struct{}

func (defaultSearchProvider) Search(ctx context.Context, ri *repos.RepoInstance, emb store.Embedder, branch string, q store.SearchOptions) ([]store.SearchResult, error) {
	// Generate query vector if text is provided and an embedder is available.
	if q.Text != "" && emb != nil && len(q.QueryVec) == 0 {
		vec, err := emb.EmbedQuery(ctx, q.Text)
		if err != nil {
			log.Warn().Err(err).Msg("search: embed query failed")
		} else {
			q.QueryVec = vec
		}
	}

	var (
		out []store.SearchResult
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.FactQuery().Search(ctx, branch, q)
	})
	return out, err
}

// searchResultItem is one item in the search results collection.
// Body is intentionally absent — collection items omit body.
type searchResultItem struct {
	Path       string      `json:"path"`
	Title      string      `json:"title"`
	Score      float64     `json:"score"`
	Kind       string      `json:"kind,omitempty"` // omitted when epistemic (the default)
	Type       string      `json:"type,omitempty"`
	Domain     []string    `json:"domain,omitempty"`
	Entities   []string    `json:"entities,omitempty"`
	Motifs     []string    `json:"motifs,omitempty"`
	Confidence float64     `json:"confidence,omitempty"`
	Expires    string      `json:"expires,omitempty"`
	Expired    bool        `json:"expired,omitempty"`
	Links      hal.LinkMap `json:"_links"`
}

// handleSearch serves GET /repos/{repo}/branches/{branch}/search.
func handleSearch(b hal.URLBuilder, provider searchProvider, emb store.Embedder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		branch := BranchFromContext(r.Context())
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

		q := store.SearchOptions{
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
			results []store.SearchResult
			err     error
		)
		if !empty {
			results, err = provider.Search(r.Context(), ri, emb, branch, q)
		}
		if err != nil {
			log.Debug().Err(err).Msg("hal search failed")
			writeStoreError(w, r, err, "Search failed", branch)
			return
		}

		selfURL := selfWithQuery(b.Branch(repoName, a)+"/search", r)

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
				Expired:    expiredAt(res.Expires, now),
				Links:      hal.LinkMap{"self": {Href: b.Fact(repoName, a, res.Path)}},
			}
			items = append(items, item)
		}

		view := hal.CollectionView[searchResultItem]{
			Count: len(items),
			Links: hal.LinkMap{"self": {Href: selfURL}},
			Embedded: map[string][]searchResultItem{
				"results": items,
			},
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}
