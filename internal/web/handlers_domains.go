package web

import (
	"context"
	"net/http"
	"sort"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// domainsProvider is the narrow interface the domains handlers depend on.
type domainsProvider interface {
	// DomainStats returns the domain histogram for a branch.
	DomainStats(ctx context.Context, ri *repos.RepoInstance, branch string) (map[string]int, error)

	// DomainFacts returns facts filtered by domain.
	DomainFacts(ctx context.Context, ri *repos.RepoInstance, branch, domain string) ([]store.SearchResult, error)
}

// defaultDomainsProvider implements domainsProvider using the store.
type defaultDomainsProvider struct{}

func (defaultDomainsProvider) DomainStats(ctx context.Context, ri *repos.RepoInstance, branch string) (map[string]int, error) {
	var (
		result map[string]int
		err    error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		stats, serr := svc.Search().Stats(ctx, branch, "")
		if serr != nil {
			err = serr
			return
		}
		result = stats.Domains
	})
	return result, err
}

func (defaultDomainsProvider) DomainFacts(ctx context.Context, ri *repos.RepoInstance, branch, domain string) ([]store.SearchResult, error) {
	var (
		out []store.SearchResult
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.Search().Search(ctx, branch, store.SearchOptions{
			Domain: []string{domain},
			Limit:  500,
		})
	})
	return out, err
}

// domainEntry is one item in the domains collection.
type domainEntry struct {
	Name  string      `json:"name"`
	Count int         `json:"count"`
	Links hal.LinkMap `json:"_links"`
}

// factSummaryItem is a fact summary (no body) used in domain fact lists.
type factSummaryItem struct {
	Path       string      `json:"path"`
	Title      string      `json:"title"`
	Type       string      `json:"type,omitempty"`
	Confidence float64     `json:"confidence,omitempty"`
	Links      hal.LinkMap `json:"_links"`
}

// handleHALDomains serves GET /repos/{repo}/branches/{branch}/domains.
func handleHALDomains(b hal.URLBuilder, m *repos.Manager, provider domainsProvider) http.HandlerFunc {
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

		domains, err := provider.DomainStats(r.Context(), ri, branch)
		if err != nil {
			writeStoreError(w, r, err, "Failed to load domains", branch)
			return
		}
		if domains == nil {
			domains = map[string]int{}
		}

		selfURL := b.Branch(repoName, a) + "/domains"

		// Sort domain names for stable output.
		names := make([]string, 0, len(domains))
		for name := range domains {
			names = append(names, name)
		}
		sort.Strings(names)

		items := make([]domainEntry, 0, len(names))
		for _, name := range names {
			items = append(items, domainEntry{
				Name:  name,
				Count: domains[name],
				Links: hal.LinkMap{
					"self": {Href: selfURL + "/" + name},
				},
			})
		}

		view := hal.CollectionView[domainEntry]{
			Count: len(items),
			Links: hal.LinkMap{"self": {Href: selfURL}},
			Embedded: map[string][]domainEntry{
				"domains": items,
			},
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// handleHALDomainFacts serves GET /repos/{repo}/branches/{branch}/domains/{name}.
func handleHALDomainFacts(b hal.URLBuilder, m *repos.Manager, provider domainsProvider) http.HandlerFunc {
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
		domainName := chi.URLParam(r, "name")

		results, err := provider.DomainFacts(r.Context(), ri, branch, domainName)
		if err != nil {
			writeStoreError(w, r, err, "Failed to load domain facts", branch)
			return
		}
		if results == nil {
			results = []store.SearchResult{}
		}

		selfURL := b.Branch(repoName, a) + "/domains/" + domainName

		items := make([]factSummaryItem, 0, len(results))
		for _, res := range results {
			items = append(items, factSummaryItem{
				Path:       res.Path,
				Title:      res.Title,
				Type:       res.Type,
				Confidence: res.Confidence,
				Links:      hal.LinkMap{"self": {Href: b.Fact(repoName, a, res.Path)}},
			})
		}

		view := hal.CollectionView[factSummaryItem]{
			Count: len(items),
			Links: hal.LinkMap{"self": {Href: selfURL}},
			Embedded: map[string][]factSummaryItem{
				"facts": items,
			},
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}
