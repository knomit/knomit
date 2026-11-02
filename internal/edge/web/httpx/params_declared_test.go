package httpx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// A family declares only its members: "context." alone, or a name that only
// shares a stem, is unknown.
func TestDeclaredParams_FamilyPrefix(t *testing.T) {
	d := DeclareParams("q", "context.<key>")
	require.True(t, d.Has("q"))
	require.True(t, d.Has("context.task"))
	require.False(t, d.Has("context."))
	require.False(t, d.Has("contextual"))
	require.False(t, d.Has("qq"))
	require.Equal(t, []string{"context.<key>", "q"}, d.Names())
}

// The empty set refuses every parameter, the first in name order, and a
// malformed query before any name; a request naming only declared parameters
// proceeds and nothing is written.
func TestDeclaredParams_RefuseUnknown(t *testing.T) {
	for raw, want := range map[string]string{
		"b=1&a=1": `unknown parameter \"a\"`,
		"%zz=1":   `malformed query string: invalid URL escape \"%zz\"`,
		"a;b=1":   "malformed query string: invalid semicolon separator in query",
	} {
		rec := httptest.NewRecorder()
		ok := DeclareParams().RefuseUnknown(rec, httptest.NewRequest(http.MethodGet, "/x?"+raw, nil))
		require.False(t, ok, raw)
		require.Equal(t, http.StatusBadRequest, rec.Code, raw)
		require.Contains(t, rec.Body.String(), `"title":"Invalid parameter"`, raw)
		require.Contains(t, rec.Body.String(), want, raw)
	}
	rec := httptest.NewRecorder()
	require.True(t, DeclareParams("limit").RefuseUnknown(rec, httptest.NewRequest(http.MethodGet, "/x?limit=1", nil)))
	require.Zero(t, rec.Body.Len())
}
