package store

import (
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/okf"
)

// TestOKF_E2E_CloneIsConformant proves the headline UX end to end: a plain
// `git clone <url> -b okf/main` against the store's smart-HTTP handler yields
// a checkout that passes okf.Validate. This exercises the full stack — fact
// writes, lazy OKF branch generation on the advertise path, and the store's
// custom single-ack smart-HTTP negotiation — with no production code of its
// own; it is pure proof-by-clone.
func TestOKF_E2E_CloneIsConformant(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	require.NoError(t, svc.InitRepo(map[string]string{}, "main"))

	ctx := context.Background()
	_, err = svc.Facts().WriteFact(ctx, "main",
		"kb/decisions/okf/scope/d9d6557d.md",
		testFactBody("Scope decision", 0.9, nil),
		"seed scope", "learn")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "main",
		"kb/invariants/okf/refs/3209d651.md",
		testFactBody("Ref safety invariant", 0.8, nil),
		"seed refs", "learn")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "main",
		"kb/decisions/okf/tarball/7a1c9e02.md",
		testFactBody("Tarball endpoint shape", 0.7, []string{"kb/decisions/okf/scope/d9d6557d.md"}),
		"seed tarball", "learn")
	require.NoError(t, err)

	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	// Guarantee okf/main is generated and advertised before the clone: the
	// advertise path (GET /info/refs?service=git-upload-pack) is what runs
	// ensureOKFBranches in production, so drive it the same way rather than
	// reaching into the service internals.
	resp, err := http.Get(srv.URL + "/info/refs?service=git-upload-pack")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusOK, resp.StatusCode)

	clone := t.TempDir()
	_, err = git.PlainClone(clone, false, &git.CloneOptions{
		URL:           srv.URL,
		ReferenceName: plumbing.NewBranchReferenceName("okf/main"),
		SingleBranch:  true,
	})
	require.NoError(t, err, "git clone -b okf/main against the store's smart-HTTP handler")

	var bundle okf.Bundle
	sawIndex := false
	sawLog := false
	err = filepath.WalkDir(clone, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		rel, err := filepath.Rel(clone, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		bundle.Files = append(bundle.Files, okf.File{Path: rel, Content: content})
		if rel == "index.md" {
			sawIndex = true
			require.Contains(t, string(content), `okf_version: "0.1"`,
				"root index.md must carry okf_version")
		}
		if rel == "log.md" {
			sawLog = true
		}
		return nil
	})
	require.NoError(t, err)

	require.True(t, sawIndex, "checkout is missing root index.md")
	require.True(t, sawLog, "checkout is missing root log.md")
	require.NoError(t, okf.Validate(bundle), "cloned checkout must be a conformant OKF bundle")
}
