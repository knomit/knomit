package web

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	knomitfact "knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/web/hal"
)

// factCreateRequest is the JSON body for POST /repos/{repo}/branches/{branch}/facts.
type factCreateRequest struct {
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Kind       string   `json:"kind"`
	Type       string   `json:"type"`
	Domain     []string `json:"domain"`
	Entities   []string `json:"entities"`
	Refs       []string `json:"refs"`
	Confidence float64  `json:"confidence"`
	Sources    int      `json:"sources"`
}

// handleFactCreate serves POST /repos/{repo}/branches/{branch}/facts.
// Allocates a new path via fact.BuildFactPath, serializes the fact, writes it
// to the branch and returns 201 Created with a Location header and FactView.
func handleFactCreate(b hal.URLBuilder, m *repos.Manager, ontologyRoot string, writer FactWriter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := m.Get(repoName)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}

		branch := BranchFromContext(r.Context())

		var req factCreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body",
				err.Error(), r.URL.Path)
			return
		}
		if req.Title == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Missing title",
				"title is required", r.URL.Path)
			return
		}

		// Determine the domain-based category for path allocation.
		domain := req.Domain
		if len(domain) == 0 {
			domain = []string{"uncategorized"}
		}
		// Use the first domain segment as both topic and category so the path
		// is ontologyRoot/domain[0]/domain[0]/<uuid8>.md, which is a reasonable
		// default that tests can override via the request body.
		topic := domain[0]
		category := domain[0]
		if len(domain) > 1 {
			category = domain[1]
		}

		path := knomitfact.BuildFactPath(ontologyRoot, topic, category)

		// Resolve kind and leaf type. SerializeFact validates the (kind,
		// type) pair below, so we don't pre-validate here — that lets a
		// single rule reject mismatched values (e.g. pragmatic kind with
		// epistemic leaf) instead of having two checks drift.
		kind := knomitfact.Kind(req.Kind)
		if kind == "" {
			kind = knomitfact.DefaultKind
		}
		eType := knomitfact.Type(req.Type)
		if eType == "" && kind == knomitfact.Epistemic {
			eType = knomitfact.DefaultEpistemicType
		}

		f := knomitfact.NewFact(path)
		f.Title = req.Title
		f.Body = req.Body
		f.Kind = kind
		f.Type = eType
		f.Domain = domain
		f.Entities = req.Entities
		if f.Entities == nil {
			f.Entities = []string{}
		}
		f.Refs = req.Refs
		if f.Refs == nil {
			f.Refs = []string{}
		}
		f.Confidence = req.Confidence
		f.Sources = req.Sources

		content, err := knomitfact.SerializeFact(f)
		if err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Failed to serialize fact",
				err.Error(), r.URL.Path)
			return
		}
		msg := "create: " + req.Title + " via API"

		if _, err := writer.Write(r.Context(), ri, branch, path, content, msg); err != nil {
			writeStoreError(w, r, err, "Failed to write fact", branch)
			return
		}

		a := hal.Anchor{Branch: branch}
		// Resolver anchored at HEAD (commit:""): the just-created fact is
		// now the active state of the branch, so HEAD walk-back correctly
		// classifies outgoing refs in the response view.
		resolver := readerRefResolver{ctx: r.Context(), reader: defaultFactReader{}, ri: ri, branch: branch, commit: ""}
		view := BuildFactView(b, repoName, a, "", f, resolver)
		locationURL := b.Fact(repoName, a, path)
		w.Header().Set("Location", locationURL)
		hal.WriteHAL(w, http.StatusCreated, view)
	}
}
