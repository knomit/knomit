package gitfixture

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitIn runs git in dir with a fixed identity, failing the test on error.
func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// pushStarts pushes one commit into bare with git's trace2 event log on and
// reports how many git-receive-pack processes ran and how many of them
// started an auto-maintenance child.
func pushStarts(t *testing.T, bare string) (receivePacks, maintenance int) {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "trace2.json")
	work := t.TempDir()
	gitIn(t, work, "init", "-q", "-b", "main")
	gitIn(t, work, "-c", "maintenance.auto=false", "commit", "-q", "--allow-empty", "-m", "one")
	t.Setenv("GIT_TRACE2_EVENT", trace)
	// A fresh branch per push: the commit's hash depends only on the clock's
	// second, so pushing it to an existing branch could be a no-op or a
	// non-fast-forward.
	gitIn(t, work, "push", "-q", "file://"+bare, "HEAD:refs/heads/push-"+filepath.Base(work))
	raw, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		var ev struct {
			Event string   `json:"event"`
			Argv  []string `json:"argv"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		joined := strings.Join(ev.Argv, " ")
		if ev.Event == "start" && strings.Contains(joined, "receive-pack") {
			receivePacks++
		}
		if (ev.Event == "start" || ev.Event == "child_start") && strings.Contains(joined, "maintenance run") {
			maintenance++
		}
	}
	return receivePacks, maintenance
}

// A push into a bare repository made by InitBare (and by CloneBare) starts no
// git auto-maintenance. The oracle is git's own trace2 log, not timing.
// Positive control: a bare repository made by plain `git init --bare` does
// start one, so the oracle can see it.
// SABOTAGE: drop NoAutoMaintenance from InitBare -> the first push starts
// `git maintenance run --auto` -> red.
func TestInitBare_APushStartsNoAutoMaintenance(t *testing.T) {
	bare := InitBare(t, filepath.Join(t.TempDir(), "r.git"), "main")
	rp, m := pushStarts(t, bare)
	if rp == 0 {
		t.Fatal("reached: no traced git-receive-pack ran")
	}
	if m != 0 {
		t.Fatalf("a push into an InitBare repository started %d auto-maintenance children", m)
	}

	clone := CloneBare(t, bare, filepath.Join(t.TempDir(), "c.git"))
	if _, m := pushStarts(t, clone); m != 0 {
		t.Fatalf("a push into a CloneBare repository started %d auto-maintenance children", m)
	}

	plain := filepath.Join(t.TempDir(), "plain.git")
	gitIn(t, "", "init", "-q", "--bare", plain)
	if _, m := pushStarts(t, plain); m == 0 {
		t.Fatal("positive control: a plain bare repository's push starts auto-maintenance (git's default); the oracle saw none")
	}
}
