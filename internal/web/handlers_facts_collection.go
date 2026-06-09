package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	knomitfact "knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// factsTopicPrefix is the ontology root under which `?topic=X` translates to
// `path=kb/X/`. Kept in sync with internal/fact.NewFact's lowercased root.
const factsTopicPrefix = "kb/"

// factsCollectionProvider is the narrow interface the facts collection handler depends on.
type factsCollectionProvider interface {
	RecentFacts(ri *repos.RepoInstance, branch string, opts store.SearchOptions) ([]store.RecentFactEntry, int, error)
}

// defaultFactsCollectionProvider implements factsCollectionProvider using the store.
type defaultFactsCollectionProvider struct{}

func (defaultFactsCollectionProvider) RecentFacts(
	ri *repos.RepoInstance, branch string, opts store.SearchOptions,
) ([]store.RecentFactEntry, int, error) {
	var (
		out   []store.RecentFactEntry
		total int
		err   error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, total, err = svc.Search().RecentFacts(contextTODO(), branch, opts)
	})
	return out, total, err
}

// recentFactItem is one item in the facts collection response.
type recentFactItem struct {
	Path        string      `json:"path"`
	Title       string      `json:"title"`
	Kind        string      `json:"kind,omitempty"` // omitted when epistemic (the default)
	Type        string      `json:"type,omitempty"`
	Domain      []string    `json:"domain,omitempty"`
	Entities    []string    `json:"entities,omitempty"`
	CommittedAt int64       `json:"committed_at,omitempty"`
	Operation   string      `json:"operation,omitempty"`
	Links       hal.LinkMap `json:"_links"`
}

// handleHALFactsCollection serves GET /repos/{repo}/branches/{branch}/facts
// (exact path, no wildcard).
func handleHALFactsCollection(b hal.URLBuilder, m *repos.Manager, provider factsCollectionProvider) http.HandlerFunc {
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

		opts := store.SearchOptions{
			Path:          path,
			Text:          qp.Get("q"),
			Limit:         limit,
			Offset:        offset,
			MinConfidence: minConfidence,
			Domain:        splitCSV(qp.Get("domain")),
			Entities:      entities,
			IncludeTypes:  splitCSV(qp.Get("type")),
			ExcludeTypes:  splitCSV(qp.Get("exclude_type")),
			IncludeKinds:  splitCSV(qp.Get("kind")),
			ExcludeKinds:  splitCSV(qp.Get("exclude_kind")),
			EpisodeOps:    splitCSV(qp.Get("ep")),
		}

		entries, total, err := provider.RecentFacts(ri, branch, opts)
		if err != nil {
			writeStoreError(w, r, err, "Failed to list facts", branch)
			return
		}
		if entries == nil {
			entries = []store.RecentFactEntry{}
		}

		factsBase := b.Branch(repoName, a) + "/facts"
		selfURL := factsBase
		if r.URL.RawQuery != "" {
			selfURL += "?" + r.URL.RawQuery
		}

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
				CommittedAt: e.CommittedAt,
				Operation:   e.Operation,
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
