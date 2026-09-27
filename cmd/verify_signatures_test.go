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
)

// TestVerifyCmd_SignatureSubcommandsRegistered: F09's report, accept and ci
// live under `knomit verify`, the name E4's refusal tells the user to run.
func TestVerifyCmd_SignatureSubcommandsRegistered(t *testing.T) {
	c := verifyCmd()
	for _, name := range []string{"report", "accept", "ci"} {
		sub, _, err := c.Find([]string{name})
		require.NoError(t, err)
		require.Equal(t, name, sub.Name())
	}
	accept, _, _ := c.Find([]string{"accept"})
	require.Contains(t, accept.Long, "NEVER waives a change to verify_signatures or verify_signers")
	for _, f := range []string{"repo", "note", "list"} {
		require.NotNil(t, accept.Flags().Lookup(f), "accept --%s", f)
	}
	ci, _, _ := c.Find([]string{"ci"})
	require.Contains(t, ci.Long, "never the candidate head's ontology file")
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
	err = runVerifyReport(c, "", "", false)
	require.ErrorContains(t, err, "--repo is required")
	err = runVerifyReport(c, "x", "abc", false)
	require.ErrorContains(t, err, "40-hex")
}

// TestVerifyCI_CouldNotRunIsExitTwo: commands never call os.Exit; `verify ci`
// returns an ExitCodeError that main.go honours, so "could not run" (2) stays
// distinct from "blocked" (1) for a CI job.
func TestVerifyCI_CouldNotRunIsExitTwo(t *testing.T) {
	c := verifyCICmd()
	c.SetArgs([]string{"--dir", t.TempDir(), "--candidate", "HEAD"})
	err := c.Execute()
	var coded *ExitCodeError
	require.ErrorAs(t, err, &coded)
	require.Equal(t, 2, coded.ExitCode(), "could not run is exit 2, literally: a CI job reads the number")
}

// ciRepo builds a plain git repository: main holds an ontology with no
// verify attributes, agent/enable switches verify_signatures to log, and
// agent/plain only adds a note. No commit is signed.
func ciRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	sig := &object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(1700000000, 0)}
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
	const ont = "id: x\nname: X\ntopics:\n  notes:\n    description: d\n"
	write(".knomit/ontology.yaml", ont)
	base := commit("root")
	branch("upstream", base)
	write("kb/n.md", "n")
	branch("agent/plain", commit("note"))
	require.NoError(t, wt.Checkout(&gogit.CheckoutOptions{Hash: base, Force: true}))
	write(".knomit/ontology.yaml", "id: x\nname: X\nattributes:\n  verify_signatures: log\ntopics:\n  notes:\n    description: d\n")
	branch("agent/enable", commit("enable"))
	return dir
}

// TestVerifyCI_ExitCodes pins the contract a forge job reads, as literal
// numbers: 1 blocked, 0 mergeable. The blocked case is an enable the job
// cannot judge without the operator key.
func TestVerifyCI_ExitCodes(t *testing.T) {
	t.Setenv("KNOMIT_VERIFY_OPERATOR_KEY", "")
	dir := ciRepo(t)
	run := func(candidate string) (string, error) {
		c := verifyCICmd()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetArgs([]string{"--dir", dir, "--upstream", "upstream", "--candidate", candidate})
		err := c.Execute()
		return out.String(), err
	}

	out, err := run("agent/enable")
	var coded *ExitCodeError
	require.ErrorAs(t, err, &coded, out)
	require.Equal(t, 1, coded.ExitCode(), "blocked is exit 1, literally")
	require.Contains(t, err.Error(), "blocked: unrooted")

	out, err = run("agent/plain")
	require.NoError(t, err, "an off repository is mergeable: %s", out)
}
