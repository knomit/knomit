package cmd

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/spf13/cobra"

	"knomit/internal/app"
	"knomit/internal/config"
	"knomit/internal/store"
)

// verifyAcceptCmd builds `knomit verify accept`: record a waiver on THIS
// instance (control.db). Its only reader is E4: when a clone refuses origin's
// copy of this instance's agent branch because it holds commits this instance
// did not sign (commits from before signing existed, say), accepting each one
// lets the next clone adopt the branch.
func verifyAcceptCmd() *cobra.Command {
	var (
		repoName string
		note     string
		list     bool
	)
	cmd := &cobra.Command{
		Use:   "accept <commit>",
		Short: "Waive the own-branch check (E4) for one commit on this instance (F09)",
		Long: `Records, on THIS instance, a waiver for one commit on origin's copy of this
instance's own agent branch. A clone refuses that branch when it holds commits
this instance did not sign (for example commits from before signing existed)
and lists them; accept each one, then clone again. The waiver lives in this
instance's control database, so it works before the repository exists.

An accept waives nothing else: the gate that advances a knowledge base's main
(knomit verify ci) never reads it.

Without --repo the waiver applies to that commit in ANY repository (commits
are content-addressed). With --repo it applies to that repository only.

  knomit verify accept --list     show every waiver on this instance`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runVerifyAccept(cmd, args, repoName, note, list); err != nil {
				return &ExitCodeError{Code: exitFailed, Err: err}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "repo", "", "scope the waiver to this repository")
	cmd.Flags().StringVar(&note, "note", "", "why this commit is accepted (kept with the waiver)")
	cmd.Flags().BoolVar(&list, "list", false, "list every waiver on this instance")
	return cmd
}

func bootForVerify(cmd *cobra.Command) (*app.App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	a, err := app.New(cmd.Context(), cfg, app.Options{})
	if err != nil {
		return nil, fmt.Errorf("init app: %w", err)
	}
	return a, nil
}

func runVerifyAccept(cmd *cobra.Command, args []string, repoName, note string, list bool) error {
	if !list && len(args) != 1 {
		return fmt.Errorf("name the commit to accept (a full 40-hex hash), or pass --list")
	}
	var commit plumbing.Hash
	if !list {
		if !plumbing.IsHash(args[0]) {
			return fmt.Errorf("%q is not a full 40-hex commit hash", args[0])
		}
		commit = plumbing.NewHash(args[0])
	}
	a, err := bootForVerify(cmd)
	if err != nil {
		return err
	}
	defer a.Close()
	accepts := a.Manager().VerifyAccepts()
	if accepts == nil {
		return fmt.Errorf("the accept list is not available")
	}
	out := cmd.OutOrStdout()
	if list {
		all, err := accepts.List()
		if err != nil {
			return err
		}
		for _, x := range all {
			scope := "any repository"
			if x.RepoUID != "" {
				scope = "repository " + x.RepoUID
			}
			fmt.Fprintf(out, "%s  (%s)  %s\n", x.Commit, scope, x.Note)
		}
		return nil
	}

	uid := ""
	if repoName != "" {
		ri := a.Manager().Get(repoName)
		if ri == nil {
			return fmt.Errorf("repo %q not found", repoName)
		}
		uid = ri.UID()
		exists := false
		_ = ri.WithRead(func(svc *store.Service) {
			if svc != nil {
				exists = svc.CommitExists(commit)
			}
		})
		if !exists {
			fmt.Fprintf(cmd.ErrOrStderr(), "note: %s is not in %s's objects yet; the waiver is recorded and applies when it arrives\n", commit, ri.Name())
		}
	}
	if err := accepts.Add(commit, uid, note); err != nil {
		return err
	}
	scope := "any repository"
	if repoName != "" {
		scope = "repository " + repoName
	}
	fmt.Fprintf(out, "accepted %s on this instance (%s)\n", commit, scope)
	return nil
}

// verifyCICmd builds `knomit verify ci`: F09's acceptance gate for a knowledge
// base's repository, run by the job that advances its main (the GitHub
// workflow that merges agent branches). It is store.CheckRange over plain git
// checkouts: it needs no knomit home and boots nothing.
func verifyCICmd() *cobra.Command {
	var (
		dir, upstream, candidate, fleet, fleetRev, fleetRoot string
		asJSON                                               bool
	)
	cmd := &cobra.Command{
		Use:   "ci",
		Short: "Check a candidate branch before it is merged into a knowledge base's main (F09 gate)",
		Long: `Reads verify_signatures at the upstream's tip FIRST. Off or absent: exits 0 at
once and touches nothing else (no fleet is needed). On (log or enforce): loads
the member records of the fleet repository checkout --fleet and checks every
commit the candidate adds to the upstream: signed; its author's agent id has
exactly one member record; the signing key is that record's current key and no
other record's; the agent is active. An unsigned merge passes only if it adds
nothing beyond its parents.

  knomit verify ci --upstream origin/main --candidate "$TIP" --fleet ../fleet

Exit codes: 0 mergeable (verification off, a clean range, or failures in log
mode, which are reported only), 1 blocked (failures in enforce), 2 could not
run (a missing or unreadable fleet while verification is on, a checkout whose
ontology is not the fleet preset, an unknown verify_signatures value).`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			v, err := store.CheckRange(store.RangeInput{
				KBDir: dir, Main: upstream, Candidate: candidate,
				FleetDir: fleet, FleetRev: fleetRev, FleetRoot: fleetRoot,
			})
			if err != nil {
				if errors.Is(err, store.ErrNoFleet) {
					err = fmt.Errorf("%w: pass --fleet <fleet repository checkout>", err)
				}
				return &ExitCodeError{Code: exitFailed, Err: err}
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(v); err != nil {
					return &ExitCodeError{Code: exitFailed, Err: err}
				}
			} else if v.Mode == store.VerifyOff {
				fmt.Fprintf(out, "verification is off in %s: nothing to check\n", upstream)
			} else {
				fmt.Fprintf(out, "%s onto %s: mode %s, %d new commit(s), %d refused\n",
					candidate, upstream, v.Mode, v.Checked, len(v.Refused))
				for _, r := range v.Refused {
					fmt.Fprintf(out, "  REFUSED %s  %s: %s\n", short8(r.Commit), r.Rule, r.Reason)
				}
			}
			if code := v.ExitCode(); code != 0 {
				return &ExitCodeError{Code: code, Err: fmt.Errorf("%s is blocked: %d refused commit(s)", candidate, len(v.Refused))}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "the knowledge base checkout")
	cmd.Flags().StringVar(&upstream, "upstream", "origin/main", "the upstream revision the candidate would merge into")
	cmd.Flags().StringVar(&candidate, "candidate", "HEAD", "the candidate revision (the agent branch tip)")
	cmd.Flags().StringVar(&fleet, "fleet", "", "the fleet repository checkout (needed only when verification is on)")
	cmd.Flags().StringVar(&fleetRev, "fleet-rev", "HEAD", "the fleet revision holding the accepted member records")
	cmd.Flags().StringVar(&fleetRoot, "fleet-root", "kb", "the fleet repository's fact root")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the verdict as JSON")
	return cmd
}
