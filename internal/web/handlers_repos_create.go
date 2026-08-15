package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

type createRepoRequest struct {
	Name           string `json:"name"`
	Mode           string `json:"mode"`
	OntologyPreset string `json:"ontology_preset"`
	OntologyYAML   string `json:"ontology_yaml"`
	Origin         *struct {
		URL        string `json:"url"`
		Branch     string `json:"branch"`
		AuthMethod string `json:"auth_method"`
		AuthToken  string `json:"auth_token"`
	} `json:"origin"`
}

// handleHALReposCreate serves POST /api/v1/repos. It pre-validates (returning
// problem+json on rejection), then streams newline-delimited JSON progress
// (application/x-ndjson) ending in a terminal {"type":"done"} or
// {"type":"error"} line.
func handleHALReposCreate(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ontology_yaml rides in this body for modes "custom" and "seed", so the
		// cap MaxOntologyBytes names has to be applied here too — :validate
		// alone leaves the create path unbounded, and nothing forces a client
		// to visit :validate first. The rest of the envelope (name, mode,
		// origin) is a few hundred bytes, so capping the whole body at the
		// ontology limit bounds the ontology.
		var req createRepoRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxOntologyBytes)).Decode(&req); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				hal.WriteProblem(w, http.StatusRequestEntityTooLarge, "Ontology too large",
					"ontology exceeds the maximum accepted size", r.URL.Path)
				return
			}
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid body", err.Error(), r.URL.Path)
			return
		}
		// Local-origin policy is enforced at the clone boundary
		// (Manager.ResolveAuth, invoked by the clone-mode Create below).
		spec := repos.CreateSpec{
			Name:           req.Name,
			Mode:           req.Mode,
			OntologyPreset: req.OntologyPreset,
			OntologyYAML:   req.OntologyYAML,
		}
		if req.Origin != nil {
			spec.Origin = &repos.OriginSpec{
				URL:        req.Origin.URL,
				Branch:     req.Origin.Branch,
				AuthMethod: req.Origin.AuthMethod,
				AuthToken:  req.Origin.AuthToken,
			}
		}

		if err := m.CreatePreflight(r.Context(), spec); err != nil {
			status, title := createErrStatus(err)
			hal.WriteProblem(w, status, title, err.Error(), r.URL.Path)
			return
		}

		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		emit := func(e repos.Event) {
			_ = enc.Encode(map[string]any{
				"type": "progress", "step": e.Step, "message": e.Message, "pct": e.Pct,
			})
			if flusher != nil {
				flusher.Flush()
			}
		}

		ri, err := m.Create(r.Context(), spec, emit)
		if err != nil {
			_ = enc.Encode(map[string]any{
				"type": "error", "title": "Create failed", "detail": err.Error(),
			})
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		_ = enc.Encode(map[string]any{
			"type": "done",
			"repo": map[string]any{
				"name":   ri.Name(),
				"_links": hal.LinkMap{"self": {Href: b.Repo(ri.Name())}},
			},
		})
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func createErrStatus(err error) (int, string) {
	switch {
	case errors.Is(err, repos.ErrInvalidName):
		return http.StatusBadRequest, "Invalid name"
	case errors.Is(err, repos.ErrRepoExists):
		return http.StatusConflict, "Repo exists"
	case errors.Is(err, repos.ErrRepoNameConflictsLens):
		return http.StatusConflict, "Repo name conflicts with a lens"
	case errors.Is(err, repos.ErrCreateInFlight):
		return http.StatusConflict, "Create in flight"
	case errors.Is(err, repos.ErrOriginInUse):
		return http.StatusConflict, "Origin in use"
	case errors.Is(err, repos.ErrRemoteNotEmpty):
		return http.StatusConflict, "Remote is not empty"
	default:
		return http.StatusInternalServerError, "Create failed"
	}
}
