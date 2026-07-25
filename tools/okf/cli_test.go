package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// ---- fixture knowledge base -------------------------------------------------

func factBody(title string, conf float64) string {
	return "---\nkind: epistemic\ntype: observation\ndomain: [x]\nconfidence: " +
		strconv.FormatFloat(conf, 'g', -1, 64) + "\n---\n# " + title + "\n\nBody.\n"
}

var kbTime = time.Date(2026, 5, 1, 9, 0, 0, 0, time.UTC)

// newKB builds a real on-disk knowledge base repo, which is what the CLI
// fetches from — the tests therefore exercise the same fetch/refspec path a
// user gets against GitHub.
func newKB(t *testing.T) (dir string, repo *git.Repository) {
	t.Helper()
	dir = t.TempDir()
	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)
	kbCommit(t, repo, dir, "learn: seed", map[string]string{
		"kb/decisions/x/aaaaaaaa.md": factBody("Alpha", 0.9),
		"kb/decisions/x/bbbbbbbb.md": factBody("Beta", 0.8),
	}, nil)
	return dir, repo
}

// kbCommit writes/removes files in the KB and commits them.
func kbCommit(t *testing.T, repo *git.Repository, dir, msg string, write map[string]string, remove []string) plumbing.Hash {
	t.Helper()
	wt, err := repo.Worktree()
	require.NoError(t, err)
	for p, body := range write {
		abs := filepath.Join(dir, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(body), 0o644))
		_, err = wt.Add(p)
		require.NoError(t, err)
	}
	for _, p := range remove {
		_, err = wt.Remove(p)
		require.NoError(t, err)
	}
	sig := &object.Signature{Name: "kb", Email: "a+learn@agents.knomit.io", When: kbTime}
	h, err := wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)
	return h
}

// clone runs the real clone command into a fresh directory.
func cloneKB(t *testing.T, kbDir string, extraArgs ...string) (outDir string, log string) {
	t.Helper()
	outDir = filepath.Join(t.TempDir(), "okf")
	var buf bytes.Buffer
	args := append(append([]string{}, extraArgs...), kbDir, outDir)
	require.NoError(t, runClone(args, &buf))
	return outDir, buf.String()
}

func sync(t *testing.T, outDir string, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, runSync(args, outDir, &buf))
	return buf.String()
}

func commitCount(t *testing.T, dir string) int {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	iter, err := repo.Log(&git.LogOptions{From: head.Hash()})
	require.NoError(t, err)
	n := 0
	require.NoError(t, iter.ForEach(func(*object.Commit) error { n++; return nil }))
	return n
}

// ---- tests ------------------------------------------------------------------

// TestCLI_CloneThenIdempotentSync is the load-bearing one: it proves
// determinism makes a no-change sync a no-op, which is what keeps commits
// tracking knowledge rather than tool runs.
func TestCLI_CloneThenIdempotentSync(t *testing.T) {
	kbDir, _ := newKB(t)
	outDir, log := cloneKB(t, kbDir)

	require.Contains(t, log, "committed master")
	require.FileExists(t, filepath.Join(outDir, "index.md"))
	require.FileExists(t, filepath.Join(outDir, "log.md"))
	require.FileExists(t, filepath.Join(outDir, configFile))
	require.DirExists(t, filepath.Join(outDir, "kb"))
	require.DirExists(t, filepath.Join(outDir, "views"))
	require.Equal(t, 1, commitCount(t, outDir))

	// The output branch mirrors the source branch name.
	repo, err := git.PlainOpen(outDir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, "master", head.Name().Short())

	cfg, err := readConfig(outDir)
	require.NoError(t, err)
	require.Equal(t, "master", cfg.Branch)
	require.Len(t, cfg.SyncedCommit, 40)

	out := sync(t, outDir)
	require.Contains(t, out, "already up to date")
	require.Equal(t, 1, commitCount(t, outDir), "an unchanged source must produce no second commit")
}

func TestCLI_SyncAfterFactWriteCommitsTheDiff(t *testing.T) {
	kbDir, kbRepo := newKB(t)
	outDir, _ := cloneKB(t, kbDir)
	require.Equal(t, 1, commitCount(t, outDir))

	kbCommit(t, kbRepo, kbDir, "learn: add gamma", map[string]string{
		"kb/decisions/x/cccccccc.md": factBody("Gamma", 0.7),
	}, nil)

	out := sync(t, outDir)
	require.Contains(t, out, "committed master")
	require.Equal(t, 2, commitCount(t, outDir))

	// The new fact has a document, and the config moved with it.
	require.FileExists(t, filepath.Join(outDir, "kb/decisions/x/gamma-cccccccc.md"))
	cfg, err := readConfig(outDir)
	require.NoError(t, err)
	require.Equal(t, 3, len(mustFacts(t, outDir)), "all three facts are published")
	require.NotEmpty(t, cfg.SyncedCommit)
}

// A retired fact's document must be REMOVED. Overlaying files can never delete
// them, so without reconciliation a withdrawn claim stays published forever —
// contradicting the views/retired.md in the same bundle.
func TestCLI_RetiredFactDocumentIsRemoved(t *testing.T) {
	kbDir, kbRepo := newKB(t)
	outDir, _ := cloneKB(t, kbDir)
	doc := filepath.Join(outDir, "kb/decisions/x/beta-bbbbbbbb.md")
	require.FileExists(t, doc)

	kbCommit(t, kbRepo, kbDir, "manual-review: retract kb/decisions/x/bbbbbbbb.md",
		nil, []string{"kb/decisions/x/bbbbbbbb.md"})

	sync(t, outDir)

	require.NoFileExists(t, doc, "a retired fact's document must be removed")
	require.FileExists(t, filepath.Join(outDir, "views/retired.md"))
	raw, err := os.ReadFile(filepath.Join(outDir, "views/retired.md"))
	require.NoError(t, err)
	require.Contains(t, string(raw), "Beta")

	// And it is gone from the COMMIT, not merely from the working tree.
	require.NotContains(t, committedPaths(t, outDir), "kb/decisions/x/beta-bbbbbbbb.md")
}

func TestCLI_PublisherReadmeSurvivesSync(t *testing.T) {
	kbDir, kbRepo := newKB(t)
	outDir, _ := cloneKB(t, kbDir)

	// The publisher adds and commits their own files.
	readme := filepath.Join(outDir, "README.md")
	require.NoError(t, os.WriteFile(readme, []byte("# My knowledge base\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(outDir, ".github"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outDir, ".github/ci.yml"), []byte("on: push\n"), 0o644))
	repo, err := git.PlainOpen(outDir)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	_, err = wt.Add("README.md")
	require.NoError(t, err)
	_, err = wt.Add(".github/ci.yml")
	require.NoError(t, err)
	sig := &object.Signature{Name: "pub", Email: "p@example.com", When: kbTime}
	_, err = wt.Commit("add readme", &git.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)

	kbCommit(t, kbRepo, kbDir, "learn: add gamma", map[string]string{
		"kb/decisions/x/cccccccc.md": factBody("Gamma", 0.7),
	}, nil)
	sync(t, outDir)

	require.FileExists(t, readme, "a publisher's README must survive a sync")
	require.FileExists(t, filepath.Join(outDir, ".github/ci.yml"))
	paths := committedPaths(t, outDir)
	require.Contains(t, paths, "README.md", "and must stay committed")
	require.Contains(t, paths, ".github/ci.yml")
}

// The source history lives under refs/knomit-okf/source/*, outside
// refs/heads/*, so git's default push refspec never publishes it.
func TestCLI_SourceRefsAreNotPushed(t *testing.T) {
	kbDir, _ := newKB(t)
	outDir, _ := cloneKB(t, kbDir)

	// The source refs exist locally...
	repo, err := git.PlainOpen(outDir)
	require.NoError(t, err)
	branches, err := sourceBranches(repo)
	require.NoError(t, err)
	require.NotEmpty(t, branches, "source history must be fetched")

	// ...and are NOT checked out.
	require.NoFileExists(t, filepath.Join(outDir, "kb/decisions/x/aaaaaaaa.md"),
		"the source tree must never appear in the working tree")

	// Push to a bare remote with git's DEFAULT refspec.
	bare := filepath.Join(t.TempDir(), "bare.git")
	_, err = git.PlainInit(bare, true)
	require.NoError(t, err)
	_, err = repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}})
	require.NoError(t, err)
	require.NoError(t, repo.Push(&git.PushOptions{RemoteName: "origin"}))

	bareRepo, err := git.PlainOpen(bare)
	require.NoError(t, err)
	iter, err := bareRepo.References()
	require.NoError(t, err)
	var pushed []string
	require.NoError(t, iter.ForEach(func(ref *plumbing.Reference) error {
		if ref.Name() != plumbing.HEAD {
			pushed = append(pushed, ref.Name().String())
		}
		return nil
	}))
	require.NotEmpty(t, pushed)
	for _, r := range pushed {
		require.NotContains(t, r, "knomit-okf/source",
			"source refs must never reach a remote; pushed: %v", pushed)
		require.Contains(t, r, "refs/heads/", "only branches may be pushed; pushed: %v", pushed)
	}
}

func TestCLI_PublishSourceControlsTheConfigField(t *testing.T) {
	kbDir, _ := newKB(t)

	// Default: the KB address is NOT published.
	plain, _ := cloneKB(t, kbDir)
	cfg, err := readConfig(plain)
	require.NoError(t, err)
	require.Empty(t, cfg.Source, "a private KB's address must never be published by default")
	raw, err := os.ReadFile(filepath.Join(plain, configFile))
	require.NoError(t, err)
	require.NotContains(t, string(raw), "source:")

	// Opt in: the address is recorded so a stranger can sync the published repo.
	published, _ := cloneKB(t, kbDir, "--publish-source")
	cfg, err = readConfig(published)
	require.NoError(t, err)
	require.Equal(t, kbDir, cfg.Source)

	// A later bare sync PRESERVES it: silently un-publishing would break every
	// downstream clone that relies on the field to find the source.
	kbDir2, kbRepo2 := kbDir, mustOpen(t, kbDir)
	kbCommit(t, kbRepo2, kbDir2, "learn: add gamma", map[string]string{
		"kb/decisions/x/cccccccc.md": factBody("Gamma", 0.7),
	}, nil)
	sync(t, published)
	cfg, err = readConfig(published)
	require.NoError(t, err)
	require.Equal(t, kbDir, cfg.Source, "a published source URL survives a bare sync")
}

// A second source branch becomes its own ORPHAN output branch: the two bundles
// are unrelated snapshots, so a shared history would only produce meaningless
// diffs between them.
func TestCLI_SecondBranchGetsItsOwnOrphanBranch(t *testing.T) {
	kbDir, kbRepo := newKB(t)
	outDir, _ := cloneKB(t, kbDir)

	// A second branch in the KB with its own fact.
	wt, err := kbRepo.Worktree()
	require.NoError(t, err)
	require.NoError(t, wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName("agent/foobar"), Create: true,
	}))
	kbCommit(t, kbRepo, kbDir, "learn: agent fact", map[string]string{
		"kb/decisions/x/dddddddd.md": factBody("Delta", 0.6),
	}, nil)

	out := sync(t, outDir, "-b", "agent/foobar")
	require.Contains(t, out, "committed agent/foobar")

	repo, err := git.PlainOpen(outDir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, "agent/foobar", head.Name().Short())

	// Orphan: exactly one commit, no parents.
	require.Equal(t, 1, commitCount(t, outDir), "a new output branch is an orphan")
	c, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	require.Zero(t, c.NumParents())

	// It carries the branch's own fact and records its own source branch.
	require.FileExists(t, filepath.Join(outDir, "kb/decisions/x/delta-dddddddd.md"))
	cfg, err := readConfig(outDir)
	require.NoError(t, err)
	require.Equal(t, "agent/foobar", cfg.Branch)

	// The original branch is untouched and still syncs on its own terms.
	require.NoError(t, wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("master")}))
	out = sync(t, outDir, "-b", "master")
	require.Contains(t, out, "already up to date")
}

// A branch that does not exist upstream must fail with an actionable message
// rather than exporting the wrong knowledge.
func TestCLI_UnknownBranchListsWhatWasFetched(t *testing.T) {
	kbDir, _ := newKB(t)
	outDir := filepath.Join(t.TempDir(), "okf")
	var buf bytes.Buffer
	err := runClone([]string{"-b", "nope", kbDir, outDir}, &buf)
	require.Error(t, err)
	require.Contains(t, err.Error(), `"nope" not found`)
	require.Contains(t, err.Error(), "master", "the error must list what WAS fetched")
}

// ---- helpers ----------------------------------------------------------------

func mustOpen(t *testing.T, dir string) *git.Repository {
	t.Helper()
	r, err := git.PlainOpen(dir)
	require.NoError(t, err)
	return r
}

// committedPaths lists every path in the output branch's HEAD tree.
func committedPaths(t *testing.T, dir string) []string {
	t.Helper()
	repo := mustOpen(t, dir)
	head, err := repo.Head()
	require.NoError(t, err)
	c, err := repo.CommitObject(head.Hash())
	require.NoError(t, err)
	tree, err := c.Tree()
	require.NoError(t, err)
	var out []string
	require.NoError(t, tree.Files().ForEach(func(f *object.File) error {
		out = append(out, f.Name)
		return nil
	}))
	return out
}

// mustFacts lists the concept documents in the committed bundle.
func mustFacts(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, p := range committedPaths(t, dir) {
		if filepath.Base(p) != "index.md" && filepath.Ext(p) == ".md" &&
			len(p) > 3 && p[:3] == "kb/" {
			out = append(out, p)
		}
	}
	return out
}
