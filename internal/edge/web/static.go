package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	indexPage = "index.html"

	// assetsPrefix is the one URL prefix Vite content-hashes. Everything
	// under it — JS, CSS, fonts — carries a build hash in its filename, so a
	// changed file never reuses a URL and the response can be cached for as
	// long as the browser is willing to keep it.
	assetsPrefix = "/assets/"

	// immutableCacheControl is a year plus `immutable`: revalidate never.
	// Safe only because of the content hash above.
	immutableCacheControl = "public, max-age=31536000, immutable"

	// revalidateCacheControl is "store it, but ask every time" (RFC 9111
	// §5.2.2.4) — NOT "do not store". Paired with a per-file ETag it turns a
	// warm load of index.html and config.js into empty 304s instead of full
	// bodies.
	revalidateCacheControl = "no-cache"
)

// etagger hands out a strong validator for the files Vite does NOT
// content-hash: index.html, config.js, favicon.svg.
//
// The validator is the sha256 of the file's own bytes, deliberately not the
// build identity. A build-identity ETag collapses exactly when it matters: it
// is "dev" for any bare `go build`, and the Makefile's short SHA does not move
// on a dirty-tree rebuild. The browser would then be told 304 for an
// index.html that points at bundle names the new build no longer contains — a
// blank app, recoverable only by a manual cache clear. Hashing the bytes is
// correct for every build variant and needs no version plumbing.
//
// Tags are memoized: neither an embed.FS nor a test's MapFS changes while the
// process runs.
type etagger struct {
	fsys fs.FS
	mu   sync.RWMutex
	tags map[string]string
}

func newETagger(fsys fs.FS) *etagger {
	return &etagger{fsys: fsys, tags: make(map[string]string)}
}

// tag returns the quoted sha256 of name's contents, or "" when it cannot be
// read. An empty tag means the response simply carries no ETag, and so no
// revalidation — never a failed request.
func (e *etagger) tag(name string) string {
	e.mu.RLock()
	t, ok := e.tags[name]
	e.mu.RUnlock()
	if ok {
		return t
	}

	t = ""
	if f, err := e.fsys.Open(name); err == nil {
		h := sha256.New()
		if _, err := io.Copy(h, f); err == nil {
			t = `"` + hex.EncodeToString(h.Sum(nil)) + `"`
		}
		f.Close()
	}

	e.mu.Lock()
	e.tags[name] = t
	e.mu.Unlock()
	return t
}

// identityForRangeRequests keeps the compressor off any request that asks for
// a byte range.
//
// http.ServeContent answers a Range with 206 and a Content-Range describing
// IDENTITY bytes. Compressing that body leaves the headers describing a body
// the response does not contain: Content-Range said `bytes 0-99/2017` while
// the body carried 90 gzipped bytes. Dropping Accept-Encoding is what makes
// chi select no encoder at all, so the 206 goes out as the identity bytes it
// claims to be. Stripping Range instead would also be well-formed, but it
// would answer a range request with the whole file — the bytes this change
// exists to save.
func identityForRangeRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			r = r.Clone(r.Context())
			r.Header.Del("Accept-Encoding")
		}
		next.ServeHTTP(w, r)
	})
}

// immutableWriter stamps the year-long directive only once the status says
// the file was actually found.
//
// A 404 under /assets/ must never be cached for a year: the browser would go
// on serving that 404 from disk long after the next deploy put the file
// there, and nothing short of a manual cache clear would fix it.
type immutableWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

// Unwrap exposes the underlying writer to http.ResponseController and to any
// other wrapper that looks for it. Nothing on the static path uses a
// ResponseController today — this is here so that this wrapper is not the
// thing that severs the chain if something ever does.
func (w *immutableWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *immutableWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		switch code {
		case http.StatusOK, http.StatusNotModified, http.StatusPartialContent:
			w.Header().Set("Cache-Control", immutableCacheControl)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *immutableWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// staticHandlerFor serves fsys as a file tree. A nil fsys — the noembed
// build, or an embed that failed to open — answers 404 for everything, which
// is what the noembed build has always done.
func staticHandlerFor(fsys fs.FS) http.Handler {
	if fsys == nil {
		return http.NotFoundHandler()
	}
	return http.FileServer(http.FS(fsys))
}

// fsName turns a request path into the fs.FS name it addresses, or "" when
// the path cannot name a file. fs.ValidPath is the containment check: it
// rejects any path with a ".." or "." element, so nothing here can address a
// file outside the tree.
func fsName(urlPath string) string {
	name := strings.TrimSuffix(strings.TrimPrefix(urlPath, "/"), "/")
	if name == "" || !fs.ValidPath(name) {
		return ""
	}
	return name
}

// newAssetHandler serves /assets/* — everything Vite content-hashes.
func newAssetHandler(fsys fs.FS, static http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fsys == nil {
			http.NotFound(w, r)
			return
		}
		// A directory has to 404 before anything else happens. Handed to the
		// FileServer it comes back as an HTML listing enumerating every
		// bundle name in the build — and, before this guard, stamped
		// immutable for a year.
		name := fsName(r.URL.Path)
		if name == "" {
			http.NotFound(w, r)
			return
		}
		st, err := fs.Stat(fsys, name)
		if err != nil || st.IsDir() {
			http.NotFound(w, r)
			return
		}
		static.ServeHTTP(&immutableWriter{ResponseWriter: w}, r)
	}
}

// newSPAHandler serves a real file when fsys has one at the request path and
// falls back to index.html when it does not, so client-side routes resolve to
// the app instead of a 404. Either way the response revalidates against that
// file's own content hash.
func newSPAHandler(fsys fs.FS, static http.Handler, tags *etagger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fsys == nil {
			static.ServeHTTP(w, r)
			return
		}
		if name := fsName(r.URL.Path); name != "" && name != indexPage {
			// A directory falls through to the app rather than to a
			// FileServer listing: the UI surface never serves one.
			if st, err := fs.Stat(fsys, name); err == nil && !st.IsDir() {
				setRevalidate(w, tags.tag(name))
				static.ServeHTTP(w, r)
				return
			}
		}
		setRevalidate(w, tags.tag(indexPage))
		serveIndex(fsys, w, r)
	}
}

// setRevalidate writes the cache policy BEFORE the body handler runs.
// http.ServeContent compares If-None-Match against an ETag already on the
// header map and writes the 304 itself, and its writeNotModified drops
// Content-Type — which is what keeps the compressor off an empty body.
// Hand-rolling the comparison would leave Content-Type set and come back as a
// gzipped 304 with nothing in it.
func setRevalidate(w http.ResponseWriter, etag string) {
	h := w.Header()
	h.Set("Cache-Control", revalidateCacheControl)
	if etag != "" {
		h.Set("ETag", etag)
	}
}

// serveIndex writes index.html for "/", for "/index.html" and for every
// unmatched client-side route.
//
// It goes through http.ServeContent rather than handing a rewritten path back
// to the FileServer, because the FileServer answers ANY request whose path
// ends in /index.html with a 301 to "./" — and "./" resolves against the
// request, so a deep route like /repos/alpha/browse redirects to
// /repos/alpha/, which redirects to itself. That is a redirect loop, not a
// fallback.
func serveIndex(fsys fs.FS, w http.ResponseWriter, r *http.Request) {
	f, err := fsys.Open(indexPage)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	rs, ok := f.(io.ReadSeeker)
	if !ok {
		// Not every fs.FS hands back a seeker; index.html is small enough
		// that buffering it is cheaper than caring.
		b, err := io.ReadAll(f)
		if err != nil {
			http.Error(w, "index unavailable", http.StatusInternalServerError)
			return
		}
		rs = bytes.NewReader(b)
	}
	// A zero modtime suppresses Last-Modified: embedded files carry no
	// meaningful one, and the content ETag is the validator that matters.
	http.ServeContent(w, r, indexPage, time.Time{}, rs)
}
