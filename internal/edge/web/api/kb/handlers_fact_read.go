package kb

import (
	"context"
	"errors"
	"knomit/internal/edge/web/httpx"
	"net/http"

	"knomit/internal/engine/store/model"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/repos"
	knomitfact "knomit/internal/domain/fact"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/facts"
)

// errFactNotFound is the sentinel the fact handlers map to 404 problem+json.
// readFact returns it when ReadFact surfaces model.ErrPathNotFound —
// distinguishing "no fact at this path/commit" from a real backend error
// (which propagates to 500 instead of masquerading as 404).
var errFactNotFound = errors.New("fact not found")

// readerRefResolver is what buildRefViews and buildFactView take to classify a
// fact-path ref: Exists asks the store whether a ref target has a navigable
// version at the per-request anchor (ri, branch, commit) baked into it. The
// handler builds one of these per response so the resolver carries the same
// anchor as the surrounding fact view. It is a concrete struct, not an
// interface: it has one implementation, and tests build it over a real store.
type readerRefResolver struct {
	// ctx is baked in for the same reason the anchor is: buildRefViews calls
	// Exists once per ref with only the ref's path, so the request context
	// travels in the struct with the anchor. It is request-scoped — this struct
	// must not outlive the handler invocation that built it.
	ctx    context.Context
	ri     *repos.RepoInstance
	branch string
	commit string // "" for HEAD
}

// Exists reports whether path has a navigable version at the resolver's
// anchor, through FactQuery.FactExistsAt (factExistsAt). Honors the
// historical-graph invariant: commit-anchored views classify refs against the
// anchor (with walk-back through retractions), and HEAD views use
// branch_facts (with the same walk-back fallback for paths that have been
// retracted at HEAD).
//
// On error the ref degrades to "broken" in the view (Exists answers per ref,
// and one ref's failed lookup does not fail the whole response), but the error
// is logged so transient DB issues are observable rather than silently
// misclassifying refs.
//
// Cancellation is the one error class that logs at DEBUG instead. A page with
// N refs calls this N times, so a client that hangs up mid-render would emit N
// ERROR records describing nothing more than "the reader left" — noise that
// would drown the real DB faults this log exists to surface. The ref still
// degrades to "broken", but nobody is left to receive the response anyway.
func (r readerRefResolver) Exists(path string) bool {
	ok, err := factExistsAt(r.ctx, r.ri, r.branch, path, r.commit)
	if err != nil {
		ev := log.Error()
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			ev = log.Debug()
		}
		ev.Err(err).
			Str("branch", r.branch).
			Str("path", path).
			Str("commit", r.commit).
			Msg("FactExistsAt failed — ref will classify as broken")
		return false
	}
	return ok
}

// readFact loads the fact at (ri, anchor, path) through the store's ReadFact.
// It returns errFactNotFound when the path does not exist; other store errors
// are returned as-is for the handler to surface as 500. A store that cannot be
// reached is WithRead's error, returned as-is (never errFactNotFound: nothing
// was looked up), which httpx.WriteStoreError answers 503. The returned head is
// the resolved commit to stamp into the view's as_of: the branch's head commit
// when the anchor is HEAD, or the pinned sha unchanged when commit-anchored.
//
// fallback is only consulted for commit-anchored reads: when set, a path
// missing at the pinned commit walks back to the most recent ancestor where it
// existed, and head is that ancestor.
func readFact(
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
	if aerr := ri.WithRead(func(svc *store.Service) {
		opts := &facts.ReadFactOpts{WithHash: false}
		if !a.IsHEAD() {
			opts.AtCommit = a.Commit
		}
		res, rerr := svc.Facts().ReadFact(ctx, a.Branch, path, opts)
		if rerr != nil {
			// Opt-in fallback: when the file doesn't exist at the pinned commit,
			// walk back to the most recent ancestor where it did. Only the
			// History view's retract-commit case sets this — when it's off the
			// caller wants the raw "not found" answer.
			if fallback && !a.IsHEAD() && errors.Is(rerr, model.ErrPathNotFound) {
				fbOpts := &facts.ReadFactOpts{BeforeCommit: a.Commit}
				fbRes, fbErr := svc.Facts().ReadFact(ctx, a.Branch, path, fbOpts)
				if fbErr != nil {
					if errors.Is(fbErr, model.ErrPathNotFound) {
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
			if errors.Is(rerr, model.ErrPathNotFound) {
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
			h, herr := svc.HeadCommit(ctx, a.Branch)
			if herr != nil {
				log.Error().Err(herr).Str("branch", a.Branch).Msg("HeadCommit failed")
				err = herr
				return
			}
			head = h
		} else {
			head = a.Commit
		}
	}); aerr != nil {
		return knomitfact.Fact{}, "", aerr
	}
	return f, head, err
}

// handleHALFact serves GET /api/v1/repos/{repo}/branches/{branch}/facts/{path...}.
// It also dispatches sub-resource requests (*/commits, */incoming, */outgoing).
func handleHALFact(b hal.URLBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		branch := httpx.BranchFromContext(r.Context())

		ri := repos.RepoFromContext(r.Context())

		// A file under .knomit/ is readable by its exact path, raw (user
		// ruling 2026-10-06); PUT/DELETE of it stay refused.
		if serveSystemFileRead(w, r, ri, branch, "", chi.URLParam(r, "*")) {
			return
		}
		// F25: every OTHER dot path is closed to the fact endpoints for reads
		// too — sub-resources included, so this runs before their dispatch.
		if refusePrivateRead(w, r, chi.URLParam(r, "*")) {
			return
		}

		// Dispatch sub-resource requests before any other processing.
		if dispatchFactSubResource(b, ri, repoName, branch, w, r) {
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
		f, head, err := readFact(r.Context(), ri, a, path, false)
		if err != nil {
			if errors.Is(err, errFactNotFound) {
				hal.WriteProblem(w, http.StatusNotFound, "Fact not found",
					`no fact at path "`+path+`" on branch "`+branch+`"`, r.URL.Path)
				return
			}
			httpx.WriteStoreError(w, r, err, "Failed to read fact", branch)
			return
		}

		// HEAD anchor — empty commit. Ref kind classification walks back from
		// the branch's HEAD via FactExistsAt to honor the historical-graph
		// invariant (retracted-but-recoverable targets still classify as fact).
		resolver := readerRefResolver{ctx: r.Context(), ri: ri, branch: branch, commit: ""}
		view := buildFactView(b, repoName, a, head, f, resolver, knomitfact.ID12(ri.ID()))
		view.AsOf.Date = factVersionDate(r.Context(), ri, a.Branch, f.Path(), view.AsOf.Commit)
		hal.WriteHAL(w, http.StatusOK, view)
	}
}
