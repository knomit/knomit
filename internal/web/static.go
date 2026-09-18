package web

import (
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"knomit/internal/platform/version"
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
	// §5.2.2.4) — NOT "do not store". Paired with the build-identity ETag it
	// turns a warm load of index.html and config.js into empty 304s instead
	// of full bodies.
	revalidateCacheControl = "no-cache"
)

// buildETag is the strong validator for every file Vite does not hash:
// index.html, config.js, favicon.svg and the SPA fallback. It is the build
// identity GET /api/v1/version reports, quoted — the value changes exactly
// when the binary changes, which is exactly when those files can change.
func buildETag() string { return `"` + version.String() + `"` }

// withCacheHeaders applies knomit's two cache policies by URL: a year for the
// content-hashed assets, revalidate-every-time plus an ETag for everything
// else the UI serves.
func withCacheHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, assetsPrefix) {
			next.ServeHTTP(&immutableWriter{ResponseWriter: w}, r)
			return
		}
		// Set BEFORE delegating. http.ServeContent compares If-None-Match
		// against an ETag already on the header map and writes the 304
		// itself, and its writeNotModified drops Content-Type — which is
		// what keeps the compressor off an empty body. Hand-rolling the
		// comparison here would leave Content-Type set and come back as a
		// gzipped 304 with nothing in it.
		h := w.Header()
		h.Set("Cache-Control", revalidateCacheControl)
		h.Set("ETag", buildETag())
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

// Unwrap lets http.ResponseController reach the real writer through this one.
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

// newSPAHandler serves a real file when fsys has one at the request path and
// falls back to index.html when it does not, so client-side routes resolve to
// the app instead of a 404.
func newSPAHandler(fsys fs.FS, static http.Handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if fsys == nil {
			static.ServeHTTP(w, r)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" && path != indexPage {
			if f, err := fsys.Open(path); err == nil {
				f.Close()
				static.ServeHTTP(w, r)
				return
			}
		}
		serveIndex(fsys, w, r)
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
// fallback. ServeContent has no such rewrite, and it does the If-None-Match
// comparison against the ETag withCacheHeaders has already set.
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
	// meaningful one, and the build ETag is the validator that matters.
	http.ServeContent(w, r, indexPage, time.Time{}, rs)
}
