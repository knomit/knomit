package kb

import (
	"context"
	"errors"
	"knomit/internal/edge/web/httpx"
	"net/http"

	"knomit/internal/engine/store/index"

	"github.com/go-chi/chi/v5"

	"knomit/internal/corpus/repos"
	"knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/facts"
)

// commitFileView is one entry in the commit detail files list. It carries the
// enriched title alongside the raw path and action from the store.
type commitFileView struct {
	Path   string      `json:"path"`
	Action string      `json:"action"`
	Title  string      `json:"title,omitempty"`
	Links  hal.LinkMap `json:"_links,omitempty"`
}

// logPaginated returns a paged branch-wide commit log of ri's store. Use an
// empty path for all commits on the branch. A store that cannot be reached is
// WithRead's error, returned as-is (httpx.WriteStoreError answers it 503).
func logPaginated(
	ctx context.Context,
	ri *repos.RepoInstance, branch, path string, limit int, after, from, before string,
) ([]index.LogEntryWithTags, string, string, error) {
	var (
		entries []index.LogEntryWithTags
		next    string
		prev    string
		err     error
	)
	if aerr := ri.WithRead(func(svc *store.Service) {
		entries, next, prev, err = svc.HistoryQuery().LogPaginated(ctx, branch, path, limit, after, from, before)
	}); aerr != nil {
		return entries, next, prev, aerr
	}
	return entries, next, prev, err
}

// commitDetail returns the detail for a single commit, including files
// enriched with titles. Only files under ontologyRoot are returned.
func commitDetail(
	ctx context.Context,
	ri *repos.RepoInstance, branch, hash, ontologyRoot string,
) (*index.CommitDetailResult, []commitFileView, error) {
	// Acquire (not WithRead): the per-file title resolution below reads
	// through gs/idx long after CommitDetail returns, so the store reference
	// must outlive a single closure. Copying the handles out of a WithRead
	// closure would let a concurrent SwapStore/Archive close the DB mid-loop
	// — see RepoInstance.Acquire's doc-comment.
	//
	// Acquire's refusal (httpx.StoreUnavailable) is returned as-is, and the handler
	// answers it 503 rather than the 404 of a commit that is not there.
	svc, release, err := ri.Acquire()
	if err != nil {
		return nil, nil, err
	}
	defer release()

	gs := svc.Facts()
	idx := svc.FactQuery()
	detail, err := svc.HistoryQuery().CommitDetail(ctx, hash, ontologyRoot)
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
			if fb, ferr := idx.GetByPath(ctx, branch, f.Path); ferr == nil && fb != nil {
				files[i].Title = fb.Title
				continue
			}
		}
		// Fallback: read the fact as it was at this commit.
		if gs != nil {
			if result, rerr := gs.ReadFact(ctx, branch, f.Path, &facts.ReadFactOpts{AtCommit: hash}); rerr == nil && result.Content != "" {
				if parsed, perr := fact.ParseFact(f.Path, result.Content); perr == nil {
					files[i].Title = parsed.Title
					continue
				}
			}
			// Last resort: find last commit where the file existed (covers deletions).
			if result, rerr := gs.ReadFact(ctx, branch, f.Path, &facts.ReadFactOpts{BeforeCommit: hash}); rerr == nil && result.Content != "" {
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
	Commit    string             `json:"commit"`
	Date      string             `json:"date"`
	Message   string             `json:"message"`
	Operation string             `json:"operation,omitempty"`
	Author    index.CommitAuthor `json:"author"`
	Files     index.FileCounts   `json:"files,omitempty"`
	Links     hal.LinkMap        `json:"_links"`
}

// handleHALCommitsList serves GET /repos/{repo}/branches/{branch}/commits.
func handleHALCommitsList(b hal.URLBuilder, ontologyRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		branch := httpx.BranchFromContext(r.Context())
		a := hal.Anchor{Branch: branch}

		limit, ok := limitParam(w, r)
		if !ok {
			return
		}

		after := r.URL.Query().Get("after")
		from := r.URL.Query().Get("from")
		before := r.URL.Query().Get("before")

		entries, next, prev, err := logPaginated(r.Context(), ri, branch, ontologyRoot, limit, after, from, before)
		if err != nil {
			httpx.WriteStoreError(w, r, err, "Failed to list commits", branch)
			return
		}
		if entries == nil {
			entries = []index.LogEntryWithTags{}
		}

		branchURL := b.Branch(repoName, a)
		commitsBase := branchURL + "/commits"
		selfURL := httpx.SelfWithQuery(commitsBase, r)

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
				Author:    e.Author,
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
func handleHALCommitDetail(b hal.URLBuilder, ontologyRoot string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		branch := httpx.BranchFromContext(r.Context())
		sha := chi.URLParam(r, "sha")
		if sha == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Missing commit SHA",
				"commit SHA is required", r.URL.Path)
			return
		}

		detail, files, err := commitDetail(r.Context(), ri, branch, sha, ontologyRoot)
		if httpx.StoreUnavailable(err) {
			httpx.WriteStoreUnavailable(w, r)
			return
		}
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
			"author":    detail.Author,
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
