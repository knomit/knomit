package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	knomitfact "knomit/internal/fact"
	"knomit/internal/store"
)

// #428 follow-ups (N1, N4): the REST write door checks a fact's NEW ref to a
// .knomit/ file against the tree at the branch tip, and a symlink under
// .knomit/ is never followed — it reads as absent at every REST door.

const restSymlink = ".knomit/skills/link.md" // -> x.md, a regular file beside it

// seedSymlink commits restSymlink pointing at a regular file inside .knomit/.
func (h threeRootsREST) seedSymlink(t *testing.T) {
	t.Helper()
	require.NoError(t, h.ri.WithRead(func(svc *store.Service) {
		_, err := svc.RawSymlinkForTest(context.Background(), h.ri.AgentBranch(), restSymlink, "x.md", "a symlink pushed through git")
		require.NoError(t, err)
	}))
}

func putWithRefs(t *testing.T, refs ...string) string {
	t.Helper()
	list, err := json.Marshal(refs)
	require.NoError(t, err)
	content, err := json.Marshal(map[string]string{"content": "---\ntype: observation\nconfidence: 0.6\ndomain: [test]\nentities: []\nrefs: " +
		string(list) + "\n---\n# real\n\nbody cites a system file\n"})
	require.NoError(t, err)
	return string(content)
}

// TestFactPUT_SystemFileRefMustExist: a PUT whose content ADDS a ref to a
// .knomit/ file is refused 422 with the gate's .knomit/ section when the file
// is missing or is a symlink (never followed), and nothing moves; the same PUT
// citing the regular file lands with the ref stored canonical.
// Sabotage: systemFileResolver returns true always → the missing and symlink
// PUTs land → red; returns false always → the existing-file PUT is 422 → red.
func TestFactPUT_SystemFileRefMustExist(t *testing.T) {
	h := newThreeRootsREST(t)
	h.seedSymlink(t)
	url := "/repos/alpha/branches/" + h.b + "/facts/kb/verdicts/real.md"

	for _, ref := range []string{".knomit/skills/missing.md", restSymlink} {
		before := h.tip(t)
		rec := h.do(t, http.MethodPut, url, putWithRefs(t, ref))
		require.Equalf(t, http.StatusUnprocessableEntity, rec.Code, "PUT citing %s: %s", ref, rec.Body.String())
		require.Containsf(t, rec.Body.String(), "does not exist under .knomit/ at the tip", "PUT citing %s", ref)
		require.Containsf(t, rec.Body.String(), ref, "the refusal names the ref")
		require.Equalf(t, before, h.tip(t), "a refused PUT citing %s writes nothing", ref)
	}

	rec := h.do(t, http.MethodPut, url, putWithRefs(t, ".knomit/skills/x.md"))
	require.Lessf(t, rec.Code, 300, "PUT citing an existing file: %d %s", rec.Code, rec.Body.String())
	c, ok := h.content(t, "kb/verdicts/real.md")
	require.True(t, ok)
	require.Contains(t, c, "kb://"+knomitfact.ID12(h.ri.ID())+"/.knomit/skills/x.md", "stored canonical")
}

// TestFactGET_SystemFileSymlinkIs404 (N4, user ruling 2026-10-08: "we do NOT
// want to follow symlinks, so 404"): a symlink under .knomit/ that points at a
// regular file beside it is 404 on the branch, commit and lens GET routes,
// while the regular file it points at is still served.
// Sabotage: drop the symlink clause in store.SystemFileAt → 200 with the link
// text "x.md" → red.
func TestFactGET_SystemFileSymlinkIs404(t *testing.T) {
	h := newThreeRootsREST(t)
	h.seedSymlink(t)
	tip := h.tip(t)
	for _, u := range []string{
		"/repos/alpha/branches/" + h.b + "/facts/" + restSymlink,
		"/repos/alpha/branches/" + h.b + "/commits/" + tip + "/facts/" + restSymlink,
		"/lenses/eng/facts/" + restSymlink,
		"/lenses/eng/facts/" + url.PathEscape("kb://"+knomitfact.ID12(h.ri.ID())+"/"+restSymlink),
	} {
		rec := h.do(t, http.MethodGet, u, "")
		require.Equalf(t, http.StatusNotFound, rec.Code, "GET %s: %s", u, rec.Body.String())
		require.NotEqualf(t, "x.md", rec.Body.String(), "GET %s must not serve the link text", u)
	}
	rec := h.do(t, http.MethodGet, "/repos/alpha/branches/"+h.b+"/facts/.knomit/skills/x.md", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
