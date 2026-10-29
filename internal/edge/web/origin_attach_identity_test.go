package web

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/testsupport/gitfixture"
)

// An attach with replace_main replaces X's knowledge base: the first sync
// round force-updates X's main to the origin's and replays X's agent branch
// onto it, so the root of X's read branch becomes the origin's. That round
// records it (reidentifyAfterRewind, Identify's rule), not X's next mount:
//
//   - before, HeldByAnotherActiveRepo did not name X for the origin's
//     knowledge base until X remounted, so a second local copy of it could be
//     created in between; and
//   - at X's next mount, Identify then found that copy holding X's new root
//     and turned X Unavailable (identityConflict).
//
// The fixture moves the origin AHEAD after the attach and creates the copy
// from a mirror, so X holds none of its tips (create's held-tip layer stays
// silent) and the URL differs (the same-URL preflight stays silent): only the
// recorded identity can refuse the copy, the trap
// kb/decisions/repos/identity/three-layer-duplicate-refusal names.
//
// SABOTAGE: drop the rewound callback in runReconcileLoop -> the repo_id
// assertion is red; with it skipped, the copy is created and X is Unavailable
// after the restart.
func TestOriginEscalation_ReplaceMainReidentifiesAtOnce(t *testing.T) {
	ctx := context.Background()
	f := newEscFixture(t)
	riX := f.createX(t)
	oldRoot := riX.ID()
	publishOnMain(t, riX, "kb/own.md", "X-OWN-FACT")
	oauthH := f.s.OAuthHandler()
	tok := f.mintOperatorToken(t, "x")
	bare := filepath.Join(f.root, "team-kb.git")
	url := f.seedForeignKB(t, "team-kb.git")
	out, err := escGitOut(t, bare, "rev-list", "--first-parent", "--max-parents=0", "main")
	require.NoError(t, err, out)
	teamRoot := strings.TrimSpace(out)
	require.NotEqual(t, oldRoot, teamRoot, "precondition: unrelated histories")

	rec := escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+url+`","branch":"main","replace_main":true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	reg, ok, err := f.m.Repos().Get(riX.UID())
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, teamRoot, reg.RepoID, "X's recorded identity is the origin's root after the replace round, before any remount")
	holder, err := repos.HeldByAnotherActiveRepo(f.m, "", teamRoot)
	require.NoError(t, err)
	require.Equal(t, "x", holder)

	// The team moves its main ahead, so X holds none of its tips, and a
	// mirror of that main gives the same knowledge base another URL.
	work := t.TempDir()
	runGitForTest(t, "", "clone", "-q", bare, work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "kb", "later.md"), []byte(originFactBody("LATER")), 0o644))
	runGitForTest(t, work, "add", "-A")
	runGitForTest(t, work, "commit", "-q", "-m", "later")
	runGitForTest(t, work, "push", "-q", "origin", "main")
	mirror := gitfixture.InitBare(t, filepath.Join(f.root, "team-mirror.git"), "main")
	runGitForTest(t, work, "push", "-q", mirror, "main")

	_, err = f.m.Create(ctx, repos.CreateSpec{Name: "z", Mode: "clone", Origin: &repos.OriginSpec{URL: mirror}}, nil)
	require.ErrorIs(t, err, repos.ErrKnowledgeBaseAlreadyLocal, "a second local copy of the knowledge base X now holds")
	require.Nil(t, f.m.Get("z"))

	// X stays available across a remount: Identify re-derives the same root
	// from X's store and records it again.
	require.NoError(t, f.m.Close())
	m2 := repos.New(ctx, f.deps)
	t.Cleanup(func() { _ = m2.Close() })
	require.NoError(t, m2.Start())
	require.Empty(t, m2.Unavailable(), "X must not be Unavailable after its next mount")
	x2 := m2.Get("x")
	require.NotNil(t, x2)
	require.Equal(t, teamRoot, x2.ID(), "X's store carries the origin's knowledge base")
}

// replace_main never admits a root another active repo holds: the attach is
// refused up front, with nothing stored and X's identity untouched, so no
// repo can end up holding the same knowledge base and Unavailable later.
//
// SABOTAGE: drop the root-layer holder check in guardAttachIdentity -> 200,
// and X's next mount would conflict with Y -> red.
func TestOriginEscalation_ReplaceMainOntoAnotherReposRootIsRefused(t *testing.T) {
	f := newEscFixture(t)
	ctx := context.Background()
	yBare := filepath.Join(f.root, "y.git")
	yURL := seedBareRemoteKBForTest(t, yBare)
	_, err := f.m.Create(ctx, repos.CreateSpec{Name: "y", Mode: "clone", Origin: &repos.OriginSpec{URL: yURL}}, nil)
	require.NoError(t, err)
	// A plain git remote one commit ahead of Y: no claim, no tip Y holds, so
	// only the root names Y.
	zBare := gitfixture.InitBare(t, filepath.Join(f.root, "z.git"), "main")
	work := t.TempDir()
	runGitForTest(t, "", "clone", "-q", yBare, work)
	require.NoError(t, os.MkdirAll(filepath.Join(work, "kb"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, "kb", "secret.md"), []byte(originFactBody(escSecret)), 0o644))
	runGitForTest(t, work, "add", "-A")
	runGitForTest(t, work, "commit", "-q", "-m", "ahead of y")
	runGitForTest(t, work, "push", "-q", zBare, "main")

	riX := f.createX(t)
	publishOnMain(t, riX, "kb/own.md", "X-OWN-FACT")
	before, ok, err := f.m.Repos().Get(riX.UID())
	require.NoError(t, err)
	require.True(t, ok)
	oauthH := f.s.OAuthHandler()
	tok := f.mintOperatorToken(t, "x")
	rec := escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+zBare+`","branch":"main","replace_main":true}`)
	escProblem(t, rec, http.StatusConflict, "Knowledge base already local")
	f.requireNoOrigin(t, riX)
	after, _, err := f.m.Repos().Get(riX.UID())
	require.NoError(t, err)
	require.Equal(t, before.RepoID, after.RepoID, "a refused attach leaves X's identity as it was")
}
