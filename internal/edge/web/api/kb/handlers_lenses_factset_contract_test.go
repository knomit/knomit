package kb

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// B2. There is NO lens FactSet route: kb.Mount registers search/factset and
// its export on the branch router only. On a lens the request is answered as
// any other unknown lens route, with the same status and the same body, while
// the lens's real routes and the repo's factset route answer 200 on the same
// data. SUPERSEDED by the lens facade's REST tests (F3), which add the route.
//
// Sabotage: register `lens.Get("/search/factset", ...)` in Mount; the lens
// factset leaves the unknown-route answer and the equality assertions fail.
func TestFactSet_Contract_NoLensFactSetRoute(t *testing.T) {
	m := newMatLensManager(t, "w", "r")
	createLens(t, m, "both", "w", "r")
	w := m.Get("w")
	writeMatFact(t, w, w.AgentBranch(), "kb/technology/u/a.md", "A", "A body.")
	h := asPrincipal("u1", asLocalCaller(newTestRouter(m)))

	// Reached: the lens lists the fact, and the repo's factset route serves it.
	// (Indexing of a write is asynchronous: wait for the lens to list it.)
	var code int
	var body string
	require.Eventually(t, func() bool {
		code, _, body = getMaterialize(t, h, "/lenses/both/facts?path=kb/technology/u/")
		return code == http.StatusOK && strings.Contains(body, "kb/technology/u/a.md")
	}, 10*time.Second, 50*time.Millisecond, "the lens never listed the fact: %d %s", code, body)
	code, _, body = getMaterialize(t, h, "/repos/w/branches/"+urlBranch(w.AgentBranch())+"/search/factset?path=kb/technology/u/")
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, body, "kb/technology/u/a.md")

	unkCode, unkType, unkBody := getMaterialize(t, h, "/lenses/both/nosuch")
	require.Equal(t, http.StatusNotFound, unkCode, unkBody)
	for _, u := range []string{
		"/lenses/both/search/factset?path=kb/technology/u/",
		"/lenses/both/search/factset/export?path=kb/technology/u/",
	} {
		code, ctype, got := getMaterialize(t, h, u)
		require.Equal(t, unkCode, code, u)
		require.Equal(t, unkType, ctype, u)
		require.Equal(t, unkBody, got, "%s answers exactly like an unknown lens route", u)
	}
}
