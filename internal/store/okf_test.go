package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"knomit/internal/okf"
)

// okfTestBranchTip returns the branch-tip commit hash for branch, i.e. the
// source SHA the OKF exporter reads a snapshot at.
func okfTestBranchTip(t *testing.T, svc *Service, branch string) plumbing.Hash {
	t.Helper()
	ref, err := svc.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	require.NoError(t, err)
	return ref.Hash()
}

// okfTestService builds a store with two facts written on "main" and returns
// the service and the branch-tip source SHA.
func okfTestService(t *testing.T) (*Service, plumbing.Hash) {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, "main", "kb/decisions/okf/scope/d9d6557d.md", testFactBody("Scope", 0.9, nil), "seed scope", "learn")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "main", "kb/invariants/okf/refs/3209d651.md", testFactBody("Refs", 0.8, nil), "seed refs", "learn")
	require.NoError(t, err)

	return svc, okfTestBranchTip(t, svc, "main")
}

func TestOKFReadFacts_EnumeratesTreeAtCommit(t *testing.T) {
	svc, sha := okfTestService(t)
	ctx := context.Background()

	facts, err := svc.okfReadFacts(ctx, sha)
	require.NoError(t, err)
	require.Len(t, facts, 2, "want the two kb/ facts written on main")

	for _, f := range facts {
		require.False(t, f.Timestamp.IsZero(), "fact %s has zero authoring timestamp", f.Fact.Path())
	}
}

// TestOKFReadFacts_ReadsTreeNotIndex pins determinism: enumeration is a pure
// function of the source SHA. A snapshot taken at an earlier commit must not
// include a fact added in a later commit.
func TestOKFReadFacts_ReadsTreeNotIndex(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	r1, err := svc.Facts().WriteFact(ctx, "main", "kb/decisions/okf/scope/aaaaaaaa.md", testFactBody("Scope", 0.9, nil), "seed scope", "learn")
	require.NoError(t, err)
	early := plumbing.NewHash(r1.CommitHash)

	_, err = svc.Facts().WriteFact(ctx, "main", "kb/decisions/okf/later/bbbbbbbb.md", testFactBody("Later", 0.9, nil), "seed later", "learn")
	require.NoError(t, err)

	facts, err := svc.okfReadFacts(ctx, early)
	require.NoError(t, err)
	require.Len(t, facts, 1, "snapshot at the earlier commit sees only the first fact")
	require.Equal(t, "kb/decisions/okf/scope/aaaaaaaa.md", facts[0].Fact.Path())
}

// TestOKFHistory_ClassifiesCreationAndUpdate checks the log walk labels the
// first touch of a path Creation and a later touch Update, and that the
// authoring-time map records the earliest commit time for each path.
func TestOKFHistory_ClassifiesCreationAndUpdate(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	const path = "kb/decisions/okf/scope/d9d6557d.md"
	_, err = svc.Facts().WriteFact(ctx, "main", path, testFactBody("Scope", 0.5, nil), "create", "learn")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "main", path, testFactBody("Scope", 0.9, nil), "revise", "learn")
	require.NoError(t, err)

	sha := okfTestBranchTip(t, svc, "main")
	events, authored, err := svc.okfHistory(ctx, sha)
	require.NoError(t, err)

	var creations, updates int
	for _, e := range events {
		if e.Path != path {
			continue
		}
		switch e.Kind {
		case "Creation":
			creations++
		case "Update":
			updates++
		}
	}
	require.Equal(t, 1, creations, "exactly one Creation for the path")
	require.GreaterOrEqual(t, updates, 1, "at least one Update for the path")

	ts, ok := authored[path]
	require.True(t, ok, "authoring map has the path")
	require.False(t, ts.IsZero(), "authoring time is non-zero")
}

// ensureOKFTestService builds a store with a single fact on "main".
func ensureOKFTestService(t *testing.T) *Service {
	t.Helper()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, "main", "kb/decisions/okf/scope/d9d6557d.md", testFactBody("Scope", 0.9, nil), "seed scope", "learn")
	require.NoError(t, err)
	return svc
}

func TestEnsureOKF_GeneratesAndCaches(t *testing.T) {
	svc := ensureOKFTestService(t)
	ctx := context.Background()

	h1, err := svc.EnsureOKF(ctx, "main")
	require.NoError(t, err)
	require.False(t, h1.IsZero(), "expected a non-zero okf commit")

	// The ref exists and points at h1.
	ref, err := svc.rh.gits.Reference(plumbing.NewBranchReferenceName("okf/main"))
	require.NoError(t, err)
	require.Equal(t, h1, ref.Hash(), "okf/main ref must point at the generated commit")

	// Second call with no source change is a marker hit: identical hash.
	h2, err := svc.EnsureOKF(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, h1, h2, "cache miss: second call should return the cached hash")
}

// TestEnsureOKF_TreeRoundTrips writes the OKF commit, reads it back through
// go-git's own tree-walking path, and asserts every bundle file is present
// with byte-identical content. This is the guard against a tree entry
// ordering mismatch (go-git's reader relies on git-canonical order).
func TestEnsureOKF_TreeRoundTrips(t *testing.T) {
	svc := ensureOKFTestService(t)
	ctx := context.Background()

	h, err := svc.EnsureOKF(ctx, "main")
	require.NoError(t, err)

	// Independently regenerate the expected bundle to know what files to expect.
	sourceSHA := okfTestBranchTip(t, svc, "main")
	facts, err := svc.okfReadFacts(ctx, sourceSHA)
	require.NoError(t, err)
	logEntries, _, err := svc.okfHistory(ctx, sourceSHA)
	require.NoError(t, err)
	rootSHA, err := svc.RootCommit(ctx, "main")
	require.NoError(t, err)
	repoID := rootSHA
	if len(repoID) > 12 {
		repoID = repoID[:12]
	}
	bundle, _ := okf.Build(okf.RepoIdentity{ID: repoID}, facts, logEntries)
	require.NotEmpty(t, bundle.Files, "bundle should contain files")

	// Read the OKF commit's tree back through go-git and collect every file.
	commit, err := object.GetCommit(svc.rh.gits, h)
	require.NoError(t, err)
	tree, err := commit.Tree()
	require.NoError(t, err)
	got := map[string][]byte{}
	require.NoError(t, tree.Files().ForEach(func(f *object.File) error {
		c, err := f.Contents()
		if err != nil {
			return err
		}
		got[f.Name] = []byte(c)
		return nil
	}))

	for _, want := range bundle.Files {
		content, ok := got[want.Path]
		require.Truef(t, ok, "bundle file %q missing from written tree (ordering issue?)", want.Path)
		require.Equalf(t, want.Content, content, "content mismatch for %q", want.Path)
	}

	// The written tree must contain exactly the bundle's files: no extras
	// left over from a stale entry or an over-broad walk.
	require.Lenf(t, got, len(bundle.Files),
		"written tree has %d files, bundle has %d — extra or missing entries", len(got), len(bundle.Files))
}

func TestEnsureOKF_Deterministic_IdenticalSHA(t *testing.T) {
	// Regenerating the SAME store twice after clearing the marker, with the
	// okf/main ref still present, hits the tree-equality short-circuit and
	// returns the EXISTING tip commit unchanged. This validates idempotence
	// of EnsureOKF's cache-miss path, not a freshly minted commit's
	// determinism — see TestEnsureOKF_RemintIsDeterministic for that.
	svc := ensureOKFTestService(t)
	ctx := context.Background()

	a, err := svc.EnsureOKF(ctx, "main")
	require.NoError(t, err)

	require.NoError(t, svc.rh.gits.OKFMarkerSet("main", ""), "force regeneration")

	b, err := svc.EnsureOKF(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, a, b, "regeneration not deterministic")
}

// TestEnsureOKF_RemintIsDeterministic exercises a TRUE re-mint: with both the
// marker cleared and the okf/main ref removed, EnsureOKF has no tip to
// short-circuit against and no parent to chain onto, so it must mint a brand
// new commit object. The first generation also had no okf/main ref yet (no
// prior generation), so it too was parentless — a re-mint with no tip is
// parent-identical to the first mint. Combined with the fixed okfIdentity and
// the source-commit-derived timestamp (never the clock), the freshly minted
// commit must be byte-identical, and therefore SHA-identical, to the first.
func TestEnsureOKF_RemintIsDeterministic(t *testing.T) {
	svc := ensureOKFTestService(t)
	ctx := context.Background()

	a, err := svc.EnsureOKF(ctx, "main")
	require.NoError(t, err)

	require.NoError(t, svc.rh.gits.OKFMarkerSet("main", ""), "force regeneration")
	require.NoError(t, svc.rh.gits.RemoveReference(plumbing.NewBranchReferenceName("okf/main")),
		"remove the tip so there is nothing to short-circuit against or chain onto")

	b, err := svc.EnsureOKF(ctx, "main")
	require.NoError(t, err)
	require.Equal(t, a, b, "a genuinely re-minted commit must reproduce the identical SHA")
}

func TestOKFTarball_ServesValidBundle(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, "main", "kb/decisions/okf/scope/d9d6557d.md",
		testFactBody("Scope", 0.9, nil), "seed", "learn")
	require.NoError(t, err)

	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/okf/main.tar.gz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/gzip", resp.Header.Get("Content-Type"))

	// Gunzip + untar; collect files.
	gz, err := gzip.NewReader(resp.Body)
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		var buf bytes.Buffer
		_, err = io.Copy(&buf, tr)
		require.NoError(t, err)
		files[hdr.Name] = buf.Bytes()
	}
	require.Contains(t, files, "index.md")
	require.Contains(t, files, "log.md")

	// The extracted tree must be OKF-conformant.
	var bundle okf.Bundle
	for name, content := range files {
		bundle.Files = append(bundle.Files, okf.File{Path: name, Content: content})
	}
	require.NoError(t, okf.Validate(bundle))
}

func TestOKFTarball_UnknownBranch404(t *testing.T) {
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/okf/nonexistent.tar.gz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
