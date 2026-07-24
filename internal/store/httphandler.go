package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gogitserver "github.com/go-git/go-git/v5/plumbing/transport/server"

	storegit "knomit/internal/store/git"
)

// Handler returns an http.Handler implementing the read-only Smart HTTP git
// protocol (https://git-scm.com/docs/http-protocol) for this store.
// Only upload-pack (clone/fetch) is supported; push is not.
// The handler is built lazily and cached.
//
//   - GET  /info/refs?service=git-upload-pack — advertise refs
//   - POST /git-upload-pack                   — serve a fetch
func (s *Service) Handler() http.Handler {
	s.handlerOnce.Do(func() {
		s.handler = newGitHTTPHandler(s.rh.gits, func(ctx context.Context) {
			s.ensureOKFBranches(ctx)
		})
	})
	return s.handler
}

// repoLoader adapts a storer.Storer to go-git's server.Loader interface,
// always returning the same storer regardless of endpoint.
type repoLoader struct {
	sto storer.Storer
}

func (l *repoLoader) Load(_ *transport.Endpoint) (storer.Storer, error) {
	return l.sto, nil
}

// newGitHTTPHandler builds an http.Handler serving the read-only git smart
// HTTP endpoints for a single repository. Push (receive-pack) is not exposed.
func newGitHTTPHandler(sto *storegit.Storer, onAdvertise func(context.Context)) http.Handler {
	loader := &repoLoader{sto: sto}
	srv := gogitserver.NewServer(loader)

	mux := http.NewServeMux()

	mux.HandleFunc("/info/refs", func(w http.ResponseWriter, r *http.Request) {
		service := r.URL.Query().Get("service")
		if service != "git-upload-pack" {
			http.Error(w, "only git-upload-pack is supported", http.StatusForbidden)
			return
		}

		if onAdvertise != nil {
			onAdvertise(r.Context()) // generate okf/* refs before advertising them
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
		ctx := r.Context()

		body, err := gitRequestBody(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer body.Close()

		// Buffer the request so we can both decode the negotiation and detect
		// the trailing "done" line — packp.UploadPackRequest.Decode reads the
		// wants/haves but silently discards "done".
		raw, err := io.ReadAll(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		req := packp.NewUploadPackRequest()
		if err := req.Decode(bytes.NewReader(raw)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("Cache-Control", "no-cache")
		enc := pktline.NewEncoder(w)

		// Single-ack negotiation. git fetches over smart HTTP in rounds: each
		// POST carries the wants plus a batch of "have" lines and, only on the
		// final round, "done". Until the client is done — or until we
		// acknowledge a commit we already hold as the common base — the
		// response MUST contain the ACK/NAK section ONLY. The go-git built-in
		// server ignores this and appends the packfile on every POST, so a
		// fetch that needs more than git's first ~16-have batch breaks with
		// "bad line length character: PACK" when the client reads the raw pack
		// bytes where it expects the next pkt-line.
		common, haveCommon := firstCommonHave(sto, req.Haves)
		if !requestHasDone(raw) && !haveCommon {
			_ = enc.Encodef("%s\n", "NAK")
			return
		}

		// Negotiation is settled: emit the acknowledgement, then the packfile.
		sess, err := srv.NewUploadPackSession(&transport.Endpoint{}, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer sess.Close()

		// AdvertisedReferencesContext must run before UploadPack to initialise
		// the session's capability list.
		if _, err = sess.AdvertisedReferencesContext(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		resp, err := sess.UploadPack(ctx, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer resp.Close()

		if haveCommon {
			_ = enc.Encodef("%s %s\n", "ACK", common.String())
		} else {
			_ = enc.Encodef("%s\n", "NAK")
		}
		// resp as an io.Reader yields the packfile only — its Encode method is
		// what would prepend a NAK, which we have already written ourselves.
		if _, err := io.Copy(w, resp); err != nil {
			return
		}
	})

	return mux
}

// firstCommonHave returns the first "have" the storer already holds, marking
// the common base for single-ack negotiation. The boolean is false when none
// of the haves are present locally (e.g. an initial clone with no haves).
func firstCommonHave(sto storer.EncodedObjectStorer, haves []plumbing.Hash) (plumbing.Hash, bool) {
	for _, h := range haves {
		if sto.HasEncodedObject(h) == nil {
			return h, true
		}
	}
	return plumbing.ZeroHash, false
}

// requestHasDone reports whether an upload-pack request body contains the
// "done" pkt-line, which signals the client has finished negotiating and now
// expects the packfile.
func requestHasDone(raw []byte) bool {
	s := pktline.NewScanner(bytes.NewReader(raw))
	for s.Scan() {
		if string(bytes.TrimSpace(s.Bytes())) == "done" {
			return true
		}
	}
	return false
}

// gitRequestBody returns a reader for the request body, transparently
// decompressing gzip-encoded bodies that git sends for pack negotiations.
func gitRequestBody(r *http.Request) (io.ReadCloser, error) {
	if strings.EqualFold(r.Header.Get("Content-Encoding"), "gzip") {
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		return gr, nil
	}
	return r.Body, nil
}
