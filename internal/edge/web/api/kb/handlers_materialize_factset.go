package kb

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/engine/store/model"
	"knomit/internal/engine/store/sessions"
	"knomit/internal/foundation/config"
)

// The factset routes: GET /repos/{repo}/branches/{branch}/search/factset pages a
// search materialized as a factset (materialize.FactSetFirstPage / FactSetNextPage,
// the same pages knomit_materialize's factset mode returns),
// GET …/search/factset/export sends the whole factset as one agent document, and
// DELETE …/search/factset?cursor= closes it. The lens twins
// (/lenses/{lens}/search/factset, …/export, DELETE) are the same handlers over
// the lens binding: a lens is a virtual repo, so its FactSet takes the same
// parameters and returns the same shapes, plus what the engine adds on a lens
// (env.anchors, repo_name on unit roots, the lens_repo leaf) and the lens
// list's repo= narrowing.
const (
	factsetRouteSuffix  = "/search/factset"
	factsetExportSuffix = "/export"
)

// factsetSearchParams are the search filters the first call reads: the repo
// /search parameters, plus `sort` (relevance = /search, recent = the /facts
// collection's sort=recent) and since_fork, exactly as the UI's results list
// sends them.
var factsetSearchParams = map[string]bool{
	"q": true, "entities": true, "domain": true, "domain_exact": true, "path": true,
	"min_confidence": true, "min_similarity": true, "type": true, "exclude_type": true,
	"kind": true, "exclude_kind": true, "origin": true, "ep": true, "since_fork": true,
	"motifs": true, "motif_match": true, "sort": true,
}

// factsetOptionParams are the factset's own first-call parameters.
var factsetOptionParams = map[string]bool{
	"facts": true, "depth": true, "max_nodes": true, "unpin": true, "cross_kb": true, "limit": true,
}

// factsetUnitParam is the seek parameter, the only one a cursor call may add.
const factsetUnitParam = "unit"

// factsetLensRepoParam is the lens list's repo= narrowing (repeatable, a
// member's repo name), accepted on the lens routes only: named exactly as
// GET /lenses/{lens}/search names it.
const factsetLensRepoParam = "repo"

// factsetSurface is what differs between the repo and the lens factset
// routes: where the binding comes from, and whether the first call reads the
// lens list's parameters and wiring.
type factsetSurface struct {
	// binding resolves the request's binding: the {repo}/{branch} of the URL,
	// or the {lens} LensMiddleware resolved.
	binding func(r *http.Request) *repos.Binding
	// lens: the first call accepts repo= and passes the lens list's motif
	// cluster reader and query embedding (lensMotifClusters, lensEmbed).
	lens bool
}

// repoFactSets is the surface of /repos/{repo}/branches/{branch}/search/factset.
var repoFactSets = factsetSurface{binding: func(r *http.Request) *repos.Binding {
	return repos.NewBindingOfRepo(repos.RepoFromContext(r.Context()), httpx.BranchFromContext(r.Context()))
}}

// lensFactSets is the surface of /lenses/{lens}/search/factset.
var lensFactSets = factsetSurface{lens: true, binding: func(r *http.Request) *repos.Binding {
	return repos.BindingFromContext(r.Context())
}}

// factsetScope pins a factset to the binding (pin and read set) and to the
// caller's principal ALONE (materialize.FactSetOwner): not to the route, so a
// factset made over MCP by the same principal with the same binding pages,
// exports and closes here, and the reverse.
func factsetScope(r *http.Request, b *repos.Binding, ts *sessions.ToolIndex) materialize.CursorScope {
	return materialize.CursorScope{
		Sessions: ts, Branch: b.WriteMountBranch(), Pin: b.PinID(),
		ReadSet: federate.ReadSetFingerprint(b), Owner: materialize.FactSetOwner(r.Context()),
	}
}

// handleSearchFactSet serves GET …/search/factset on surface sf.
func handleSearchFactSet(sf factsetSurface, emb model.Embedder, mgr *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b := sf.binding(r)
		q := r.URL.Query()

		cursor, isResume := q[materializeCursorParam]
		unit := 0
		if isResume {
			for k := range q {
				if k != materializeCursorParam && k != factsetUnitParam {
					hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
						strconv.Quote(k)+" cannot be passed with cursor: the factset was fixed by the first call; pass only cursor (and unit to seek), or start over without it", r.URL.Path)
					return
				}
			}
			if q.Get(materializeCursorParam) == "" {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", "cursor is empty", r.URL.Path)
				return
			}
			if v, ok := q[factsetUnitParam]; ok {
				n, err := strconv.Atoi(v[0])
				if err != nil || n < 1 {
					hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", "unit must be a whole number of at least 1", r.URL.Path)
					return
				}
				unit = n
			}
		}

		svc, release, err := b.Write().Acquire()
		if err != nil {
			httpx.WriteStoreUnavailable(w, r)
			return
		}
		defer release()
		sc := factsetScope(r, b, svc.ToolSession())
		ctx := r.Context()

		var page materialize.FactSetPage
		if isResume {
			page, err = materialize.FactSetNextPage(ctx, sc, b, mgr, callerMayRead(r), cursor[0], unit)
		} else {
			req, ok := factsetRequest(w, r, q, sf, b, mgr, emb)
			if !ok {
				return
			}
			page, _, err = materialize.FactSetFirstPage(ctx, sc, req)
		}
		if err != nil {
			writeFactSetError(w, r, err)
			return
		}
		out, err := json.Marshal(page)
		if err != nil {
			writeFactSetError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}
}

// handleCloseFactSet serves DELETE …/search/factset?cursor=…: it closes the
// factset of cursor (any of its cursors) and answers 204. Another owner's
// factset, and one that does not exist, answer the same 404 as a GET with
// that cursor, and nothing is closed.
func handleCloseFactSet(sf factsetSurface) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b := sf.binding(r)
		q := r.URL.Query()
		for k := range q {
			if k != materializeCursorParam {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
					"closing a factset takes only cursor, got "+strconv.Quote(k), r.URL.Path)
				return
			}
		}
		cursor := q.Get(materializeCursorParam)
		if cursor == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", "cursor is required", r.URL.Path)
			return
		}
		svc, release, err := b.Write().Acquire()
		if err != nil {
			httpx.WriteStoreUnavailable(w, r)
			return
		}
		defer release()
		if err := materialize.CloseFactSet(r.Context(), factsetScope(r, b, svc.ToolSession()), cursor); err != nil {
			writeFactSetError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleSearchFactSetExport serves GET …/search/factset/export?cursor=…: the whole
// factset as one agent document, an attachment, streamed a chunk at a time.
func handleSearchFactSetExport(sf factsetSurface, mgr *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b := sf.binding(r)
		q := r.URL.Query()
		for k := range q {
			if k != materializeCursorParam {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
					"the export takes only cursor, got "+strconv.Quote(k), r.URL.Path)
				return
			}
		}
		cursor := q.Get(materializeCursorParam)
		if cursor == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", "cursor is required", r.URL.Path)
			return
		}
		svc, release, err := b.Write().Acquire()
		if err != nil {
			httpx.WriteStoreUnavailable(w, r)
			return
		}
		defer release()
		started := false
		err = materialize.FactSetExport(r.Context(), factsetScope(r, b, svc.ToolSession()), b, mgr, callerMayRead(r), cursor,
			func(env materialize.FactSetEnv) io.Writer {
				started = true
				w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
				w.Header().Set("Content-Disposition", `attachment; filename="`+materialize.FactSetExportFileName(env, cursor)+`"`)
				w.WriteHeader(http.StatusOK)
				return &deadlineWriter{w: w, rc: http.NewResponseController(w)}
			})
		switch {
		case err == nil:
		case !started:
			writeFactSetError(w, r, err)
		default:
			// The export is never partial: a failure after the first bytes
			// aborts the connection, so the client sees a failed download,
			// not a short file.
			log.Error().Err(err).Msg("factset export failed mid-stream")
			panic(http.ErrAbortHandler)
		}
	}
}

// factsetExportWriteTimeout bounds each write of an export's stream. The
// listeners run with no WriteTimeout (a long download is legitimate), so
// without it a client that stops reading would hold its reader and its
// FactSet's cache entry forever; with it, a write that cannot complete in this time fails,
// and the export aborts (never a short file). It is the server's FactSet
// stall limit, which also caps session.factset_anchor_wait.
const factsetExportWriteTimeout = config.FactSetStallLimit

// deadlineWriter sets the connection's write deadline before every write.
// A ResponseWriter that cannot (a test recorder) just writes.
type deadlineWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (d *deadlineWriter) Write(p []byte) (int, error) {
	_ = d.rc.SetWriteDeadline(time.Now().Add(factsetExportWriteTimeout))
	return d.w.Write(p)
}

// factsetRequest parses a first call. On failure it has written the response.
func factsetRequest(w http.ResponseWriter, r *http.Request, q url.Values, sf factsetSurface,
	b *repos.Binding, mgr *repos.Manager, emb model.Embedder) (materialize.FactSetRequest, bool) {
	bad := func(detail string) (materialize.FactSetRequest, bool) {
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", detail, r.URL.Path)
		return materialize.FactSetRequest{}, false
	}
	for k := range q {
		if !factsetSearchParams[k] && !factsetOptionParams[k] && (!sf.lens || k != factsetLensRepoParam) {
			return bad("unknown parameter " + strconv.Quote(k))
		}
	}
	s := materialize.FactSetSearch{
		Text: q.Get("q"), Entities: splitCSV(q.Get("entities")), Domains: splitCSV(q.Get("domain")),
		DomainExact: boolParam(q.Get("domain_exact")), Path: q.Get("path"),
		Types: splitCSV(q.Get("type")), ExcludeTypes: splitCSV(q.Get("exclude_type")),
		Kinds: splitCSV(q.Get("kind")), ExcludeKinds: splitCSV(q.Get("exclude_kind")),
		Origins: splitCSV(q.Get("origin")), Episodes: splitCSV(q.Get("ep")),
		Motifs: splitCSV(q.Get("motifs")), MotifMatch: q.Get("motif_match"),
		SinceFork: boolParam(q.Get("since_fork")), Sort: q.Get("sort"),
	}
	if sf.lens {
		s.Repos = q[factsetLensRepoParam]
	}
	if s.Sort == "" {
		s.Sort = materialize.SortRelevance
	}
	for _, p := range []struct {
		name string
		dst  *float64
	}{{"min_confidence", &s.MinConfidence}, {"min_similarity", &s.MinSimilarity}} {
		if v := q.Get(p.name); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return bad("invalid " + p.name + " value")
			}
			*p.dst = f
		}
	}
	req := materialize.FactSetRequest{
		Binding: b, Manager: mgr, MayRead: callerMayRead(r), Search: s,
		Facts: materialize.DefaultFactSetFacts, Limit: materialize.HumanDefaultLimit,
	}
	switch {
	case sf.lens && b.IsLens():
		// The lens list's wiring (GET /lenses/{lens}/search): the query is
		// embedded ONCE for the whole fan-out and motif terms widen to their
		// merged cluster over the lens's read set, so the FactSet's roots are
		// the lens list's.
		req.Embed, req.Clusters = lensEmbed(emb), lensMotifClusters
	case emb != nil:
		// A repository, and a one-member lens (the engine serves it on the
		// repository path, byte for byte the write repo's FactSet).
		req.Embed = emb.EmbedQuery
	}
	for _, p := range []struct {
		name string
		dst  *int
	}{{"facts", &req.Facts}, {"limit", &req.Limit}} {
		if v := q.Get(p.name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return bad(p.name + " must be a whole number")
			}
			*p.dst = n
		}
	}
	oq := url.Values{}
	for _, k := range []string{"depth", "max_nodes", "unpin", "cross_kb"} {
		if v, ok := q[k]; ok {
			oq[k] = v
		}
	}
	opts, err := materializeOptions(oq)
	if err != nil {
		return bad(err.Error())
	}
	req.Options = opts
	if s.SinceFork {
		// since_fork is the WRITE member's experiment on a lens (ruling Lens
		// 6); on a repository the write member is the repository.
		paths, ok := experimentChangedPaths(w, r, b.Write(), b.WriteMountBranch())
		if !ok {
			return materialize.FactSetRequest{}, false
		}
		req.PathsIn, req.NoResults = paths, len(paths) == 0
	}
	return req, true
}

func boolParam(v string) bool { return v == "true" || v == "1" }

// writeFactSetError maps a factset error to its answer. Internal error text never
// reaches the caller: a store failure is logged and answered generically.
//
// A lens FactSet's repo= naming no member of the lens reaches here as
// ErrInvalid wrapping *federate.UnknownRepoError; it is answered as the lens
// list answers it (planLensRead's StageRepos): 422 "Unknown repo" with the
// error's own text, not the 400 every other ErrInvalid gets.
func writeFactSetError(w http.ResponseWriter, r *http.Request, err error) {
	var unknownRepo *federate.UnknownRepoError
	switch {
	case errors.As(err, &unknownRepo):
		hal.WriteProblem(w, http.StatusUnprocessableEntity, "Unknown repo", unknownRepo.Error(), r.URL.Path)
	case errors.Is(err, materialize.ErrCursorExpired):
		hal.WriteProblem(w, http.StatusNotFound, "Cursor not found", errMaterializeCursorExpired, r.URL.Path)
	case errors.Is(err, materialize.ErrInvalid):
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid request", err.Error(), r.URL.Path)
	case errors.Is(err, materialize.ErrFactSetBusy), errors.Is(err, materialize.ErrStoreBusy),
		errors.Is(err, materialize.ErrIndexMoved), errors.Is(err, materialize.ErrNotReady):
		w.Header().Set("Retry-After", materializeBusyRetryAfter)
		detail := "the server is busy building other factsets; retry shortly"
		switch {
		case errors.Is(err, materialize.ErrIndexMoved):
			detail = materialize.ErrIndexMoved.Error()
		case errors.Is(err, materialize.ErrNotReady):
			detail = materialize.ErrNotReady.Error()
		case errors.Is(err, materialize.ErrStoreBusy):
			detail = errMaterializeCursorBusy
		}
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Busy", detail, r.URL.Path)
	default:
		log.Error().Err(err).Str("path", r.URL.Path).Msg("materialize factset failed")
		hal.WriteProblem(w, http.StatusInternalServerError, "Materialize failed", "the factset could not be served", r.URL.Path)
	}
}
