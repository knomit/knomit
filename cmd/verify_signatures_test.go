package cmd

import (
	"strings"
	"testing"

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
	require.Equal(t, exitFailed, coded.ExitCode())
}
