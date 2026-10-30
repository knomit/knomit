package kb

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/engine/store/model"
	"knomit/internal/engine/store/sessions"
	"knomit/internal/foundation/auth"
)

// The factset routes: GET /repos/{repo}/branches/{branch}/search/factset pages a
// search materialized as a factset (materialize.FactSetFirstPage / FactSetNextPage,
// the same pages knomit_materialize's factset mode returns), and
// GET …/search/factset/export sends the whole factset as one agent document.
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

// restFactSetOwner is who owns a factset cursor: the factset route (the export route
// shares it) plus the caller's principal.
func restFactSetOwner(r *http.Request) string {
	p, _ := auth.FromContext(r.Context())
	return strings.TrimSuffix(r.URL.Path, factsetExportSuffix) + " " + p.String()
}

func factsetScope(r *http.Request, b *repos.Binding, ts *sessions.ToolIndex) materialize.CursorScope {
	return materialize.CursorScope{
		Sessions: ts, Branch: b.WriteMountBranch(), Pin: b.PinID(),
		ReadSet: federate.ReadSetFingerprint(b), Owner: restFactSetOwner(r),
	}
}

// handleSearchFactSet serves GET …/search/factset.
func handleSearchFactSet(emb model.Embedder, mgr *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		branch := httpx.BranchFromContext(r.Context())
		b := repos.NewBindingOfRepo(ri, branch)
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
			req, ok := factsetRequest(w, r, q, ri, branch, b, mgr, emb)
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

// handleSearchFactSetExport serves GET …/search/factset/export?cursor=…: the whole
// factset as one agent document, an attachment, streamed a chunk at a time.
func handleSearchFactSetExport(mgr *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		b := repos.NewBindingOfRepo(ri, httpx.BranchFromContext(r.Context()))
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
// without it a client that stops reading would hold its export slot and
// reader forever; with it, a write that cannot complete in this time fails,
// and the export aborts (never a short file).
const factsetExportWriteTimeout = time.Minute

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
func factsetRequest(w http.ResponseWriter, r *http.Request, q url.Values, ri *repos.RepoInstance, branch string,
	b *repos.Binding, mgr *repos.Manager, emb model.Embedder) (materialize.FactSetRequest, bool) {
	bad := func(detail string) (materialize.FactSetRequest, bool) {
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", detail, r.URL.Path)
		return materialize.FactSetRequest{}, false
	}
	for k := range q {
		if !factsetSearchParams[k] && !factsetOptionParams[k] {
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
	if emb != nil {
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
		paths, ok := experimentChangedPaths(w, r, ri, branch)
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
func writeFactSetError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, materialize.ErrCursorExpired):
		hal.WriteProblem(w, http.StatusNotFound, "Cursor not found", errMaterializeCursorExpired, r.URL.Path)
	case errors.Is(err, materialize.ErrInvalid):
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid request", err.Error(), r.URL.Path)
	case errors.Is(err, materialize.ErrFactSetTooLarge):
		hal.WriteProblem(w, http.StatusUnprocessableEntity, "FactSet too large", err.Error(), r.URL.Path)
	case errors.Is(err, materialize.ErrCursorInUse):
		hal.WriteProblem(w, http.StatusConflict, "Cursor in use",
			"another call is reading this cursor; a cursor has one reader at a time", r.URL.Path)
	case errors.Is(err, materialize.ErrFactSetBusy), errors.Is(err, materialize.ErrStoreBusy), errors.Is(err, materialize.ErrFactSetExportsBusy):
		w.Header().Set("Retry-After", materializeBusyRetryAfter)
		detail := "the server is busy building other factsets; retry shortly"
		if errors.Is(err, materialize.ErrFactSetExportsBusy) {
			detail = materialize.ErrFactSetExportsBusy.Error()
		} else if errors.Is(err, materialize.ErrStoreBusy) {
			detail = errMaterializeCursorBusy
		}
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Busy", detail, r.URL.Path)
	default:
		log.Error().Err(err).Str("path", r.URL.Path).Msg("materialize factset failed")
		hal.WriteProblem(w, http.StatusInternalServerError, "Materialize failed", "the factset could not be served", r.URL.Path)
	}
}
