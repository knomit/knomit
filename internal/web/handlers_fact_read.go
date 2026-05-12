package web

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	knomitfact "knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// errFactNotFound is the sentinel the fact handler maps to 404 problem+json.
// Production code returns this from defaultFactReader when ReadFact surfaces
// store.ErrPathNotFound — distinguishing "no fact at this path/commit" from a
// real backend error (which propagates to 500 instead of masquerading as 404).
var errFactNotFound = errors.New("fact not found")

// FactReader is the narrow interface the HAL fact handler depends on.
// It combines reading a fact-at-anchor with checking whether a ref target
// exists at the same anchor, so the handler can treat them as a single
// dependency for test injection.
type FactReader interface {
	// Read loads the fact at (repo, anchor, path). Returns errFactNotFound
	// when the path does not exist; other store errors are propagated as-is
	// for the handler to surface as 500. The returned headOrCommit string is
	// the resolved commit to stamp into the view's as_of (HEAD's head-commit
	// when the anchor is HEAD, or the pinned sha unchanged when commit-
	// anchored).
	//
	// fallback is only consulted for commit-anchored reads: when set, a
	// path missing at the pinned commit walks back to the most recent
	// ancestor where it existed, and headOrCommit reflects that ancestor.
	Read(ri *repos.RepoInstance, a hal.Anchor, path string, fallback bool) (_ knomitfact.Fact, headOrCommit string, _ error)

	// Exists reports whether the given fact path is visible on the anchor.
	Exists(path string) bool
}

// defaultFactReader is the production FactReader wired over the store.
// Stateless adapter — takes RepoInstance per call and uses WithRead.
type defaultFactReader struct{}

func (defaultFactReader) Read(
	ri *repos.RepoInstance,
	a hal.Anchor,
	path string,
	fallback bool,
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
			// Opt-in fallback: when the file doesn't exist at the pinned commit,
			// walk back to the most recent ancestor where it did. Only the
			// History view's retract-commit case sets this — when it's off the
			// caller wants the raw "not found" answer.
			if fallback && !a.IsHEAD() && errors.Is(rerr, store.ErrPathNotFound) {
				fbOpts := &store.ReadFactOpts{BeforeCommit: a.Commit}
				fbRes, fbErr := svc.Facts().ReadFact(contextTODO(), a.Branch, path, fbOpts)
				if fbErr != nil {
					if errors.Is(fbErr, store.ErrPathNotFound) {
						err = errFactNotFound
						return
					}
					log.Error().Err(fbErr).Str("path", path).Str("commit", a.Commit).Msg("ReadFact fallback failed")
					err = fbErr
					return
				}
				parsed, perr := knomitfact.ParseFact(path, fbRes.Content)
				if perr != nil {
					err = perr
					return
				}
				f = parsed
				head = fbRes.FromCommit
				return
			}
			if errors.Is(rerr, store.ErrPathNotFound) {
				err = errFactNotFound
				return
			}
			log.Error().Err(rerr).Str("path", path).Bool("head_anchored", a.IsHEAD()).Msg("ReadFact failed")
			err = rerr
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
			if herr != nil {
				log.Error().Err(herr).Str("branch", a.Branch).Msg("HeadCommit failed")
				err = herr
				return
			}
			head = h
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
		// HEAD-anchored reads ignore the fallback parameter (the file either
		// exists at HEAD or it doesn't — there's no "previous version" to
		// fall back to). Always pass false here.
		f, head, err := reader.Read(ri, a, path, false)
		if err != nil {
			if errors.Is(err, errFactNotFound) {
				hal.WriteProblem(w, http.StatusNotFound, "Fact not found",
					`no fact at path "`+path+`" on branch "`+branch+`"`, r.URL.Path)
				return
			}
			writeStoreError(w, r, err, "Failed to read fact", branch)
			return
		}

		view := BuildFactView(b, repoName, a, head, f, reader)
		hal.WriteHAL(w, http.StatusOK, view)
	}
}
