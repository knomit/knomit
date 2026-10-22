package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const testIndexHTML = "<!doctype html><title>knomit</title><script src=/assets/index-abc123.js></script>"

// defaultUIFiles is a stand-in for a built web/dist. The Go tests must not
// depend on the npm build: web/dist holds only .gitkeep until `make web` runs.
func defaultUIFiles() fstest.MapFS {
	return fstest.MapFS{
		"index.html":  {Data: []byte(testIndexHTML)},
		"config.js":   {Data: []byte("window.KNOMIT_CONFIG={apiBase:'/api/v1'};\n")},
		"favicon.svg": {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"></svg>`)},
		"assets/index-abc123.js": {Data: []byte(
			"export const x=1;" + strings.Repeat("// padding so the gzip is smaller than the source\n", 40))},
		"assets/nested/deep-9f8e7d.css": {Data: []byte("body{margin:0}")},
	}
}

func staticServerWith(t *testing.T, files fstest.MapFS) *Server {
	t.Helper()
	return &Server{Manager: newTestManagerWithRepos(t), staticFS: files}
}

func staticTestServer(t *testing.T) *Server {
	t.Helper()
	return staticServerWith(t, defaultUIFiles())
}

func TestAssets_AreGzippedAndImmutable(t *testing.T) {
	h := staticTestServer(t).Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/assets/index-abc123.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding: got %q, want gzip", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != immutableCacheControl {
		t.Errorf("Cache-Control: got %q, want %q", got, immutableCacheControl)
	}
	body := gunzip(t, rec.Body)
	if !strings.HasPrefix(string(body), "export const x=1;") {
		t.Errorf("gunzipped asset body: got %.40q…, want it to start with the source", body)
	}
}

// A Range request must never come back compressed. http.ServeContent writes
// Content-Range describing IDENTITY bytes, so a gzipped 206 is a response
// whose headers describe a body it does not contain.
func TestAssets_RangeRequestIsNotCompressed(t *testing.T) {
	h := staticTestServer(t).Handler()
	full := defaultUIFiles()["assets/index-abc123.js"].Data

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/assets/index-abc123.js", nil)
	req.Header.Set("Range", "bytes=0-99")
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status: got %d, want 206", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding on a 206: got %q, want empty", got)
	}
	if got, want := rec.Body.Len(), 100; got != want {
		t.Errorf("body length: got %d, want %d", got, want)
	}
	if got := rec.Body.String(); got != string(full[:100]) {
		t.Errorf("body: got %q, want the first 100 identity bytes", got)
	}
	if got, want := rec.Header().Get("Content-Range"), "bytes 0-99/2017"; got != want {
		t.Errorf("Content-Range: got %q, want %q", got, want)
	}
}

// A content-hashed URL that does not exist must not be cached for a year:
// the browser would hold that 404 long after the next deploy fixed it.
func TestAssets_MissingFileIsNotCachedForever(t *testing.T) {
	h := staticTestServer(t).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/gone-deadbee.js", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control on a 404: got %q, want no immutable directive", got)
	}
}

// http.FileServer lists a directory when asked for one. Under /assets/ that
// enumerates every bundle name and — before this guard — stamped the listing
// immutable for a year.
func TestAssets_DirectoriesAre404NotAListing(t *testing.T) {
	h := staticTestServer(t).Handler()

	for _, path := range []string{"/assets/", "/assets/nested/"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusNotFound {
				t.Fatalf("status: got %d, want 404", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "index-abc123.js") {
				t.Errorf("body leaked a directory listing: %q", rec.Body.String())
			}
			if got := rec.Header().Get("Cache-Control"); strings.Contains(got, "immutable") {
				t.Errorf("Cache-Control: got %q, want no immutable directive", got)
			}
		})
	}
}

// The UI surface must never answer with a directory listing, on either route.
func TestSPA_DirectoryFallsBackToTheApp(t *testing.T) {
	h := staticTestServer(t).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets", nil))
	if strings.Contains(rec.Body.String(), "<a href=") {
		t.Errorf("served a directory listing for /assets: %q", rec.Body.String())
	}
}

func TestIndex_NoCacheWithContentETagAnd304(t *testing.T) {
	h := staticTestServer(t).Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != revalidateCacheControl {
		t.Errorf("Cache-Control: got %q, want %q", got, revalidateCacheControl)
	}
	etag := rec.Header().Get("ETag")
	if !strings.HasPrefix(etag, `"`) || !strings.HasSuffix(etag, `"`) || len(etag) < 4 {
		t.Fatalf("ETag: got %q, want a quoted strong validator", etag)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding: got %q, want gzip", got)
	}
	if body := gunzip(t, rec.Body); !strings.Contains(string(body), "<title>knomit</title>") {
		t.Errorf("index body: got %q, want the index.html contents", body)
	}

	// Stable across requests — otherwise every warm load is a full body.
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec2.Header().Get("ETag"); got != etag {
		t.Errorf("ETag is not stable: got %q then %q", etag, got)
	}

	// Warm load: the browser echoes the ETag and gets nothing back.
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	req3.Header.Set("If-None-Match", etag)
	req3.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusNotModified {
		t.Fatalf("conditional status: got %d, want 304", rec3.Code)
	}
	if rec3.Body.Len() != 0 {
		t.Errorf("304 body: got %d bytes, want 0", rec3.Body.Len())
	}
	// net/http's writeNotModified drops Content-Type, which is what keeps
	// the compressor off an empty body. A hand-rolled 304 would not.
	if got := rec3.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("304 Content-Encoding: got %q, want empty", got)
	}
	if got := rec3.Header().Get("Cache-Control"); got != revalidateCacheControl {
		t.Errorf("304 Cache-Control: got %q, want %q", got, revalidateCacheControl)
	}
}

// The validator must track the FILE, not the build. A build-identity ETag
// collapses whenever the commit does not move — "dev" for a bare `go build`,
// and an unchanged SHA on a dirty-tree rebuild — and the browser then keeps a
// stale index.html pointing at bundles that no longer exist: a blank app that
// only a manual cache clear recovers.
func TestIndex_ETagTracksContentNotBuildIdentity(t *testing.T) {
	etagFor := func(t *testing.T, index string) string {
		t.Helper()
		files := defaultUIFiles()
		files["index.html"] = &fstest.MapFile{Data: []byte(index)}
		rec := httptest.NewRecorder()
		staticServerWith(t, files).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status: got %d, want 200", rec.Code)
		}
		return rec.Header().Get("ETag")
	}

	same1 := etagFor(t, testIndexHTML)
	same2 := etagFor(t, testIndexHTML)
	changed := etagFor(t, testIndexHTML+"<!-- rebuilt, new bundle names -->")

	if same1 == "" {
		t.Fatal("ETag is empty")
	}
	if same1 != same2 {
		t.Errorf("identical content produced different ETags: %q vs %q", same1, same2)
	}
	if same1 == changed {
		t.Errorf("changed content produced the same ETag %q — a stale index.html would 304 forever", same1)
	}
}

// Every client-side route falls back to index.html, and that fallback needs
// the same revalidation handshake as / does.
func TestSPAFallback_NoCacheWithContentETagAnd304(t *testing.T) {
	h := staticTestServer(t).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/alpha/browse", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<title>knomit</title>") {
		t.Fatalf("fallback body: got %q, want index.html", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != revalidateCacheControl {
		t.Errorf("Cache-Control: got %q, want %q", got, revalidateCacheControl)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("ETag is empty on the SPA fallback")
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/repos/alpha/browse", nil)
	req2.Header.Set("If-None-Match", etag)
	h.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusNotModified {
		t.Fatalf("conditional status: got %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 body: got %d bytes, want 0", rec2.Body.Len())
	}
}

// config.js and favicon.svg are served from the root, not /assets/, so Vite
// never content-hashes them and they must stay revalidated — each with its
// OWN validator, not one shared build string.
func TestRootFiles_AreRevalidatedNotImmutable(t *testing.T) {
	h := staticTestServer(t).Handler()
	seen := map[string]string{}

	for _, path := range []string{"/config.js", "/favicon.svg"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200", rec.Code)
			}
			cc := rec.Header().Get("Cache-Control")
			if cc != revalidateCacheControl {
				t.Errorf("Cache-Control: got %q, want %q", cc, revalidateCacheControl)
			}
			if strings.Contains(cc, "immutable") {
				t.Errorf("Cache-Control: got %q, want no immutable directive", cc)
			}
			etag := rec.Header().Get("ETag")
			if etag == "" {
				t.Fatalf("ETag is empty for %s", path)
			}
			seen[path] = etag

			rec2 := httptest.NewRecorder()
			req2 := httptest.NewRequest(http.MethodGet, path, nil)
			req2.Header.Set("If-None-Match", etag)
			h.ServeHTTP(rec2, req2)
			if rec2.Code != http.StatusNotModified {
				t.Errorf("conditional status: got %d, want 304", rec2.Code)
			}
		})
	}

	if len(seen) == 2 && seen["/config.js"] == seen["/favicon.svg"] {
		t.Errorf("config.js and favicon.svg share an ETag %q — the validator is not per-file", seen["/config.js"])
	}
}
