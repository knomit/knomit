package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"knomit/internal/store"
)

// ExitCodeError carries the process exit code a command wants, for the one
// place allowed to exit (main.go). Commands never call os.Exit
// (TestCmd_NoProcessExit).
type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string { return e.Err.Error() }
func (e *ExitCodeError) Unwrap() error { return e.Err }
func (e *ExitCodeError) ExitCode() int { return e.Code }

// verifyAuditCmd builds `knomit verify audit`: F09's forensic check. It
// re-checks a WHOLE branch of a knowledge base against every version of the
// fleet repository's member records, on demand, over plain git checkouts. It
// never moves a ref and is never in a write path. Use it after a suspected
// breach: `--key <fingerprint>` lists everything that key signed.
func verifyAuditCmd() *cobra.Command {
	var (
		dir, branch, fleet, fleetRev, fleetRoot, key string
		asJSON                                       bool
	)
	c := &cobra.Command{
		Use:   "audit",
		Short: "Re-check a branch's commit signatures against the fleet's member history (F09)",
		Long: `Walks the whole history of --branch in the knowledge base checkout --dir and
resolves every commit's signer against EVERY version of the member records in
the fleet repository checkout --fleet. A departed agent, or a key since rotated
away, is attributed to its agent; findings are commits that are unsigned, signed
by a key that was never on a member record, signed by a key of another agent
than the author claims, or signed by a key whose agent was revoked.

  knomit verify audit --dir . --fleet ../fleet --key 1da053fe

Exit codes: 0 clean, 1 findings, 2 could not run.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if fleet == "" {
				return &ExitCodeError{Code: exitFailed, Err: fmt.Errorf("--fleet is required: the fleet repository checkout")}
			}
			rep, err := store.Audit(store.AuditInput{KBDir: dir, Branch: branch, FleetDir: fleet, FleetRev: fleetRev, FleetRoot: fleetRoot, Key: key})
			if err != nil {
				return &ExitCodeError{Code: exitFailed, Err: err}
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(rep); err != nil {
					return &ExitCodeError{Code: exitFailed, Err: err}
				}
			} else {
				fmt.Fprintf(out, "%s: %d commit(s), %d finding(s)\n", branch, rep.Commits, len(rep.Findings))
				for _, f := range rep.Findings {
					fmt.Fprintf(out, "  %s  %s: %s\n", short8(f.Commit), f.Rule, f.Reason)
				}
				if key != "" {
					fmt.Fprintf(out, "signed by %s: %d commit(s)\n", key, len(rep.KeySigned))
					for _, h := range rep.KeySigned {
						fmt.Fprintf(out, "  %s\n", h)
					}
				}
			}
			if code := rep.ExitCode(); code != 0 {
				return &ExitCodeError{Code: code, Err: fmt.Errorf("%d finding(s)", len(rep.Findings))}
			}
			return nil
		},
	}
	c.Flags().StringVar(&dir, "dir", ".", "the knowledge base checkout")
	c.Flags().StringVar(&branch, "branch", "HEAD", "the revision whose whole history is audited")
	c.Flags().StringVar(&fleet, "fleet", "", "the fleet repository checkout (required)")
	c.Flags().StringVar(&fleetRev, "fleet-rev", "HEAD", "the fleet revision to read the member history from")
	c.Flags().StringVar(&fleetRoot, "fleet-root", "kb", "the fleet repository's fact root")
	c.Flags().StringVar(&key, "key", "", "list every commit signed by this key (full fingerprint or a prefix)")
	c.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	return c
}

func short8(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
}
