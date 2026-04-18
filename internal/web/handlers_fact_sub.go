package web

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// factSubProvider is the narrow interface for fact sub-resource handlers
// (commits, incoming, outgoing). Production wires it to commitsProvider +
// search index; tests inject stubs.
type factSubProvider interface {
	// LogPaginatedForPath returns a paged commit log for a specific fact path.
	LogPaginatedForPath(ri *repos.RepoInstance, branch, path string, limit int, after, from, before string) ([]store.LogEntryWithTags, string, string, error)

	// ExplainFact returns the incoming and outgoing ref graph for a fact.
	ExplainFact(ri *repos.RepoInstance, branch, path string) (store.ExplainResult, error)
}

// defaultFactSubProvider implements factSubProvider using the store.
type defaultFactSubProvider struct{}

func (defaultFactSubProvider) LogPaginatedForPath(
	ri *repos.RepoInstance, branch, path string, limit int, after, from, before string,
) ([]store.LogEntryWithTags, string, string, error) {
	var (
		entries []store.LogEntryWithTags
		next    string
		prev    string
		err     error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		entries, next, prev, err = svc.Search().LogPaginated(contextTODO(), branch, path, limit, after, from, before)
	})
	return entries, next, prev, err
}

func (defaultFactSubProvider) ExplainFact(
	ri *repos.RepoInstance, branch, path string,
) (store.ExplainResult, error) {
	var (
		result store.ExplainResult
		err    error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		result, err = svc.Search().ExplainFact(contextTODO(), branch, path)
	})
	return result, err
}

// commitEntry is one item in the per-fact commit log collection.
type commitEntry struct {
	Commit    string           `json:"commit"`
	Date      string           `json:"date"`
	Message   string           `json:"message"`
	Operation string           `json:"operation,omitempty"`
	Files     store.FileCounts `json:"files,omitempty"`
	Links     hal.LinkMap      `json:"_links"`
}

// graphRefEntry is one item in the incoming/outgoing graph collection.
type graphRefEntry struct {
	Path    string      `json:"path"`
	Title   string      `json:"title"`
	Deleted bool        `json:"deleted,omitempty"`
	Links   hal.LinkMap `json:"_links,omitempty"`
}

// handleFactCommits serves GET /repos/{repo}/branches/{branch}/facts/*/commits.
// Dispatched from handleHALFact when the wildcard path ends with "/commits".
func handleFactCommits(b hal.URLBuilder, m *repos.Manager, provider factSubProvider, repoName, branch, factPath string, w http.ResponseWriter, r *http.Request) {
	ri := m.Get(repoName)
	if ri == nil {
		hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
			`no repo named "`+repoName+`"`, r.URL.Path)
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n := parsePositiveInt(v); n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}

	after := r.URL.Query().Get("after")
	from := r.URL.Query().Get("from")
	before := r.URL.Query().Get("before")

	entries, next, prev, err := provider.LogPaginatedForPath(ri, branch, factPath, limit, after, from, before)
	if err != nil {
		writeStoreError(w, r, err, "Failed to list commits", branch)
		return
	}
	if entries == nil {
		entries = []store.LogEntryWithTags{}
	}

	a := hal.Anchor{Branch: branch}
	commitsURL := b.FactCommits(repoName, a, factPath)
	selfURL := commitsURL
	if r.URL.RawQuery != "" {
		selfURL += "?" + r.URL.RawQuery
	}

	links := hal.LinkMap{"self": {Href: selfURL}}
	if next != "" {
		links["next"] = hal.Link{Href: commitsURL + buildCursorQuery(r, "after", next)}
	}
	if prev != "" {
		links["prev"] = hal.Link{Href: commitsURL + buildCursorQuery(r, "after", prev)}
	}

	branchURL := b.Branch(repoName, a)
	items := make([]commitEntry, 0, len(entries))
	for _, e := range entries {
		items = append(items, commitEntry{
			Commit:    e.Commit,
			Date:      e.Date,
			Message:   e.Message,
			Operation: e.Operation,
			Files:     e.Files,
			Links: hal.LinkMap{
				"self": {Href: branchURL + "/commits/" + e.Commit},
			},
		})
	}

	view := hal.CollectionView[commitEntry]{
		Count: len(items),
		Links: links,
		Embedded: map[string][]commitEntry{
			"commits": items,
		},
	}
	hal.WriteHAL(w, http.StatusOK, view)
}

// handleFactIncoming serves GET /repos/{repo}/branches/{branch}/facts/*/incoming.
func handleFactIncoming(b hal.URLBuilder, m *repos.Manager, provider factSubProvider, repoName, branch, factPath string, w http.ResponseWriter, r *http.Request) {
	ri := m.Get(repoName)
	if ri == nil {
		hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
			`no repo named "`+repoName+`"`, r.URL.Path)
		return
	}

	result, err := provider.ExplainFact(ri, branch, factPath)
	if err != nil {
		writeStoreError(w, r, err, "Failed to load incoming refs", branch)
		return
	}

	a := hal.Anchor{Branch: branch}
	selfURL := b.FactIncoming(repoName, a, factPath)

	items := buildGraphRefItems(b, repoName, a, result.Incoming)
	view := hal.CollectionView[graphRefEntry]{
		Count: len(items),
		Links: hal.LinkMap{"self": {Href: selfURL}},
		Embedded: map[string][]graphRefEntry{
			"refs": items,
		},
	}
	hal.WriteHAL(w, http.StatusOK, view)
}

// handleFactOutgoing serves GET /repos/{repo}/branches/{branch}/facts/*/outgoing.
func handleFactOutgoing(b hal.URLBuilder, m *repos.Manager, provider factSubProvider, repoName, branch, factPath string, w http.ResponseWriter, r *http.Request) {
	ri := m.Get(repoName)
	if ri == nil {
		hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
			`no repo named "`+repoName+`"`, r.URL.Path)
		return
	}

	result, err := provider.ExplainFact(ri, branch, factPath)
	if err != nil {
		writeStoreError(w, r, err, "Failed to load outgoing refs", branch)
		return
	}

	a := hal.Anchor{Branch: branch}
	selfURL := b.FactOutgoing(repoName, a, factPath)

	items := buildGraphRefItems(b, repoName, a, result.Outgoing)
	view := hal.CollectionView[graphRefEntry]{
		Count: len(items),
		Links: hal.LinkMap{"self": {Href: selfURL}},
		Embedded: map[string][]graphRefEntry{
			"refs": items,
		},
	}
	hal.WriteHAL(w, http.StatusOK, view)
}

// buildGraphRefItems converts []store.RefSummary into []graphRefEntry with
// self links for non-deleted facts.
func buildGraphRefItems(b hal.URLBuilder, repoName string, a hal.Anchor, refs []store.RefSummary) []graphRefEntry {
	items := make([]graphRefEntry, 0, len(refs))
	for _, ref := range refs {
		item := graphRefEntry{
			Path:    ref.Path,
			Title:   ref.Title,
			Deleted: ref.Deleted,
		}
		if !ref.Deleted {
			item.Links = hal.LinkMap{
				"self": {Href: b.Fact(repoName, a, ref.Path)},
			}
		}
		items = append(items, item)
	}
	return items
}

// parsePositiveInt parses s as a positive int; returns 0 on error or if <= 0.
func parsePositiveInt(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// dispatchFactSubResource checks if a fact wildcard path ends with a known
// sub-resource suffix and dispatches accordingly. Returns true if dispatched.
func dispatchFactSubResource(
	b hal.URLBuilder,
	m *repos.Manager,
	subProvider factSubProvider,
	repoName, branch string,
	w http.ResponseWriter,
	r *http.Request,
) bool {
	path := chi.URLParam(r, "*")

	if strings.HasSuffix(path, "/commits") {
		actualPath := strings.TrimSuffix(path, "/commits")
		handleFactCommits(b, m, subProvider, repoName, branch, actualPath, w, r)
		return true
	}
	if strings.HasSuffix(path, "/incoming") {
		actualPath := strings.TrimSuffix(path, "/incoming")
		handleFactIncoming(b, m, subProvider, repoName, branch, actualPath, w, r)
		return true
	}
	if strings.HasSuffix(path, "/outgoing") {
		actualPath := strings.TrimSuffix(path, "/outgoing")
		handleFactOutgoing(b, m, subProvider, repoName, branch, actualPath, w, r)
		return true
	}
	return false
}
