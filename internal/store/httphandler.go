package store

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// packWindow is the delta-compression window the packfile encoder searches.
// It is a bandwidth/CPU knob, the same value go-git's own server uses; it
// describes nothing about the repository being served.
const packWindow = 10

// Handler returns an http.Handler implementing the read-only Smart HTTP git
// protocol (https://git-scm.com/docs/http-protocol) for this store.
// Only upload-pack (clone/fetch) is supported; push is not.
// The handler is built lazily and cached.
//
//   - GET  /info/refs?service=git-upload-pack — advertise refs
//   - POST /git-upload-pack                   — serve a fetch
func (s *Service) Handler() http.Handler {
	s.handlerOnce.Do(func() {
		s.handler = newGitHTTPHandler(s.rh, s.UpstreamBranch)
	})
	return s.handler
}

// newGitHTTPHandler builds an http.Handler serving the read-only git smart
// HTTP endpoints for a single repository. Push (receive-pack) is not exposed.
//
// Neither endpoint goes through go-git's built-in server any more: that server
// advertises only agent and ofs-delta, rejects every capability it did not
// advertise, and has no shallow implementation — so a depth-1 request (what
// the create wizard's branch probe sends) came back as an HTTP 500. The
// advertisement is built by buildAdvRefs and the packfile by packObjects.
//
// upstream is evaluated PER REQUEST, not captured once: a repo can gain an
// origin (and with it a configured Remote.Branch) after the handler is built,
// and the advertisement must follow.
func newGitHTTPHandler(rh *repoHandler, upstream func() string) http.Handler {
	sto := rh.gits
	mux := http.NewServeMux()

	mux.HandleFunc("/info/refs", func(w http.ResponseWriter, r *http.Request) {
		service := r.URL.Query().Get("service")
		if service != "git-upload-pack" {
			http.Error(w, "only git-upload-pack is supported", http.StatusForbidden)
			return
		}

		advRefs, _, err := buildAdvRefs(rh, upstream())
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

		// Wants must be tips of the CURATED advertisement (section 2): knowing
		// the hash of a hidden ref must not be enough to fetch it. This is
		// git's own uploadpack.allowAnySHA1InWant=false default; a real client
		// refuses such a want before sending it, so this answers the ones that
		// speak the protocol directly.
		_, tips, err := buildAdvRefs(rh, upstream())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.Header().Set("Cache-Control", "no-cache")
		enc := pktline.NewEncoder(w)

		for _, want := range req.Wants {
			if _, ok := tips[want]; !ok {
				// A protocol-level refusal is an ERR pkt inside a 200, not an
				// HTTP error: that is the only form a git client renders.
				_ = enc.Encodef("ERR %s: %s\n", errWantNotAdvertised.Error(), want.String())
				return
			}
		}

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

		// The COMMIT-level negotiation is cheap (commit objects, no trees) and
		// every round needs it, because every round carrying a depth must be
		// answered with a shallow section.
		neg, err := negotiateCommits(rh, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		upd := neg.update

		// The object set is the EXPENSIVE half, and only the FINAL round needs
		// it. git fetches over stateless HTTP in rounds — wants plus a batch of
		// haves each time — and discards everything until it says "done" or we
		// acknowledge a common base. Building the object set on every round
		// meant a full history walk plus a recursive tree walk per commit, all
		// thrown away: measured at seven builds and six discards for one
		// incremental pull against an 800-commit store, 1.1s where the code
		// this replaced returned NAK before touching a single tree.
		//
		// It is built BEFORE anything is written, so a failure is still a clean
		// 500 rather than an error appended to a half-written response.
		settled := requestHasDone(raw) || haveCommon
		var objs []plumbing.Hash
		if settled {
			objs, err = neg.objects(rh, req)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		// The shallow section precedes the ACK/NAK section, and is present on
		// EVERY round of a request that carries a depth — git's
		// consume_shallow_list reads one before each ACK/NAK batch whenever it
		// sent a deepen, and go-git decodes one whenever req.Depth is set.
		// A request carrying only "shallow" lines (an ordinary fetch by a
		// shallow client) gets NO such section: neither client reads one, and
		// sending it would desynchronise the stream.
		if !req.Depth.IsZero() {
			if err := upd.Encode(w); err != nil {
				return
			}
		}

		// The DEEPEN PROBE: git sends the want+deepen section and nothing else
		// as its own round, reads the shallow list up to the flush, and then
		// sends the haves on a fresh request. Its reader is one continuous
		// stream across those rounds, so an ACK/NAK section appended here is
		// still sitting in the buffer when the next round's shallow list is
		// read — "fatal: git fetch-pack: expected shallow list". git's own
		// upload-pack answers this shape with the shallow section alone (the
		// request body is exhausted, so it never reaches the ACK/NAK code),
		// and so must this one.
		if !req.Depth.IsZero() && len(req.Haves) == 0 && !requestHasDone(raw) {
			return
		}

		if !settled {
			_ = enc.Encodef("%s\n", "NAK")
			return
		}

		// Negotiation is settled: emit the acknowledgement, then the packfile.
		if haveCommon {
			_ = enc.Encodef("%s %s\n", "ACK", common.String())
		} else {
			_ = enc.Encodef("%s\n", "NAK")
		}

		pr, pw := io.Pipe()
		go func() {
			e := packfile.NewEncoder(pw, sto, false)
			_, err := e.Encode(objs, packWindow)
			_ = pw.CloseWithError(err)
		}()
		defer pr.Close()
		if _, err := io.Copy(w, pr); err != nil {
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
