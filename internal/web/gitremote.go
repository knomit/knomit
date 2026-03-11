package web

import (
	"net/http"
	"strings"

	gogitserver "github.com/go-git/go-git/v5/plumbing/transport/server"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"knomit/internal/gitstorer"
)

// GitRemoteStore is the interface gitremote needs from the git store.
type GitRemoteStore interface {
	Storer() *gitstorer.Storer
}

type repoLoader struct {
	sto storer.Storer
}

func (l *repoLoader) Load(_ *transport.Endpoint) (storer.Storer, error) {
	return l.sto, nil
}

// GitRemoteHandler returns an http.Handler for the Smart HTTP git protocol.
// Mount it at /git in the router.
// If apiKey is non-empty, receive-pack (push) endpoints require Bearer token auth.
func GitRemoteHandler(gs GitRemoteStore, apiKey string) http.Handler {
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

		req := packp.NewUploadPackRequest()
		if err := req.Decode(r.Body); err != nil {
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

		req := packp.NewReferenceUpdateRequest()
		if err := req.Decode(r.Body); err != nil {
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

	return http.StripPrefix("/git", mux)
}

func bearerAuth(r *http.Request, key string) bool {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return tok == key
}
