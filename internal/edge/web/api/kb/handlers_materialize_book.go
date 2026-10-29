package kb

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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

// The book routes: GET /repos/{repo}/branches/{branch}/search/book pages a
// search materialized as a book (materialize.BookFirstPage / BookNextPage,
// the same pages knomit_materialize's book mode returns), and
// GET …/search/book/export sends the whole book as one agent document.
const (
	bookRouteSuffix  = "/search/book"
	bookExportSuffix = "/export"
)

// bookSearchParams are the search filters the first call reads: the repo
// /search parameters, plus `sort` (relevance = /search, recent = the /facts
// collection's sort=recent) and since_fork, exactly as the UI's results list
// sends them.
var bookSearchParams = map[string]bool{
	"q": true, "entities": true, "domain": true, "domain_exact": true, "path": true,
	"min_confidence": true, "min_similarity": true, "type": true, "exclude_type": true,
	"kind": true, "exclude_kind": true, "origin": true, "ep": true, "since_fork": true,
	"motifs": true, "motif_match": true, "sort": true,
}

// bookOptionParams are the book's own first-call parameters.
var bookOptionParams = map[string]bool{
	"facts": true, "depth": true, "max_nodes": true, "unpin": true, "cross_kb": true, "limit": true,
}

// bookUnitParam is the seek parameter, the only one a cursor call may add.
const bookUnitParam = "unit"

// restBookOwner is who owns a book cursor: the book route (the export route
// shares it) plus the caller's principal.
func restBookOwner(r *http.Request) string {
	p, _ := auth.FromContext(r.Context())
	return strings.TrimSuffix(r.URL.Path, bookExportSuffix) + " " + p.String()
}

func bookScope(r *http.Request, b *repos.Binding, ts *sessions.ToolIndex) materialize.CursorScope {
	return materialize.CursorScope{
		Sessions: ts, Branch: b.WriteMountBranch(), Pin: b.PinID(),
		ReadSet: federate.ReadSetFingerprint(b), Owner: restBookOwner(r),
	}
}

// handleSearchBook serves GET …/search/book.
func handleSearchBook(emb model.Embedder, mgr *repos.Manager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ri := repos.RepoFromContext(r.Context())
		branch := httpx.BranchFromContext(r.Context())
		b := repos.NewBindingOfRepo(ri, branch)
		q := r.URL.Query()

		cursor, isResume := q[materializeCursorParam]
		unit := 0
		if isResume {
			for k := range q {
				if k != materializeCursorParam && k != bookUnitParam {
					hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
						strconv.Quote(k)+" cannot be passed with cursor: the book was fixed by the first call; pass only cursor (and unit to seek), or start over without it", r.URL.Path)
					return
				}
			}
			if q.Get(materializeCursorParam) == "" {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", "cursor is empty", r.URL.Path)
				return
			}
			if v, ok := q[bookUnitParam]; ok {
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
		sc := bookScope(r, b, svc.ToolSession())
		ctx := r.Context()

		var page materialize.BookPage
		if isResume {
			page, err = materialize.BookNextPage(ctx, sc, b, mgr, callerMayRead(r), cursor[0], unit)
		} else {
			req, ok := bookRequest(w, r, q, ri, branch, b, mgr, emb)
			if !ok {
				return
			}
			page, _, err = materialize.BookFirstPage(ctx, sc, req)
		}
		if err != nil {
			writeBookError(w, r, err)
			return
		}
		out, err := json.Marshal(page)
		if err != nil {
			writeBookError(w, r, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}
}

// handleSearchBookExport serves GET …/search/book/export?cursor=…: the whole
// book as one agent document, an attachment, streamed a chunk at a time.
func handleSearchBookExport(mgr *repos.Manager) http.HandlerFunc {
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
		err = materialize.BookExport(r.Context(), bookScope(r, b, svc.ToolSession()), b, mgr, callerMayRead(r), cursor,
			func(env materialize.BookEnv) io.Writer {
				started = true
				w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
				w.Header().Set("Content-Disposition", `attachment; filename="`+materialize.BookExportFileName(env, cursor)+`"`)
				w.WriteHeader(http.StatusOK)
				return w
			})
		switch {
		case err == nil:
		case !started:
			writeBookError(w, r, err)
		default:
			// The export is never partial: a failure after the first bytes
			// aborts the connection, so the client sees a failed download,
			// not a short file.
			log.Error().Err(err).Msg("book export failed mid-stream")
			panic(http.ErrAbortHandler)
		}
	}
}

// bookRequest parses a first call. On failure it has written the response.
func bookRequest(w http.ResponseWriter, r *http.Request, q url.Values, ri *repos.RepoInstance, branch string,
	b *repos.Binding, mgr *repos.Manager, emb model.Embedder) (materialize.BookRequest, bool) {
	bad := func(detail string) (materialize.BookRequest, bool) {
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", detail, r.URL.Path)
		return materialize.BookRequest{}, false
	}
	for k := range q {
		if !bookSearchParams[k] && !bookOptionParams[k] {
			return bad("unknown parameter " + strconv.Quote(k))
		}
	}
	s := materialize.BookSearch{
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
	req := materialize.BookRequest{
		Binding: b, Manager: mgr, MayRead: callerMayRead(r), Search: s,
		Facts: materialize.DefaultBookFacts, Limit: materialize.HumanDefaultLimit,
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
			return materialize.BookRequest{}, false
		}
		req.PathsIn, req.NoResults = paths, len(paths) == 0
	}
	return req, true
}

func boolParam(v string) bool { return v == "true" || v == "1" }

// writeBookError maps a book error to its answer. Internal error text never
// reaches the caller: a store failure is logged and answered generically.
func writeBookError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, materialize.ErrCursorExpired):
		hal.WriteProblem(w, http.StatusNotFound, "Cursor not found", errMaterializeCursorExpired, r.URL.Path)
	case errors.Is(err, materialize.ErrInvalid):
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid request", err.Error(), r.URL.Path)
	case errors.Is(err, materialize.ErrBookTooLarge):
		hal.WriteProblem(w, http.StatusUnprocessableEntity, "Book too large", err.Error(), r.URL.Path)
	case errors.Is(err, materialize.ErrCursorInUse):
		hal.WriteProblem(w, http.StatusConflict, "Cursor in use",
			"another call is reading this cursor; a cursor has one reader at a time", r.URL.Path)
	case errors.Is(err, materialize.ErrBookBusy), errors.Is(err, materialize.ErrStoreBusy), errors.Is(err, materialize.ErrBookChanged):
		w.Header().Set("Retry-After", materializeBusyRetryAfter)
		detail := "the server is busy building other books; retry shortly"
		if errors.Is(err, materialize.ErrBookChanged) {
			detail = materialize.ErrBookChanged.Error()
		} else if errors.Is(err, materialize.ErrStoreBusy) {
			detail = errMaterializeCursorBusy
		}
		hal.WriteProblem(w, http.StatusServiceUnavailable, "Busy", detail, r.URL.Path)
	default:
		log.Error().Err(err).Str("path", r.URL.Path).Msg("materialize book failed")
		hal.WriteProblem(w, http.StatusInternalServerError, "Materialize failed", "the book could not be served", r.URL.Path)
	}
}
