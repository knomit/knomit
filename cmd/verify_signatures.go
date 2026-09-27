package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/spf13/cobra"

	"knomit/internal/app"
	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// verifyReportCmd builds `knomit verify report`: F09's dry run over one
// repository's upstream. It folds the history exactly as a fresh clone would,
// moves and writes nothing, and works whatever the repository's mode (off
// included), so an operator can see what turning verification on would do
// and which keys have signed, BEFORE listing them.
func verifyReportCmd() *cobra.Command {
	var (
		repoName string
		from     string
		asJSON   bool
	)
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Dry-run commit signature verification on a repository's upstream (F09)",
		Long: `Folds the upstream history of a repository exactly as a fresh clone would and
prints what signature verification decides, without moving or writing anything.
Works on every repository, including those with verification off.

It lists every key that signed a commit: its full fingerprint, the author claims
seen on its commits, the first commit and date it signed, how many, whether it
is in verify_signers, and whether it is the configured [verify].operator_key.
Admit the keys you RECOGNISE: a key in this list only proves it signed a commit
that reached the upstream, not that it belongs in the fleet.

Exit codes: 0 report printed, 2 the report could not run.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runVerifyReport(cmd, repoName, from, asJSON); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
				os.Exit(exitFailed)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "repo", "", "repo name (required)")
	cmd.Flags().StringVar(&from, "from", "", "tally signers only for commits not reachable from this commit")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	return cmd
}

// verifyAcceptCmd builds `knomit verify accept`: record a waiver on THIS
// instance (control.db). It waives a failing signature on one commit, or an
// unsigned merge that fails merge rule M3. It never waives a policy change or
// an author claim; when --repo is given and the commit is already known to be
// one of those, the accept is refused here rather than silently ignored later.
func verifyAcceptCmd() *cobra.Command {
	var (
		repoName string
		note     string
		list     bool
	)
	cmd := &cobra.Command{
		Use:   "accept <commit>",
		Short: "Waive one commit's signature failure on this instance (F09)",
		Long: `Records, on THIS instance, a waiver for one commit:
  - a failing or missing signature on that commit, or
  - an unsigned merge that fails merge rule M3 (for example a criss-cross
    auto-merge).
It NEVER waives a change to verify_signatures or verify_signers, or an author
email that claims another agent's fingerprint.

It is also how a clone that refused origin's copy of this instance's agent
branch (commits from before signing) is let through: accept each commit the
refusal listed, then clone again. The waiver lives in this instance's control
database, so it works before the repository exists.

Without --repo the waiver applies to that commit in ANY repository (commits
are content-addressed). With --repo it applies to that repository only, and
the commit is checked against it first.

  knomit verify accept --list     show every waiver on this instance`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := runVerifyAccept(cmd, args, repoName, note, list); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
				os.Exit(exitFailed)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoName, "repo", "", "scope the waiver to this repository (and check the commit against it)")
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

// upstreamOf is the repository's upstream branch: the origin record's, else
// "main" (an origin-less repository's local consensus branch).
func upstreamOf(m *repos.Manager, ri *repos.RepoInstance) string {
	if o := m.Origins(); o != nil {
		if org, err := o.Get(ri.UID()); err == nil && org != nil && org.Branch != "" {
			return org.Branch
		}
	}
	return "main"
}

func runVerifyReport(cmd *cobra.Command, repoName, from string, asJSON bool) error {
	if repoName == "" {
		return fmt.Errorf("--repo is required")
	}
	var fromHash plumbing.Hash
	if from != "" {
		if !plumbing.IsHash(from) {
			return fmt.Errorf("--from %q is not a full 40-hex commit hash", from)
		}
		fromHash = plumbing.NewHash(from)
	}
	a, err := bootForVerify(cmd)
	if err != nil {
		return err
	}
	defer a.Close()
	ri := a.Manager().Get(repoName)
	if ri == nil {
		return fmt.Errorf("repo %q not found", repoName)
	}
	upstream := upstreamOf(a.Manager(), ri)
	var rep store.SignatureReport
	var rerr error
	if err := ri.WithRead(func(svc *store.Service) {
		rep, rerr = svc.SignatureReport(cmd.Context(), upstream, fromHash)
	}); err != nil {
		return err
	}
	if rerr != nil {
		return rerr
	}
	out := cmd.OutOrStdout()
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	printSignatureReport(out, repoName, ri.UID(), rep)
	return nil
}

func printSignatureReport(out io.Writer, name, uid string, rep store.SignatureReport) {
	fmt.Fprintf(out, "repo %s   upstream %s   tip %s\n", name, rep.Upstream, short8(rep.Tip))
	fmt.Fprintf(out, "mode: %s", rep.Mode)
	if rep.WouldAnchor != "" {
		fmt.Fprintf(out, "   anchor after this fold: %s", short8(rep.WouldAnchor))
	}
	if rep.Anchor != "" {
		fmt.Fprintf(out, "   current anchor: %s", short8(rep.Anchor))
	}
	if rep.Unrooted {
		fmt.Fprint(out, "   UNROOTED: configure [verify].operator_key")
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "commits %d: unsigned %d, non-SSH signature %d, bad SSH signature %d\n",
		rep.Commits, rep.Unsigned, rep.NotSSH, rep.BadSig)
	fmt.Fprintf(out, "signers (%d), in order first seen:\n", len(rep.Signers))
	for _, s := range rep.Signers {
		var tags []string
		if s.Listed {
			tags = append(tags, "listed")
		}
		if s.Operator {
			tags = append(tags, "OPERATOR")
		}
		fmt.Fprintf(out, "  %s  first %s on %s  %d commit(s)  claims %v  %s\n",
			s.Fingerprint, short8(s.FirstCommit), s.FirstDate.Format("2006-01-02"), s.Count, s.Claims, strings.Join(tags, " "))
		fmt.Fprintf(out, "      %s\n", s.Key)
	}
	for _, grp := range []struct {
		title string
		rs    []store.Refusal
	}{{"refused", rep.Refused}, {"reported", rep.Reported}} {
		if len(grp.rs) == 0 {
			continue
		}
		fmt.Fprintf(out, "%s (%d):\n", grp.title, len(grp.rs))
		for _, r := range grp.rs {
			fmt.Fprintf(out, "  %s  %s: %s\n", short8(r.Commit), r.Rule, r.Reason)
		}
	}
	if len(rep.Accepted) > 0 {
		fmt.Fprintf(out, "accepted on this instance, matched in this history (%d):\n", len(rep.Accepted))
		for _, x := range rep.Accepted {
			scope := "any repository"
			if x.RepoUID != "" {
				scope = "this repository"
				if x.RepoUID != uid {
					scope = "repository " + x.RepoUID
				}
			}
			fmt.Fprintf(out, "  %s  (%s)  %s\n", x.Commit, scope, x.Note)
		}
	}
}

func short8(h string) string {
	if len(h) > 8 {
		return h[:8]
	}
	return h
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
		if err := refuseUnwaivable(cmd, a.Manager(), ri, commit); err != nil {
			return err
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
	if repoName == "" {
		fmt.Fprintln(out, "unchecked: without --repo the commit could not be checked; it waives a signature or an unsigned merge failing M3 only, never a policy change or an author claim")
	}
	return nil
}

// refuseUnwaivable refuses an accept the fold would never honour: a commit the
// repository's own dry run already shows as an unauthorised policy change or
// an author-claim mismatch.
func refuseUnwaivable(cmd *cobra.Command, m *repos.Manager, ri *repos.RepoInstance, commit plumbing.Hash) error {
	var rep store.SignatureReport
	var rerr error
	exists := false
	if err := ri.WithRead(func(svc *store.Service) {
		exists = svc.CommitExists(commit)
		rep, rerr = svc.SignatureReport(cmd.Context(), upstreamOf(m, ri), plumbing.ZeroHash)
	}); err != nil {
		return err
	}
	if rerr != nil {
		return rerr
	}
	if !exists {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: %s is not in %s's objects yet; the waiver is recorded and applies when it arrives\n", commit, ri.Name())
	}
	return repos.CheckWaivable(rep, commit)
}

// verifyCICmd builds `knomit verify ci`: the forge-side check for a knowledge
// base's repository (the GitHub job that auto-merges agent branches). It runs
// the SAME fold over a plain git checkout, needs no knomit home and boots
// nothing, and blocks a candidate branch whose new commits are refused.
func verifyCICmd() *cobra.Command {
	var (
		dir, upstream, candidate, operatorKey string
		asJSON                                bool
	)
	cmd := &cobra.Command{
		Use:   "ci",
		Short: "Check a candidate branch in a git checkout before it is merged (F09 forge CI)",
		Long: `Folds the candidate's history from the root exactly as a fresh knomit clone
would, with the repository's own verify_signatures / verify_signers as the fold
computes them (never the candidate head's ontology file), and checks the
commits the candidate adds to the upstream.

  knomit verify ci --upstream origin/main --candidate "$TIP"

The operator key comes from --operator-key or KNOMIT_VERIFY_OPERATOR_KEY (a
forge variable set by an admin, never a file in the repository). Without it, a
candidate that carries a policy change cannot be judged and is blocked.

Limits: a CI job has no accept list, so a commit or merge that needs a waiver
is blocked here and left to the operator. It checks what reaches the job:
direct pushes that bypass the workflow, and repositories you do not run CI on,
are covered by each knomit instance's own verification, not by this.

Exit codes: 0 mergeable (or verification off), 1 blocked, 2 could not run.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			key := operatorKey
			if key == "" {
				key = os.Getenv("KNOMIT_VERIFY_OPERATOR_KEY")
			}
			root, err := store.NewStaticRoot(key)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
				os.Exit(exitFailed)
			}
			v, err := store.VerifyCheckout(dir, upstream, candidate, root, nil)
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
				os.Exit(exitFailed)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				_ = enc.Encode(v)
			} else {
				fmt.Fprintf(out, "%s onto %s: mode %s, %d new commit(s), %d refused, %d noted\n",
					candidate, upstream, v.Mode, v.New, len(v.Refused), len(v.Reported))
				for _, r := range v.Refused {
					fmt.Fprintf(out, "  REFUSED %s  %s: %s\n", short8(r.Commit), r.Rule, r.Reason)
				}
				for _, r := range v.Reported {
					fmt.Fprintf(out, "  noted   %s  %s: %s\n", short8(r.Commit), r.Rule, r.Reason)
				}
				if v.Unrooted {
					fmt.Fprintln(out, "  UNROOTED: a policy change needs the operator key (KNOMIT_VERIFY_OPERATOR_KEY)")
				}
			}
			if v.Blocked() {
				os.Exit(exitDirty)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "the git checkout")
	cmd.Flags().StringVar(&upstream, "upstream", "origin/main", "the upstream revision the candidate would merge into")
	cmd.Flags().StringVar(&candidate, "candidate", "HEAD", "the candidate revision (the agent branch tip)")
	cmd.Flags().StringVar(&operatorKey, "operator-key", "", "the operator's ssh-ed25519 public key (default $KNOMIT_VERIFY_OPERATOR_KEY)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the verdict as JSON")
	return cmd
}
