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

// GitRemoteHandler returns an http.Handler implementing the Smart HTTP git
// protocol (https://git-scm.com/docs/http-protocol). It exposes three
// endpoints (relative to the mount point):
//
//   - GET  /{repo}/info/refs?service=git-upload-pack   — advertise refs for fetch
//   - GET  /{repo}/info/refs?service=git-receive-pack  — advertise refs for push
//   - POST /{repo}/git-upload-pack                     — serve a fetch
//   - POST /{repo}/git-receive-pack                    — accept a push
//
// If apiKey is non-empty, receive-pack (push) endpoints require a Bearer
// token matching apiKey. Upload-pack (fetch) is always public.
func GitRemoteHandler(rm *RepoManager, apiKey string) http.Handler {
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

		gs, ok := ri.GS.(GitRemoteStore)
		if !ok {
			http.Error(w, "git serving not supported for this repo", http.StatusInternalServerError)
			return
		}

		h, _ := cache.LoadOrStore(gs, newRepoGitHandler(gs, apiKey))

		// Rewrite the request URL to just the git-protocol suffix so the inner
		// mux can match /info/refs, /git-upload-pack, /git-receive-pack directly.
		// Shallow copy is sufficient — inner handlers never mutate request fields.
		u2 := *r.URL
		u2.Path = "/" + repoSuffix
		u2.RawPath = ""
		r2 := r.WithContext(r.Context())
		r2.URL = &u2
		h.(http.Handler).ServeHTTP(w, r2)
	})
}

// newRepoGitHandler builds the inner mux that handles the three git smart HTTP
// endpoints for a single repository. The caller is responsible for stripping
// the repo-name prefix before dispatching to this handler.
func newRepoGitHandler(gs GitRemoteStore, apiKey string) http.Handler {
	loader := &repoLoader{sto: gs.Storer()}
	srv := gogitserver.NewServer(loader)

	mux := http.NewServeMux()

	mux.HandleFunc("/info/refs", func(w http.ResponseWriter, r *http.Request) {
		service := r.URL.Query().Get("service")
		ep := &transport.Endpoint{}
		ctx := r.Context()

		switch service {
		case "git-upload-pack":
			sess, err := srv.NewUploadPackSession(ep, nil)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer sess.Close()

			advRefs, err := sess.AdvertisedReferencesContext(ctx)
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

		case "git-receive-pack":
			if apiKey != "" && !bearerAuth(r, apiKey) {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}

			sess, err := srv.NewReceivePackSession(ep, nil)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			defer sess.Close()

			advRefs, err := sess.AdvertisedReferencesContext(ctx)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			advRefs.Prefix = [][]byte{
				[]byte("# service=git-receive-pack"),
				pktline.Flush,
			}

			w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
			w.Header().Set("Cache-Control", "no-cache")
			if err := advRefs.Encode(w); err != nil {
				return
			}

		default:
			http.Error(w, "unknown service", http.StatusBadRequest)
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

	mux.HandleFunc("/git-receive-pack", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if apiKey != "" && !bearerAuth(r, apiKey) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		ep := &transport.Endpoint{}
		ctx := r.Context()

		sess, err := srv.NewReceivePackSession(ep, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer sess.Close()

		// AdvertisedReferencesContext must be called before ReceivePack to
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

		req := packp.NewReferenceUpdateRequest()
		if err := req.Decode(body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		report, err := sess.ReceivePack(ctx, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.Header().Set("Cache-Control", "no-cache")
		if report != nil {
			if err := report.Encode(w); err != nil {
				return
			}
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

// bearerAuth checks that the request carries a Bearer token matching key.
func bearerAuth(r *http.Request, key string) bool {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return tok == key
}
