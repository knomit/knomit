package manage

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"knomit/internal/corpus/repos"
	"knomit/internal/foundation/config"
	"knomit/internal/platform/fileuri"
	"knomit/internal/testsupport/testsigner"
)

// Fixtures the package-private tests here share. They are COPIES of the
// helpers of the same name in internal/edge/web's tests (a test file cannot
// be imported, and web's full-router tests keep theirs): a change to one does
// not reach the other.

// newTestManagerWithRepos builds a repos.Manager and registers empty
// RepoInstance stubs under the given names.
func newTestManagerWithRepos(t *testing.T, names ...string) *repos.Manager {
	t.Helper()
	m := repos.New(context.Background(), repos.Deps{})
	for _, name := range names {
		m.Set(name, repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
			Name: name, AgentBranch: "agent/test",
		}))
	}
	return m
}

func runGitForTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// seedBareRemoteForTest builds a bare git repo with one commit on "main"
// under bare and returns a file:// URL pointing at it.
func seedBareRemoteForTest(t *testing.T, bare string) string {
	t.Helper()
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatalf("mkdir bare: %v", err)
	}
	runGitForTest(t, "", "init", "--bare", "--initial-branch=main", bare)
	work := t.TempDir()
	runGitForTest(t, "", "clone", bare, work)
	if err := os.WriteFile(filepath.Join(work, "seed.txt"), []byte("seed"), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	runGitForTest(t, work, "add", "seed.txt")
	runGitForTest(t, work, "commit", "-m", "seed")
	runGitForTest(t, work, "push", "origin", "main")
	runGitForTest(t, bare, "symbolic-ref", "HEAD", "refs/heads/main")
	return fileuri.New(bare)
}

// newControlDBManager boots a REAL Manager, control.db opened via Start(),
// with a repo "alpha" mounted via the real Create path. originsRoot becomes
// LocalOriginRoot so a file:// origin under it clears the local-origin policy
// gate.
func newControlDBManager(t *testing.T, originsRoot string) (*repos.Manager, *repos.RepoInstance) {
	t.Helper()
	home := t.TempDir()
	keyPath := filepath.Join(home, "agent.key")
	if err := os.WriteFile(keyPath, []byte("agent-key-material-for-hkdf"), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	m := repos.New(context.Background(), repos.Deps{
		Cfg:     config.Config{Home: home, OntologyRoot: "kb", LocalOriginRoot: originsRoot},
		AgentID: testsigner.FixtureAgentID("agent/test"), AgentBranch: "agent/test",
		KeyPath: keyPath,
		Machine: repos.Options{Synchronous: true},
	})
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Start(); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	ri, err := m.Create(context.Background(), repos.CreateSpec{Name: "alpha", Mode: "preset"}, nil)
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	return m, ri
}
