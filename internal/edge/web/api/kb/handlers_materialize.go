package kb

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/federate"
	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/edge/web/httpx"
	"knomit/internal/foundation/auth"
)

// materializeSuffix is the fact sub-resource that returns the agent document
// of knomit_materialize: the fact and the facts it stands on, as one
// markdown document, sent as a download (Content-Disposition: attachment).
// It is the EXPORT: always the whole materialization at the requested depth
// and count, built in memory (at most 500 facts, about 2 MB). Its bytes
// equal the concatenation of the MCP agent pages for the same (fact, anchor,
// options).
const materializeSuffix = "/materialize"

// materializePagesSuffix is the fact sub-resource that pages the same
// materialization as data, exactly like knomit_materialize with audience
// "human": the first call (no cursor) plans the walk and freezes its order
// and every node's version in a cursor; every later call passes ONLY that
// cursor. Each response is byte-for-byte the MCP human page for the same
// input. It never returns a whole materialization in one response.
const materializePagesSuffix = "/materialize/pages"

// materializeParams are the query parameters the document routes read. Any
// other is refused: an unread parameter would run a different request and
// report success.
var materializeParams = map[string]bool{"depth": true, "max_nodes": true, "unpin": true, "cross_kb": true}

// materializeCursorParam is the one parameter a later pages call takes.
const materializeCursorParam = "cursor"

// materializePageLimit is the REST pages route's fact limit: the MCP human
// audience's default page, so a REST page and an MCP human page for the same
// input are the same page. Every page is also measured against
// materialize.PageCap.
const materializePageLimit = materialize.HumanDefaultLimit

// errMaterializeCursorExpired is the REST answer for every cursor it cannot
// serve (materialize.ErrCursorExpired): fixed text, so an unknown cursor, a
// finished one and another caller's read the same.
const errMaterializeCursorExpired = "the cursor is expired or not found; start over without a cursor"

// splitMaterialize reports whether path addresses the materialize document
// sub-resource, and returns the fact path without the suffix.
func splitMaterialize(path string) (string, bool) {
	if p, ok := strings.CutSuffix(path, materializeSuffix); ok && p != "" {
		return p, true
	}
	return path, false
}

// splitMaterializePages reports whether path addresses the materialize pages
// sub-resource, and returns the fact path without the suffix. A fact path
// ends in .md, so neither suffix can be part of one.
func splitMaterializePages(path string) (string, bool) {
	if p, ok := strings.CutSuffix(path, materializePagesSuffix); ok && p != "" {
		return p, true
	}
	return path, false
}

// serveMaterializeSub dispatches both materialize sub-resources of path;
// false when path addresses neither. See serveMaterialize for the arguments.
func serveMaterializeSub(w http.ResponseWriter, r *http.Request, b *repos.Binding, mgr *repos.Manager, path, commit string, notFound func()) bool {
	if factPath, ok := splitMaterializePages(path); ok {
		serveMaterializePages(w, r, b, mgr, factPath, commit, notFound)
		return true
	}
	if factPath, ok := splitMaterialize(path); ok {
		serveMaterialize(w, r, b, mgr, factPath, commit, notFound)
		return true
	}
	return false
}

// serveMaterialize writes the agent document of file as of commit ("" = the
// tip of the binding's branch for that fact's mount), as an attachment named
// materialize.ExportFileName. notFound, when non-nil, answers every request
// whose fact cannot be addressed or read: the lens route passes its
// byte-identical 404, so an unmounted id, a malformed kb:// path and a
// missing fact stay indistinguishable there.
func serveMaterialize(w http.ResponseWriter, r *http.Request, b *repos.Binding, mgr *repos.Manager, file, commit string, notFound func()) {
	doc, ok := planMaterialize(w, r, b, mgr, file, commit, notFound)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+materialize.ExportFileName(doc)+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(materialize.RenderAgent(doc)))
}

// serveMaterializePages serves one page of file's materialization, the MCP
// human page. Without `cursor` it is a first call: the document options are
// read, the walk is planned as of commit, and page 1 comes back with a
// cursor when more remain. With `cursor`, any other parameter is a 400: the
// order was frozen by the first call, so new options mean a new first call.
//
// The cursor belongs to this route's URL (which names the fact and its repo
// or lens) AND the caller's principal, the endpoint + principal ownership of
// knomit_materialize's cursors; it is also pinned to the binding's pin and
// read set. Any mismatch, and an unknown or finished cursor, is the same
// 404, and consumes nothing.
func serveMaterializePages(w http.ResponseWriter, r *http.Request, b *repos.Binding, mgr *repos.Manager, file, commit string, notFound func()) {
	q := r.URL.Query()
	cursor := q.Get(materializeCursorParam)
	if _, has := q[materializeCursorParam]; has {
		for k := range q {
			if k != materializeCursorParam {
				hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter",
					strconv.Quote(k)+" cannot be passed with cursor: the materialization was fixed by the first call; pass only cursor, or start over without it", r.URL.Path)
				return
			}
		}
		if cursor == "" {
			hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", "cursor is empty", r.URL.Path)
			return
		}
	}

	svc, release, err := b.Write().Acquire()
	if err != nil {
		httpx.WriteStoreUnavailable(w, r)
		return
	}
	defer release()
	ctx := r.Context()
	sc := materialize.CursorScope{
		Sessions: svc.ToolSession(), Branch: b.WriteMountBranch(), Pin: b.PinID(),
		ReadSet: federate.ReadSetFingerprint(b), Owner: restCursorOwner(r),
	}

	var page materialize.Page
	if cursor == "" {
		doc, ok := planMaterialize(w, r, b, mgr, file, commit, notFound)
		if !ok {
			return
		}
		page, err = materialize.FirstPage(ctx, sc, doc, materialize.AudienceHuman, materializePageLimit)
	} else {
		page, err = materialize.NextPage(ctx, sc, b, mgr, callerMayRead(r), cursor,
			func(audience string) (int, error) {
				// A REST cursor is always a human one; an agent cursor here
				// would be the MCP tool's, and its owner never matches.
				if audience != materialize.AudienceHuman {
					return 0, materialize.ErrCursorExpired
				}
				return materializePageLimit, nil
			})
	}
	switch {
	case errors.Is(err, materialize.ErrCursorExpired):
		hal.WriteProblem(w, http.StatusNotFound, "Cursor not found", errMaterializeCursorExpired, r.URL.Path)
		return
	case err != nil:
		log.Error().Err(err).Str("file", file).Msg("materialize page failed")
		hal.WriteProblem(w, http.StatusInternalServerError, "Materialize failed", err.Error(), r.URL.Path)
		return
	}
	out, err := json.Marshal(page)
	if err != nil {
		hal.WriteProblem(w, http.StatusInternalServerError, "Materialize failed", err.Error(), r.URL.Path)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// restCursorOwner is who owns a pages cursor: the route's URL path (the
// REST endpoint, which names the fact and its repo or lens) plus the
// caller's principal, the same shape as knomit_materialize's owner (MCP
// endpoint + principal). An MCP cursor's owner names an MCP endpoint, so it
// never resumes here, nor a REST cursor there.
func restCursorOwner(r *http.Request) string {
	p, _ := auth.FromContext(r.Context())
	return r.URL.Path + " " + p.String()
}

// callerMayRead is the cross-KB read check for a repo outside the binding:
// only repos the caller's credential reaches by their own routes
// (auth.ReachesRepo).
func callerMayRead(r *http.Request) func(string) bool {
	ctx := r.Context()
	return func(repo string) bool { return auth.ReachesRepo(ctx, repo) }
}

// planMaterialize parses the document options and plans file's walk as of
// commit. On any failure it has written the response and returns false.
func planMaterialize(w http.ResponseWriter, r *http.Request, b *repos.Binding, mgr *repos.Manager, file, commit string, notFound func()) (*materialize.Document, bool) {
	opts, err := materializeOptions(r)
	if err != nil {
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", err.Error(), r.URL.Path)
		return nil, false
	}
	doc, err := materialize.Plan(r.Context(), materialize.Request{
		Binding: b, Manager: mgr, File: file, Commit: commit, Options: opts,
		MayRead: callerMayRead(r),
	})
	switch {
	case notFound != nil && (errors.Is(err, materialize.ErrInvalid) || errors.Is(err, materialize.ErrNotFound)):
		notFound()
	case errors.Is(err, materialize.ErrInvalid):
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid request", err.Error(), r.URL.Path)
	case errors.Is(err, materialize.ErrNotFound):
		hal.WriteProblem(w, http.StatusNotFound, "Fact not found", err.Error(), r.URL.Path)
	case err != nil:
		log.Error().Err(err).Str("file", file).Msg("materialize failed")
		hal.WriteProblem(w, http.StatusInternalServerError, "Materialize failed", err.Error(), r.URL.Path)
	default:
		return doc, true
	}
	return nil, false
}

// materializeOptions parses the query: the defaults for what is absent, a 400
// for an unknown parameter or a malformed or out-of-range value.
func materializeOptions(r *http.Request) (materialize.Options, error) {
	opts := materialize.DefaultOptions()
	q := r.URL.Query()
	for k := range q {
		if !materializeParams[k] {
			return opts, errors.New("unknown parameter " + strconv.Quote(k) + "; valid parameters are cross_kb, depth, max_nodes, unpin")
		}
	}
	for _, p := range []struct {
		name string
		dst  *int
	}{{"depth", &opts.Depth}, {"max_nodes", &opts.MaxNodes}} {
		if v := q.Get(p.name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return opts, errors.New(p.name + " must be a whole number")
			}
			*p.dst = n
		}
	}
	for _, p := range []struct {
		name string
		dst  *bool
	}{{"unpin", &opts.Unpin}, {"cross_kb", &opts.CrossKB}} {
		if v := q.Get(p.name); v != "" {
			bv, err := strconv.ParseBool(v)
			if err != nil {
				return opts, errors.New(p.name + " must be true or false")
			}
			*p.dst = bv
		}
	}
	return opts, opts.Validate()
}
