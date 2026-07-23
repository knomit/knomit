package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

type archivedView struct {
	ID         string      `json:"id"`
	Name       string      `json:"name"`
	Origin     string      `json:"origin"`
	ArchivedAt string      `json:"archivedAt"`
	Links      hal.LinkMap `json:"_links"`
}

func archivedViewOf(b hal.URLBuilder, a repos.ArchiveInfo) archivedView {
	return archivedView{
		ID: a.ID, Name: a.Name, Origin: a.Origin, ArchivedAt: a.ArchivedAt,
		Links: hal.LinkMap{
			"self":    {Href: b.ArchivedItem(a.ID)},
			"restore": {Href: b.ArchivedItem(a.ID) + "/restore"},
		},
	}
}

// handleHALRepoArchive serves DELETE /api/v1/repos/{repo} (archive).
func handleHALRepoArchive(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := chi.URLParam(r, "repo")
		info, err := m.Archive(name)
		if err != nil {
			status, title := archiveErrStatus(err)
			hal.WriteProblem(w, status, title, err.Error(), r.URL.Path)
			return
		}
		hal.WriteHAL(w, http.StatusOK, archivedViewOf(b, info))
	}
}

// handleHALArchived serves GET /api/v1/archived.
func handleHALArchived(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := m.ListArchived()
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "List failed", err.Error(), r.URL.Path)
			return
		}
		items := make([]archivedView, 0, len(list))
		for _, a := range list {
			items = append(items, archivedViewOf(b, a))
		}
		hal.WriteHAL(w, http.StatusOK, hal.CollectionView[archivedView]{
			Count:    len(items),
			Links:    hal.LinkMap{"self": {Href: b.Archived()}},
			Embedded: map[string][]archivedView{"archived": items},
		})
	}
}

type restoreRequest struct {
	NewName string `json:"new_name"`
}

// handleHALArchivedRestore serves POST /api/v1/archived/{id}/restore.
func handleHALArchivedRestore(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var req restoreRequest
		_ = json.NewDecoder(r.Body).Decode(&req) // empty body ok
		ri, err := m.Restore(id, req.NewName)
		if err != nil {
			status, title := archiveErrStatus(err)
			hal.WriteProblem(w, status, title, err.Error(), r.URL.Path)
			return
		}
		hal.WriteHAL(w, http.StatusOK, map[string]any{
			"name":   ri.Name(),
			"_links": hal.LinkMap{"self": {Href: b.Repo(ri.Name())}},
		})
	}
}

// handleHALArchivedPurge serves DELETE /api/v1/archived/{id}.
func handleHALArchivedPurge(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := m.Purge(id); err != nil {
			status, title := archiveErrStatus(err)
			hal.WriteProblem(w, status, title, err.Error(), r.URL.Path)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func archiveErrStatus(err error) (int, string) {
	switch {
	case errors.Is(err, repos.ErrArchiveNotFound):
		return http.StatusNotFound, "Not found"
	case errors.Is(err, repos.ErrRepoNotFound):
		return http.StatusNotFound, "Not found"
	case errors.Is(err, repos.ErrCannotArchiveDefault):
		return http.StatusConflict, "Cannot archive default"
	case errors.Is(err, repos.ErrCannotArchiveLast):
		return http.StatusConflict, "Cannot archive last repo"
	case errors.Is(err, repos.ErrRepoExists):
		return http.StatusConflict, "Name in use"
	case errors.Is(err, repos.ErrRepoNameConflictsLens):
		return http.StatusConflict, "Repo name conflicts with a lens"
	case errors.Is(err, repos.ErrOriginInUse):
		return http.StatusConflict, "Origin in use"
	case errors.Is(err, repos.ErrCreateInFlight):
		return http.StatusConflict, "Operation in flight"
	case errors.Is(err, repos.ErrInvalidName):
		return http.StatusBadRequest, "Invalid name"
	default:
		return http.StatusInternalServerError, "Operation failed"
	}
}
