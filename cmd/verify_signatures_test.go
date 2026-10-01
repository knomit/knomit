package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// TestVerifyCmd_SignatureSubcommandsRegistered: F09's ci (the gate), audit
// (forensics) and accept (E4's waiver) live under `knomit verify`, the name
// E4's refusal tells the user to run. There is no `report` any more.
func TestVerifyCmd_SignatureSubcommandsRegistered(t *testing.T) {
	c := verifyCmd()
	for _, name := range []string{"accept", "ci", "audit"} {
		sub, _, err := c.Find([]string{name})
		require.NoError(t, err)
		require.Equal(t, name, sub.Name())
	}
	sub, _, _ := c.Find([]string{"report"})
	require.NotEqual(t, "report", sub.Name(), "report was replaced by audit")
	accept, _, _ := c.Find([]string{"accept"})
	require.Contains(t, accept.Long, "never reads it", "an accept waives E4 only, never the gate")
	for _, f := range []string{"repo", "note", "list"} {
		require.NotNil(t, accept.Flags().Lookup(f), "accept --%s", f)
	}
	ci, _, _ := c.Find([]string{"ci"})
	require.Contains(t, ci.Long, "verify_signatures at the upstream's tip FIRST")
	require.NotNil(t, ci.Flags().Lookup("fleet"))
}

// TestVerifyAccept_RefusesBeforeBooting: argument mistakes fail before the app
// boots (no repos are opened for a typo).
func TestVerifyAccept_RefusesBeforeBooting(t *testing.T) {
	c := &cobra.Command{}
	err := runVerifyAccept(c, []string{"not-a-hash"}, "", "", false)
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "40-hex"))
	err = runVerifyAccept(c, nil, "", "", false)
	require.ErrorContains(t, err, "name the commit")
}

// ciRepo builds a plain git repository: branch "upstream" holds a KB ontology
// with verify_signatures = mode ("" = absent); branch "candidate" adds one
// UNSIGNED note on top.
func ciRepo(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	sig := &object.Signature{Name: "t", Email: "agent-x+learn@agents.knomit.io", When: time.Unix(1700000000, 0)}
	write := func(path, body string) {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(path)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, path), []byte(body), 0o644))
		_, err := wt.Add(path)
		require.NoError(t, err)
	}
	commit := func(msg string) plumbing.Hash {
		h, err := wt.Commit(msg, &gogit.CommitOptions{Author: sig, Committer: sig})
		require.NoError(t, err)
		return h
	}
	branch := func(name string, h plumbing.Hash) {
		require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), h)))
	}
	ont := "id: x\nname: X\n"
	if mode != "" {
		ont += "attributes:\n  verify_signatures: " + mode + "\n"
	}
	ont += "topics:\n  notes:\n    description: d\n"
	write(fact.OntologyFile, ont)
	branch("upstream", commit("root"))
	write("kb/notes/n.md", "n")
	branch("candidate", commit("note"))
	return dir
}

// fleetRepo is a plain git checkout whose ontology is the fleet preset, with
// no member records.
func fleetRepo(t *testing.T) string {
	t.Helper()
	y, err := fact.FleetOntology().Serialize()
	require.NoError(t, err)
	return gitRepoWith(t, map[string]string{fact.OntologyFile: string(y)})
}

// TestVerifyCI_ExitCodes pins the contract the merge job reads, as LITERALS:
// 0 when the KB is off (and then no fleet is needed at all), 1 when an
// enforce KB's candidate is refused, 0 when a log KB's candidate is only
// reported, 2 when the gate could not run (no fleet, or a checkout that is
// not a fleet).
func TestVerifyCI_ExitCodes(t *testing.T) {
	run := func(args ...string) (string, error) {
		c := verifyCICmd()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetArgs(append([]string{"--upstream", "upstream", "--candidate", "candidate"}, args...))
		err := c.Execute()
		return out.String(), err
	}
	code := func(err error) int {
		t.Helper()
		if err == nil {
			return 0
		}
		var coded *ExitCodeError
		require.ErrorAs(t, err, &coded)
		return coded.ExitCode()
	}

	off := ciRepo(t, "")
	out, err := run("--dir", off)
	require.Equal(t, 0, code(err), "off: exit 0 with no --fleet at all")
	require.Contains(t, out, "nothing to check")

	enforce := ciRepo(t, "enforce")
	fleet := fleetRepo(t)
	out, err = run("--dir", enforce, "--fleet", fleet)
	require.Equal(t, 1, code(err), "an unsigned commit in an enforce KB is blocked: %s", out)
	require.Contains(t, out, "REFUSED")

	logKB := ciRepo(t, "log")
	out, err = run("--dir", logKB, "--fleet", fleet)
	require.Equal(t, 0, code(err), "log reports and passes")
	require.Contains(t, out, "REFUSED")

	_, err = run("--dir", enforce)
	require.Equal(t, 2, code(err), "on, but no fleet given: could not run")

	_, err = run("--dir", enforce, "--fleet", off)
	require.Equal(t, 2, code(err), "a checkout that is not a fleet: could not run")

	_, err = run("--dir", t.TempDir())
	require.Equal(t, 2, code(err), "not a git repository: could not run")
}

// With no --upstream, `verify ci` checks against the branch origin's HEAD names
// (here trunk), never a branch assumed by name; with no origin/HEAD it cannot
// run (exit 2) and says how to set it.
//
// SABOTAGE: restore the flag default "origin/main" → the checkout has no
// origin/main, CheckRange cannot resolve it → exit 2, not 0 → red.
func TestVerifyCI_UpstreamDefaultsToOriginHead(t *testing.T) {
	dir := ciRepo(t, "")
	repo, err := gogit.PlainOpen(dir)
	require.NoError(t, err)
	up, err := repo.Reference(plumbing.NewBranchReferenceName("upstream"), true)
	require.NoError(t, err)

	run := func() (string, error) {
		c := verifyCICmd()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetArgs([]string{"--dir", dir, "--candidate", "candidate"})
		err := c.Execute()
		return out.String(), err
	}

	_, err = run()
	var coded *ExitCodeError
	require.ErrorAs(t, err, &coded, "no origin/HEAD: the gate cannot run")
	require.Equal(t, 2, coded.ExitCode())
	require.ErrorContains(t, err, "set-head")

	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "trunk"), up.Hash())))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.NewRemoteHEADReferenceName("origin"), plumbing.NewRemoteReferenceName("origin", "trunk"))))
	out, err := run()
	require.NoError(t, err, out)
	require.Contains(t, out, "verification is off in origin/trunk")

	// An origin/HEAD naming an agent branch is not a consensus branch.
	require.NoError(t, repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewRemoteReferenceName("origin", "agent/x"), up.Hash())))
	require.NoError(t, repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.NewRemoteHEADReferenceName("origin"), plumbing.NewRemoteReferenceName("origin", "agent/x"))))
	_, err = run()
	require.ErrorAs(t, err, &coded)
	require.Equal(t, 2, coded.ExitCode())
}

// The shipped CI template names no branch: it sets origin/HEAD from the forge
// and lets `verify ci` read it. Comment lines are prose and do not count.
//
// SABOTAGE: restore `git fetch --no-tags origin main` or
// `--upstream origin/main` → red.
func TestVerifyCITemplate_NamesNoBranch(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "tools", "ci", "verify-signatures.yml"))
	require.NoError(t, err)
	for i, line := range strings.Split(string(b), "\n") {
		code := strings.TrimSpace(line)
		if strings.HasPrefix(code, "#") {
			continue
		}
		for _, w := range strings.FieldsFunc(code, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
		}) {
			require.NotContains(t, []string{"main", "master"}, w, "line %d names a branch: %s", i+1, line)
		}
	}
	require.Contains(t, string(b), "git remote set-head origin --auto")
}
