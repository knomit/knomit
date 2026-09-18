package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"knomit/internal/platform/version"
)

// staticTestServer builds a Server whose UI is a MapFS rather than the
// embedded dist. web/dist holds only .gitkeep until `make web` runs, and the
// Go tests must not depend on the npm build.
func staticTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		Manager: newTestManagerWithRepos(t),
		staticFS: fstest.MapFS{
			"index.html":  {Data: []byte("<!doctype html><title>knomit</title><script src=/assets/index-abc123.js></script>")},
			"config.js":   {Data: []byte("window.KNOMIT_CONFIG={apiBase:'/api/v1'};\n")},
			"favicon.svg": {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 16 16"></svg>`)},
			"assets/index-abc123.js": {Data: []byte(
				"export const x=1;" + strings.Repeat("// padding so the gzip is smaller than the source\n", 40))},
		},
	}
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
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control: got %q, want %q", got, "public, max-age=31536000, immutable")
	}
	body := gunzip(t, rec.Body)
	if !strings.HasPrefix(string(body), "export const x=1;") {
		t.Errorf("gunzipped asset body: got %.40q…, want it to start with the source", body)
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

func TestIndex_NoCacheWithBuildETagAnd304(t *testing.T) {
	h := staticTestServer(t).Handler()
	wantETag := `"` + version.String() + `"`

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control: got %q, want no-cache", got)
	}
	if got := rec.Header().Get("ETag"); got != wantETag {
		t.Fatalf("ETag: got %q, want %q", got, wantETag)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("Content-Encoding: got %q, want gzip", got)
	}
	if body := gunzip(t, rec.Body); !strings.Contains(string(body), "<title>knomit</title>") {
		t.Errorf("index body: got %q, want the index.html contents", body)
	}

	// Warm load: the browser echoes the ETag and gets nothing back.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("If-None-Match", wantETag)
	req2.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusNotModified {
		t.Fatalf("conditional status: got %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 body: got %d bytes, want 0", rec2.Body.Len())
	}
	// net/http's writeNotModified drops Content-Type, which is what keeps
	// the compressor off an empty body. A hand-rolled 304 would not.
	if got := rec2.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("304 Content-Encoding: got %q, want empty", got)
	}
	if got := rec2.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("304 Cache-Control: got %q, want no-cache", got)
	}
}

// Every client-side route falls back to index.html, and that fallback needs
// the same revalidation handshake as / does.
func TestSPAFallback_NoCacheWithBuildETagAnd304(t *testing.T) {
	h := staticTestServer(t).Handler()
	wantETag := `"` + version.String() + `"`

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/repos/alpha/browse", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<title>knomit</title>") {
		t.Fatalf("fallback body: got %q, want index.html", rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control: got %q, want no-cache", got)
	}
	if got := rec.Header().Get("ETag"); got != wantETag {
		t.Fatalf("ETag: got %q, want %q", got, wantETag)
	}

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodGet, "/repos/alpha/browse", nil)
	req2.Header.Set("If-None-Match", wantETag)
	h.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusNotModified {
		t.Fatalf("conditional status: got %d, want 304", rec2.Code)
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("304 body: got %d bytes, want 0", rec2.Body.Len())
	}
}

// config.js and favicon.svg are served from the root, not /assets/, so Vite
// never content-hashes them and they must stay revalidated.
func TestRootFiles_AreRevalidatedNotImmutable(t *testing.T) {
	h := staticTestServer(t).Handler()
	wantETag := `"` + version.String() + `"`

	for _, path := range []string{"/config.js", "/favicon.svg"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

			if rec.Code != http.StatusOK {
				t.Fatalf("status: got %d, want 200", rec.Code)
			}
			cc := rec.Header().Get("Cache-Control")
			if cc != "no-cache" {
				t.Errorf("Cache-Control: got %q, want no-cache", cc)
			}
			if strings.Contains(cc, "immutable") {
				t.Errorf("Cache-Control: got %q, want no immutable directive", cc)
			}
			if got := rec.Header().Get("ETag"); got != wantETag {
				t.Errorf("ETag: got %q, want %q", got, wantETag)
			}
		})
	}
}
