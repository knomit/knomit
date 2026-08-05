package repos

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/stretchr/testify/require"

	"knomit/internal/config"
	"knomit/internal/store"
)

// newCredentialManager builds a started Manager whose registry HAS a Crypt.
//
// The key file is not decoration: SetOriginCredential refuses to store a token
// without one (never plaintext), so a test that omitted it would fail on that
// refusal rather than on the behaviour under test, and would keep failing after
// the behaviour was correct.
//
// Extra mutators run AFTER the LocalOriginRoot/KeyPath defaults above, so a
// caller can layer additional Deps (e.g. a server-wide Cfg.Remote fallback)
// without having to reconstruct the key-file setup itself.
func newCredentialManager(t *testing.T, home, originRoot string, mutators ...func(*Deps)) *Manager {
	t.Helper()
	keyPath := filepath.Join(home, "id_ed25519")
	require.NoError(t, os.WriteFile(keyPath, []byte("fake-key-material"), 0o600))
	base := []func(*Deps){func(d *Deps) {
		d.Cfg.LocalOriginRoot = originRoot
		d.KeyPath = keyPath
	}}
	m := newTestManager(t, home, append(base, mutators...)...)
	require.NoError(t, m.Start())
	return m
}

func TestSetOriginWritesControlDBBeforeStore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	m := newCredentialManager(t, home, root)
	mustCreateRepo(t, m, "work")

	require.NoError(t, m.SetOrigin(ctx, "work",
		OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "s3cret"}, 300, 300))

	// control.db holds the credential...
	method, token, err := m.RepoRegistry().OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "token", method)
	require.Equal(t, "s3cret", token)

	// ...and the store holds the wiring but NOT the secret.
	svc := testService(t, m.Get("work"))
	sm, sTok, err := svc.Remote().LegacyAuth("origin")
	require.NoError(t, err)
	require.Equal(t, "", sm, "the store must not keep the auth method")
	require.Equal(t, "", sTok, "the store must not keep the token")

	rm, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.Equal(t, url, rm.URL)

	// The registry row carries the origin too — a repo whose database is lost
	// has to be re-clonable from control.db alone.
	rec, found, err := m.RepoRegistry().ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, url, rec.OriginURL)
	require.Equal(t, "main", rec.OriginBranch)
}

func TestClearOriginForgetsTheCredential(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	m := newCredentialManager(t, home, root)
	mustCreateRepo(t, m, "work")
	require.NoError(t, m.SetOrigin(ctx, "work",
		OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "s3cret"}, 300, 300))

	require.NoError(t, m.ClearOrigin(ctx, "work"))

	method, token, err := m.RepoRegistry().OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "", method)
	require.Equal(t, "", token)

	rec, found, err := m.RepoRegistry().ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, found, "clearing an origin must not unregister the repo")
	require.Equal(t, "", rec.OriginURL)

	// The store is unwired too, so nothing keeps syncing to the origin the
	// user just disconnected.
	svc := testService(t, m.Get("work"))
	rm, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.Nil(t, rm, "the store must not keep a remote control.db no longer names")
}

// TestSetOriginKeepsCredentialWhenStoreWriteFails is the ORDERING test, and it
// is the only evidence that the rule is enforced rather than merely asserted in
// a comment. Every other test here pins the end state of a run where both steps
// succeeded, which the reverse order would satisfy just as well.
//
// It forces step 2 to fail — the instance has no store handle, so Acquire
// returns ErrStoreUnavailable — and then asserts the credential and the origin
// row are in control.db anyway. That is precisely the state a crash between the
// two steps leaves behind, and the state the next boot repairs by
// materializing. Written store-first instead, this run would record nothing
// recoverable: the origin would exist nowhere but in the caller's arguments.
func TestSetOriginKeepsCredentialWhenStoreWriteFails(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	m := newCredentialManager(t, home, "")

	// A registered repo whose store cannot be acquired. NewTestInstanceWithDeps
	// with no Svc leaves the handle nil, which is exactly what Acquire reports
	// as ErrStoreUnavailable — no real store to tear down, and deterministic.
	m.Set("work", NewTestInstanceWithDeps(TestInstanceConfig{
		Name: "work", AgentBranch: "machine/test",
	}))

	const url = "https://example.invalid/acme/kb.git"
	err := m.SetOrigin(ctx, "work",
		OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "s3cret"}, 300, 300)
	require.Error(t, err, "a failed store write must be reported, not swallowed")
	require.ErrorIs(t, err, ErrStoreUnavailable)

	// ...and control.db kept BOTH halves regardless.
	method, token, cerr := m.RepoRegistry().OriginCredential("work")
	require.NoError(t, cerr)
	require.Equal(t, "token", method, "the credential must survive a failed store write")
	require.Equal(t, "s3cret", token, "the credential must survive a failed store write")

	rec, found, rerr := m.RepoRegistry().ActiveRecord("work")
	require.NoError(t, rerr)
	require.True(t, found)
	require.Equal(t, url, rec.OriginURL, "the origin must survive a failed store write")
	require.Equal(t, "main", rec.OriginBranch)
}

// TestClearOriginForgetsCredentialEvenWhenStoreUnwireFails is the mirror of
// TestSetOriginKeepsCredentialWhenStoreWriteFails, and it exists for the mirror
// reason. ClearOrigin's doc comment claims that clearing control.db first means
// a crash leaves only stale wiring the next boot removes — whereas clearing the
// store first would leave control.db still naming the origin, and the next boot
// would dutifully re-materialize the origin the user just disconnected. That is
// the origin-resurrection failure, and until this test it was asserted by the
// comment alone: reordering the two steps passed the whole suite.
//
// Same lever as the SetOrigin case — an instance with no store handle, so step
// 2 fails at Acquire — and the assertion is that control.db has ALREADY
// forgotten both halves by then.
//
// The registry is seeded directly rather than through SetOrigin, so a
// regression in SetOrigin cannot make this test vacuous.
func TestClearOriginForgetsCredentialEvenWhenStoreUnwireFails(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	m := newCredentialManager(t, home, "")

	const url = "https://example.invalid/acme/kb.git"
	reg := m.RepoRegistry()
	require.NoError(t, reg.Upsert(RepoRecord{
		Name: "work", State: RepoActive, CreatedAt: time.Now().UTC(),
		OriginURL: url, OriginBranch: "main",
	}))
	require.NoError(t, reg.SetOriginCredential("work", "token", "s3cret"))

	m.Set("work", NewTestInstanceWithDeps(TestInstanceConfig{
		Name: "work", AgentBranch: "machine/test",
	}))

	err := m.ClearOrigin(ctx, "work")
	require.Error(t, err, "a failed store unwire must be reported, not swallowed")
	require.ErrorIs(t, err, ErrStoreUnavailable)

	// ...and control.db had already forgotten the origin before it tried.
	method, token, cerr := reg.OriginCredential("work")
	require.NoError(t, cerr)
	require.Equal(t, "", method, "a revoked credential must not outlive the disconnect")
	require.Equal(t, "", token, "a revoked credential must not outlive the disconnect")

	rec, found, rerr := reg.ActiveRecord("work")
	require.NoError(t, rerr)
	require.True(t, found, "clearing an origin must not unregister the repo")
	require.Equal(t, "", rec.OriginURL,
		"control.db still naming the origin is how a disconnected origin resurrects itself at the next boot")
	require.Equal(t, "", rec.OriginBranch)
}

// TestCreateCloneRecordsTheCredentialInControlDB covers the one origin write
// that cannot go through SetOrigin: initClone runs before the RepoInstance
// exists. Create's registry write-through is what carries the credential
// instead, and the store must come out of the clone with empty auth columns.
func TestCreateCloneRecordsTheCredentialInControlDB(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	m := newCredentialManager(t, home, root)

	_, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "clone",
		Origin: &OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "s3cret"},
	}, nil)
	require.NoError(t, err)

	method, token, err := m.RepoRegistry().OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "token", method)
	require.Equal(t, "s3cret", token)

	svc := testService(t, m.Get("work"))
	sm, sTok, err := svc.Remote().LegacyAuth("origin")
	require.NoError(t, err)
	require.Equal(t, "", sm, "the clone must not leave the auth method in the store")
	require.Equal(t, "", sTok, "the clone must not leave the token in the store")
}

// TestStartMaterializesControlDBIntoAnUnwiredStore pins the repair that the
// ordering rule promises: control.db knows the origin, the store does not
// (a crashed SetOrigin), and the next boot must push it down rather than
// erase control.db to match the store.
func TestStartMaterializesControlDBIntoAnUnwiredStore(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	keyPath := filepath.Join(home, "id_ed25519")
	require.NoError(t, os.WriteFile(keyPath, []byte("fake-key-material"), 0o600))
	deps := func(d *Deps) { d.Cfg.LocalOriginRoot = root; d.KeyPath = keyPath }

	first := newTestManager(t, home, deps)
	require.NoError(t, first.Start())
	mustCreateRepo(t, first, "work")
	// Record the origin in control.db ONLY — leave the store unwired, which is
	// exactly what a crash between SetOrigin's two steps leaves behind.
	reg := first.RepoRegistry()
	rec, found, err := reg.ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, found)
	rec.OriginURL, rec.OriginBranch = url, "main"
	require.NoError(t, reg.Upsert(rec))
	require.NoError(t, reg.SetOriginCredential("work", "token", "s3cret"))
	require.NoError(t, first.Close())

	second := newTestManager(t, home, deps)
	require.NoError(t, second.Start())

	// control.db must still know the origin...
	rec2, found, err := second.RepoRegistry().ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, url, rec2.OriginURL, "boot must not erase control.db to match an unwired store")
	require.Equal(t, "main", rec2.OriginBranch)

	// ...and the store must now be wired to it.
	svc := testService(t, second.Get("work"))
	rm, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, rm, "boot must materialize control.db's origin into the store")
	require.Equal(t, url, rm.URL, "boot must materialize control.db's origin into the store")

	// The store must still hold no credential.
	sm, sTok, err := svc.Remote().LegacyAuth("origin")
	require.NoError(t, err)
	require.Equal(t, "", sm)
	require.Equal(t, "", sTok)
}

// TestStartMaterializeKeepsTheConfiguredSyncCadence pins that a materialize-down
// repairs the URL and branch WITHOUT resetting the intervals.
//
// SetRemote takes interval and push_interval positionally, so the reconcile has
// to pass something; passing the 300/300 default unconditionally would silently
// throw away a cadence the user chose, on every boot where control.db and the
// store disagree about anything at all. The repair is of the wiring, not of the
// schedule, so the store's own values are carried over.
func TestStartMaterializeKeepsTheConfiguredSyncCadence(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	deps := func(d *Deps) { d.Cfg.LocalOriginRoot = root }

	first := newTestManager(t, home, deps)
	require.NoError(t, first.Start())
	mustCreateRepo(t, first, "work")
	// The store is wired with a non-default cadence and the WRONG branch, so the
	// next boot must materialize; control.db carries the branch to repair to.
	svc := testService(t, first.Get("work"))
	require.NoError(t, svc.Remote().SetRemote(
		"origin", url, "develop", "machine/test", 900, 1800, "", ""))
	reg := first.RepoRegistry()
	rec, found, err := reg.ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, found)
	rec.OriginURL, rec.OriginBranch = url, "main"
	require.NoError(t, reg.Upsert(rec))
	require.NoError(t, first.Close())

	second := newTestManager(t, home, deps)
	require.NoError(t, second.Start())

	svc2 := testService(t, second.Get("work"))
	rm, err := svc2.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, rm)
	require.Equal(t, "main", rm.Branch, "precondition: the boot must actually have materialized")
	require.Equal(t, 900, rm.Interval, "a materialize must not reset the configured sync interval")
	require.Equal(t, 1800, rm.PushInterval, "a materialize must not reset the configured push interval")
}

// TestStartAdoptsStoreOriginWhenControlDBHasNone protects legacy and adopted
// rows. adoptFromFilesystem writes rows with an EMPTY OriginURL because it
// never opens the stores, so a boot that unwired those repos would strip a
// working origin off every pre-registry install.
func TestStartAdoptsStoreOriginWhenControlDBHasNone(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	deps := func(d *Deps) { d.Cfg.LocalOriginRoot = root }

	first := newTestManager(t, home, deps)
	require.NoError(t, first.Start())
	mustCreateRepo(t, first, "legacy")
	// Store has an origin; control.db does not — the adopted-row shape.
	svc := testService(t, first.Get("legacy"))
	require.NoError(t, svc.Remote().SetRemote(
		"origin", url, "main", "machine/test", 300, 300, "", ""))
	reg := first.RepoRegistry()
	rec, _, err := reg.ActiveRecord("legacy")
	require.NoError(t, err)
	rec.OriginURL, rec.OriginBranch = "", ""
	require.NoError(t, reg.Upsert(rec))
	require.NoError(t, first.Close())

	second := newTestManager(t, home, deps)
	require.NoError(t, second.Start())

	rec2, found, err := second.RepoRegistry().ActiveRecord("legacy")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, url, rec2.OriginURL,
		"a blank control.db row must ADOPT the store's origin, never unwire it")

	svc2 := testService(t, second.Get("legacy"))
	rm, err := svc2.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, rm, "the working origin must survive the boot")
	require.Equal(t, url, rm.URL, "the working origin must survive the boot")
}

// TestStartNeverPinsAnUndetectedBranch runs TWO boots, because one is not
// enough to catch the failure it guards.
//
// store.SetRemote hardcodes "main" for an empty branch — it never asks the
// remote — so a materialize-down of a row with no recorded branch used to leave
// "main" in the store, and the NEXT boot, seeing the URLs finally agree, would
// adopt that substitution into control.db as though detectUpstream had found it.
// A single-pass test cannot see this: the laundering happens one boot later.
//
// The damage is not cosmetic. rebuildFromOrigin passes rec.OriginBranch into
// OriginSpec.Branch, and initClone calls detectUpstream ONLY when that branch is
// empty. So once control.db holds "main", every rebuild clones with Branch:
// "main" explicitly and bypasses the detection that would have found "master" —
// the wrong pin permanently disables its own correction. A blank branch, by
// contrast, re-detects forever.
func TestStartNeverPinsAnUndetectedBranch(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	newURL := seedBareRemote(t, filepath.Join(root, "new.git"))
	oldURL := seedBareRemote(t, filepath.Join(root, "old.git"))
	deps := func(d *Deps) { d.Cfg.LocalOriginRoot = root }

	first := newTestManager(t, home, deps)
	require.NoError(t, first.Start())
	mustCreateRepo(t, first, "work")
	// The store is still wired to the OLD origin, on a branch of its own...
	svc := testService(t, first.Get("work"))
	require.NoError(t, svc.Remote().SetRemote(
		"origin", oldURL, "develop", "machine/test", 300, 300, "", ""))
	// ...while control.db carries a re-point that crashed before the store
	// write, with no branch because the caller let the remote decide.
	reg := first.RepoRegistry()
	rec, found, err := reg.ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, found)
	rec.OriginURL, rec.OriginBranch = newURL, ""
	require.NoError(t, reg.Upsert(rec))
	require.NoError(t, first.Close())

	// Boot 1: the URLs disagree, so this is the pass that materializes.
	second := newTestManager(t, home, deps)
	require.NoError(t, second.Start())

	svc2 := testService(t, second.Get("work"))
	rm, err := svc2.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, rm, "precondition: the boot must have materialized the new origin")
	require.Equal(t, newURL, rm.URL, "precondition: the boot must have materialized the new origin")

	rec2, found, err := second.RepoRegistry().ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "", rec2.OriginBranch, "the materializing pass must not record a branch")
	require.NoError(t, second.Close())

	// Boot 2: the URLs now agree, and this is where the laundering used to
	// happen — the store's branch is whatever boot 1 left there.
	third := newTestManager(t, home, deps)
	require.NoError(t, third.Start())

	rec3, found, err := third.RepoRegistry().ActiveRecord("work")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, newURL, rec3.OriginURL)
	require.Equal(t, "", rec3.OriginBranch,
		"a branch nobody detected must never be pinned in control.db: blank re-detects on every "+
			"rebuild, whereas a pin bypasses detectUpstream forever")
}

// TestCreateRecordsTheDetectedUpstreamBranch pins the flow that makes a
// boot-time branch rule unnecessary in the first place.
//
// The branch control.db is entitled to hold is the one detectUpstream actually
// RESOLVED, and Create records it: initClone resolves the remote's real default
// for a spec that names no branch, and Create's write-through copies it. Boot
// therefore never has to learn a branch from the store — which is exactly why
// reconcileOrigin does not try, since a branch read back out of a store cannot
// be told apart from SetRemote's hardcoded substitution.
func TestCreateRecordsTheDetectedUpstreamBranch(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	deps := func(d *Deps) { d.Cfg.LocalOriginRoot = root }

	m := newTestManager(t, home, deps)
	require.NoError(t, m.Start())
	_, err := m.Create(context.Background(), CreateSpec{
		// No branch: the remote decides, and control.db must end up holding
		// what it decided.
		Name: "cloned", Mode: "clone", Origin: &OriginSpec{URL: url},
	}, nil)
	require.NoError(t, err)

	rec, found, err := m.RepoRegistry().ActiveRecord("cloned")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "main", rec.OriginBranch,
		"control.db learns the branch at create time, from the clone that actually resolved it")
}

// TestStartLeavesABlankBranchBlank is the inverse of what an earlier draft of
// this reconcile asserted, and the inversion is deliberate.
//
// That draft filled a blank branch from the store at boot, on the theory that it
// was recovering detectUpstream's discovered value. It cannot know that: the
// branch in the store is equally likely to be the "main" that SetRemote
// substitutes for an empty one, and pinning it makes every future rebuild clone
// with an explicit branch, which is precisely the input that makes initClone
// SKIP detectUpstream. Blank is the self-correcting state, so boot leaves it
// alone; TestCreateRecordsTheDetectedUpstreamBranch covers where the real value
// comes from.
func TestStartLeavesABlankBranchBlank(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	deps := func(d *Deps) { d.Cfg.LocalOriginRoot = root }

	first := newTestManager(t, home, deps)
	require.NoError(t, first.Start())
	_, err := first.Create(context.Background(), CreateSpec{
		Name: "cloned", Mode: "clone", Origin: &OriginSpec{URL: url},
	}, nil)
	require.NoError(t, err)
	reg := first.RepoRegistry()
	rec, _, err := reg.ActiveRecord("cloned")
	require.NoError(t, err)
	rec.OriginBranch = "" // blank it, keeping the URL
	require.NoError(t, reg.Upsert(rec))
	require.NoError(t, first.Close())

	second := newTestManager(t, home, deps)
	require.NoError(t, second.Start())
	rec2, _, err := second.RepoRegistry().ActiveRecord("cloned")
	require.NoError(t, err)
	require.Equal(t, "", rec2.OriginBranch,
		"boot must not pin a branch it cannot tell apart from SetRemote's substitution")

	// ...and the store is left alone too: the URLs agree and control.db names no
	// branch, so there is nothing this boot could repair.
	svc := testService(t, second.Get("cloned"))
	rm, err := svc.Remote().GetRemote("origin")
	require.NoError(t, err)
	require.NotNil(t, rm)
	require.Equal(t, "main", rm.Branch, "the store keeps the branch the clone resolved")
}

func TestOriginAuthReadsFromControlDB(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	// newCredentialManager (not newTestManager): SetOrigin routes the token
	// through reg.SetOriginCredential, which refuses to persist anything
	// without a Crypt, so the registry needs an agent key wired in.
	m := newCredentialManager(t, home, root)
	mustCreateRepo(t, m, "work")
	require.NoError(t, m.SetOrigin(ctx, "work",
		OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "s3cret"}, 300, 300))

	cfg, err := m.OriginAuth("work")
	require.NoError(t, err)
	require.Equal(t, "token", cfg.AuthMethod)
	require.Equal(t, "s3cret", cfg.Token)
}

func TestOriginAuthIsEmptyForRepoWithoutCredential(t *testing.T) {
	home := t.TempDir()
	m := newTestManager(t, home)
	require.NoError(t, m.Start())
	mustCreateRepo(t, m, "work")

	cfg, err := m.OriginAuth("work")
	require.NoError(t, err)
	require.Equal(t, "", cfg.AuthMethod)
	require.Equal(t, "", cfg.Token)
}

// TestOriginAuthSplitsBasicOnFirstColonOnly pins that Manager.OriginAuth
// splits a "basic" credential's "user:password" token the SAME way the
// deleted remoteAuthFromRecord did: on the FIRST colon (strings.Cut), not a
// naive single split, so a password that itself contains a colon survives
// intact instead of being truncated at the wrong point.
func TestOriginAuthSplitsBasicOnFirstColonOnly(t *testing.T) {
	home := t.TempDir()
	m := newCredentialManager(t, home, "")
	mustCreateRepo(t, m, "work")
	require.NoError(t, m.RepoRegistry().SetOriginCredential("work", "basic", "alice:p:a:ss"))

	cfg, err := m.OriginAuth("work")
	require.NoError(t, err)
	require.Equal(t, "basic", cfg.AuthMethod)
	require.Equal(t, "alice", cfg.User)
	require.Equal(t, "p:a:ss", cfg.Password,
		"a password containing ':' must survive whole; splitting on every colon would truncate it")
}

// TestOriginAuthBasicSplitUsesMergedMethod is the regression test for the
// Minor the reviewer flagged: OriginAuth used to decide the "basic" split on
// the RAW method OriginCredential returned, before it was merged onto the
// fallback config. A repo can hold a stored TOKEN with no auth_method of its
// own (method == "") and rely on a server-wide "basic" from cfg.Remote — that
// case must still split "user:password", not fall through to dumping the
// whole string into cfg.Token because the raw (pre-merge) method was empty.
func TestOriginAuthBasicSplitUsesMergedMethod(t *testing.T) {
	home := t.TempDir()
	m := newCredentialManager(t, home, "", func(d *Deps) {
		d.Cfg.Remote = config.RemoteAuthConfig{AuthMethod: "basic"}
	})
	mustCreateRepo(t, m, "work")
	// Method left empty on purpose: only the token is stored, so the merge
	// must fall back to the server-wide "basic" to decide how to split it.
	require.NoError(t, m.RepoRegistry().SetOriginCredential("work", "", "bob:s3:cr:et"))

	cfg, err := m.OriginAuth("work")
	require.NoError(t, err)
	require.Equal(t, "basic", cfg.AuthMethod, "the server-wide fallback method must carry through")
	require.Equal(t, "bob", cfg.User)
	require.Equal(t, "s3:cr:et", cfg.Password)
	require.Equal(t, "", cfg.Token, "a basic credential must never land in cfg.Token")
}

// TestActivateSyncPropagatesAnUnresolvableCredential is the regression test
// for the Critical the reviewer found: builder.go used to call authResolve()
// EAGERLY, log its error, and substitute the credential-less cfg.Remote
// fallback BEFORE ever reaching makeRemoteAuthFn — so a repo whose stored
// credential could no longer be decrypted (agent key rotated or corrupted)
// synced ANONYMOUSLY against a remote that happened to permit it, silently
// and forever, with no RecordSyncError to surface it for escalation.
//
// This reproduces exactly that scenario: a credential is stored normally,
// then the registry's Crypt is swapped for one derived from DIFFERENT key
// material (simulating an agent-key rotation), so the stored ciphertext can
// no longer be decrypted. Task 7's migration gate does not catch this case —
// it keys on the STORE's own auth_token column, which is empty by design
// after migration — so this failure mode is only visible through the sync
// path itself, which is what this test exercises via ActivateSync (the one
// call site with a synchronous, directly-observable return value).
func TestActivateSyncPropagatesAnUnresolvableCredential(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	m := newCredentialManager(t, home, root)
	mustCreateRepo(t, m, "work")
	require.NoError(t, m.SetOrigin(ctx, "work",
		OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "s3cret"}, 300, 300))

	rotated, err := store.NewCrypt([]byte("a-different-agent-key-entirely"))
	require.NoError(t, err)
	m.RepoRegistry().SetCrypt(rotated)

	ri := m.Get("work")
	require.NotNil(t, ri)

	activateErr := ri.ActivateSync(url)
	require.Error(t, activateErr,
		"an unresolvable credential must fail ActivateSync visibly, not sync anonymously")
	require.Contains(t, activateErr.Error(), "auth resolution failed")

	svc := testService(t, ri)
	rm, rerr := svc.Remote().GetRemote("origin")
	require.NoError(t, rerr)
	require.NotNil(t, rm.LastStatus)
	require.Equal(t, "error", *rm.LastStatus,
		"the resolution failure must be recorded on the remote so the reconcile loop counts it toward escalation")
}

// TestReconcileAuthFnPicksUpRefreshedToken is the regression test for the
// whole resolver-function deviation this task made from the brief's literal
// "thread an already-resolved value" instruction: a SINGLE authFn, built
// once by makeRemoteAuthFn, must present the CURRENT control.db credential on
// every call, not the one that existed when it was built. Without this the
// "a token just refreshed via PUT /origin is honoured immediately" comment in
// builder.go would be provably false, and a future refactor that hoisted the
// resolve() call back out of makeRemoteAuthFn would regress silently — no
// other test asserts on the actual credential VALUE an authFn hands to git,
// only on what ends up persisted in control.db.
func TestReconcileAuthFnPicksUpRefreshedToken(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	home := t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	m := newCredentialManager(t, home, root)
	mustCreateRepo(t, m, "work")
	require.NoError(t, m.SetOrigin(ctx, "work",
		OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "first-token"}, 300, 300))

	resolve := func() (config.RemoteAuthConfig, error) { return m.OriginAuth("work") }
	authFn := makeRemoteAuthFn(resolve, "")
	remote := &store.Remote{URL: url}

	auth1, err := authFn(remote)
	require.NoError(t, err)
	ba1, ok := auth1.(*githttp.BasicAuth)
	require.True(t, ok, "token auth must resolve to BasicAuth")
	require.Equal(t, "first-token", ba1.Password)

	require.NoError(t, m.SetOrigin(ctx, "work",
		OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "second-token"}, 300, 300))

	auth2, err := authFn(remote)
	require.NoError(t, err)
	ba2, ok := auth2.(*githttp.BasicAuth)
	require.True(t, ok)
	require.Equal(t, "second-token", ba2.Password,
		"the SAME authFn must present the refreshed token on its next call, not the value captured when it was built")
}

// rawStoredCredential reads a repo's auth_token column WITHOUT decrypting it,
// so a test can compare stored ciphertext across boots that cannot read it.
func rawStoredCredential(t *testing.T, m *Manager, name string) string {
	t.Helper()
	var raw string
	require.NoError(t, m.RepoRegistry().db.QueryRow(
		`SELECT auth_token FROM repos WHERE name=? AND archive_id=''`, name).Scan(&raw))
	return raw
}

// TestRebuildFromOriginKeepsTheStoredCredential covers the HAPPY rebuild path,
// where rebuildSpec reads the credential successfully and hands it back to
// Create: the write-through then rewrites the same value, and the credential
// must still be intact and readable afterwards.
//
// It does NOT cover Create's empty-credential guard. It cannot: the spec it
// exercises carries a credential, so the guard is not even consulted. The
// read-FAILED path is what needs the guard, and
// TestRebuildKeepsTheStoredCredentialWhenItCannotBeRead covers that.
func TestRebuildFromOriginKeepsTheStoredCredential(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))

	m := newCredentialManager(t, home, root)
	_, err := m.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "clone",
		Origin: &OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "s3cret"},
	}, nil)
	require.NoError(t, err)
	require.NoError(t, m.Close())

	// Lose the repo's database — control.db is now the only record of it.
	require.NoError(t, os.Remove(filepath.Join(home, "repos", "work.db")))

	m2 := newCredentialManager(t, home, root)
	require.NotNil(t, m2.Get("work"), "the repo should have been rebuilt from its recorded origin")

	_, token, err := m2.RepoRegistry().OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "s3cret", token, "a rebuild that brings no credential must not erase the stored one")
}

// TestRebuildKeepsTheStoredCredentialWhenItCannotBeRead is the regression for
// Create's empty-credential guard, and the ONLY test that exercises it.
//
// rebuildSpec supplies the credential on the happy path, so the guard's live
// purpose is the read-FAILED branch: when OriginCredential cannot be read,
// rebuildSpec logs it and deliberately attempts an unauthenticated clone with a
// BLANK credential in the spec — against a row that still holds the credential
// the rebuild is being attempted with. Writing that blank through would destroy
// the only surviving copy at the exact moment of recovery, and the loss is
// permanent: nothing else on the machine has it.
//
// The failure is arranged by rotating the agent key, which is the realistic
// trigger (a re-provisioned host, a restored volume with a fresh key). The
// ciphertext is then undecryptable, so the credential is unreadable but NOT
// gone — which is precisely why erasing it would be the wrong response, and why
// this test asserts it comes back once the right key does.
func TestRebuildKeepsTheStoredCredentialWhenItCannotBeRead(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	url := seedBareRemote(t, filepath.Join(root, "remote.git"))
	keyPath := filepath.Join(home, "id_ed25519")

	first := newCredentialManager(t, home, root)
	_, err := first.Create(context.Background(), CreateSpec{
		Name: "work", Mode: "clone",
		Origin: &OriginSpec{URL: url, Branch: "main", AuthMethod: "token", AuthToken: "s3cret"},
	}, nil)
	require.NoError(t, err)
	stored := rawStoredCredential(t, first, "work")
	require.NotEmpty(t, stored, "setup: the credential must be in control.db to begin with")
	require.NoError(t, first.Close())

	// Lose the database, and rotate the agent key so the stored credential can no
	// longer be DECRYPTED. rebuildSpec's OriginCredential read now fails, and it
	// hands Create a spec with a blank credential.
	dbPath := filepath.Join(home, "repos", "work.db")
	require.NoError(t, os.Remove(dbPath))
	os.Remove(dbPath + "-wal")
	os.Remove(dbPath + "-shm")
	require.NoError(t, os.WriteFile(keyPath, []byte("rotated-key-material"), 0o600))

	second := newKeyedManager(t, home, root)
	require.NoError(t, second.Start())
	require.NotNil(t, second.Get("work"),
		"the rebuild must still be attempted: an unreadable credential is no reason to "+
			"abandon a repo whose origin may not need one")

	// The stored ciphertext must be untouched, byte for byte. Comparing raw bytes
	// is the point — this boot cannot decrypt it, so "still readable" is not a
	// question it could answer.
	require.Equal(t, stored, rawStoredCredential(t, second, "work"),
		"a rebuild whose credential could not be READ must leave the stored one alone; "+
			"writing the blank spec through erases the only surviving copy at the moment of recovery")
	require.NoError(t, second.Close())

	// And it must still be the real credential, not merely non-empty: restore the
	// original key and read it back through the ordinary path.
	require.NoError(t, os.WriteFile(keyPath, []byte("fake-key-material"), 0o600))
	third := newKeyedManager(t, home, root)
	require.NoError(t, third.Start())
	method, token, err := third.RepoRegistry().OriginCredential("work")
	require.NoError(t, err)
	require.Equal(t, "token", method)
	require.Equal(t, "s3cret", token,
		"with the right key back, the credential the rebuild could not read must still be there")
}
