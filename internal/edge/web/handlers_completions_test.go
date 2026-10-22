package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/web/hal"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/index"
)

// newRealStoreManager is a Manager whose repos each hold a REAL store (agent
// branch "agent/test", the branch newTestManagerWithRepos names), so a handler
// test reads what a store holds and fails a read through the owner's fault
// hook rather than a stub. It returns the manager and the stores by repo name.
func newRealStoreManager(t *testing.T, names ...string) (*repos.Manager, map[string]*store.Service) {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{})
	svcs := map[string]*store.Service{}
	for _, name := range names {
		svc, err := store.Open(filepath.Join(t.TempDir(), name+".db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = svc.Close() })
		require.NoError(t, svc.InitRepo(context.Background(), map[string]string{}, "agent/test"))
		m.Set(name, repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
			Name: name, AgentBranch: "agent/test", Svc: svc,
		}))
		svcs[name] = svc
	}
	return m, svcs
}

// writeDomainFact writes one observation carrying the given domains to the
// agent branch of svc.
func writeDomainFact(t *testing.T, svc *store.Service, path, title string, domains ...string) {
	t.Helper()
	dom, err := json.Marshal(domains)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), "agent/test", path,
		"---\ntype: observation\nconfidence: 0.9\ndomain: "+string(dom)+"\n---\n# "+title+"\n\nbody\n",
		"add "+path, "")
	require.NoError(t, err)
}

// failIndexRead fails exactly the named index read with err and fails no other
// read. hits counts how often the named read reached the hook; it is atomic
// because the lens fan-out reads every mount on its own goroutine.
func failIndexRead(name index.ReadName, err error, hits *atomic.Int64) index.ReadFault {
	return func(read index.ReadName) error {
		if read != name {
			return nil
		}
		hits.Add(1)
		return err
	}
}

func getAPI(t *testing.T, s *Server, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	s.NewAPIRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec
}

func TestHandleHALCompletions_ReturnsHAL(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha")
	writeDomainFact(t, svcs["alpha"], "kb/a/one.md", "One", "ai", "ai safety")
	writeDomainFact(t, svcs["alpha"], "kb/a/two.md", "Two", "alignment")
	s := &Server{Manager: m}

	rec := getAPI(t, s, "/repos/alpha/branches/agent:test/completions?category=domain&prefix=ai")

	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != hal.ContentType {
		t.Errorf("content-type: got %q, want %q", got, hal.ContentType)
	}

	var body struct {
		Values []string    `json:"values"`
		Links  hal.LinkMap `json:"_links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The prefix narrows to the two "ai…" domains; "alignment" is not one.
	sort.Strings(body.Values)
	require.Equal(t, []string{"ai", "ai safety"}, body.Values)
	if _, ok := body.Links["self"]; !ok {
		t.Error("missing self link")
	}
	// Self link must include query params.
	wantSelf := APIBase + "/repos/alpha/branches/agent:test/completions?category=domain&prefix=ai"
	if got := body.Links["self"].Href; got != wantSelf {
		t.Errorf("self: got %q, want %q", got, wantSelf)
	}
}

// A repo whose store holds no value for the category answers values:[], never
// null.
func TestHandleHALCompletions_NoValuesIsAnEmptyArray(t *testing.T) {
	m, _ := newRealStoreManager(t, "alpha")
	rec := getAPI(t, &Server{Manager: m}, "/repos/alpha/branches/agent:test/completions?category=domain")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), `"values":[]`)
}

func TestHandleHALCompletions_UnknownRepo_Returns404(t *testing.T) {
	m, _ := newRealStoreManager(t)
	rec := getAPI(t, &Server{Manager: m}, "/repos/missing/branches/main/completions")

	if rec.Code != http.StatusNotFound {
		t.Errorf("status: %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("content-type: %q", got)
	}
}

// The store's Completions read fails: the handler answers 500 carrying THAT
// error (the injected sentinel's text), and the fault fired for the
// Completions read and no other.
func TestHandleHALCompletions_StoreError_Returns500(t *testing.T) {
	m, svcs := newRealStoreManager(t, "alpha")
	injected := errors.New("injected completions read failure")
	var hits atomic.Int64
	t.Cleanup(svcs["alpha"].Search().SetReadFaultForTest(failIndexRead(index.ReadCompletions, injected, &hits)))

	rec := getAPI(t, &Server{Manager: m}, "/repos/alpha/branches/agent:test/completions")

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), injected.Error())
	require.EqualValues(t, 1, hits.Load(), "the Completions read must have reached the hook exactly once")
}
