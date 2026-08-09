package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

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
	Name        string        `json:"name"`
	Write       string        `json:"write"`
	Description string        `json:"description,omitempty"`
	Reads       []lensReadDTO `json:"reads"`
	CreatedAt   int64         `json:"created_at"`
	UpdatedAt   int64         `json:"updated_at"`
	Links       hal.LinkMap   `json:"_links"`
}

// createLensRequest is the POST body for creating a lens.
type createLensRequest struct {
	Name        string        `json:"name"`
	Write       string        `json:"write"`
	Description string        `json:"description"`
	Reads       []lensReadDTO `json:"reads"`
}

// patchLensRequest is the PATCH body for editing a lens. Every field is a
// pointer so an omitted field (JSON key absent → nil) is distinguishable from a
// provided-but-empty one: omitted = keep the current value, provided = replace it
// wholesale (reads replace as a set, never merge). The name is immutable and has
// no field here.
type patchLensRequest struct {
	Write       *string        `json:"write"`
	Description *string        `json:"description"`
	Reads       *[]lensReadDTO `json:"reads"`
}

// Lens membership is stored by registry uid; the REST surface still speaks repo
// NAMES. The two helpers below translate at the handler boundary so the wire
// contract is unchanged by the re-keying. Making the API itself uid-aware is a
// separate, deliberate change — until then this is the only place that knows
// both spellings.

// lensMemberName renders a stored member uid as the repo name clients expect.
// An unknown uid renders as itself rather than vanishing: the row is real, and
// showing the raw uid is a legible symptom, where an empty string would not be.
func lensMemberName(m *repos.Manager, uid string) string {
	reg := m.Repos()
	if reg == nil || uid == "" {
		return uid
	}
	// Get, not ByName: an ARCHIVED member still has a row and a display name.
	rec, ok, err := reg.Get(uid)
	if err != nil || !ok {
		return uid
	}
	return rec.Name
}

// lensMemberUID resolves a wire repo name to the uid membership is keyed by.
// An unknown name passes through UNCHANGED so validation downstream rejects it
// with ErrRepoNotFound naming exactly what the caller asked for — inventing a
// uid here would turn a 422 "unknown repo" into a confusing internal error.
func lensMemberUID(m *repos.Manager, name string) string {
	reg := m.Repos()
	if reg == nil || name == "" {
		return name
	}
	rec, ok, err := reg.ByName(name)
	if err != nil || !ok {
		return name
	}
	return rec.UID
}

func lensViewOf(b hal.URLBuilder, m *repos.Manager, l repos.Lens) lensView {
	reads := make([]lensReadDTO, len(l.Reads))
	for i, r := range l.Reads {
		reads[i] = lensReadDTO{Repo: lensMemberName(m, r.RepoUID), Branch: r.Branch, Source: r.Source}
	}
	return lensView{
		Name: l.Name, Write: lensMemberName(m, l.WriteUID), Description: l.Description, Reads: reads,
		CreatedAt: l.CreatedAt, UpdatedAt: l.UpdatedAt,
		Links: hal.LinkMap{"self": {Href: b.Lens(l.Name)}},
	}
}

// handleHALLenses serves GET /api/v1/lenses.
func handleHALLenses(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reg := m.LensRegistry()
		if reg == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens registry unavailable",
				"the lens registry is not open", r.URL.Path)
			return
		}
		lenses, err := reg.List()
		if err != nil {
			log.Error().Err(err).Str("path", r.URL.Path).Msg("list lenses failed")
			hal.WriteProblem(w, http.StatusInternalServerError, "List failed", "list lenses failed", r.URL.Path)
			return
		}
		views := make([]lensView, 0, len(lenses))
		for _, l := range lenses {
			views = append(views, lensViewOf(b, m, l))
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
		reg := m.LensRegistry()
		if reg == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens registry unavailable",
				"the lens registry is not open", r.URL.Path)
			return
		}
		name := chi.URLParam(r, "lens")
		l, ok, err := reg.Get(name)
		if err != nil {
			log.Error().Err(err).Str("path", r.URL.Path).Str("lens", name).Msg("get lens failed")
			hal.WriteProblem(w, http.StatusInternalServerError, "Get failed", "get lens failed", r.URL.Path)
			return
		}
		if !ok {
			hal.WriteProblem(w, http.StatusNotFound, "Lens not found",
				`no lens named "`+name+`"`, r.URL.Path)
			return
		}
		hal.WriteHAL(w, http.StatusOK, lensViewOf(b, m, l))
	}
}

// handleHALLensesCreate serves POST /api/v1/lenses. All validation runs inside
// Manager.CreateLens (name grammar, repo collision, replica, branch pins) — the
// handler never calls LensRegistry.Create directly.
func handleHALLensesCreate(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m.LensRegistry() == nil {
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
			reads[i] = repos.LensRead{RepoUID: lensMemberUID(m, rd.Repo), Branch: rd.Branch, Source: rd.Source}
		}
		now := time.Now().Unix() // the caller stamps timestamps; the registry never reads the clock
		lens := repos.Lens{
			Name: req.Name, WriteUID: lensMemberUID(m, req.Write), Description: req.Description, Reads: reads,
			CreatedAt: now, UpdatedAt: now,
		}
		created, err := m.CreateLens(r.Context(), lens)
		if err != nil {
			status, title := lensCreateErrStatus(err)
			detail := err.Error()
			// The mapped domain arms (400/409/422) carry clean, load-bearing
			// strings; only the 500 fall-through would leak a wrapped SQL/driver
			// error, so scrub it and log the real cause server-side.
			if status == http.StatusInternalServerError {
				log.Error().Err(err).Str("path", r.URL.Path).Str("lens", req.Name).Msg("create lens failed")
				detail = "create lens failed"
			}
			hal.WriteProblem(w, status, title, detail, r.URL.Path)
			return
		}
		hal.WriteHAL(w, http.StatusCreated, lensViewOf(b, m, created))
	}
}

// handleHALLensPatch serves PATCH /api/v1/lenses/{lens}. It edits a lens's write
// repo, read mounts, and description; the name is immutable. Omitted fields keep
// their current value, provided fields replace wholesale. The merge starts from
// the persisted lens (a 404 if unknown), then Manager.UpdateLens re-runs the full
// create-time validation (member existence, replica, branch pins, description
// cap) under the same locking discipline before persisting.
func handleHALLensPatch(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reg := m.LensRegistry()
		if reg == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens registry unavailable",
				"the lens registry is not open", r.URL.Path)
			return
		}
		name := chi.URLParam(r, "lens")
		current, ok, err := reg.Get(name)
		if err != nil {
			log.Error().Err(err).Str("path", r.URL.Path).Str("lens", name).Msg("get lens failed")
			hal.WriteProblem(w, http.StatusInternalServerError, "Get failed", "get lens failed", r.URL.Path)
			return
		}
		if !ok {
			hal.WriteProblem(w, http.StatusNotFound, "Lens not found",
				`no lens named "`+name+`"`, r.URL.Path)
			return
		}
		var req patchLensRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body", err.Error(), r.URL.Path)
			return
		}

		// Start from the persisted lens; apply only the provided fields. created_at
		// is carried through unchanged; the caller stamps a fresh updated_at.
		lens := current
		lens.UpdatedAt = time.Now().Unix()
		if req.Write != nil {
			lens.WriteUID = lensMemberUID(m, *req.Write)
		}
		if req.Description != nil {
			lens.Description = *req.Description
		}
		if req.Reads != nil {
			reads := make([]repos.LensRead, len(*req.Reads))
			for i, rd := range *req.Reads {
				reads[i] = repos.LensRead{RepoUID: lensMemberUID(m, rd.Repo), Branch: rd.Branch, Source: rd.Source}
			}
			lens.Reads = reads
		}

		updated, err := m.UpdateLens(r.Context(), lens)
		if err != nil {
			status, title := lensPatchErrStatus(err)
			detail := err.Error()
			// As with create, only the 500 fall-through risks leaking a wrapped
			// SQL/driver error — scrub it and log the real cause server-side.
			if status == http.StatusInternalServerError {
				log.Error().Err(err).Str("path", r.URL.Path).Str("lens", name).Msg("update lens failed")
				detail = "update lens failed"
			}
			hal.WriteProblem(w, status, title, detail, r.URL.Path)
			return
		}
		hal.WriteHAL(w, http.StatusOK, lensViewOf(b, m, updated))
	}
}

// handleHALLensDelete serves DELETE /api/v1/lenses/{lens}. Get-check-then-Delete
// so an unknown lens is a 404 rather than a silent 204 (Delete is idempotent).
func handleHALLensDelete(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reg := m.LensRegistry()
		if reg == nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Lens registry unavailable",
				"the lens registry is not open", r.URL.Path)
			return
		}
		name := chi.URLParam(r, "lens")
		_, ok, err := reg.Get(name)
		if err != nil {
			log.Error().Err(err).Str("path", r.URL.Path).Str("lens", name).Msg("get lens failed")
			hal.WriteProblem(w, http.StatusInternalServerError, "Get failed", "get lens failed", r.URL.Path)
			return
		}
		if !ok {
			hal.WriteProblem(w, http.StatusNotFound, "Lens not found",
				`no lens named "`+name+`"`, r.URL.Path)
			return
		}
		if err := reg.Delete(name); err != nil {
			log.Error().Err(err).Str("path", r.URL.Path).Str("lens", name).Msg("delete lens failed")
			hal.WriteProblem(w, http.StatusInternalServerError, "Delete failed", "delete lens failed", r.URL.Path)
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
	case errors.Is(err, repos.ErrCreateInFlight):
		return http.StatusConflict, "Create in flight"
	case errors.Is(err, repos.ErrReplicaInLens):
		return http.StatusConflict, "Replica mounts not allowed"
	case errors.Is(err, repos.ErrRepoNotFound):
		return http.StatusUnprocessableEntity, "Lens references an unknown repo"
	case errors.Is(err, repos.ErrLensBranchUnknown):
		return http.StatusUnprocessableEntity, "Lens pins an unknown branch"
	case errors.Is(err, repos.ErrLensWriteEmpty):
		return http.StatusBadRequest, "Lens write repo required"
	case errors.Is(err, repos.ErrLensDescriptionTooLong):
		return http.StatusUnprocessableEntity, "Lens description too long"
	case errors.Is(err, repos.ErrLensNotFound):
		// Reached only via PATCH, when the lens is deleted between the handler's
		// Get and UpdateLens's persist. Create never produces it.
		return http.StatusNotFound, "Lens not found"
	default:
		return http.StatusInternalServerError, "Create lens failed"
	}
}

// lensPatchErrStatus reuses lensCreateErrStatus's sentinel→(status,title)
// mapping — the 4xx/422 validation arms are identical on the PATCH path — but
// relabels the scrubbed-500 default arm so the problem title names the actual
// operation ("Update lens failed" rather than "Create lens failed").
func lensPatchErrStatus(err error) (int, string) {
	status, title := lensCreateErrStatus(err)
	if status == http.StatusInternalServerError {
		title = "Update lens failed"
	}
	return status, title
}
