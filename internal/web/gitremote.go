package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	gogitserver "github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"knomit/internal/repos"
	storegit "knomit/internal/store/git"
)

// GitRemoteStore is the narrow interface gitremote needs — just the
// underlying go-git storer so it can serve pack negotiations.
type GitRemoteStore interface {
	Storer() *storegit.Storer
}

// repoLoader adapts a storer.Storer to go-git's server.Loader interface,
// always returning the same storer regardless of endpoint.
type repoLoader struct {
	sto storer.Storer
}

func (l *repoLoader) Load(_ *transport.Endpoint) (storer.Storer, error) {
	return l.sto, nil
}

// GitRemoteHandler returns an http.Handler implementing the read-only Smart
// HTTP git protocol (https://git-scm.com/docs/http-protocol). Only
// upload-pack (clone/fetch) is supported; push is not.
//
//   - GET  /{repo}/info/refs?service=git-upload-pack — advertise refs
//   - POST /{repo}/git-upload-pack                   — serve a fetch
func GitRemoteHandler(rm *repos.Manager) http.Handler {
	// Cache per-repo handlers by GitRemoteStore identity so we don't rebuild
	// the mux and go-git server on every request.
	var cache sync.Map // key: GitRemoteStore, value: http.Handler

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// chi's RoutePath is the mount-relative path, e.g. "/knomit/info/refs".
		// Fall back to r.URL.Path when called outside a chi routing context (e.g. tests).
		routePath := r.URL.Path
		if rctx := chi.RouteContext(r.Context()); rctx != nil {
			routePath = rctx.RoutePath
		}
		p := strings.TrimPrefix(routePath, "/")
		repoName, repoSuffix, _ := strings.Cut(p, "/")
		if repoName == "" {
			http.NotFound(w, r)
			return
		}

		ri := rm.Get(repoName)
		if ri == nil {
			http.NotFound(w, r)
			return
		}

		ri.RLock()
		gs, ok := ri.GS.(GitRemoteStore)
		ri.RUnlock()
		if !ok {
			http.Error(w, "git serving not supported for this repo", http.StatusInternalServerError)
			return
		}

		h, _ := cache.LoadOrStore(gs, newRepoGitHandler(gs))

		// Rewrite the request URL to just the git-protocol suffix so the inner
		// mux can match /info/refs and /git-upload-pack directly.
		// Shallow copy is sufficient — inner handlers never mutate request fields.
		u2 := *r.URL
		u2.Path = "/" + repoSuffix
		u2.RawPath = ""
		r2 := r.WithContext(r.Context())
		r2.URL = &u2
		h.(http.Handler).ServeHTTP(w, r2)
	})
}

// newRepoGitHandler builds the inner mux that handles the read-only git smart
// HTTP endpoints for a single repository. Push (receive-pack) is not exposed.
func newRepoGitHandler(gs GitRemoteStore) http.Handler {
	loader := &repoLoader{sto: gs.Storer()}
	srv := gogitserver.NewServer(loader)

	mux := http.NewServeMux()

	mux.HandleFunc("/info/refs", func(w http.ResponseWriter, r *http.Request) {
		service := r.URL.Query().Get("service")
		if service != "git-upload-pack" {
			http.Error(w, "only git-upload-pack is supported", http.StatusForbidden)
			return
		}

		ep := &transport.Endpoint{}
		sess, err := srv.NewUploadPackSession(ep, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer sess.Close()

		advRefs, err := sess.AdvertisedReferencesContext(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		advRefs.Prefix = [][]byte{
			[]byte("# service=git-upload-pack"),
			pktline.Flush,
		}

		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.Header().Set("Cache-Control", "no-cache")
		if err := advRefs.Encode(w); err != nil {
			// headers already sent, nothing to do
			return
		}
	})

	mux.HandleFunc("/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ep := &transport.Endpoint{}
		ctx := r.Context()

		sess, err := srv.NewUploadPackSession(ep, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer sess.Close()

		// AdvertisedReferencesContext must be called before UploadPack to
		// initialise the session's capability list.
		if _, err = sess.AdvertisedReferencesContext(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		body, err := requestBody(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer body.Close()

		req := packp.NewUploadPackRequest()
		if err := req.Decode(body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		resp, err := sess.UploadPack(ctx, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("Cache-Control", "no-cache")
		if err := resp.Encode(w); err != nil {
			return
		}
	})

	return mux
}

// requestBody returns a reader for the request body, transparently
// decompressing gzip-encoded bodies that git sends for pack negotiations.
func requestBody(r *http.Request) (io.ReadCloser, error) {
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		return gr, nil
	}
	return r.Body, nil
}
