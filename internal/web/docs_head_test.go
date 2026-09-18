package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// /docs is 8 KB of text/html served from the outer router, which carries no
// compressor of its own — so until this it went out raw even though
// "text/html" has been on the allowlist all along.
func TestDocs_IsGzipped(t *testing.T) {
	h := staticTestServer(t).Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	// handleSwaggerUI sets a charset parameter; chi cuts the media type at
	// ";" before matching, which is what makes the bare "text/html" entry
	// match. If that ever stops being true this is where it shows.
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("content-type: got %q, want text/html; charset=utf-8", got)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding: got %q, want gzip", got)
	}
	body := gunzip(t, rec.Body)
	if string(body) != string(swaggerHTML) {
		t.Errorf("gunzipped body differs from the embedded swagger page (%d vs %d bytes)", len(body), len(swaggerHTML))
	}
	if !strings.Contains(string(body), "knomit — API Docs") {
		t.Errorf("gunzipped body is not the swagger page: %.80q", body)
	}
	if rec.Body.Len() >= len(swaggerHTML) {
		t.Errorf("compressed size %d is not smaller than the %d-byte source", rec.Body.Len(), len(swaggerHTML))
	}
}

// A client that does not ask for gzip must still get a readable page.
func TestDocs_WithoutAcceptEncodingIsIdentity(t *testing.T) {
	h := staticTestServer(t).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/docs", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding: got %q, want empty", got)
	}
	if rec.Body.String() != string(swaggerHTML) {
		t.Errorf("identity body differs from the embedded swagger page")
	}
}

// Uptime checks HEAD the root. The SPA fallback answered 405, so every such
// check reported the app down while it was serving fine.
func TestSPA_HeadMatchesGet(t *testing.T) {
	h := staticTestServer(t).Handler()

	for _, path := range []string{"/", "/deep/client/route", "/config.js"} {
		t.Run(path, func(t *testing.T) {
			get := httptest.NewRecorder()
			h.ServeHTTP(get, httptest.NewRequest(http.MethodGet, path, nil))
			if get.Code != http.StatusOK {
				t.Fatalf("GET status: got %d, want 200", get.Code)
			}

			head := httptest.NewRecorder()
			h.ServeHTTP(head, httptest.NewRequest(http.MethodHead, path, nil))

			if head.Code != http.StatusOK {
				t.Fatalf("HEAD status: got %d, want 200", head.Code)
			}
			if head.Body.Len() != 0 {
				t.Errorf("HEAD body: got %d bytes, want 0", head.Body.Len())
			}
			// A HEAD must be able to stand in for the GET it previews, so
			// the validators have to be identical.
			for _, hdr := range []string{"ETag", "Cache-Control", "Content-Type"} {
				if g, hd := get.Header().Get(hdr), head.Header().Get(hdr); g != hd {
					t.Errorf("%s: GET %q, HEAD %q — they must match", hdr, g, hd)
				}
			}
			if got := head.Header().Get("Cache-Control"); got != revalidateCacheControl {
				t.Errorf("HEAD Cache-Control: got %q, want %q", got, revalidateCacheControl)
			}
			if head.Header().Get("ETag") == "" {
				t.Errorf("HEAD ETag is empty")
			}
		})
	}
}

// HEAD is added; nothing else is. The SPA route stays read-only.
func TestSPA_OtherMethodsAreStill405(t *testing.T) {
	h := staticTestServer(t).Handler()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, "/", nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s /: got %d, want 405", method, rec.Code)
			}
		})
	}
}

func TestDocs_HeadIs200(t *testing.T) {
	h := staticTestServer(t).Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/docs", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD /docs: got %d, want 200", rec.Code)
	}
}

// httptest.NewRecorder does NOT suppress a HEAD response body — only a real
// net/http server does, by discarding writes for HEAD requests. So a
// recorder proves nothing about the body of a handler that calls w.Write
// unconditionally, which handleSwaggerUI does.
//
// http.ServeContent checks the method itself, so the SPA paths come back
// empty either way; /docs relies entirely on the server. This test is the
// only place that distinction is actually exercised.
func TestHead_SendsNoBodyThroughARealServer(t *testing.T) {
	srv := httptest.NewServer(staticTestServer(t).Handler())
	defer srv.Close()

	for _, path := range []string{"/", "/deep/client/route", "/docs", "/assets/index-abc123.js"} {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodHead, srv.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status: got %d, want 200", resp.StatusCode)
			}
			b, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			if len(b) != 0 {
				t.Errorf("HEAD body: got %d bytes, want 0", len(b))
			}
		})
	}
}

// /assets/* used to be registered with r.Handle, which answers EVERY method —
// so POST to a bundle returned 200 with the whole file. It is a read-only
// file server, so nothing was mutable, but it is the same method-registration
// asymmetry that made HEAD / a 405, pointing the other way.
func TestAssets_AreGetAndHeadOnly(t *testing.T) {
	h := staticTestServer(t).Handler()
	const asset = "/assets/index-abc123.js"

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, asset, nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s: got %d, want 405", method, asset, rec.Code)
			}
			if rec.Body.Len() != 0 {
				t.Errorf("%s %s: body %d bytes, want 0 — the file must not be served", method, asset, rec.Body.Len())
			}
		})
	}

	// The two that must keep working.
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(method, asset, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s: got %d, want 200", method, asset, rec.Code)
			}
			if got := rec.Header().Get("Cache-Control"); got != immutableCacheControl {
				t.Errorf("%s Cache-Control: got %q, want %q", method, got, immutableCacheControl)
			}
		})
	}
}
