package web

import (
	"net/http"
	"strconv"
	"strings"

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
	Search(ri *repos.RepoInstance, emb store.Embedder, branch string, q store.SearchOptions) ([]store.SearchResult, error)
}

// defaultSearchProvider is the production searchProvider that calls through
// ri.WithRead to access the real store, and uses the server embedder for
// query vector generation.
type defaultSearchProvider struct{}

func (defaultSearchProvider) Search(ri *repos.RepoInstance, emb store.Embedder, branch string, q store.SearchOptions) ([]store.SearchResult, error) {
	// Generate query vector if text is provided and an embedder is available.
	if q.Text != "" && emb != nil && len(q.QueryVec) == 0 {
		vec, err := emb.Embed(q.Text)
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
		out, err = svc.Search().Search(contextTODO(), branch, q)
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
	Confidence float64     `json:"confidence,omitempty"`
	Links      hal.LinkMap `json:"_links"`
}

// handleSearch serves GET /repos/{repo}/branches/{branch}/search.
func handleSearch(b hal.URLBuilder, m *repos.Manager, provider searchProvider, emb store.Embedder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := m.Get(repoName)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}

		branch := BranchFromContext(r.Context())
		a := hal.Anchor{Branch: branch}

		qp := r.URL.Query()
		text := qp.Get("q")
		entitiesStr := qp.Get("entities")
		domainStr := qp.Get("domain")
		path := qp.Get("path")
		minConfidenceStr := qp.Get("min_confidence")
		minSimilarityStr := qp.Get("min_similarity")
		limitStr := qp.Get("limit")
		typeStr := qp.Get("type")
		excludeTypeStr := qp.Get("exclude_type")
		kindStr := qp.Get("kind")
		excludeKindStr := qp.Get("exclude_kind")
		epStr := qp.Get("ep")
		domainExact := qp.Get("domain_exact") == "true" || qp.Get("domain_exact") == "1"

		splitCSV := func(s string) []string {
			if s == "" {
				return nil
			}
			var out []string
			for _, part := range strings.Split(s, ",") {
				part = strings.TrimSpace(part)
				if part != "" {
					out = append(out, part)
				}
			}
			return out
		}

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

		limit := 50
		if limitStr != "" {
			v, err := strconv.Atoi(limitStr)
			if err != nil {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
					"invalid limit value", r.URL.Path)
				return
			}
			limit = v
		}
		if limit > 500 {
			limit = 500
		}

		q := store.SearchOptions{
			Text:          text,
			Path:          path,
			Entities:      splitCSV(entitiesStr),
			Domain:        splitCSV(domainStr),
			DomainExact:   domainExact,
			IncludeTypes:  splitCSV(typeStr),
			ExcludeTypes:  splitCSV(excludeTypeStr),
			IncludeKinds:  splitCSV(kindStr),
			ExcludeKinds:  splitCSV(excludeKindStr),
			EpisodeOps:    splitCSV(epStr),
			MinConfidence: minConfidence,
			MinSimilarity: minSimilarity,
			Limit:         limit,
			GraphHops:     1, // default
		}

		log.Debug().Str("q", text).Str("branch", branch).Int("limit", limit).Msg("hal search")

		results, err := provider.Search(ri, emb, branch, q)
		if err != nil {
			log.Debug().Err(err).Msg("hal search failed")
			writeStoreError(w, r, err, "Search failed", branch)
			return
		}

		selfURL := b.Branch(repoName, a) + "/search"
		if r.URL.RawQuery != "" {
			selfURL += "?" + r.URL.RawQuery
		}

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
				Confidence: res.Confidence,
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
