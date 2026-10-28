package kb

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/corpus/materialize"
	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
)

// materializeSuffix is the fact sub-resource that returns the agent document
// of knomit_materialize: the fact and the facts it stands on, as one
// markdown document. REST has no transport cap and no list cursor, so the
// whole document is one response; its bytes equal the concatenation of the
// MCP agent pages for the same (fact, anchor, options).
const materializeSuffix = "/materialize"

// materializeParams are the query parameters the route reads. Any other is
// refused: an unread parameter would run a different request and report
// success.
var materializeParams = map[string]bool{"depth": true, "max_nodes": true, "unpin": true, "cross_kb": true}

// splitMaterialize reports whether path addresses the materialize
// sub-resource, and returns the fact path without the suffix.
func splitMaterialize(path string) (string, bool) {
	if p, ok := strings.CutSuffix(path, materializeSuffix); ok && p != "" {
		return p, true
	}
	return path, false
}

// serveMaterialize writes the agent document of file as of commit ("" = the
// tip of the binding's branch for that fact's mount). notFound, when non-nil,
// answers every request whose fact cannot be addressed or read: the lens
// route passes its byte-identical 404, so an unmounted id, a malformed
// kb:// path and a missing fact stay indistinguishable there.
func serveMaterialize(w http.ResponseWriter, r *http.Request, b *repos.Binding, mgr *repos.Manager, file, commit string, notFound func()) {
	opts, err := materializeOptions(r)
	if err != nil {
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid parameter", err.Error(), r.URL.Path)
		return
	}
	doc, err := materialize.Plan(r.Context(), materialize.Request{
		Binding: b, Manager: mgr, File: file, Commit: commit, Options: opts,
	})
	switch {
	case notFound != nil && (errors.Is(err, materialize.ErrInvalid) || errors.Is(err, materialize.ErrNotFound)):
		notFound()
		return
	case errors.Is(err, materialize.ErrInvalid):
		hal.WriteProblem(w, http.StatusBadRequest, "Invalid request", err.Error(), r.URL.Path)
		return
	case errors.Is(err, materialize.ErrNotFound):
		hal.WriteProblem(w, http.StatusNotFound, "Fact not found", err.Error(), r.URL.Path)
		return
	case err != nil:
		log.Error().Err(err).Str("file", file).Msg("materialize failed")
		hal.WriteProblem(w, http.StatusInternalServerError, "Materialize failed", err.Error(), r.URL.Path)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(materialize.RenderAgent(doc)))
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
