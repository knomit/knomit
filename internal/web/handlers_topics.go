package web

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// TopicLister is the narrow interface the topic browse handlers depend on.
// Both methods receive the RepoInstance so they can call through WithRead.
// Production wires these via defaultTopicLister; tests inject stubs.
type TopicLister interface {
	ListDir(ri *repos.RepoInstance, branch, path string) ([]store.DirEntry, error)
	GetByPath(ri *repos.RepoInstance, branch, path string) (*store.FactWithBody, error)
}

// defaultTopicLister is the production TopicLister that calls through
// ri.WithRead to access the real store.
type defaultTopicLister struct{}

func (defaultTopicLister) ListDir(ri *repos.RepoInstance, branch, path string) ([]store.DirEntry, error) {
	var (
		out []store.DirEntry
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.Facts().ListDir(contextTODO(), branch, path)
	})
	return out, err
}

func (defaultTopicLister) GetByPath(ri *repos.RepoInstance, branch, path string) (*store.FactWithBody, error) {
	var (
		out *store.FactWithBody
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.Search().GetByPath(contextTODO(), branch, path)
	})
	return out, err
}

// topicEntry is one item in a topic collection. Directories carry a link to
// the sub-topic node; files carry a link to the fact resource plus optional
// type/title enrichment from the search index.
type topicEntry struct {
	Name  string      `json:"name"`
	IsDir bool        `json:"is_dir"`
	Type  string      `json:"type,omitempty"`
	Title string      `json:"title,omitempty"`
	Links hal.LinkMap `json:"_links"`
}

// handleTopics serves GET /repos/{repo}/branches/{branch}/topics.
// It lists the ontology root directory and returns a HAL collection.
func handleTopics(b hal.URLBuilder, m *repos.Manager, ontologyRoot string, lister TopicLister) http.HandlerFunc {
	return topicHandler(b, m, ontologyRoot, lister, false)
}

// handleTopicNode serves GET /repos/{repo}/branches/{branch}/topics/*.
// It lists a subdirectory of the ontology and returns a HAL collection.
func handleTopicNode(b hal.URLBuilder, m *repos.Manager, ontologyRoot string, lister TopicLister) http.HandlerFunc {
	return topicHandler(b, m, ontologyRoot, lister, true)
}

// topicHandler is the shared implementation for both topic endpoints.
// When node is true the handler reads the wildcard path segment from chi.
func topicHandler(b hal.URLBuilder, m *repos.Manager, ontologyRoot string, lister TopicLister, node bool) http.HandlerFunc {
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

		// Determine the directory path to list and the URL node path for links.
		var dirPath, nodePath string
		if node {
			nodePath = chi.URLParam(r, "*")
			dirPath = ontologyRoot + "/" + nodePath
		} else {
			dirPath = ontologyRoot
			nodePath = ""
		}

		entries, err := lister.ListDir(ri, branch, dirPath)
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError,
				"Failed to list topics", err.Error(), r.URL.Path)
			return
		}

		branchURL := b.Branch(repoName, a)

		// Compute self URL for this collection.
		var selfURL string
		if nodePath == "" {
			selfURL = branchURL + "/topics"
		} else {
			selfURL = branchURL + "/topics/" + nodePath
		}

		items := make([]topicEntry, 0, len(entries))
		for _, e := range entries {
			entry := topicEntry{
				Name:  e.Name,
				IsDir: e.IsDir,
			}
			if e.IsDir {
				var childURL string
				if nodePath == "" {
					childURL = branchURL + "/topics/" + e.Name
				} else {
					childURL = branchURL + "/topics/" + nodePath + "/" + e.Name
				}
				entry.Links = hal.LinkMap{"self": {Href: childURL}}
			} else {
				var fullPath string
				if nodePath == "" {
					fullPath = ontologyRoot + "/" + e.Name
				} else {
					fullPath = ontologyRoot + "/" + nodePath + "/" + e.Name
				}
				// Enrich with type/title from search index (best-effort).
				if fb, gerr := lister.GetByPath(ri, branch, fullPath); gerr == nil && fb != nil {
					entry.Type = fb.Type
					entry.Title = fb.Title
				}
				entry.Links = hal.LinkMap{"self": {Href: b.Fact(repoName, a, fullPath)}}
			}
			items = append(items, entry)
		}

		view := hal.CollectionView[topicEntry]{
			Count: len(items),
			Links: hal.LinkMap{"self": {Href: selfURL}},
			Embedded: map[string][]topicEntry{
				"topics": items,
			},
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}
