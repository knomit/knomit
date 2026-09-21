// REST for the experiment lifecycle: the UI's and any HTTP client's route to
// the same store operations knomit_experiment drives over MCP.
//
// Deliberately a sibling of the MCP tool rather than a layer over it. Both
// call store.ExperimentIndex directly, so neither can drift into a different
// notion of what "commit" means — the shape the two surfaces must share is the
// STORE's, not a wrapper's.
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// experimentView is one experiment as the API renders it.
//
// ExpiresAt is OMITTED when expiry is disabled rather than sent as a
// far-future date or a null: absence means "never", and a computed date
// would be a claim the server never made.
type experimentView struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	Branch       string `json:"branch"`
	Parent       string `json:"parent"`
	ForkCommit   string `json:"fork_commit"`
	CreatedAt    string `json:"created_at"`
	LastActivity string `json:"last_activity"`
	ExpiresAt    string `json:"expires_at,omitempty"`
	// Writable is this instance's own eligibility answer for the
	// experiment's branch. An experiment whose parent is no longer this
	// agent branch still LISTS — it exists and can be rolled back — but it
	// cannot be written or committed, and the UI needs to say which.
	Writable bool        `json:"writable"`
	Links    hal.LinkMap `json:"_links"`
}

// experimentsCollection is the list response. ExpiryDays rides along because
// the UI has to explain the policy next to the ages it is showing, and 0
// means never.
type experimentsCollection struct {
	Count      int                         `json:"count"`
	ExpiryDays int                         `json:"expiry_days"`
	Links      hal.LinkMap                 `json:"_links"`
	Embedded   map[string][]experimentView `json:"_embedded"`
}

// experimentViewOf renders one experiment.
func experimentViewOf(b hal.URLBuilder, repoName string, ri *repos.RepoInstance, e store.Experiment, expiryDays int) experimentView {
	self := b.Experiment(repoName, e.Name)
	v := experimentView{
		Name:         e.Name,
		Description:  e.Description,
		Branch:       e.Branch(),
		Parent:       e.Parent,
		ForkCommit:   e.ForkCommit,
		CreatedAt:    e.CreatedAt.UTC().Format(time.RFC3339),
		LastActivity: e.LastActivityAt.UTC().Format(time.RFC3339),
		Writable:     ri.WritableBranch(e.Branch()),
		Links: hal.LinkMap{
			"self":     {Href: self},
			"branch":   {Href: b.Branch(repoName, hal.Anchor{Branch: e.Branch()})},
			"commit":   {Href: self + "/commit"},
			"rollback": {Href: self + "/rollback"},
			"sync":     {Href: self + "/sync"},
		},
	}
	if expiryDays > 0 {
		v.ExpiresAt = e.LastActivityAt.Add(time.Duration(expiryDays) * 24 * time.Hour).UTC().Format(time.RFC3339)
	}
	return v
}

// handleHALExperiments serves GET /repos/{repo}/experiments.
func handleHALExperiments(b hal.URLBuilder, expiryDays int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		var (
			list []store.Experiment
			err  error
		)
		if aerr := ri.WithRead(func(svc *store.Service) {
			list, err = svc.Experiments().ListExperiments(r.Context())
		}); aerr != nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Store unavailable",
				aerr.Error(), r.URL.Path)
			return
		}
		if err != nil {
			hal.WriteProblem(w, http.StatusInternalServerError, "Failed to list experiments",
				err.Error(), r.URL.Path)
			return
		}

		items := make([]experimentView, 0, len(list))
		for _, e := range list {
			items = append(items, experimentViewOf(b, repoName, ri, e, expiryDays))
		}
		hal.WriteHAL(w, http.StatusOK, experimentsCollection{
			Count:      len(items),
			ExpiryDays: expiryDays,
			Links: hal.LinkMap{
				"self": {Href: b.Experiments(repoName)},
				"repo": {Href: b.Repo(repoName)},
			},
			Embedded: map[string][]experimentView{"experiments": items},
		})
	}
}

// experimentCreateRequest is the POST body for opening one.
type experimentCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// handleExperimentOpen serves POST /repos/{repo}/experiments.
//
// Create-or-resume, like the store op it calls: posting an existing name
// updates the description and returns 200 rather than colliding, because the
// UI's "open" button means "put me in this experiment" and an agent
// reconnecting should not have to know which of the two it is doing.
func handleExperimentOpen(b hal.URLBuilder, expiryDays int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		var req experimentCreateRequest
		if derr := json.NewDecoder(r.Body).Decode(&req); derr != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body",
				derr.Error(), r.URL.Path)
			return
		}
		// The refusals that are ABOUT THE REPO come first and name the repo's
		// state, so the client is not left reading a generic 400 for a
		// condition it cannot fix by changing the name.
		if ri.Subscribed() {
			hal.WriteProblem(w, http.StatusConflict, "Repository is a subscription",
				"a subscription follows a remote branch read-only and has no agent branch to fork an experiment from",
				r.URL.Path)
			return
		}
		if oerr := ri.OntologyError(); oerr != nil {
			hal.WriteProblem(w, http.StatusConflict, "Repository has no ontology",
				"this repository accepts no writes on any branch, experiments included: "+oerr.Error(),
				r.URL.Path)
			return
		}

		var (
			exp store.Experiment
			err error
		)
		if aerr := ri.WithRead(func(svc *store.Service) {
			exp, err = svc.Experiments().OpenExperiment(r.Context(), req.Name, req.Description, ri.AgentBranch())
		}); aerr != nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Store unavailable", aerr.Error(), r.URL.Path)
			return
		}
		if err != nil {
			writeExperimentError(w, r, "Could not open experiment", err)
			return
		}
		w.Header().Set("Location", b.Experiment(repoName, exp.Name))
		hal.WriteHAL(w, http.StatusCreated, experimentViewOf(b, repoName, ri, exp, expiryDays))
	}
}

// handleExperimentAction serves the three custom actions, POST
// /repos/{repo}/experiments/{name}/{commit|rollback|sync}, and DELETE
// /repos/{repo}/experiments/{name} (rollback).
//
// One handler parameterised by action rather than three near-identical ones:
// they share every step except the store call, and the shared steps are where
// the error rendering lives.
func handleExperimentAction(b hal.URLBuilder, action string, expiryDays int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		name := chi.URLParam(r, "name")
		ri := repos.RepoFromContext(r.Context())

		// REST takes the same resolutions as the MCP tool — the mirrored
		// surfaces must not disagree about what commit MEANS. It is a body on
		// a POST, optional and absent for an ordinary commit.
		resolutions, perr := decodeResolutions(r)
		if perr != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid resolutions", perr.Error(), r.URL.Path)
			return
		}
		if len(resolutions) > 0 && action != "commit" {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid resolutions",
				"resolutions settle a refused commit and are meaningless to "+action, r.URL.Path)
			return
		}

		var (
			res store.AgentReconcileResult
			err error
		)
		if aerr := ri.WithRead(func(svc *store.Service) {
			switch action {
			case "commit":
				res, err = svc.Experiments().CommitExperiment(r.Context(), name, resolutions)
			case "sync":
				res, err = svc.Experiments().SyncExperiment(r.Context(), name)
			case "rollback":
				err = svc.Experiments().RollbackExperiment(r.Context(), name)
			}
		}); aerr != nil {
			hal.WriteProblem(w, http.StatusServiceUnavailable, "Store unavailable", aerr.Error(), r.URL.Path)
			return
		}
		if err != nil {
			writeExperimentError(w, r, "Could not "+action+" experiment", err)
			return
		}

		// Commit and rollback DELETE the experiment, so there is nothing left
		// to render: 204 says so exactly, and a body describing a resource
		// that no longer exists would invite a client to re-read it.
		if action != "sync" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var (
			exp store.Experiment
			ok  bool
		)
		_ = ri.WithRead(func(svc *store.Service) {
			exp, ok, _ = svc.Experiments().GetExperiment(r.Context(), name)
		})
		if !ok {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		view := experimentViewOf(b, repoName, ri, exp, expiryDays)
		hal.WriteHAL(w, http.StatusOK, map[string]any{
			"experiment": view,
			"mode":       string(res.Mode),
		})
	}
}

// writeExperimentError maps the store's typed failures onto statuses a client
// can branch on.
//
// A refused COMMIT is the one that matters: it is a 409 carrying the
// conflicting paths, because the reader's next move is chosen by looking at
// WHICH paths, and a bare "conflict" string would make the UI guess.
//
// The detail does NOT tell the caller to sync. Sync resolves every collision
// in the agent branch's favour, so recommending it here would be recommending
// that the experiment's version of exactly these facts be overwritten
// unseen — which is why the UI has no sync control at all. Resolution needs
// all three versions, and that is an agent's job through knomit_experiment.
// decodeResolutions reads the optional resolutions body of a commit.
//
// "ours"/"theirs" mean the same thing here as on the MCP tool — ours is the
// EXPERIMENT (the merge source), theirs is the agent branch it lands on — for
// the reason the store's ResolutionSide doc gives: the two surfaces are one
// feature, and a client that learned the vocabulary from one must not be
// surprised by the other.
func decodeResolutions(r *http.Request) (map[string]store.Resolution, error) {
	if r.Body == nil {
		return nil, nil
	}
	var body struct {
		Resolutions map[string]json.RawMessage `json:"resolutions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil // no body at all: an ordinary commit
		}
		return nil, fmt.Errorf("body is not JSON: %w", err)
	}
	if len(body.Resolutions) == 0 {
		return nil, nil
	}
	out := make(map[string]store.Resolution, len(body.Resolutions))
	for path, raw := range body.Resolutions {
		var side string
		if err := json.Unmarshal(raw, &side); err == nil {
			switch side {
			case "ours":
				out[path] = store.Resolution{Side: store.ResolveSrc}
			case "theirs":
				out[path] = store.Resolution{Side: store.ResolveDst}
			default:
				return nil, fmt.Errorf("resolution for %q must be \"ours\", \"theirs\", or {\"body\": \"...\"}, got %q", path, side)
			}
			continue
		}
		var obj struct {
			Body *string `json:"body"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil || obj.Body == nil {
			return nil, fmt.Errorf("resolution for %q must be \"ours\", \"theirs\", or {\"body\": \"...\"}", path)
		}
		if *obj.Body == "" {
			return nil, fmt.Errorf("resolution body for %q is empty; to drop the fact, retract it instead", path)
		}
		out[path] = store.Resolution{Body: []byte(*obj.Body)}
	}
	return out, nil
}

func writeExperimentError(w http.ResponseWriter, r *http.Request, title string, err error) {
	var conflict *store.MergeConflictError
	if errors.As(err, &conflict) {
		hal.WriteProblemWithExtra(w, http.StatusConflict, "Experiment has conflicting changes",
			"both the experiment and the agent branch changed the same fact(s) since the fork; "+
				"nothing was merged and nothing changed. An agent resolves these with the "+
				"knomit_experiment tool, which can read all three versions of each fact; "+
				"otherwise roll the experiment back",
			r.URL.Path, map[string]any{"conflicting_paths": conflict.Paths})
		return
	}
	switch {
	case errors.Is(err, store.ErrNoSuchExperiment):
		hal.WriteProblem(w, http.StatusNotFound, "Experiment not found", err.Error(), r.URL.Path)
	case errors.Is(err, store.ErrInvalidExperimentName):
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid experiment name", err.Error(), r.URL.Path)
	case errors.Is(err, store.ErrOrphanExperimentRef):
		hal.WriteProblem(w, http.StatusConflict, "Branch already exists", err.Error(), r.URL.Path)
	case errors.Is(err, store.ErrStaleExperimentParent):
		hal.WriteProblem(w, http.StatusConflict, "Experiment is orphaned", err.Error(), r.URL.Path)
	default:
		hal.WriteProblem(w, http.StatusInternalServerError, title, err.Error(), r.URL.Path)
	}
}
