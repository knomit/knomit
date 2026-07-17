package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// lensReadDTO is the wire shape for one read mount of a lens. The domain model
// (repos.LensRead) stays wire-agnostic; this DTO carries the json tags.
type lensReadDTO struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
	Source string `json:"source,omitempty"`
}

// lensView is the HAL representation of a lens.
type lensView struct {
	Name      string        `json:"name"`
	Write     string        `json:"write"`
	Reads     []lensReadDTO `json:"reads"`
	CreatedAt int64         `json:"created_at"`
	UpdatedAt int64         `json:"updated_at"`
	Links     hal.LinkMap   `json:"_links"`
}

// createLensRequest is the POST body for creating a lens.
type createLensRequest struct {
	Name  string        `json:"name"`
	Write string        `json:"write"`
	Reads []lensReadDTO `json:"reads"`
}

func lensViewOf(b hal.URLBuilder, l repos.Lens) lensView {
	reads := make([]lensReadDTO, len(l.Reads))
	for i, r := range l.Reads {
		reads[i] = lensReadDTO{Repo: r.Repo, Branch: r.Branch, Source: r.Source}
	}
	return lensView{
		Name: l.Name, Write: l.Write, Reads: reads,
		CreatedAt: l.CreatedAt, UpdatedAt: l.UpdatedAt,
		Links: hal.LinkMap{"self": {Href: b.Lens(l.Name)}},
	}
}

// handleHALLenses serves GET /api/v1/lenses.
func handleHALLenses(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reg := m.Registry()
		if reg == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens registry unavailable",
				"the lens registry is not open", r.URL.Path)
			return
		}
		lenses, err := reg.List()
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "List failed", err.Error(), r.URL.Path)
			return
		}
		views := make([]lensView, 0, len(lenses))
		for _, l := range lenses {
			views = append(views, lensViewOf(b, l))
		}
		hal.WriteHAL(w, http.StatusOK, hal.CollectionView[lensView]{
			Count:    len(views),
			Links:    hal.LinkMap{"self": {Href: b.Lenses()}},
			Embedded: map[string][]lensView{"lenses": views},
		})
	}
}

// handleHALLens serves GET /api/v1/lenses/{lens}.
func handleHALLens(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reg := m.Registry()
		if reg == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens registry unavailable",
				"the lens registry is not open", r.URL.Path)
			return
		}
		name := chi.URLParam(r, "lens")
		l, ok, err := reg.Get(name)
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Get failed", err.Error(), r.URL.Path)
			return
		}
		if !ok {
			hal.WriteProblem(w, http.StatusNotFound, "Lens not found",
				`no lens named "`+name+`"`, r.URL.Path)
			return
		}
		hal.WriteHAL(w, http.StatusOK, lensViewOf(b, l))
	}
}

// handleHALLensesCreate serves POST /api/v1/lenses. All validation runs inside
// Manager.CreateLens (name grammar, repo collision, replica, branch pins) — the
// handler never calls LensRegistry.Create directly.
func handleHALLensesCreate(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m.Registry() == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens registry unavailable",
				"the lens registry is not open", r.URL.Path)
			return
		}
		var req createLensRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body", err.Error(), r.URL.Path)
			return
		}
		reads := make([]repos.LensRead, len(req.Reads))
		for i, rd := range req.Reads {
			reads[i] = repos.LensRead{Repo: rd.Repo, Branch: rd.Branch, Source: rd.Source}
		}
		now := time.Now().Unix() // the caller stamps timestamps; the registry never reads the clock
		lens := repos.Lens{
			Name: req.Name, Write: req.Write, Reads: reads,
			CreatedAt: now, UpdatedAt: now,
		}
		created, err := m.CreateLens(r.Context(), lens)
		if err != nil {
			status, title := lensCreateErrStatus(err)
			hal.WriteProblem(w, status, title, err.Error(), r.URL.Path)
			return
		}
		hal.WriteHAL(w, http.StatusCreated, lensViewOf(b, created))
	}
}

// handleHALLensDelete serves DELETE /api/v1/lenses/{lens}. Get-check-then-Delete
// so an unknown lens is a 404 rather than a silent 204 (Delete is idempotent).
func handleHALLensDelete(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reg := m.Registry()
		if reg == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens registry unavailable",
				"the lens registry is not open", r.URL.Path)
			return
		}
		name := chi.URLParam(r, "lens")
		_, ok, err := reg.Get(name)
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Get failed", err.Error(), r.URL.Path)
			return
		}
		if !ok {
			hal.WriteProblem(w, http.StatusNotFound, "Lens not found",
				`no lens named "`+name+`"`, r.URL.Path)
			return
		}
		if err := reg.Delete(name); err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Delete failed", err.Error(), r.URL.Path)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// lensCreateErrStatus maps CreateLens validation sentinels to HTTP status +
// problem title, mirroring archiveErrStatus.
func lensCreateErrStatus(err error) (int, string) {
	switch {
	case errors.Is(err, repos.ErrInvalidLensName):
		return http.StatusBadRequest, "Invalid lens name"
	case errors.Is(err, repos.ErrLensNameConflictsRepo):
		return http.StatusConflict, "Lens name conflicts with a repo"
	case errors.Is(err, repos.ErrLensExists):
		return http.StatusConflict, "Lens already exists"
	case errors.Is(err, repos.ErrReplicaInLens):
		return http.StatusConflict, "Replica mounts not allowed"
	case errors.Is(err, repos.ErrRepoNotFound):
		return http.StatusUnprocessableEntity, "Lens references an unknown repo"
	case errors.Is(err, repos.ErrLensBranchUnknown):
		return http.StatusUnprocessableEntity, "Lens pins an unknown branch"
	case errors.Is(err, repos.ErrLensWriteEmpty):
		return http.StatusBadRequest, "Lens write repo required"
	default:
		return http.StatusInternalServerError, "Create lens failed"
	}
}
