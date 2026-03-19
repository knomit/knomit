package web

import (
	"net/http"
	"sort"
)

// handleRepos handles GET /api/v1/repos — returns all registered repos.
func handleRepos(rm *RepoManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type repoEntry struct {
			Name   string `json:"name"`
			Branch string `json:"branch"`
		}

		var repos []repoEntry
		rm.ForEach(func(name string, ri *RepoInstance) {
			repos = append(repos, repoEntry{
				Name:   name,
				Branch: ri.GS.Branch(),
			})
		})

		sort.Slice(repos, func(i, j int) bool {
			return repos[i].Name < repos[j].Name
		})

		if repos == nil {
			repos = []repoEntry{}
		}

		writeJSON(w, http.StatusOK, repos)
	}
}
