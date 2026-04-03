package web

import (
	"net/http"
	"sort"

	"knomit/internal/repos"
)

// handleRepos handles GET /api/v1/repos — returns all registered repos.
func handleRepos(rm *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		type repoEntry struct {
			Name   string `json:"name"`
			Branch string `json:"branch"`
		}

		var repoList []repoEntry
		rm.ForEach(func(name string, ri *repos.RepoInstance) {
			repoList = append(repoList, repoEntry{
				Name:   name,
				Branch: ri.AgentBranch(),
			})
		})

		sort.Slice(repoList, func(i, j int) bool {
			return repoList[i].Name < repoList[j].Name
		})

		if repoList == nil {
			repoList = []repoEntry{}
		}

		writeJSON(w, http.StatusOK, repoList)
	}
}
