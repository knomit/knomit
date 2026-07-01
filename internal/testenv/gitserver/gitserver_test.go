package gitserver

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-billy/v5/memfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

// testSig returns a fixed-timestamp signature for use in test commits.
// The fixed timestamp avoids Date.now flakiness across reruns.
func testSig() *object.Signature {
	return &object.Signature{Name: "t", Email: "t@t", When: time.Unix(1700000000, 0)}
}

func runGit(t testing.TB, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initBareRepoWithCommit creates <root>/<name> as a bare repo with one
// commit on main and receive-pack enabled. Returns the URL path segment.
func initBareRepoWithCommit(t testing.TB, root, name string) string {
	t.Helper()
	bare := filepath.Join(root, name)
	runGit(t, root, "init", "--bare", "-b", "main", bare)
	runGit(t, root, "--git-dir="+bare, "config", "http.receivepack", "true")

	work := t.TempDir()
	runGit(t, work, "init", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(work, "seed.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	runGit(t, work, "add", "seed.txt")
	runGit(t, work, "commit", "-m", "seed")
	runGit(t, work, "remote", "add", "origin", bare)
	runGit(t, work, "push", "origin", "main")
	return "/" + name
}

func TestFaultPlan_Status401FailsClone(t *testing.T) {
	root := t.TempDir()
	repoPath := initBareRepoWithCommit(t, root, "core.git")
	srv := New(t, root)
	defer srv.Close()

	srv.Fault().SetStatus(ClassInfoRefs, http.StatusUnauthorized)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := gogit.CloneContext(ctx, memory.NewStorage(), memfs.New(), &gogit.CloneOptions{
		URL: srv.URL + repoPath,
	})
	if err == nil {
		t.Fatal("expected clone to fail with 401, got nil")
	}
}

func TestFaultPlan_HangAbortsWithDeadline(t *testing.T) {
	root := t.TempDir()
	repoPath := initBareRepoWithCommit(t, root, "core.git")
	srv := New(t, root)
	defer srv.Close()

	srv.Fault().SetHang(ClassInfoRefs, true)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := gogit.CloneContext(ctx, memory.NewStorage(), memfs.New(), &gogit.CloneOptions{
		URL: srv.URL + repoPath,
	})
	if err == nil {
		t.Fatal("expected deadline-bounded clone to fail against a hanging server")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("clone did not abort promptly: %v", elapsed)
	}
}

func TestFaultPlan_TruncateFailsClone(t *testing.T) {
	root := t.TempDir()
	repoPath := initBareRepoWithCommit(t, root, "core.git")
	srv := New(t, root)
	defer srv.Close()

	// Truncate the packfile upload (fetch) response after 64 bytes.
	srv.Fault().SetTruncateAfter(ClassUploadPack, 64)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := gogit.CloneContext(ctx, memory.NewStorage(), memfs.New(), &gogit.CloneOptions{
		URL: srv.URL + repoPath,
	})
	if err == nil {
		t.Fatal("expected clone to fail on truncated pack")
	}
}

func TestFaultPlan_AuthAndExpiry(t *testing.T) {
	root := t.TempDir()
	repoPath := initBareRepoWithCommit(t, root, "core.git")
	srv := New(t, root)
	defer srv.Close()

	srv.Fault().RequireBasicAuth("alice", "s3cret")

	cloneWith := func(user, pass string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := gogit.CloneContext(ctx, memory.NewStorage(), memfs.New(), &gogit.CloneOptions{
			URL:  srv.URL + repoPath,
			Auth: &githttp.BasicAuth{Username: user, Password: pass},
		})
		return err
	}

	if err := cloneWith("alice", "wrong"); err == nil {
		t.Fatal("bad password should fail (A1/A5)")
	}
	if err := cloneWith("alice", "s3cret"); err != nil {
		t.Fatalf("good creds should clone: %v", err)
	}
}

func TestServer_CloneServesBareRepo(t *testing.T) {
	root := t.TempDir()
	repoPath := initBareRepoWithCommit(t, root, "core.git")

	srv := New(t, root)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r, err := gogit.CloneContext(ctx, memory.NewStorage(), memfs.New(), &gogit.CloneOptions{
		URL: srv.URL + repoPath,
	})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	head, err := r.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head.Name().Short() != "main" {
		t.Fatalf("HEAD = %q, want main", head.Name().Short())
	}
}

// TestServer_PushAndReceivePackReject exercises A4: pushing to the server
// succeeds normally, then SetStatus(ClassReceivePack, 403) rejects the push,
// and clearing the fault lets the push through again.
//
// go-git issues two requests when pushing:
//   - GET /info/refs?service=git-receive-pack → classified ClassInfoRefs
//   - POST /git-receive-pack                  → classified ClassReceivePack
//
// Rejecting only ClassReceivePack is the more precise A4 model (advertisement
// succeeds, actual pack transfer is forbidden). If that proves insufficient
// (go-git short-circuits on the advertisement), also set ClassInfoRefs.
func TestServer_PushAndReceivePackReject(t *testing.T) {
	root := t.TempDir()
	repoPath := initBareRepoWithCommit(t, root, "core.git")
	srv := New(t, root)
	defer srv.Close()

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	r, err := gogit.PlainCloneContext(ctx, dir, false, &gogit.CloneOptions{URL: srv.URL + repoPath})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	wt, _ := r.Worktree()
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("b.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("c2", &gogit.CommitOptions{Author: testSig()}); err != nil {
		t.Fatal(err)
	}

	// Reject pushes only (A4: pack POST forbidden, advertisement allowed).
	srv.Fault().SetStatus(ClassReceivePack, http.StatusForbidden)
	if err := r.PushContext(ctx, &gogit.PushOptions{}); err == nil {
		t.Fatal("expected push to be rejected (A4)")
	}

	// Clear and confirm push now succeeds.
	srv.Fault().SetStatus(ClassReceivePack, 0)
	if err := r.PushContext(ctx, &gogit.PushOptions{}); err != nil {
		t.Fatalf("push after clearing fault: %v", err)
	}
}
