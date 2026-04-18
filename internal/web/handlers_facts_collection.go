package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// factsCollectionProvider is the narrow interface the facts collection handler depends on.
type factsCollectionProvider interface {
	RecentFacts(ri *repos.RepoInstance, branch, pathPrefix, query string, limit, offset int, includeTypes, excludeTypes, domain, entities, epOps []string) ([]store.RecentFactEntry, int, error)
}

// defaultFactsCollectionProvider implements factsCollectionProvider using the store.
type defaultFactsCollectionProvider struct{}

func (defaultFactsCollectionProvider) RecentFacts(
	ri *repos.RepoInstance, branch, pathPrefix, query string, limit, offset int,
	includeTypes, excludeTypes, domain, entities, epOps []string,
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
		out, total, err = svc.Search().RecentFacts(contextTODO(), branch, pathPrefix, query, limit, offset, includeTypes, excludeTypes, domain, entities, epOps)
	})
	return out, total, err
}

// recentFactItem is one item in the facts collection response.
type recentFactItem struct {
	Path        string      `json:"path"`
	Title       string      `json:"title"`
	Type        string      `json:"type,omitempty"`
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

		pathPrefix := qp.Get("path")
		query := qp.Get("q")
		domain := splitCSV(qp.Get("domain"))
		entities := splitCSV(qp.Get("entities"))
		includeTypes := splitCSV(qp.Get("type"))
		excludeTypes := splitCSV(qp.Get("exclude_type"))
		epOps := splitCSV(qp.Get("ep"))

		entries, total, err := provider.RecentFacts(ri, branch, pathPrefix, query, limit, offset, includeTypes, excludeTypes, domain, entities, epOps)
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError,
				"Failed to list facts", err.Error(), r.URL.Path)
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
			items = append(items, recentFactItem{
				Path:        e.Path,
				Title:       e.Title,
				Type:        e.Type,
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
