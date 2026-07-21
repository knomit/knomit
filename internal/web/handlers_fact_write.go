package web

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	knomitfact "knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// FactWriter is the narrow write interface the fact PUT/DELETE handlers depend on.
// Tests inject a stub; production wires through RepoInstance.WithRead.
type FactWriter interface {
	Write(ctx context.Context, ri *repos.RepoInstance, branch, path, content, message string) (string, error)
	Delete(ctx context.Context, ri *repos.RepoInstance, branch, path, message string) (string, error)
}

// defaultFactWriter is the production FactWriter backed by the store.
type defaultFactWriter struct{}

func (defaultFactWriter) Write(ctx context.Context, ri *repos.RepoInstance, branch, path, content, message string) (string, error) {
	var (
		commitHash string
		err        error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errFactNotFound
			return
		}
		res, werr := svc.Facts().WriteFact(ctx, branch, path, content, message, "update")
		if werr != nil {
			err = werr
			return
		}
		commitHash = res.CommitHash
	})
	return commitHash, err
}

func (defaultFactWriter) Delete(ctx context.Context, ri *repos.RepoInstance, branch, path, message string) (string, error) {
	var (
		commitHash string
		err        error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errFactNotFound
			return
		}
		h, derr := svc.Facts().DeleteFact(ctx, branch, path, message)
		if derr != nil {
			err = derr
			return
		}
		commitHash = h
	})
	return commitHash, err
}

// handleFactUpdate serves PUT /repos/{repo}/branches/{branch}/facts/{path...}.
// Body: JSON {"content": "<full markdown with YAML frontmatter>"}.
// Returns HAL FactView with 200 OK.
func handleFactUpdate(b hal.URLBuilder, m *repos.Manager, writer FactWriter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := m.Get(repoName)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}

		branch := BranchFromContext(r.Context())
		path := chi.URLParam(r, "*")
		if path == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Missing fact path",
				"fact path is required", r.URL.Path)
			return
		}

		var body struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body",
				err.Error(), r.URL.Path)
			return
		}

		msg := "edit: update " + path + " via API"
		if _, err := writer.Write(r.Context(), ri, branch, path, body.Content, msg); err != nil {
			writeStoreError(w, r, err, "Failed to write fact", branch)
			return
		}

		f, err := knomitfact.ParseFact(path, body.Content)
		if err != nil {
			hal.WriteProblem(w, http.StatusUnprocessableEntity,
				"Failed to parse fact", err.Error(), r.URL.Path)
			return
		}

		a := hal.Anchor{Branch: branch}
		// Resolver anchored at HEAD (commit:""): the just-written content is
		// now the active state of the branch, so HEAD walk-back correctly
		// classifies the fact's outgoing refs.
		resolver := readerRefResolver{ctx: r.Context(), reader: defaultFactReader{}, ri: ri, branch: branch, commit: ""}
		view := BuildFactView(b, repoName, a, "", f, resolver)
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// handleFactDelete serves DELETE /repos/{repo}/branches/{branch}/facts/{path...}.
// Returns 204 No Content on success.
func handleFactDelete(b hal.URLBuilder, m *repos.Manager, writer FactWriter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := m.Get(repoName)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}

		branch := BranchFromContext(r.Context())
		path := chi.URLParam(r, "*")
		if path == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Missing fact path",
				"fact path is required", r.URL.Path)
			return
		}

		msg := "manual-review: retract " + path
		if _, err := writer.Delete(r.Context(), ri, branch, path, msg); err != nil {
			writeStoreError(w, r, err, "Failed to delete fact", branch)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
