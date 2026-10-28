package web

// Regression tests for the origin escalation: an OAuth token confined to repo
// X (audience /api/v1/repos/X, read+write) must not be able to point X's
// origin at another knowledge base and read it through X. Two layers close
// it, and each has its own tests here:
//
//   - the operator gate on the origin routes (PUT/PATCH/DELETE /origin and
//     the origin-session wizard): the P1, P1b and P2 tests, red without it;
//   - the attach guard's identity checks (repos guardAttachIdentity): an
//     origin holding another active repo's knowledge base is refused even for
//     the operator, and an origin whose main shares no history with X's main
//     is refused unless the operator asks to replace it (replace_main), or
//     X's main holds no fact yet.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/corpus/repos"
	"knomit/internal/edge/oauth"
	"knomit/internal/engine/store"
	"knomit/internal/engine/store/migrate"
	"knomit/internal/foundation/auth"
	"knomit/internal/foundation/config"
	"knomit/internal/testsupport/gitfixture"
	"knomit/internal/testsupport/testsigner"
)

const escAgent = "agent/test"

type escFixture struct {
	m      *repos.Manager
	s      *Server
	store  *oauth.Store
	grants *auth.SQLGrants
	root   string
}

func newEscFixture(t *testing.T) *escFixture {
	return newEscFixtureWith(t, nil)
}

func newEscFixtureWith(t *testing.T, tune func(*config.Config)) *escFixture {
	t.Helper()
	root := t.TempDir()
	home := t.TempDir()
	keyPath := filepath.Join(home, "agent.key")
	require.NoError(t, os.WriteFile(keyPath, []byte("agent-key-material-for-hkdf"), 0o600))
	cfg := config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: root}
	if tune != nil {
		tune(&cfg)
	}
	m := repos.New(context.Background(), repos.Deps{
		Cfg:     cfg,
		AgentID: testsigner.FixtureAgentID(escAgent), AgentBranch: escAgent,
		KeyPath: keyPath,
		Machine: repos.Options{Synchronous: true},
	})
	t.Cleanup(func() { _ = m.Close() })
	require.NoError(t, m.Start())

	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "control.db")+"?_busy_timeout=5000&_journal_mode=WAL"+store.SyncDSNParam())
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, migrate.Control(db))
	f := &escFixture{m: m, root: root, grants: auth.NewSQLGrants(db), store: oauth.NewStore(db, 2*time.Hour, 14*24*time.Hour)}
	f.s = &Server{
		Manager:    m,
		GitHandler: GitRemoteHandler(m), // [git].serve defaults to true
		Auth:       config.AuthConfig{LoopbackDefault: config.Defaults().Auth.LoopbackDefault},
		Grants:     f.grants,
		OAuthIssuer: oauth.NewIssuer(oauth.Options{
			Issuer: testIssuer, Store: f.store, Clients: oauth.NewResolver(nil), Grants: f.grants,
		}),
		BearerVerifier: oauth.NewVerifier(testIssuer, f.store),
	}
	return f
}

// mintRepoToken: a token whose audience is /api/v1/repos/<repo>, scopes
// read+write, grants rows read+write (the ordinary write-capable client).
func (f *escFixture) mintRepoToken(t *testing.T, repo string) string {
	return f.mintToken(t, repo, "x-client", auth.Read, auth.Write)
}

// mintOperatorToken: the same audience, with operator in its scopes and its
// grants, as `knomit oauth approve --scopes read,write,operator` gives.
func (f *escFixture) mintOperatorToken(t *testing.T, repo string) string {
	return f.mintToken(t, repo, "op-client", auth.Read, auth.Write, auth.Operator)
}

func (f *escFixture) mintToken(t *testing.T, repo, subject string, perms ...auth.Permission) string {
	return f.mintTokenAt(t, "/api/v1/repos/"+repo, subject, perms...)
}

// mintTokenAt mints a token whose audience is the issuer plus resourcePath.
func (f *escFixture) mintTokenAt(t *testing.T, resourcePath, subject string, perms ...auth.Permission) string {
	t.Helper()
	ctx := context.Background()
	scopes := make([]string, 0, len(perms))
	for _, p := range perms {
		scopes = append(scopes, string(p))
	}
	fam, err := f.store.CreateFamily(ctx, oauth.FamilySpec{ClientID: "kb", Subject: subject,
		Scopes: scopes, Resource: testIssuer + resourcePath})
	require.NoError(t, err)
	out, err := f.store.IssuePair(ctx, fam.ID)
	require.NoError(t, err)
	for _, p := range perms {
		require.NoError(t, f.grants.Grant(ctx, oauth.TokenPrincipal(subject), p, "test"))
	}
	return out.Access
}

func escDo(h http.Handler, tok, method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func escGitOut(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func escRefHash(forEachRef, ref string) string {
	for _, line := range strings.Split(forEachRef, "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == ref {
			return f[1]
		}
	}
	return ""
}

func escRefs(t *testing.T, bare string) string {
	t.Helper()
	out, err := escGitOut(t, bare, "for-each-ref", "--format=%(refname) %(objectname)")
	require.NoError(t, err, out)
	return out
}

const escSecret = "Y-SECRET-the-X-token-must-never-see"

// createYWithSecret creates repo Y on the same manager, writes the secret
// fact on its agent branch and publishes it on Y's main.
func (f *escFixture) createYWithSecret(t *testing.T) *repos.RepoInstance {
	t.Helper()
	riY, err := f.m.Create(context.Background(), repos.CreateSpec{Name: "y", Mode: "preset"}, nil)
	require.NoError(t, err)
	publishOnMain(t, riY, "kb/secret.md", escSecret)
	return riY
}

// publishOnMain writes a fact on the agent branch and advances the repo's
// main to it, so main holds a fact.
func publishOnMain(t *testing.T, ri *repos.RepoInstance, path, title string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, ri.WithRead(func(s *store.Service) {
		_, werr := s.Facts().WriteFact(ctx, escAgent, path, originFactBody(title), "fact", "")
		require.NoError(t, werr)
		_, rerr := s.Remote().AdvanceLocalUpstream(ctx, escAgent, "main")
		require.NoError(t, rerr)
	}))
}

// seedForeignKB is a knowledge base under local_origin_root that no repo on
// this instance holds, with the secret on its main; disjoint from every repo.
func (f *escFixture) seedForeignKB(t *testing.T, name string) string {
	t.Helper()
	bare := filepath.Join(f.root, name)
	url := seedKnomitRemoteForTest(t, bare, "foreign kb seed "+name)
	work := t.TempDir()
	runGitForTest(t, "", "clone", bare, work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "kb", "secret.md"), []byte(originFactBody(escSecret)), 0o644))
	runGitForTest(t, work, "add", "-A")
	runGitForTest(t, work, "commit", "-m", "secret")
	runGitForTest(t, work, "push", "origin", "main")
	return url
}

// yWithItsOwnOrigin is the realistic layout: y.git under local_origin_root
// is the origin of registered repo Y, which has pushed its agent branch (and
// its secret) there.
func (f *escFixture) yWithItsOwnOrigin(t *testing.T) (*repos.RepoInstance, string, string) {
	t.Helper()
	ctx := context.Background()
	bare := filepath.Join(f.root, "y.git")
	yURL := seedBareRemoteKBForTest(t, bare)
	riY, err := f.m.Create(ctx, repos.CreateSpec{Name: "y", Mode: "clone", Origin: &repos.OriginSpec{URL: yURL}}, nil)
	require.NoError(t, err)
	require.NoError(t, riY.WithRead(func(s *store.Service) {
		_, werr := s.Facts().WriteFact(ctx, escAgent, "kb/secret.md", originFactBody(escSecret), "secret", "")
		require.NoError(t, werr)
	}))
	require.NoError(t, riY.OriginRoundForTest(ctx))
	require.NotEmpty(t, escRefHash(escRefs(t, bare), "refs/heads/"+escAgent), "precondition: Y pushed its agent branch to its origin")
	return riY, yURL, bare
}

func (f *escFixture) createX(t *testing.T) *repos.RepoInstance {
	t.Helper()
	riX, err := f.m.Create(context.Background(), repos.CreateSpec{Name: "x", Mode: "preset"}, nil)
	require.NoError(t, err)
	return riX
}

// requireNoOrigin: X has no origin, so nothing was attached.
func (f *escFixture) requireNoOrigin(t *testing.T, riX *repos.RepoInstance) {
	t.Helper()
	o, err := f.m.Origins().Get(riX.UID())
	require.NoError(t, err)
	require.Nil(t, o, "the refused attach must persist no origin")
}

// requireXMainLacksSecret reads X's main the way the token would.
func requireXMainLacksSecret(t *testing.T, h http.Handler, tok string) {
	t.Helper()
	rec := escDo(h, tok, http.MethodGet, "/api/v1/repos/x/branches/main/facts/kb/secret.md", "")
	require.NotEqual(t, http.StatusOK, rec.Code, "X's main must not serve the foreign secret: %s", rec.Body.String())
	require.NotContains(t, rec.Body.String(), escSecret)
}

// escProblem asserts the status and the problem title.
func escProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, title string) {
	t.Helper()
	require.Equal(t, status, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"title":"`+title+`"`, "body=%s", rec.Body.String())
}

// --- The operator gate -------------------------------------------------------
//
// SABOTAGE (against the branch commit): drop .With(operator) from the PUT
// /origin route in api/manage/mount.go -> P1, P1b and P2 below go red (the
// guard answers 409 or 200 instead of 403).

// P2: X's token points X's origin at the server's own plaintext /git/y.
func TestOriginEscalation_P2_SelfGitMountIsRefused(t *testing.T) {
	f := newEscFixture(t)
	riX := f.createX(t)
	f.createYWithSecret(t)

	plain := httptest.NewServer(f.s.Handler()) // the plaintext listener, on 127.0.0.1
	defer plain.Close()
	oauthH := f.s.OAuthHandler()
	tok := f.mintRepoToken(t, "x")
	rec := escDo(oauthH, tok, http.MethodGet, "/api/v1/repos/y/branches/main/facts/kb/secret.md", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code, "precondition: the X token must not read Y")

	rec = escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+plain.URL+`/git/y","branch":"main"}`)
	escProblem(t, rec, http.StatusForbidden, "Permission denied")
	require.Contains(t, rec.Body.String(), "operator")
	f.requireNoOrigin(t, riX)
	requireXMainLacksSecret(t, oauthH, tok)
}

// P1: a foreign knowledge base under local_origin_root.
func TestOriginEscalation_P1_LocalOriginIsRefused(t *testing.T) {
	f := newEscFixture(t)
	riX := f.createX(t)
	oauthH := f.s.OAuthHandler()
	tok := f.mintRepoToken(t, "x")
	url := f.seedForeignKB(t, "other-kb.git")
	before := escRefs(t, filepath.Join(f.root, "other-kb.git"))

	rec := escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+url+`","branch":"main"}`)
	escProblem(t, rec, http.StatusForbidden, "Permission denied")
	f.requireNoOrigin(t, riX)
	requireXMainLacksSecret(t, oauthH, tok)
	require.Equal(t, before, escRefs(t, filepath.Join(f.root, "other-kb.git")), "nothing was pushed into the foreign repo")
}

// P1b: Y's own origin, which Y has pushed its agent branch to.
func TestOriginEscalation_P1b_OtherReposOwnOriginIsRefused(t *testing.T) {
	f := newEscFixture(t)
	_, yURL, bare := f.yWithItsOwnOrigin(t)
	riX := f.createX(t)
	oauthH := f.s.OAuthHandler()
	tok := f.mintRepoToken(t, "x")
	before := escRefs(t, bare)

	rec := escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+yURL+`","branch":"main"}`)
	escProblem(t, rec, http.StatusForbidden, "Permission denied")
	f.requireNoOrigin(t, riX)
	for _, br := range []string{"main", "agent:test"} {
		rec = escDo(oauthH, tok, http.MethodGet, "/api/v1/repos/x/branches/"+br+"/facts/kb/secret.md", "")
		require.NotContains(t, rec.Body.String(), escSecret, "X's %s", br)
	}
	require.Equal(t, before, escRefs(t, bare), "Y's agent branch in y.git is untouched")
}

// The other origin writes and the wizard sit behind the same gate.
func TestOriginEscalation_OtherOriginRoutesNeedTheOperator(t *testing.T) {
	f := newEscFixture(t)
	f.createX(t)
	oauthH := f.s.OAuthHandler()
	tok := f.mintRepoToken(t, "x")
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPatch, "/api/v1/repos/x/origin/upstream", `{"branch":"main"}`},
		{http.MethodDelete, "/api/v1/repos/x/origin", ""},
		{http.MethodPost, "/api/v1/repos/x/origin-sessions", `{"url":"https://example.invalid/kb.git","auth_method":"none"}`},
		{http.MethodGet, "/api/v1/repos/x/origin-sessions", ""},
	} {
		rec := escDo(oauthH, tok, c.method, c.path, c.body)
		escProblem(t, rec, http.StatusForbidden, "Permission denied")
	}
	// GET /origin stays a read any reader may make.
	rec := escDo(oauthH, tok, http.MethodGet, "/api/v1/repos/x/origin", "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// An operator token still sets an origin: here an empty remote, the backup
// case (nothing to compare, so the identity checks pass it).
func TestOriginEscalation_OperatorTokenSetsAnOrigin(t *testing.T) {
	f := newEscFixture(t)
	riX := f.createX(t)
	oauthH := f.s.OAuthHandler()
	tok := f.mintOperatorToken(t, "x")
	bare := filepath.Join(f.root, "backup.git")
	require.NoError(t, os.MkdirAll(bare, 0o755))
	gitfixture.InitBare(t, bare, "main")

	rec := escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+bare+`","branch":"main"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	o, err := f.m.Origins().Get(riX.UID())
	require.NoError(t, err)
	require.NotNil(t, o)
	require.Equal(t, bare, o.URL)
}

// A local socket caller (the bridge, the CLI) holds operator by the boot
// seed and keeps setting origins as before.
func TestOriginEscalation_LocalSocketCallerSetsAnOrigin(t *testing.T) {
	f := newEscFixture(t)
	riX := f.createX(t)
	ctx := context.Background()
	// The boot seed (app.seedOwnPrincipal): loopback_default for the local principal.
	me, err := auth.LocalPrincipal()
	require.NoError(t, err)
	set, err := auth.ParseSet(f.s.Auth.EffectiveLoopbackDefault())
	require.NoError(t, err)
	for p := range set {
		require.NoError(t, f.grants.Grant(ctx, me, p, "boot:loopback_default"))
	}
	bare := filepath.Join(f.root, "backup.git")
	require.NoError(t, os.MkdirAll(bare, 0o755))
	gitfixture.InitBare(t, bare, "main")

	req := httptest.NewRequest(http.MethodPut, "/api/v1/repos/x/origin", strings.NewReader(`{"url":"`+bare+`","branch":"main"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "@" // a unix conn has no ip; the peer is what counts
	req = req.WithContext(auth.WithPeer(req.Context(), auth.Peer{ID: me.ID, Via: auth.ViaSocket}))
	rec := httptest.NewRecorder()
	f.s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	o, err := f.m.Origins().Get(riX.UID())
	require.NoError(t, err)
	require.NotNil(t, o)
}

// --- One local copy per knowledge base, on attach ------------------------------
//
// X's main holds no fact in these tests, so the unrelated-history check never
// fires: only the one-local-copy check stands between the operator and Y.
//
// SABOTAGE (against the branch commit): delete the refuseIfKnowledgeBaseIsLocal
// call AND the HeldByAnotherActiveRepo(hist.Root) block in guardAttachIdentity
// -> all three tests below go red (200). Deleting only the root block turns
// TestOriginEscalation_OperatorRefusedAnotherReposKBByItsRoot red alone.

func TestOriginEscalation_OperatorRefusedAnotherReposOrigin(t *testing.T) {
	f := newEscFixture(t)
	_, yURL, bare := f.yWithItsOwnOrigin(t)
	riX := f.createX(t)
	oauthH := f.s.OAuthHandler()
	tok := f.mintOperatorToken(t, "x")
	before := escRefs(t, bare)

	// replace_main never admits another repo's copy.
	for _, body := range []string{
		`{"url":"` + yURL + `","branch":"main"}`,
		`{"url":"` + yURL + `","branch":"main","replace_main":true}`,
	} {
		rec := escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", body)
		escProblem(t, rec, http.StatusConflict, "Knowledge base already local")
		require.Contains(t, rec.Body.String(), `\"y\"`)
	}
	f.requireNoOrigin(t, riX)
	require.Equal(t, before, escRefs(t, bare))
}

func TestOriginEscalation_OperatorRefusedSelfGitMount(t *testing.T) {
	f := newEscFixture(t)
	riX := f.createX(t)
	f.createYWithSecret(t)
	plain := httptest.NewServer(f.s.Handler())
	defer plain.Close()
	oauthH := f.s.OAuthHandler()
	tok := f.mintOperatorToken(t, "x")

	rec := escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+plain.URL+`/git/y","branch":"main"}`)
	escProblem(t, rec, http.StatusConflict, "Knowledge base already local")
	f.requireNoOrigin(t, riX)
	requireXMainLacksSecret(t, oauthH, tok)
}

// The authoritative layer: a copy of Y's knowledge base that advertises no
// tip Y holds (its main is one commit ahead of Y's, and it has no other
// branch) and makes no knomit-repo-id claim (a plain git remote). Only the
// root commit of its main names Y.
func TestOriginEscalation_OperatorRefusedAnotherReposKBByItsRoot(t *testing.T) {
	f := newEscFixture(t)
	ctx := context.Background()
	yBare := filepath.Join(f.root, "y.git")
	yURL := seedBareRemoteKBForTest(t, yBare)
	_, err := f.m.Create(ctx, repos.CreateSpec{Name: "y", Mode: "clone", Origin: &repos.OriginSpec{URL: yURL}}, nil)
	require.NoError(t, err)

	zBare := filepath.Join(f.root, "z.git")
	require.NoError(t, os.MkdirAll(zBare, 0o755))
	gitfixture.InitBare(t, zBare, "main")
	work := t.TempDir()
	runGitForTest(t, "", "clone", "-q", yBare, work)
	require.NoError(t, os.MkdirAll(filepath.Join(work, "kb"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(work, "kb", "secret.md"), []byte(originFactBody(escSecret)), 0o644))
	runGitForTest(t, work, "add", "-A")
	runGitForTest(t, work, "commit", "-q", "-m", "ahead of y")
	runGitForTest(t, work, "push", "-q", zBare, "main")

	riX := f.createX(t)
	oauthH := f.s.OAuthHandler()
	tok := f.mintOperatorToken(t, "x")
	rec := escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+zBare+`","branch":"main"}`)
	escProblem(t, rec, http.StatusConflict, "Knowledge base already local")
	f.requireNoOrigin(t, riX)
}

// --- Unrelated history ---------------------------------------------------------
//
// SABOTAGE (against the branch commit): make guardAttachIdentity return nil
// where it returns ErrOriginUnrelatedHistory -> ..._UnrelatedMainIsRefused
// goes red (200, and X's main serves the foreign secret).

func TestOriginEscalation_UnrelatedMainIsRefused(t *testing.T) {
	f := newEscFixture(t)
	riX := f.createX(t)
	publishOnMain(t, riX, "kb/own.md", "X-OWN-FACT")
	oauthH := f.s.OAuthHandler()
	tok := f.mintOperatorToken(t, "x")
	url := f.seedForeignKB(t, "other-kb.git")

	rec := escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+url+`","branch":"main"}`)
	escProblem(t, rec, http.StatusConflict, "Unrelated history")
	f.requireNoOrigin(t, riX)
	requireXMainLacksSecret(t, oauthH, tok)
	rec = escDo(oauthH, tok, http.MethodGet, "/api/v1/repos/x/branches/main/facts/kb/own.md", "")
	require.Equal(t, http.StatusOK, rec.Code, "X's main keeps its own fact: %s", rec.Body.String())

	// The probe fetched the foreign history to decide, into memory only: X's
	// store holds none of it.
	//
	// SABOTAGE (against the branch commit): make probeStorage.SetEncodedObject
	// also write each object to p.local -> red.
	tip, err := escGitOut(t, filepath.Join(f.root, "other-kb.git"), "rev-parse", "refs/heads/main")
	require.NoError(t, err, tip)
	tip = strings.TrimSpace(tip)
	require.NoError(t, riX.WithRead(func(s *store.Service) {
		require.False(t, s.HasObject(tip), "the refused attach's probe wrote the foreign tip %s into X's store", tip)
	}))
}

// The explicit replace: the operator asks for it, and the next sync adopts
// the origin's main.
func TestOriginEscalation_OperatorReplaceAdmitsUnrelatedMain(t *testing.T) {
	f := newEscFixture(t)
	riX := f.createX(t)
	publishOnMain(t, riX, "kb/own.md", "X-OWN-FACT")
	oauthH := f.s.OAuthHandler()
	tok := f.mintOperatorToken(t, "x")
	url := f.seedForeignKB(t, "other-kb.git")

	rec := escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+url+`","branch":"main","replace_main":true}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = escDo(oauthH, tok, http.MethodGet, "/api/v1/repos/x/branches/main/facts/kb/secret.md", "")
	require.Equal(t, http.StatusOK, rec.Code, "the replaced main is the origin's: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), escSecret)
}

// A new repo whose main holds no fact attaching its first origin is not a
// replace: nothing on main is lost, so no flag is needed.
//
// SABOTAGE (against the branch commit): drop the branchHoldsNoFacts exemption
// in guardAttachIdentity -> red (409 Unrelated history).
func TestOriginEscalation_FirstAttachOfAnEmptyRepoNeedsNoReplace(t *testing.T) {
	f := newEscFixture(t)
	riX := f.createX(t)
	oauthH := f.s.OAuthHandler()
	tok := f.mintOperatorToken(t, "x")
	url := f.seedForeignKB(t, "team-kb.git")

	rec := escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+url+`","branch":"main"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	o, err := f.m.Origins().Get(riX.UID())
	require.NoError(t, err)
	require.NotNil(t, o)
	rec = escDo(oauthH, tok, http.MethodGet, "/api/v1/repos/x/branches/main/facts/kb/secret.md", "")
	require.Equal(t, http.StatusOK, rec.Code, "X now follows the team's main: %s", rec.Body.String())
}

// Re-attaching a repo to a remote that shares its history (here: X's own
// main pushed to a fresh remote, then one commit ahead) passes with no flag
// even though X's main holds facts.
func TestOriginEscalation_RelatedOriginNeedsNoReplace(t *testing.T) {
	f := newEscFixture(t)
	riX := f.createX(t)
	publishOnMain(t, riX, "kb/own.md", "X-OWN-FACT")
	oauthH := f.s.OAuthHandler()
	tok := f.mintOperatorToken(t, "x")

	// X's main, published by X's own first attach to an empty remote.
	bare := filepath.Join(f.root, "x.git")
	require.NoError(t, os.MkdirAll(bare, 0o755))
	gitfixture.InitBare(t, bare, "main")
	rec := escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+bare+`","branch":"main"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, riX.OriginRoundForTest(context.Background()))
	rec = escDo(oauthH, tok, http.MethodDelete, "/api/v1/repos/x/origin", "")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	// Someone else moves the remote's main ahead (from X's agent branch).
	work := t.TempDir()
	runGitForTest(t, "", "clone", "-q", "-b", escAgent, bare, work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "kb", "later.md"), []byte(originFactBody("LATER")), 0o644))
	runGitForTest(t, work, "add", "-A")
	runGitForTest(t, work, "commit", "-q", "-m", "later")
	runGitForTest(t, work, "push", "-q", "origin", "HEAD:main")

	rec = escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+bare+`","branch":"main"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// A history that does not fit the probe budget establishes nothing about
// which knowledge base the origin holds, so the attach is refused rather
// than admitted unchecked (fail closed), replace_main included.
//
// SABOTAGE (against the branch commit): make the ErrHistoryProbeTooLarge case
// in guardAttachIdentity return nil -> red (200).
func TestOriginEscalation_OriginTooLargeToInspectIsRefused(t *testing.T) {
	f := newEscFixtureWith(t, func(c *config.Config) { c.Git.MaxProbeBytes = 64 })
	riX := f.createX(t)
	oauthH := f.s.OAuthHandler()
	tok := f.mintOperatorToken(t, "x")
	url := f.seedForeignKB(t, "big-kb.git")

	for _, body := range []string{
		`{"url":"` + url + `","branch":"main"}`,
		`{"url":"` + url + `","branch":"main","replace_main":true}`,
	} {
		rec := escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", body)
		escProblem(t, rec, http.StatusConflict, "Origin not inspected")
	}
	f.requireNoOrigin(t, riX)
}

// --- Create with an origin, and the probes -------------------------------------
//
// SABOTAGE (against the branch commit): drop the operator check in
// handleHALReposCreate's `req.Origin != nil` branch -> ..._CreateWithAnOrigin...
// red (202, and in clone mode the new repo serves the foreign secret).
// Drop .With(operator) from either probe route -> ..._ProbesNeedTheOperator red.

// A token whose audience is the whole /repos collection reads every
// REGISTERED repo, but not a knowledge base under local_origin_root that no
// repo holds: creating a repo with that origin would import it.
func TestOriginEscalation_CreateWithAnOriginNeedsTheOperator(t *testing.T) {
	f := newEscFixture(t)
	oauthH := f.s.OAuthHandler()
	tok := f.mintTokenAt(t, "/api/v1/repos", "coll-client", auth.Read, auth.Write)
	foreign := f.seedForeignKB(t, "other-kb.git")
	empty := filepath.Join(f.root, "empty.git")
	require.NoError(t, os.MkdirAll(empty, 0o755))
	gitfixture.InitBare(t, empty, "main")
	emptyBefore := escRefs(t, empty)

	for _, c := range []struct{ name, mode, url string }{
		{"z1", "clone", foreign},
		{"z2", "subscribe", foreign},
		{"z3", "initialize", empty}, // initialize pushes into the origin
	} {
		body := `{"name":"` + c.name + `","mode":"` + c.mode + `","ontology_preset":"general","origin":{"url":"` + c.url + `"}}`
		rec := escDo(oauthH, tok, http.MethodPost, "/api/v1/repos", body)
		escProblem(t, rec, http.StatusForbidden, "Permission denied")
		require.Contains(t, rec.Body.String(), "operator", c.mode)
		require.Nil(t, f.m.Get(c.name), "a refused %s create registered nothing", c.mode)
	}
	// The create is a background job, so "registered nothing" alone could be
	// a job still running: no job may have started at all.
	require.Empty(t, f.m.CreateJobs(), "a refused create must start no job")
	require.Equal(t, emptyBefore, escRefs(t, empty), "nothing was pushed into the origin")

	// A create without an origin stays a `write`.
	rec := escDo(oauthH, tok, http.MethodPost, "/api/v1/repos", `{"name":"plain","mode":"preset"}`)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	// The operator still creates with an origin.
	op := f.mintTokenAt(t, "/api/v1/repos", "coll-operator", auth.Read, auth.Write, auth.Operator)
	rec = escDo(oauthH, op, http.MethodPost, "/api/v1/repos", `{"name":"z4","mode":"clone","origin":{"url":"`+foreign+`"}}`)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
}

func TestOriginEscalation_ProbesNeedTheOperator(t *testing.T) {
	f := newEscFixture(t)
	oauthH := f.s.OAuthHandler()
	url := f.seedForeignKB(t, "other-kb.git")
	writer := f.mintTokenAt(t, "/api/v1", "api-client", auth.Read, auth.Write)
	op := f.mintTokenAt(t, "/api/v1", "api-operator", auth.Read, auth.Write, auth.Operator)
	for _, path := range []string{"/api/v1/repos:probe-origin", "/api/v1/repos:probe-initialized"} {
		body := `{"url":"` + url + `","branch":"main"}`
		rec := escDo(oauthH, writer, http.MethodPost, path, body)
		escProblem(t, rec, http.StatusForbidden, "Permission denied")
		rec = escDo(oauthH, op, http.MethodPost, path, body)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
	}
}

// --- Each cheap identity layer alone, on the attach path -------------------------
//
// The root layer runs only for a remote branch that exists, so an attach that
// names a branch the remote does not have leaves the two advertisement layers
// as the only check.
//
// SABOTAGE (against the branch commit): ignore refuseIfKnowledgeBaseIsLocal's
// verdict in guardAttachIdentity -> both tests below red (200).

// Held tip: y.git advertises Y's main and agent branch, which Y holds; a
// plain git remote makes no knomit-repo-id claim.
func TestOriginEscalation_HeldTipLayerAloneRefusesAnotherReposOrigin(t *testing.T) {
	f := newEscFixture(t)
	_, yURL, _ := f.yWithItsOwnOrigin(t)
	riX := f.createX(t)
	oauthH := f.s.OAuthHandler()
	tok := f.mintOperatorToken(t, "x")

	rec := escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+yURL+`","branch":"no-such-branch"}`)
	escProblem(t, rec, http.StatusConflict, "Knowledge base already local")
	f.requireNoOrigin(t, riX)
}

// Repo-id claim: another knomit instance serves a copy of Y whose branch tips
// Y does not hold (its main is ahead), so only the knomit-repo-id capability
// (the root of its consensus branch, Y's root) names Y.
func TestOriginEscalation_ClaimLayerAloneRefusesAnotherReposKB(t *testing.T) {
	f := newEscFixture(t)
	ctx := context.Background()
	yBare := filepath.Join(f.root, "y.git")
	yURL := seedBareRemoteKBForTest(t, yBare)
	riY, err := f.m.Create(ctx, repos.CreateSpec{Name: "y", Mode: "clone", Origin: &repos.OriginSpec{URL: yURL}}, nil)
	require.NoError(t, err)

	peer := newEscFixtureWith(t, func(c *config.Config) { c.LocalOriginRoot = f.root })
	riPeer, err := peer.m.Create(ctx, repos.CreateSpec{Name: "ycopy", Mode: "clone", Origin: &repos.OriginSpec{URL: yURL}}, nil)
	require.NoError(t, err)
	publishOnMain(t, riPeer, "kb/ahead.md", "AHEAD-OF-Y")
	peerSrv := httptest.NewServer(peer.s.Handler())
	defer peerSrv.Close()
	require.Equal(t, riY.ID(), riPeer.ID(), "precondition: the peer holds Y's knowledge base")
	var peerMain string
	require.NoError(t, riPeer.WithRead(func(s *store.Service) {
		peerMain, err = s.HeadCommit(ctx, "main")
	}))
	require.NoError(t, err)
	require.NoError(t, riY.WithRead(func(s *store.Service) {
		require.False(t, s.HasObject(peerMain), "precondition: Y does not hold the peer's main, so the tip layer is silent")
	}))

	riX := f.createX(t)
	oauthH := f.s.OAuthHandler()
	tok := f.mintOperatorToken(t, "x")
	rec := escDo(oauthH, tok, http.MethodPut, "/api/v1/repos/x/origin", `{"url":"`+peerSrv.URL+`/git/ycopy","branch":"no-such-branch"}`)
	escProblem(t, rec, http.StatusConflict, "Knowledge base already local")
	require.Contains(t, rec.Body.String(), `\"y\"`)
	f.requireNoOrigin(t, riX)
}
