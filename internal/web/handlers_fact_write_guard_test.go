package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
)

// refusedByGuard drives PUT and DELETE at path and asserts BOTH are refused
// with 400 and that neither ever reached the writer. The pair matters: the two
// handlers carry the same rule in two places, and a fix applied to one only is
// the exact shape of bug this file exists to catch.
func refusedByGuard(t *testing.T, path string) {
	t.Helper()
	writer := &stubFactWriter{writeHash: "abc123"}
	s := &Server{
		Manager:   newTestManagerWithRepos(t, "alpha"),
		providers: storeProviders{factWriter: writer},
	}
	r := s.NewAPIRouter()

	rec := httptest.NewRecorder()
	req := fromLoopback(httptest.NewRequest(http.MethodPut,
		"/repos/alpha/branches/agent:test/facts/"+path,
		strings.NewReader(`{"content":"`+testFactContent+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("PUT %s: status %d, want 400, body=%s", path, rec.Code, rec.Body.String())
	}
	if writer.writeCalls != 0 {
		t.Errorf("PUT %s: writer.Write called %d times; it must never reach git", path, writer.writeCalls)
	}

	rec = httptest.NewRecorder()
	req = fromLoopback(httptest.NewRequest(http.MethodDelete,
		"/repos/alpha/branches/agent:test/facts/"+path, nil))
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("DELETE %s: status %d, want 400, body=%s", path, rec.Code, rec.Body.String())
	}
	if writer.deleteCalls != 0 {
		t.Errorf("DELETE %s: writer.Delete called %d times; it must never reach git", path, writer.deleteCalls)
	}
}

// TestFactEndpoints_CannotShadowAServerOwnedFileWithADirectory is the endpoint
// half of the dotless-area rule. The depth rule protects .knomit/ontology.yaml
// by NAME; nothing stopped that name being reused as a DIRECTORY. store's
// buildTree drops an existing entry of the same name whatever its mode, so
// PUT …/facts/.knomit/ontology.yaml/x.md replaces the ontology BLOB with a
// tree — verified end-to-end against a real store: the write succeeded and the
// next read failed with "blob: object not found", after which loadOntology
// falls through to the embedded default and every later fact is validated
// against the wrong taxonomy.
func TestFactEndpoints_CannotShadowAServerOwnedFileWithADirectory(t *testing.T) {
	for _, path := range []string{
		".knomit/ontology.yaml/x.md",
		".knomit/ontology.yaml/deeper/x.md",
		".knomit/foo.bar/x.md",
	} {
		t.Run(path, func(t *testing.T) { refusedByGuard(t, path) })
	}
}

// TestFactEndpoints_RefuseServerOwnedFiles: the fact endpoints take their path
// verbatim from the caller, and knomit's own root files are not facts. They
// have dedicated write paths — WriteReadme with its size cap and its
// exact-case WriteRootFile door, and for LICENSE no write path at all, because
// a licence is authored by whoever owns the repo. Routing them through
// PUT/DELETE …/facts/… bypasses all of that.
//
// Case-insensitively, because a fact path is LOWERCASED on the way to git: a
// PUT to "README.md" does not overwrite the manifest, it plants a SEPARATE
// root file "readme.md" — which GitHub and GitLab, resolving these names
// case-insensitively, would render as the repository's README while knomit
// keeps reporting the real one. "LICENSE" plants "license" the same way.
//
// domains/ontology.yaml is here because it is server-owned but NOT private:
// the dot-prefixed rungs of the ontology chain are covered by the private-path
// guard above, and the oldest rung would otherwise be the one location an
// agent could rewrite an unmigrated repo's taxonomy through.
func TestFactEndpoints_RefuseServerOwnedFiles(t *testing.T) {
	for _, path := range []string{
		"README.md", "readme.md", "LICENSE", "license",
		"domains/ontology.yaml",
		// The same shadow-as-a-directory trick as the ontology, on the rung
		// the private-path guard does not cover.
		"domains/ontology.yaml/x.md",
	} {
		t.Run(path, func(t *testing.T) { refusedByGuard(t, path) })
	}
}

// The guard must not swallow ordinary facts that merely LOOK adjacent.
func TestFactEndpoints_AllowOrdinaryFactsNearServerOwnedNames(t *testing.T) {
	for _, path := range []string{
		"kb/architecture/readme-rendering/a1b2c3d4.md",
		"kb/decisions/licensing/a1b2c3d4.md",
		"domains/ontology.yaml.md",
	} {
		t.Run(path, func(t *testing.T) {
			writer := &stubFactWriter{writeHash: "abc123"}
			s := &Server{
				Manager:   newTestManagerWithRepos(t, "alpha"),
				providers: storeProviders{factWriter: writer},
			}
			r := s.NewAPIRouter()

			rec := httptest.NewRecorder()
			req := fromLoopback(httptest.NewRequest(http.MethodPut,
				"/repos/alpha/branches/agent:test/facts/"+path,
				strings.NewReader(`{"content":"`+testFactContent+`"}`)))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("PUT %s: status %d, want 200, body=%s", path, rec.Code, rec.Body.String())
			}
		})
	}
}

// The REST fact writers must consult the branch write-eligibility
// classification. Two cases, and they are not the same rule:
//
//   - a SUBSCRIPTION accepts no writes on any branch; and
//   - an ORDINARY repo accepts them only on its own agent branch, so the
//     consensus branch is refused too.
//
// The second is the one that makes this a behaviour change for every REST
// client, and without a case for it the gate could be narrowed to
// `ri.Subscribed() && …` with the whole suite still green.
//
// The refusal DETAIL is asserted per case, not just the status: telling a
// subscription's client to use "the repo's own agent branch" would name a
// branch that does not exist and that the DTO deliberately omits.
func TestFactWrite_RefusesNonWritableBranchWith403(t *testing.T) {
	m := repos.New(context.Background(), repos.Deps{})
	m.Set("sub", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "sub", Subscribed: true, ReadBranch: "main"}))
	m.Set("rw", repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "rw", AgentBranch: "agent/test"}))
	writer := &stubFactWriter{writeHash: "abc123"}
	s := &Server{Manager: m, providers: storeProviders{factWriter: writer}}
	r := s.NewAPIRouter()

	const subDetail = "is a subscription: it follows a remote branch read-only"
	const rwDetail = "only the repo's own agent branch accepts writes"

	for _, tc := range []struct{ name, method, path, body, wantDetail string }{
		{"subscription PUT", http.MethodPut, "/repos/sub/branches/main/facts/kb/x.md", `{"content":"` + testFactContent + `"}`, subDetail},
		{"subscription DELETE", http.MethodDelete, "/repos/sub/branches/main/facts/kb/x.md", ``, subDetail},
		{"subscription POST", http.MethodPost, "/repos/sub/branches/main/facts", `{"title":"X","content":"` + testFactContent + `","topic":"gotchas","category":"a"}`, subDetail},
		// An ORDINARY repo, refused on the consensus branch. This is the entry
		// that pins the gate as a whole-surface rule rather than a subscription
		// special case.
		{"ordinary repo on the consensus branch", http.MethodPut, "/repos/rw/branches/main/facts/kb/x.md", `{"content":"` + testFactContent + `"}`, rwDetail},
	} {
		rec := httptest.NewRecorder()
		req := fromLoopback(httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(rec, req)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s: %s %s: body=%s", tc.name, tc.method, tc.path, rec.Body.String())
		require.Contains(t, rec.Body.String(), "Read-only branch", tc.name)
		require.Contains(t, rec.Body.String(), tc.wantDetail, "%s: wrong advice in the refusal detail", tc.name)
	}
	require.Zero(t, writer.writeCalls, "the writer must never be reached")
	require.Zero(t, writer.deleteCalls, "the writer must never be reached")
}
