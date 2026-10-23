package web

import (
	"context"
	"net/http"
	"strings"

	"knomit/internal/engine/store/index"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/repos"
	knomitfact "knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/facts"
)

// listTopicDir lists path on branch of ri's store through ri.WithRead. A nil
// store (the repo is not open) yields no entries and a nil error.
func listTopicDir(ctx context.Context, ri *repos.RepoInstance, branch, path string) ([]facts.DirEntry, error) {
	var (
		out []facts.DirEntry
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.Facts().ListDir(ctx, branch, path)
	})
	return out, err
}

// topicFactByPath reads the indexed fact at path on branch of ri's store
// through ri.WithRead, for a topic listing's type/title enrichment. A nil store
// yields a nil fact and a nil error.
func topicFactByPath(ctx context.Context, ri *repos.RepoInstance, branch, path string) (*facts.FactWithBody, error) {
	var (
		out *facts.FactWithBody
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		out, err = svc.FactQuery().GetByPath(ctx, branch, path)
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
func handleTopics(b hal.URLBuilder, ontologyRoot string) http.HandlerFunc {
	return topicHandler(b, ontologyRoot, false)
}

// handleTopicNode serves GET /repos/{repo}/branches/{branch}/topics/*.
// It lists a subdirectory of the ontology, or dispatches to /facts or /stats
// sub-resources when the wildcard path ends with those suffixes.
func handleTopicNode(b hal.URLBuilder, ontologyRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nodePath := chi.URLParam(r, "*")
		if strings.HasSuffix(nodePath, "/facts") {
			topicPath := strings.TrimSuffix(nodePath, "/facts")
			handleTopicFacts(b, ontologyRoot, topicPath)(w, r)
			return
		}
		if strings.HasSuffix(nodePath, "/stats") {
			topicPath := strings.TrimSuffix(nodePath, "/stats")
			handleTopicStats(b, ontologyRoot, topicPath)(w, r)
			return
		}
		topicHandler(b, ontologyRoot, true)(w, r)
	}
}

// handleTopicFacts serves GET .../topics/{segments...}/facts.
// Returns a CollectionView of non-directory entries (facts) directly at the topic node.
func handleTopicFacts(b hal.URLBuilder, ontologyRoot string, topicPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		branch := BranchFromContext(r.Context())
		a := hal.Anchor{Branch: branch}

		dirPath := ontologyRoot + "/" + topicPath
		entries, err := listTopicDir(r.Context(), ri, branch, dirPath)
		if err != nil {
			writeStoreError(w, r, err, "Failed to list topic facts", branch)
			return
		}

		branchURL := b.Branch(repoName, a)
		selfURL := branchURL + "/topics/" + topicPath + "/facts"

		type factSummary struct {
			Name  string      `json:"name"`
			Type  string      `json:"type,omitempty"`
			Title string      `json:"title,omitempty"`
			Links hal.LinkMap `json:"_links"`
		}

		items := make([]factSummary, 0)
		for _, e := range entries {
			if e.IsDir {
				continue
			}
			fullPath := ontologyRoot + "/" + topicPath + "/" + e.Name
			// Private paths (a dot-prefixed segment anywhere, including in
			// topicPath itself when a private directory is browsed directly)
			// are excluded from fact discovery — the same rule the indexer,
			// Verify and the OKF exporter already enforce. Without this, GET
			// .../topics/.drafts/facts would list the stash's contents with
			// live self links, contradicting spec/mbekg.md §3.8.
			if knomitfact.IsPrivatePath(fullPath) {
				continue
			}
			item := factSummary{Name: e.Name}
			if fb, gerr := topicFactByPath(r.Context(), ri, branch, fullPath); gerr == nil && fb != nil {
				item.Type = fb.Type
				item.Title = fb.Title
			}
			item.Links = hal.LinkMap{"self": {Href: b.Fact(repoName, a, fullPath)}}
			items = append(items, item)
		}

		view := hal.CollectionView[factSummary]{
			Count: len(items),
			Links: hal.LinkMap{"self": {Href: selfURL}},
			Embedded: map[string][]factSummary{
				"facts": items,
			},
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// handleTopicStats serves GET .../topics/{segments...}/stats.
// Delegates to loadStats using the topic's path prefix.
func handleTopicStats(b hal.URLBuilder, ontologyRoot string, topicPath string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		branch := BranchFromContext(r.Context())
		a := hal.Anchor{Branch: branch}

		pathPrefix := ontologyRoot + "/" + topicPath

		result, err := loadStats(r.Context(), ri, branch, pathPrefix, "")
		if err != nil {
			writeStoreError(w, r, err, "Failed to load stats", branch)
			return
		}

		branchURL := b.Branch(repoName, a)
		selfURL := branchURL + "/topics/" + topicPath + "/stats"

		domains := result.Domains
		if domains == nil {
			domains = map[string]int{}
		}
		entities := result.Entities
		if entities == nil {
			entities = map[string]int{}
		}
		types := result.Types
		if types == nil {
			types = map[string]int{}
		}
		highlights := result.Highlights
		if highlights == nil {
			highlights = []index.Highlight{}
		}
		axis := result.DefaultAxis
		if axis == "" {
			axis = index.AxisConfidence
		}

		view := statsView{
			Total:         result.Total,
			AvgConfidence: result.AvgConfidence,
			Domains:       domains,
			Entities:      entities,
			Types:         types,
			Highlights:    highlights,
			DefaultAxis:   axis,
			Links:         hal.LinkMap{"self": {Href: selfURL}},
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// topicHandler is the shared implementation for both topic endpoints.
// When node is true the handler reads the wildcard path segment from chi.
func topicHandler(b hal.URLBuilder, ontologyRoot string, node bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

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

		entries, err := listTopicDir(r.Context(), ri, branch, dirPath)
		if err != nil {
			log.Error().Err(err).Str("branch", branch).Str("path", dirPath).Msg("ListDir failed")
			writeStoreError(w, r, err, "Failed to list topics", branch)
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
			// checkPath mirrors the fullPath/childURL construction below: the
			// ontology-relative path this entry would sit at. Checking it
			// (not just e.Name) also catches the case where nodePath itself
			// is already private, so every entry under it is skipped too.
			checkPath := e.Name
			if nodePath != "" {
				checkPath = nodePath + "/" + e.Name
			}
			if knomitfact.IsPrivatePath(checkPath) {
				continue
			}
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
				if fb, gerr := topicFactByPath(r.Context(), ri, branch, fullPath); gerr == nil && fb != nil {
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
