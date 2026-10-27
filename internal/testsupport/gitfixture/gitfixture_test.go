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
		if (ev.Event == "start" || ev.Event == "child_start") && IsAutoMaintenance(ev.Argv) {
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

// IsAutoMaintenance matches git's maintenance child by its subcommand, never
// the word alone: the fixture's own `git config maintenance.auto false` is
// what a loose match counted (#654 review R2-B1).
func TestIsAutoMaintenance_MatchesTheSubcommandOnly(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{"git", "maintenance", "run", "--auto", "--quiet", "--detach"}, true},
		{[]string{"/usr/libexec/git-core/git", "maintenance", "run", "--auto"}, true},
		{[]string{"git", "gc", "--auto", "--quiet"}, true},
		{[]string{"git", "-C", "/r.git", "config", "maintenance.auto", "false"}, false},
		{[]string{"git", "gc"}, false},
		{[]string{"git-receive-pack", "/r.git"}, false},
		{[]string{"git", "maintenance"}, false},
	} {
		if got := IsAutoMaintenance(c.argv); got != c.want {
			t.Errorf("IsAutoMaintenance(%q) = %v, want %v", c.argv, got, c.want)
		}
	}
}

// The fixture's environment keeps the operating system's variables git for
// Windows needs (SYSTEMROOT, TEMP, TMP, USERPROFILE) and nothing of git's
// own: the only GIT_* entries are the three that turn config off.
// SABOTAGE: drop TEMP from osPassthrough -> red; pass os.Environ() -> GIT_DIR
// and GIT_CONFIG_COUNT appear -> red.
func TestIsolatedEnv_KeepsTheOSVariablesAndNoGitOnes(t *testing.T) {
	t.Setenv("SYSTEMROOT", `C:\Windows`)
	t.Setenv("TEMP", "/tmp/t1")
	t.Setenv("TMP", "/tmp/t2")
	t.Setenv("USERPROFILE", "/home/u")
	t.Setenv("GIT_DIR", "/elsewhere.git")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	got := map[string]string{}
	for _, kv := range isolatedEnv() {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for k, want := range map[string]string{"SYSTEMROOT": `C:\Windows`, "TEMP": "/tmp/t1", "TMP": "/tmp/t2", "USERPROFILE": "/home/u"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	for k := range got {
		if strings.HasPrefix(k, "GIT_") && k != "GIT_CONFIG_GLOBAL" && k != "GIT_CONFIG_SYSTEM" && k != "GIT_CONFIG_NOSYSTEM" {
			t.Errorf("isolatedEnv passes %s", k)
		}
	}
}

// configValue is `git config --get key` in repo, run with nothing inherited.
func configValue(t *testing.T, repo, key string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", repo, "config", "--get", key)
	cmd.Env = isolatedEnv()
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// #654 review R2-S1: the fixture's git inherits nothing from the test
// process. With GIT_DIR pointing at another repository (the suite run from a
// git hook), a template directory and a default branch set through
// GIT_CONFIG_* (a developer's config), and GIT_TRACE2_EVENT set, InitBare
// still makes its OWN repository, from git's built-in template, with the
// setting in IT, and traces nothing. Positive control: the same environment
// does reach a git started with it (the template's marker file appears).
// SABOTAGE: run the fixture's git with os.Environ() -> the setting lands in
// GIT_DIR's repository, the marker is copied and the trace is written -> red.
func TestInitBare_InheritsNothingFromTheEnvironment(t *testing.T) {
	other := filepath.Join(t.TempDir(), "other.git")
	gitIn(t, "", "init", "-q", "--bare", other)
	tmpl := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpl, "MARKER"), []byte("from the template"), 0o644); err != nil {
		t.Fatal(err)
	}
	trace := filepath.Join(t.TempDir(), "trace2.json")
	t.Setenv("GIT_DIR", other)
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "init.templateDir")
	t.Setenv("GIT_CONFIG_VALUE_0", tmpl)
	t.Setenv("GIT_CONFIG_KEY_1", "init.defaultBranch")
	t.Setenv("GIT_CONFIG_VALUE_1", "inherited")
	t.Setenv("GIT_TRACE2_EVENT", trace)

	bare := InitBare(t, filepath.Join(t.TempDir(), "fixture.git"), "")

	if got := configValue(t, bare, "maintenance.auto"); got != "false" {
		t.Fatalf("the fixture's own config holds maintenance.auto=%q, want false", got)
	}
	if got := configValue(t, other, "maintenance.auto"); got != "" {
		t.Fatalf("GIT_DIR's repository got maintenance.auto=%q: the fixture's git followed the inherited GIT_DIR", got)
	}
	if _, err := os.Stat(filepath.Join(bare, "MARKER")); err == nil {
		t.Fatal("the fixture was made from the inherited init.templateDir")
	}
	head, err := os.ReadFile(filepath.Join(bare, "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(head), "inherited") {
		t.Fatalf("the fixture's HEAD follows the inherited init.defaultBranch: %s", head)
	}
	if _, err := os.Stat(trace); err == nil {
		t.Fatal("the fixture's git wrote the inherited GIT_TRACE2_EVENT")
	}

	// Positive control: that environment does reach a git that inherits it.
	probe := filepath.Join(t.TempDir(), "probe.git")
	cmd := exec.Command("git", "init", "-q", "--bare", probe)
	cmd.Env = append(os.Environ(), "GIT_DIR="+probe)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(probe, "MARKER")); err != nil {
		t.Fatal("positive control: an inheriting git did not use the template; the check above proves nothing")
	}
}
