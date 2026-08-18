package web

import (
	"encoding/json"
	"net/http"

	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

type probeInitializedRequest struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
	// Credentials are carried for the same reason probe-origin carries them:
	// an answer obtained anonymously is an answer about a different request.
	AuthMethod string `json:"auth_method"`
	AuthToken  string `json:"auth_token"`
}

// handleReposProbeInitialized serves POST /api/v1/repos:probe-initialized —
// "does this BRANCH of this remote already hold a knomit knowledge base?"
//
// Collection level, like probe-origin, because it runs BEFORE any repo exists.
// It is a SEPARATE endpoint rather than another field on probe-origin because
// the answer is per-branch and the branch is not known when that probe runs: a
// repository can carry .knomit/ontology.yaml on main and not on develop, so one
// origin has as many answers as it has branches.
//
// Every outcome is a 200, including "the check did not finish" — which comes
// back as an ABSENT `initialized` field, the third state. Only a refused
// request (the local-origin gate) is a 4xx.
//
// That third state is not a formality and clients must not collapse it. A
// repo's ontology is fixed at create time and never user-editable afterwards,
// so guessing "initialized" discards the ontology the user chose and guessing
// "not initialized" writes one over a knowledge base that already had its own.
// Both are unrecoverable. The wizard blocks and offers a retry instead.
func handleReposProbeInitialized(m *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req probeInitializedRequest
		// Bounded like the create endpoint's body. This one takes a URL and a
		// credential and nothing else, so anything approaching the cap is not a
		// request this handler can serve — and an unbounded decode on a public
		// endpoint is a hole regardless of what the fields mean.
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxCreateBodyBytes)).Decode(&req); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid body", err.Error(), r.URL.Path)
			return
		}
		if req.URL == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid body", "url is required", r.URL.Path)
			return
		}
		res, err := m.ProbeInitialized(r.Context(), repos.OriginSpec{
			URL:        req.URL,
			Branch:     req.Branch,
			AuthMethod: req.AuthMethod,
			AuthToken:  req.AuthToken,
		})
		if err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Origin rejected", err.Error(), r.URL.Path)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	}
}
