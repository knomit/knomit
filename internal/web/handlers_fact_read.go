package web

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	knomitfact "knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// errFactNotFound is the sentinel the fact handler maps to 404 problem+json.
// Production code returns this (wrapped or otherwise) from the real
// defaultFactReader when FactIndex.ReadFact surfaces a not-found condition.
var errFactNotFound = errors.New("fact not found")

// FactReader is the narrow interface the HAL fact handler depends on.
// It combines reading a fact-at-anchor with checking whether a ref target
// exists at the same anchor. Splitting these two concerns into one interface
// lets the handler treat them as a single dependency for test injection;
// production code wires both methods through RepoInstance.WithRead.
type FactReader interface {
	// Read loads the fact at (repo, anchor, path). Returns errFactNotFound
	// when the path does not exist on the branch (or at the commit). The
	// returned headOrCommit string is the resolved commit the caller should
	// stamp into the view's as_of — HEAD's head-commit when the anchor is
	// HEAD, or the pinned sha unchanged when the anchor is commit-anchored.
	Read(ri *repos.RepoInstance, a hal.Anchor, path string) (_ knomitfact.Fact, headOrCommit string, _ error)

	// Exists reports whether the given fact path is visible on the anchor
	// (same semantics as Read's "exists" check — used for structured refs).
	Exists(path string) bool
}

// defaultFactReader is the production FactReader wired over the store. This
// type is a stateless adapter — it takes the RepoInstance per call and uses
// WithRead to access the store. Plan 02 extends this to support commit-
// anchored reads via ReadFactOpts.AtCommit.
type defaultFactReader struct{}

func (defaultFactReader) Read(
	ri *repos.RepoInstance,
	a hal.Anchor,
	path string,
) (knomitfact.Fact, string, error) {
	var (
		f    knomitfact.Fact
		head string
		err  error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errFactNotFound
			return
		}
		opts := &store.ReadFactOpts{WithHash: false}
		if !a.IsHEAD() {
			opts.AtCommit = a.Commit
		}
		res, rerr := svc.Facts().ReadFact(contextTODO(), a.Branch, path, opts)
		if rerr != nil {
			err = errFactNotFound
			return
		}
		parsed, perr := knomitfact.ParseFact(path, res.Content)
		if perr != nil {
			err = perr
			return
		}
		f = parsed
		if a.IsHEAD() {
			h, herr := svc.Branches().HeadCommit(contextTODO(), a.Branch)
			if herr == nil {
				head = h
			}
		} else {
			head = a.Commit
		}
	})
	return f, head, err
}

// Exists on defaultFactReader is stateless and always returns false — this
// is a placeholder for Plan 01. Plan 02 replaces it with a per-request
// resolver that checks against the current anchor. For now, structured refs
// in the single-fact response default to "broken" unless the test override
// says otherwise.
func (defaultFactReader) Exists(string) bool { return false }

// handleHALFact serves GET /api/v1/repos/{repo}/branches/{branch}/facts/{path...}.
// It also dispatches sub-resource requests (*/commits, */incoming, */outgoing).
func handleHALFact(b hal.URLBuilder, m *repos.Manager, reader FactReader, subProvider factSubProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		branch := BranchFromContext(r.Context())

		// Dispatch sub-resource requests before any other processing.
		if dispatchFactSubResource(b, m, subProvider, repoName, branch, w, r) {
			return
		}

		ri := m.Get(repoName)
		if ri == nil {
			hal.WriteProblem(w, http.StatusNotFound, "Repo not found",
				`no repo named "`+repoName+`"`, r.URL.Path)
			return
		}
		path := chi.URLParam(r, "*")
		if path == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Missing fact path",
				"fact path is required", r.URL.Path)
			return
		}

		a := hal.Anchor{Branch: branch}
		f, head, err := reader.Read(ri, a, path)
		if err != nil {
			if errors.Is(err, errFactNotFound) {
				hal.WriteProblem(w, http.StatusNotFound, "Fact not found",
					`no fact at path "`+path+`" on branch "`+branch+`"`, r.URL.Path)
				return
			}
			hal.WriteProblem(w, http.StatusInternalServerError,
				"Failed to read fact", err.Error(), r.URL.Path)
			return
		}

		view := BuildFactView(b, repoName, a, head, f, reader)
		hal.WriteHAL(w, http.StatusOK, view)
	}
}
