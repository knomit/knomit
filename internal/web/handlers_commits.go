package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// commitFileView is one entry in the commit detail files list. It carries the
// enriched title alongside the raw path and action from the store.
type commitFileView struct {
	Path   string      `json:"path"`
	Action string      `json:"action"`
	Title  string      `json:"title,omitempty"`
	Links  hal.LinkMap `json:"_links,omitempty"`
}

// commitsProvider is the narrow interface the commits handlers depend on.
// Production wires defaultCommitsProvider; tests inject stubs.
type commitsProvider interface {
	// LogPaginated returns a paged branch-wide commit log. Use empty path for
	// all commits on the branch.
	LogPaginated(ri *repos.RepoInstance, branch, path string, limit int, after, from, before string) ([]store.LogEntryWithTags, string, string, error)

	// CommitDetail returns the detail for a single commit, including files
	// enriched with titles. Only files under ontologyRoot are returned.
	CommitDetail(ri *repos.RepoInstance, branch, hash, ontologyRoot string) (*store.CommitDetailResult, []commitFileView, error)
}

// defaultCommitsProvider is the production commitsProvider that calls through
// ri.WithRead to access the real store.
type defaultCommitsProvider struct{}

func (defaultCommitsProvider) LogPaginated(
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

func (defaultCommitsProvider) CommitDetail(
	ri *repos.RepoInstance, branch, hash, ontologyRoot string,
) (*store.CommitDetailResult, []commitFileView, error) {
	var (
		detail *store.CommitDetailResult
		gs     store.FactIndex
		idx    store.SearchIndex
		err    error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		gs = svc.Facts()
		idx = svc.Search()
		detail, err = svc.Search().CommitDetail(contextTODO(), hash, ontologyRoot)
	})
	if err != nil {
		return nil, nil, err
	}
	if detail == nil {
		return nil, nil, errors.New("commit not found")
	}

	files := make([]commitFileView, len(detail.Files))
	for i, f := range detail.Files {
		files[i] = commitFileView{Path: f.Path, Action: f.Action}
		// Try current index first (fast path for facts still in the store).
		if idx != nil {
			if fb, ferr := idx.GetByPath(contextTODO(), branch, f.Path); ferr == nil && fb != nil {
				files[i].Title = fb.Title
				continue
			}
		}
		// Fallback: read the fact as it was at this commit.
		if gs != nil {
			if result, rerr := gs.ReadFact(contextTODO(), branch, f.Path, &store.ReadFactOpts{AtCommit: hash}); rerr == nil && result.Content != "" {
				if parsed, perr := fact.ParseFact(f.Path, result.Content); perr == nil {
					files[i].Title = parsed.Title
					continue
				}
			}
			// Last resort: find last commit where the file existed (covers deletions).
			if result, rerr := gs.ReadFact(contextTODO(), branch, f.Path, &store.ReadFactOpts{BeforeCommit: hash}); rerr == nil && result.Content != "" {
				if parsed, perr := fact.ParseFact(f.Path, result.Content); perr == nil {
					files[i].Title = parsed.Title
				}
			}
		}
	}
	return detail, files, nil
}

// commitItem is one entry in the commits collection embedded list.
type commitItem struct {
	Commit    string           `json:"commit"`
	Date      string           `json:"date"`
	Message   string           `json:"message"`
	Operation string           `json:"operation,omitempty"`
	Files     store.FileCounts `json:"files,omitempty"`
	Links     hal.LinkMap      `json:"_links"`
}

// handleHALCommitsList serves GET /repos/{repo}/branches/{branch}/commits.
func handleHALCommitsList(b hal.URLBuilder, m *repos.Manager, provider commitsProvider, ontologyRoot string) http.HandlerFunc {
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

		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		if limit > 500 {
			limit = 500
		}

		after := r.URL.Query().Get("after")
		from := r.URL.Query().Get("from")
		before := r.URL.Query().Get("before")

		entries, next, prev, err := provider.LogPaginated(ri, branch, ontologyRoot, limit, after, from, before)
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError,
				"Failed to list commits", err.Error(), r.URL.Path)
			return
		}
		if entries == nil {
			entries = []store.LogEntryWithTags{}
		}

		branchURL := b.Branch(repoName, a)
		commitsBase := branchURL + "/commits"
		selfURL := commitsBase
		if r.URL.RawQuery != "" {
			selfURL += "?" + r.URL.RawQuery
		}

		links := hal.LinkMap{"self": {Href: selfURL}}
		if next != "" {
			links["next"] = hal.Link{Href: commitsBase + buildCursorQuery(r, "after", next)}
		}
		if prev != "" {
			links["prev"] = hal.Link{Href: commitsBase + buildCursorQuery(r, "after", prev)}
		}

		items := make([]commitItem, 0, len(entries))
		for _, e := range entries {
			items = append(items, commitItem{
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

		view := hal.CollectionView[commitItem]{
			Count: len(items),
			Links: links,
			Embedded: map[string][]commitItem{
				"commits": items,
			},
		}
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// handleHALCommitDetail serves GET /repos/{repo}/branches/{branch}/commits/{sha}.
func handleHALCommitDetail(b hal.URLBuilder, m *repos.Manager, provider commitsProvider, ontologyRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := m.Get(repoName)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}

		branch := BranchFromContext(r.Context())
		sha := chi.URLParam(r, "sha")
		if sha == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Missing commit SHA",
				"commit SHA is required", r.URL.Path)
			return
		}

		detail, files, err := provider.CommitDetail(ri, branch, sha, ontologyRoot)
		if err != nil {
			hal.WriteProblem(w, http.StatusNotFound, "Commit not found",
				err.Error(), r.URL.Path)
			return
		}

		a := hal.Anchor{Branch: branch}
		branchURL := b.Branch(repoName, a)
		selfURL := branchURL + "/commits/" + sha

		// Wire self links into each file view.
		for i := range files {
			if files[i].Links == nil {
				files[i].Links = hal.LinkMap{}
			}
			files[i].Links["self"] = hal.Link{Href: b.Fact(repoName, a, files[i].Path)}
		}

		body := map[string]any{
			"commit":    detail.Commit,
			"date":      detail.Date,
			"message":   detail.Message,
			"operation": detail.Operation,
			"files":     files,
			"_links": hal.LinkMap{
				"self":   {Href: selfURL},
				"branch": {Href: branchURL},
			},
		}
		hal.WriteHAL(w, http.StatusOK, body)
	}
}

// buildCursorQuery builds a query string with the cursor parameter replaced.
// It preserves existing params (limit, from, before) while swapping `after`.
func buildCursorQuery(r *http.Request, param, value string) string {
	q := r.URL.Query()
	q.Set(param, value)
	return "?" + q.Encode()
}
