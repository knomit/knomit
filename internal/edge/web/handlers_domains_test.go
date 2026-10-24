package web

import (
	"encoding/json"
	"errors"
	"knomit/internal/edge/web/httpx"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store/index"
)

func TestHandleHALDomains_ReturnsCollection(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha")
	for i, path := range []string{"kb/a/1.md", "kb/a/2.md", "kb/a/3.md"} {
		writeDomainFact(t, svcs["alpha"], path, "AI "+string(rune('A'+i)), "ai")
	}
	writeDomainFact(t, svcs["alpha"], "kb/g/1.md", "Go 1", "go")
	writeDomainFact(t, svcs["alpha"], "kb/g/2.md", "Go 2", "go")
	s := &Server{Manager: m}

	rec := getAPI(t, s, "/repos/alpha/branches/agent:test/domains")

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
		Count    int         `json:"count"`
		Links    hal.LinkMap `json:"_links"`
		Embedded struct {
			Domains []struct {
				Name  string      `json:"name"`
				Count int         `json:"count"`
				Links hal.LinkMap `json:"_links"`
			} `json:"domains"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Count != 2 {
		t.Errorf("count: %d, want 2", body.Count)
	}
	if _, ok := body.Links["self"]; !ok {
		t.Error("missing self link")
	}
	if len(body.Embedded.Domains) != 2 {
		t.Fatalf("domains: %d, want 2", len(body.Embedded.Domains))
	}
	// Domains should be sorted alphabetically, each with the store's count.
	if body.Embedded.Domains[0].Name != "ai" || body.Embedded.Domains[0].Count != 3 {
		t.Errorf("first domain: %+v, want ai x3", body.Embedded.Domains[0])
	}
	if body.Embedded.Domains[1].Name != "go" || body.Embedded.Domains[1].Count != 2 {
		t.Errorf("second domain: %+v, want go x2", body.Embedded.Domains[1])
	}
	// Each domain should have a self link to /domains/{name}.
	wantSelf := httpx.APIBase + "/repos/alpha/branches/agent:test/domains/ai"
	if got := body.Embedded.Domains[0].Links["self"].Href; got != wantSelf {
		t.Errorf("domain self: got %q, want %q", got, wantSelf)
	}
}

func TestHandleHALDomains_UnknownRepo_Returns404(t *testing.T) {
	m, _ := newRealStoreManager(t)
	rec := getAPI(t, &Server{Manager: m}, "/repos/missing/branches/main/domains")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
}

// The domain histogram is the store's Stats read: failing it answers 500
// carrying the injected error.
func TestHandleHALDomains_StoreError_Returns500(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha")
	injected := errors.New("injected stats read failure")
	var hits atomic.Int64
	t.Cleanup(svcs["alpha"].Search().SetReadFaultForTest(failIndexRead(index.ReadStats, injected, &hits)))

	rec := getAPI(t, &Server{Manager: m}, "/repos/alpha/branches/agent:test/domains")

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), injected.Error())
	require.EqualValues(t, 1, hits.Load(), "the Stats read must have reached the hook exactly once")
}

func TestHandleHALDomainFacts_ReturnsCollection(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha")
	writeDomainFact(t, svcs["alpha"], "kb/ai/a.md", "AI Fact", "ai")
	writeDomainFact(t, svcs["alpha"], "kb/ai/b.md", "AI Fact 2", "ai")
	writeDomainFact(t, svcs["alpha"], "kb/go/c.md", "Go Fact", "go")
	s := &Server{Manager: m}

	rec := getAPI(t, s, "/repos/alpha/branches/agent:test/domains/ai")

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
		Count    int         `json:"count"`
		Links    hal.LinkMap `json:"_links"`
		Embedded struct {
			Facts []struct {
				Path  string      `json:"path"`
				Title string      `json:"title"`
				Links hal.LinkMap `json:"_links"`
			} `json:"facts"`
		} `json:"_embedded"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Only the two facts in the ai domain, not the go fact.
	if body.Count != 2 {
		t.Errorf("count: %d, want 2", body.Count)
	}
	if len(body.Embedded.Facts) != 2 {
		t.Fatalf("facts: %d, want 2", len(body.Embedded.Facts))
	}
	paths := map[string]string{}
	for _, f := range body.Embedded.Facts {
		paths[f.Path] = f.Links["self"].Href
	}
	// Each fact should have a self link to the fact URL.
	wantFactSelf := httpx.APIBase + "/repos/alpha/branches/agent:test/facts/kb/ai/a.md"
	if got := paths["kb/ai/a.md"]; got != wantFactSelf {
		t.Errorf("fact self: got %q, want %q (paths=%v)", got, wantFactSelf, paths)
	}
	if _, ok := paths["kb/ai/b.md"]; !ok {
		t.Errorf("kb/ai/b.md missing from %v", paths)
	}
}

// The domain's facts are the store's Search read: failing it answers 500
// carrying the injected error.
func TestHandleHALDomainFacts_StoreError_Returns500(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha")
	injected := errors.New("injected search read failure")
	var hits atomic.Int64
	t.Cleanup(svcs["alpha"].Search().SetReadFaultForTest(failIndexRead(index.ReadSearch, injected, &hits)))

	rec := getAPI(t, &Server{Manager: m}, "/repos/alpha/branches/agent:test/domains/ai")

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), injected.Error())
	require.EqualValues(t, 1, hits.Load(), "the Search read must have reached the hook exactly once")
}
