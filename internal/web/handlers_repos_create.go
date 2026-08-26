package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

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

// maxCreateBodyBytes bounds the whole create envelope, and is deliberately NOT
// MaxOntologyBytes. ontology_yaml rides here as a JSON STRING, and encoding
// inflates it: every newline costs two bytes, every quote and backslash two,
// every other control character six (\u00XX). An ontology is newline-dense by
// construction, so a document :validate accepted at 250 KiB of raw YAML can
// easily exceed 256 KiB once escaped — and capping the body at the ontology
// limit then answered 413 "Ontology too large" for a document that IS under
// the documented limit, an error the user has no way to act on.
//
// So the body cap only has to bound memory (6x covers the all-control-character
// worst case, plus room for the name/mode/origin fields), and the real limit is
// enforced on the DECODED ontology below, where it means what it says.
const maxCreateBodyBytes = 6*MaxOntologyBytes + 4*1024

// handleHALReposCreate serves POST /api/v1/repos. It pre-validates (returning
// problem+json on rejection), then starts the create DETACHED and answers
// 202 Accepted with the job's identity and a link to poll.
//
// THE RESPONSE NO LONGER HOLDS THE WORK (issue #67). It used to stream NDJSON
// progress until the create finished, with r.Context() passed straight into
// Manager.Create — which made a repo's creation the property of the request
// that asked for it: a client that closed its tab cancelled the create at its
// next step boundary, discarding a clone that may already have completed.
//
// 202 makes that structurally impossible rather than merely fixed. There is no
// window in which the client owns the work, because the client never holds it
// at all: it starts a job, gets an id, and asks about it afterwards. A client
// that never comes back changes nothing — the job is bounded by its own
// deadline and lands in a terminal state either way, which StartCreate also
// logs for exactly the reader who is no longer here.
func handleHALReposCreate(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ontology_yaml rides in this body for modes "custom" and "initialize", so the
		// cap MaxOntologyBytes names has to be applied here too — :validate
		// alone leaves the create path unbounded, and nothing forces a client
		// to visit :validate first. Two guards, because they answer two
		// different questions: the reader below bounds how much we will read at
		// all (see maxCreateBodyBytes), and the check after it enforces the
		// ontology limit on the ontology itself.
		var req createRepoRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCreateBodyBytes)).Decode(&req); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				hal.WriteProblem(w, http.StatusRequestEntityTooLarge, "Request too large",
					"request body exceeds the maximum accepted size", r.URL.Path)
				return
			}
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid body", err.Error(), r.URL.Path)
			return
		}
		// The ontology limit, measured on the same bytes :validate measures, so
		// the two paths agree about what "256 KiB" means.
		if len(req.OntologyYAML) > MaxOntologyBytes {
			hal.WriteProblem(w, http.StatusRequestEntityTooLarge, "Ontology too large",
				"ontology exceeds the maximum accepted size", r.URL.Path)
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

		// Preflight still runs on the REQUEST's context, and correctly so: it
		// is the only part of a create the caller genuinely owns. Its refusals
		// are the documented 4xx statuses, and they must stay refusals of the
		// request rather than becoming the terminal state of a job nobody
		// asked to start.
		if err := m.CreatePreflight(r.Context(), spec); err != nil {
			status, title := createErrStatus(err)
			hal.WriteProblem(w, status, title, err.Error(), r.URL.Path)
			return
		}

		job := m.StartCreate(spec)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Location", b.RepoCreate(job.ID()))
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(createStatusBody(b, job.Status()))
	}
}

// handleHALRepoCreateStatus serves GET /api/v1/repo-creates/{id} — the poll
// target the 202 points at. It reports progress while running and the terminal
// outcome afterwards, for CreateJobTTL past the finish.
//
// NOTE ON THE PATH: this is deliberately NOT /repos/creates/{id}. Repo names
// use the alphabet [a-z0-9_-], so "creates" is a legal repo name, and chi
// resolves a static segment in preference to a {repo} param without
// backtracking — a repo actually named "creates" would become unreachable at
// every route under /repos/{repo}. A sibling collection has no such shadow.
func handleHALRepoCreateStatus(b hal.URLBuilder, m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		job, ok := m.CreateJobByID(id)
		if !ok {
			// Unknown and expired are one answer on purpose: a client cannot
			// act differently on them, and the registry is the authoritative
			// answer to whether the repo exists.
			hal.WriteProblem(w, http.StatusNotFound, "Unknown create",
				"no create job with that id (it may have expired)", r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(createStatusBody(b, job.Status()))
	}
}

// createStatusBody renders one create job for the wire. Both the 202 and the
// poll use it, so a client parses ONE shape and the initial response is
// literally the first poll result.
func createStatusBody(b hal.URLBuilder, st repos.CreateStatus) map[string]any {
	body := map[string]any{
		"create_id": st.ID,
		"name":      st.Name,
		"mode":      st.Mode,
		"state":     string(st.State),
		"step":      st.Step,
		"message":   st.Message,
		"pct":       st.Pct,
		"_links":    hal.LinkMap{"self": {Href: b.RepoCreate(st.ID)}},
	}
	switch st.State {
	case repos.CreateDone:
		// The repo link appears only once the repo actually exists — a link
		// offered while the create is still running would 404, and one offered
		// after a failure would point at something that was rolled back.
		body["repo"] = map[string]any{
			"name":   st.Name,
			"_links": hal.LinkMap{"self": {Href: b.Repo(st.Name)}},
		}
	case repos.CreateFailed:
		body["error"] = st.Err.Error()
		// Named separately from the message because a deadline and a genuine
		// create failure call for different client behaviour: one is worth
		// retrying as-is, the other is not.
		body["timed_out"] = st.TimedOut
	}
	return body
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
	// The three ways a remote can be the wrong SHAPE for the requested mode.
	// All 409: the request is well-formed and the server understood it, but the
	// remote is in a state that conflicts with it — and each names the mode that
	// would have worked, because in every case one of the other modes does.
	case errors.Is(err, repos.ErrRemoteNoBranches):
		return http.StatusConflict, "Remote has no branches"
	case errors.Is(err, repos.ErrRemoteNotInitialized):
		return http.StatusConflict, "Remote is not a knowledge base"
	case errors.Is(err, repos.ErrRemoteAlreadyInitialized):
		return http.StatusConflict, "Remote is already a knowledge base"
	default:
		return http.StatusInternalServerError, "Create failed"
	}
}
