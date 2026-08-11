package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	knomitfact "knomit/internal/fact"
	"knomit/internal/refs"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/web/hal"
)

// FactWriter is the narrow write interface the fact POST/PUT/DELETE handlers
// depend on. Tests inject a stub; production wires through RepoInstance.WithRead.
type FactWriter interface {
	Write(ctx context.Context, ri *repos.RepoInstance, branch, path, content, message string) (string, error)
	Delete(ctx context.Context, ri *repos.RepoInstance, branch, path, message string) (string, error)
	// FactResolves backs the ref gate every write runs through. It is
	// FactExistsAt at the branch head — the SAME predicate the read path uses
	// to decide `fact` vs `broken`, walking back past retractions. The gate and
	// the reader must ask one question: a narrower "is it live right now" check
	// would reject writes whose refs the UI renders as live links.
	//
	// It lives on the WRITE interface, not the read one, so a new writer cannot
	// be introduced without supplying the check its writes are validated against.
	FactResolves(ctx context.Context, ri *repos.RepoInstance, branch, path string) (bool, error)

	// PriorRefs returns the refs the fact at path already carries, or nil when
	// there is no such fact. Those refs resolved at the commit that wrote them
	// and are never re-judged — see the refs gate's temporal contract.
	PriorRefs(ctx context.Context, ri *repos.RepoInstance, branch, path string) ([]string, error)
}

// writerGate builds the one Gate the REST write handlers use. Both POST and PUT
// call this rather than assembling a gate each, so "check then canonicalize" has
// a single spelling here exactly as it does in internal/mcp.
func writerGate(writer FactWriter, ri *repos.RepoInstance, branch string) refs.Gate {
	return refs.New(knomitfact.ID12(ri.ID()), func(ctx context.Context, path string) (bool, error) {
		return writer.FactResolves(ctx, ri, branch, path)
	})
}

// defaultFactWriter is the production FactWriter backed by the store.
type defaultFactWriter struct{}

func (defaultFactWriter) FactResolves(ctx context.Context, ri *repos.RepoInstance, branch, path string) (bool, error) {
	var (
		ok  bool
		err error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errFactNotFound
			return
		}
		// Empty commit = at the branch head, which is the commit this write is
		// about to become the parent of: the referrer's own commit.
		ok, err = svc.FactQuery().FactExistsAt(ctx, branch, path, "")
	})
	return ok, err
}

func (defaultFactWriter) PriorRefs(ctx context.Context, ri *repos.RepoInstance, branch, path string) ([]string, error) {
	var (
		refs []string
		err  error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errFactNotFound
			return
		}
		res, rerr := svc.Facts().ReadFact(ctx, branch, path, nil)
		if rerr != nil {
			// No prior version (a create, or a PUT to a fresh path) means
			// nothing is carried forward — not an error.
			if errors.Is(rerr, store.ErrPathNotFound) {
				return
			}
			err = rerr
			return
		}
		f, perr := knomitfact.ParseFact(path, res.Content)
		if perr != nil {
			// An unparseable prior version cannot tell us what it carried.
			// Exempting nothing is the safe direction — the write is checked
			// in full rather than waved through.
			return
		}
		refs = f.Refs
	})
	return refs, err
}

func (defaultFactWriter) Write(ctx context.Context, ri *repos.RepoInstance, branch, path, content, message string) (string, error) {
	var (
		commitHash string
		err        error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errFactNotFound
			return
		}
		res, werr := svc.Facts().WriteFact(ctx, branch, path, content, message, "update")
		if werr != nil {
			err = werr
			return
		}
		commitHash = res.CommitHash
	})
	return commitHash, err
}

func (defaultFactWriter) Delete(ctx context.Context, ri *repos.RepoInstance, branch, path, message string) (string, error) {
	var (
		commitHash string
		err        error
	)
	ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errFactNotFound
			return
		}
		h, derr := svc.Facts().DeleteFact(ctx, branch, path, message)
		if derr != nil {
			err = derr
			return
		}
		commitHash = h
	})
	return commitHash, err
}

// handleFactUpdate serves PUT /repos/{repo}/branches/{branch}/facts/{path...}.
// Body: JSON {"content": "<full markdown with YAML frontmatter>"}.
// Returns HAL FactView with 200 OK.
func handleFactUpdate(b hal.URLBuilder, writer FactWriter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		repoName := chi.URLParam(r, "repo")
		ri := repos.RepoFromContext(r.Context())

		branch := BranchFromContext(r.Context())
		path := chi.URLParam(r, "*")
		if path == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Missing fact path",
				"fact path is required", r.URL.Path)
			return
		}

		// Unlike the create paths, which derive their target from topic/category
		// and guard the path they construct, this handler receives the path
		// verbatim from the caller — it is also how a fresh path gets created
		// (PriorRefs returns nil for one), so a private segment here would be
		// indexed nowhere but still committed to git, permanently invisible.
		// Same rule as knomit_update — this endpoint is its REST twin, with a
		// fully caller-supplied path. knomit's own namespace is the exception:
		// .knomit/<area>/ holds state that WANTS to be invisible to readers.
		if knomitfact.IsPrivatePath(path) && !knomitfact.IsWritablePrivatePath(path) {
			hal.WriteProblem(w, http.StatusBadRequest, "Private path",
				path+": a path segment beginning with '.' is private and cannot hold a fact, "+
					"except under "+knomitfact.PrivateRoot+"/<area>/", r.URL.Path)
			return
		}

		var body struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid request body",
				err.Error(), r.URL.Path)
			return
		}

		// Parse BEFORE writing. The store performs no content validation, so
		// committing first would make an unparseable blob the branch HEAD for
		// this path while the client is handed a 422 saying the write failed —
		// and every later read of the path would then fail or drop it.
		f, err := knomitfact.ParseFact(path, body.Content)
		if err != nil {
			hal.WriteProblem(w, http.StatusUnprocessableEntity,
				"Failed to parse fact", err.Error(), r.URL.Path)
			return
		}

		// Same gate as knomit_learn / knomit_update / POST. A PUT replaces the
		// ref list wholesale, so only the refs it ADDS are checked — whatever
		// the stored version already carried resolved at its own commit and is
		// not re-judged, or editing a title would be refused over a citation
		// written months ago.
		prior, perr := writer.PriorRefs(r.Context(), ri, branch, path)
		if perr != nil {
			writeStoreError(w, r, perr, "Failed to read fact", branch)
			return
		}
		canonRefs, changed, gerr := writerGate(writer, ri, branch).Apply(r.Context(), path, f.Refs, prior)
		if gerr != nil {
			hal.WriteProblem(w, http.StatusUnprocessableEntity, "Unresolvable fact references",
				gerr.Error(), r.URL.Path)
			return
		}

		// The client's bytes are stored verbatim unless canonicalization
		// actually moved a ref — a PUT that needs no rewriting must not be
		// silently reformatted by a round trip through SerializeFact.
		content := body.Content
		if changed {
			f.Refs = canonRefs
			serialized, serr := knomitfact.SerializeFact(f)
			if serr != nil {
				hal.WriteProblem(w, http.StatusUnprocessableEntity,
					"Failed to serialize fact", serr.Error(), r.URL.Path)
				return
			}
			content = serialized
		}

		msg := "edit: update " + path + " via API"
		if _, err := writer.Write(r.Context(), ri, branch, path, content, msg); err != nil {
			writeStoreError(w, r, err, "Failed to write fact", branch)
			return
		}

		a := hal.Anchor{Branch: branch}
		// Resolver anchored at HEAD (commit:""): the just-written content is
		// now the active state of the branch, so HEAD walk-back correctly
		// classifies the fact's outgoing refs.
		resolver := readerRefResolver{ctx: r.Context(), reader: defaultFactReader{}, ri: ri, branch: branch, commit: ""}
		view := BuildFactView(b, repoName, a, "", f, resolver, knomitfact.ID12(ri.ID()))
		hal.WriteHAL(w, http.StatusOK, view)
	}
}

// handleFactDelete serves DELETE /repos/{repo}/branches/{branch}/facts/{path...}.
// Returns 204 No Content on success.
func handleFactDelete(b hal.URLBuilder, writer FactWriter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())

		branch := BranchFromContext(r.Context())
		path := chi.URLParam(r, "*")
		if path == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Missing fact path",
				"fact path is required", r.URL.Path)
			return
		}

		// A DELETE is a write, and this endpoint is the REST twin of
		// knomit_retract — which obeys the same rule. The path arrives verbatim
		// and DeleteFact performs no fact-shape check, so without this the
		// endpoint removes anything named: kb/.drafts/x.md, .github/ config,
		// or .knomit/ontology.yaml itself. Same condition, status and envelope
		// as handleFactUpdate above; knomit's own .knomit/<area>/ namespace is
		// the exception, because a job owns its state and may drop it.
		if knomitfact.IsPrivatePath(path) && !knomitfact.IsWritablePrivatePath(path) {
			hal.WriteProblem(w, http.StatusBadRequest, "Private path",
				path+": a path segment beginning with '.' is private and cannot hold a fact, "+
					"except under "+knomitfact.PrivateRoot+"/<area>/", r.URL.Path)
			return
		}

		msg := "manual-review: retract " + path
		if _, err := writer.Delete(r.Context(), ri, branch, path, msg); err != nil {
			writeStoreError(w, r, err, "Failed to delete fact", branch)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}
