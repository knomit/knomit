package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// gitRepoWith makes a work-tree repository with one unsigned commit of files.
func gitRepoWith(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	for p, body := range files {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, p), []byte(body), 0o644))
		_, err := wt.Add(p)
		require.NoError(t, err)
	}
	sig := &object.Signature{Name: "t", Email: "t@example.com", When: time.Unix(1700000000, 0)}
	_, err = wt.Commit("c", &gogit.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)
	return dir
}

// TestVerifyAudit_ExitCodes pins the contract a script reads, as LITERALS:
// 1 findings (an unsigned commit), 2 could not run (no fleet, or a fleet that
// is not a fleet repository).
func TestVerifyAudit_ExitCodes(t *testing.T) {
	fleetYAML, err := fact.FleetOntology().Serialize()
	require.NoError(t, err)
	kb := gitRepoWith(t, map[string]string{fact.OntologyFile: "id: x\nname: X\ntopics:\n  notes:\n    description: d\n"})
	fleet := gitRepoWith(t, map[string]string{fact.OntologyFile: string(fleetYAML)})

	run := func(args ...string) (string, error) {
		c := verifyAuditCmd()
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetArgs(args)
		err := c.Execute()
		return out.String(), err
	}
	code := func(err error) int {
		var coded *ExitCodeError
		require.ErrorAs(t, err, &coded)
		return coded.ExitCode()
	}

	out, err := run("--dir", kb, "--fleet", fleet)
	require.Equal(t, 1, code(err), out)
	require.Contains(t, out, "unsigned")

	_, err = run("--dir", kb)
	require.Equal(t, 2, code(err), "no --fleet: could not run")

	_, err = run("--dir", kb, "--fleet", kb)
	require.Equal(t, 2, code(err), "a KB is not a fleet: could not run")
}
