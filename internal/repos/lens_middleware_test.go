package repos

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
)

func lensMWRouter(m *Manager, probe http.HandlerFunc) *chi.Mux {
	r := chi.NewRouter()
	r.With(LensMiddleware(m)).Get("/lenses/{lens}/mcp", probe)
	return r
}

func TestLensMiddleware_SetsBinding(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Registry().Create(Lens{Name: "eng", Write: config.DefaultRepoName})
	require.NoError(t, err)

	var got *Binding
	r := lensMWRouter(m, func(w http.ResponseWriter, req *http.Request) {
		b, ok := BindingFromContextOpt(req.Context())
		require.True(t, ok)
		got = b

		// The write repo is also exposed as the context RepoInstance so
		// repo-based paths (AfterInitialize instructions) work on the lens
		// endpoint until Phase 5 makes them binding-aware.
		ri, ok := RepoFromContextOpt(req.Context())
		require.True(t, ok)
		require.Same(t, m.Get(config.DefaultRepoName), ri)

		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/lenses/eng/mcp", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.NotNil(t, got)
	require.Equal(t, "eng", got.Name())
	require.Same(t, m.Get(config.DefaultRepoName), got.Write())
}

func TestLensMiddleware_UnknownLens404(t *testing.T) {
	m := newLifecycleManager(t)
	r := lensMWRouter(m, func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(http.StatusOK) })

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/lenses/nope/mcp", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestLensMiddleware_UnavailableMember503(t *testing.T) {
	m := newLifecycleManager(t)
	_, err := m.Registry().Create(Lens{Name: "broken", Write: "ghost"})
	require.NoError(t, err)

	r := lensMWRouter(m, func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(http.StatusOK) })
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("GET", "/lenses/broken/mcp", nil))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "ghost")
}
