package web

import (
	"context"
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
	Read(ctx context.Context, ri *repos.RepoInstance, a hal.Anchor, path string, fallback bool) (_ knomitfact.Fact, headOrCommit string, _ error)

	// Exists reports whether `path` has a navigable version at (branch,
	// commit). Commit == "" means HEAD-anchored. Implementations must walk
	// back through retractions per the historical-graph invariant: a target
	// retracted before the anchor is still "exists" if any prior version
	// is reachable via fallback-before.
	Exists(ctx context.Context, ri *repos.RepoInstance, branch, path, commit string) bool
}

// readerRefResolver is a thin RefResolver that delegates Exists to a
// FactReader, baking in the per-request anchor (ri, branch, commit). The
// handler builds one of these per response so the resolver carries the
// same anchor as the surrounding fact view.
type readerRefResolver struct {
	// ctx is baked in for the same reason the anchor is: RefResolver.Exists
	// takes only a path, and BuildRefViews calls it once per ref, so there is
	// no parameter to thread the request context through. It is request-scoped
	// — this struct must not outlive the handler invocation that built it.
	// Widening RefResolver.Exists to take a ctx instead would ripple through
	// BuildRefViews and every RefResolver stub for no lifetime-safety gain.
	ctx    context.Context
	reader FactReader
	ri     *repos.RepoInstance
	branch string
	commit string // "" for HEAD
}

func (r readerRefResolver) Exists(path string) bool {
	return r.reader.Exists(r.ctx, r.ri, r.branch, path, r.commit)
}

// defaultFactReader is the production FactReader wired over the store.
// Stateless adapter — takes RepoInstance per call and uses WithRead.
type defaultFactReader struct{}

func (defaultFactReader) Read(
	ctx context.Context,
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
		res, rerr := svc.Facts().ReadFact(ctx, a.Branch, path, opts)
		if rerr != nil {
			// Opt-in fallback: when the file doesn't exist at the pinned commit,
			// walk back to the most recent ancestor where it did. Only the
			// History view's retract-commit case sets this — when it's off the
			// caller wants the raw "not found" answer.
			if fallback && !a.IsHEAD() && errors.Is(rerr, store.ErrPathNotFound) {
				fbOpts := &store.ReadFactOpts{BeforeCommit: a.Commit}
				fbRes, fbErr := svc.Facts().ReadFact(ctx, a.Branch, path, fbOpts)
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
			h, herr := svc.Branches().HeadCommit(ctx, a.Branch)
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

// Exists routes to SearchIndex.FactExistsAt via the per-request store
// snapshot. Honors the historical-graph invariant: passes commit through
// so commit-anchored reads classify refs against the anchor (with walk-
// back through retractions), and HEAD reads use branch_facts (with the
// same walk-back fallback for paths that have been retracted at HEAD).
//
// On error the ref degrades to "broken" in the view (the resolver
// interface can't propagate errors per-ref without failing the whole
// response), but the error is logged so transient DB issues are
// observable rather than silently misclassifying refs.
//
// Cancellation is the one error class that logs at DEBUG instead. A page with
// N refs calls this N times, so a client that hangs up mid-render would emit N
// ERROR records describing nothing more than "the reader left" — noise that
// would drown the real DB faults this log exists to surface. The ref still
// degrades to "broken", but nobody is left to receive the response anyway.
func (defaultFactReader) Exists(ctx context.Context, ri *repos.RepoInstance, branch, path, commit string) bool {
	var exists bool
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		ok, err := svc.FactQuery().FactExistsAt(ctx, branch, path, commit)
		if err != nil {
			ev := log.Error()
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				ev = log.Debug()
			}
			ev.Err(err).
				Str("branch", branch).
				Str("path", path).
				Str("commit", commit).
				Msg("FactExistsAt failed — ref will classify as broken")
			return
		}
		exists = ok
	})
	return exists
}

// handleHALFact serves GET /api/v1/repos/{repo}/branches/{branch}/facts/{path...}.
// It also dispatches sub-resource requests (*/commits, */incoming, */outgoing).
func handleHALFact(b hal.URLBuilder, reader FactReader, subProvider factSubProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		branch := BranchFromContext(r.Context())

		// Dispatch sub-resource requests before any other processing.
		if dispatchFactSubResource(b, subProvider, repoName, branch, w, r) {
			return
		}

		ri := repos.RepoFromContext(r.Context())
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
		f, head, err := reader.Read(r.Context(), ri, a, path, false)
		if err != nil {
			if errors.Is(err, errFactNotFound) {
				hal.WriteProblem(w, http.StatusNotFound, "Fact not found",
					`no fact at path "`+path+`" on branch "`+branch+`"`, r.URL.Path)
				return
			}
			writeStoreError(w, r, err, "Failed to read fact", branch)
			return
		}

		// HEAD anchor — empty commit. Ref kind classification walks back from
		// the branch's HEAD via FactExistsAt to honor the historical-graph
		// invariant (retracted-but-recoverable targets still classify as fact).
		resolver := readerRefResolver{ctx: r.Context(), reader: reader, ri: ri, branch: branch, commit: ""}
		view := BuildFactView(b, repoName, a, head, f, resolver)
		hal.WriteHAL(w, http.StatusOK, view)
	}
}
