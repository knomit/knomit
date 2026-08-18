package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"knomit/internal/app"
	"knomit/internal/config"
	"knomit/internal/repos"
	"knomit/internal/store"
)

// Exit codes. Kept as named constants because the difference between 1 and 2 is
// the whole contract: a script that treats "the tool could not run" as "the
// repo is damaged" pages someone at 3am for a typo in a flag.
const (
	exitClean  = 0
	exitDirty  = 1
	exitFailed = 2
)

// verifyCmd builds the `knomit verify` subcommand. It boots the full app to
// open all configured repos through the usual manager path, then runs
// Verify on one or all repos and prints the report.
func verifyCmd() *cobra.Command {
	var (
		repoName    string
		all         bool
		deep        bool
		asJSON      bool
		maxIssues   int
		allBranches bool
		pruneRefs   bool
	)
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Run an integrity check on one or all knomit repositories",
		Long: `Walks the git object chain, SQLite tables, and search index for the
specified repo (or all active repos with --all) and reports any structural
issues.

Verify is read-only and it is a SNAPSHOT: it takes the read lock on every
branch it checks up front and holds all of them until it finishes. That is
what makes the report internally consistent, and it means the run BLOCKS
EVERY WRITER on those branches for its whole duration. Do not fire it at a
busy agent expecting a cheap probe — budget for it, or stop the agent.

Only branches the index maintains are parity-checked. Other refs under
refs/heads/* — another machine's agent branch arriving by fetch, or refs
left by a removed feature — have no SQLite rows by design; they are listed
as "not indexed" rather than reported as thousands of errors. Pass
--all-branches to check every ref anyway.

--all covers ACTIVE repos. An archived repo is not opened and cannot be
verified; the run names any it skipped.

Exit codes:
  0   clean (no errors; warnings do not fail the run)
  1   integrity errors found
  2   verify itself could not run (bad flags, unknown repo, boot failure)`,
		// The command reports findings; a non-clean repo is not a misuse of the
		// CLI, so cobra must not print the usage block after the report.
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			code, err := runVerify(cmd, verifyOpts{
				repoName:    repoName,
				all:         all,
				allBranches: allBranches,
				deep:        deep,
				asJSON:      asJSON,
				maxIssues:   maxIssues,
				pruneRefs:   pruneRefs,
			})
			// os.Exit skips deferred calls, so it must not run until runVerify
			// has returned and its own defers — app.Close among them — are done.
			// Calling it inside runVerify used to leak every open repo on any
			// non-clean run.
			if err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "Error:", err)
			}
			if code != exitClean {
				os.Exit(code)
			}
			return nil
		},
	}
	// A malformed flag is "verify could not run", which the help documents as
	// exit 2. Cobra returns parse errors up to ExecuteContext, where main.go
	// prints them and exits 1 — so without this, `verify --max-issues=abc`
	// exits 1 and a CI job reads a typo as a damaged corpus. Exactly the page
	// at 3am the exit codes exist to prevent.
	cmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		fmt.Fprintln(c.ErrOrStderr(), "Error:", err)
		fmt.Fprintf(c.ErrOrStderr(), "Run '%s --help' for usage.\n", c.CommandPath())
		os.Exit(exitFailed)
		return err
	})
	cmd.Flags().StringVar(&repoName, "repo", "", "repo name (required unless --all)")
	cmd.Flags().BoolVar(&all, "all", false, "verify all active repos")
	cmd.Flags().BoolVar(&deep, "deep", false, "enable deep checks (parses every fact)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report(s) as JSON instead of text")
	cmd.Flags().IntVar(&maxIssues, "max-issues", 100,
		"maximum issues to print per repo in TEXT output (0 = all); the per-category summary is "+
			"always complete, and --json is never truncated")
	cmd.Flags().BoolVar(&allBranches, "all-branches", false,
		"parity-check every refs/heads/* ref, not only the branches the index maintains")
	cmd.Flags().BoolVar(&pruneRefs, "prune-generated-refs", false,
		"DELETE the generated okf/* refs left by the removed server-side export, and their markers")
	return cmd
}

type verifyOpts struct {
	repoName    string
	all         bool
	allBranches bool
	deep        bool
	asJSON      bool
	maxIssues   int
	pruneRefs   bool
}

// jsonReport is the --json shape. Separate from store.IntegrityReport so the
// wire format is owned here and does not shift when the store struct changes.
type jsonReport struct {
	Repo      string         `json:"repo"`
	CheckedAt string         `json:"checked_at"`
	Branches  []string       `json:"branches_checked"`
	Skipped   []string       `json:"branches_not_indexed"`
	Clean     bool           `json:"clean"`
	Counts    map[string]int `json:"counts_by_category"`
	Issues    []jsonIssue    `json:"issues"`
	Pruned    []string       `json:"pruned_refs,omitempty"`
	Markers   int64          `json:"pruned_markers,omitempty"`
}

// emptyIfNil keeps a nil slice from marshalling as JSON null. Consumers branch
// on these arrays being empty; `null` makes that a type error instead.
func emptyIfNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

type jsonIssue struct {
	Severity string `json:"severity"`
	Category string `json:"category"`
	Branch   string `json:"branch,omitempty"`
	Path     string `json:"path,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Detail   string `json:"detail"`
}

// runVerify owns the app's lifetime so every defer runs before the caller
// exits. It returns the process exit code and, separately, an error worth
// printing — a repo with integrity errors is a result (code 1), not an error.
func runVerify(cmd *cobra.Command, opts verifyOpts) (int, error) {
	// Checked BEFORE app.New: booting the app opens every repo and launches
	// index heals and startup reconciles, all of it wasted when the invocation
	// was never valid to begin with.
	if !opts.all && opts.repoName == "" {
		// No default repo exists to fall back on, so name one or ask for all.
		return exitFailed, fmt.Errorf("--repo is required (or pass --all to verify every repo)")
	}
	cfg, err := config.Load()
	if err != nil {
		return exitFailed, fmt.Errorf("load config: %w", err)
	}

	// cmd.Context(), not context.Background(): main.go installs a SIGINT/SIGTERM
	// handler that cancels this context, and Verify checks it between branches.
	// With Background the signal wiring existed and could not stop anything.
	ctx := cmd.Context()

	a, err := app.New(ctx, cfg, app.Options{})
	if err != nil {
		return exitFailed, fmt.Errorf("init app: %w", err)
	}
	defer a.Close()

	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	vopts := store.VerifyOpts{Deep: opts.deep, AllBranches: opts.allBranches}

	names := []string{opts.repoName}
	if opts.all {
		names = a.Manager().Names()
		sort.Strings(names)
		reportSkippedArchived(errOut, a.Manager())
	}

	var (
		anyDirty bool
		anyErr   bool
	)
	// Non-nil so a home with no active repos encodes as [] rather than the bare
	// `null` document a nil slice produces.
	payload := make([]jsonReport, 0, len(names))
	for _, name := range names {
		ri := a.Manager().Get(name)
		if ri == nil {
			fmt.Fprintf(errOut, "repo %q not found\n", name)
			anyErr = true
			continue
		}
		// The prune runs BEFORE the report, so the report describes the repo as
		// it stands afterwards. Running it after meant a single invocation
		// printed "remove with --prune-generated-refs" and "pruned generated
		// ref: okf/main" within three lines of each other, and left the pruned
		// refs listed under branches_not_indexed in the JSON.
		var pruned store.PruneResult
		if opts.pruneRefs {
			// pruned is reported even on error: PruneResult.Refs holds only what
			// was actually deleted, so a partial failure still has to say which
			// refs are already gone.
			var perr error
			if pruned, perr = ri.PruneGeneratedRefs(ctx); perr != nil {
				fmt.Fprintf(errOut, "prune %q: %v\n", name, perr)
				anyErr = true
			}
			if !opts.asJSON && !pruned.Empty() {
				for _, ref := range pruned.Refs {
					fmt.Fprintf(out, "pruned generated ref: %s\n", ref)
				}
				if pruned.Markers > 0 {
					fmt.Fprintf(out, "pruned %d okf marker row(s)\n", pruned.Markers)
				}
			}
		}

		report, err := ri.Verify(ctx, vopts)
		if err != nil {
			fmt.Fprintf(errOut, "verify %q: %v\n", name, err)
			anyErr = true
			// A cancelled context will not un-cancel for the next repo, so
			// carrying on just prints one "context canceled" per remaining
			// repo. One Ctrl-C should stop the run, not be re-attempted.
			if ctx.Err() != nil {
				break
			}
			continue
		}

		if opts.asJSON {
			payload = append(payload, toJSON(report, pruned))
		} else {
			fmt.Fprint(out, report.Format(opts.maxIssues))
		}
		if !report.IsClean() {
			anyDirty = true
		}
	}

	if opts.asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(payload); err != nil {
			return exitFailed, fmt.Errorf("encode json: %w", err)
		}
	}

	switch {
	case anyErr:
		return exitFailed, nil
	case anyDirty:
		return exitDirty, nil
	}
	return exitClean, nil
}

// reportSkippedArchived names the archived repos --all did not cover.
//
// Manager.Names() returns active repos only, so --all has always silently meant
// "all active repos". Archived ones are not opened and so genuinely cannot be
// verified — but an archived knowledge base is exactly the kind nobody notices
// rotting, and "verify --all said nothing about it" is the wrong way to find
// that out. State the omission instead of leaving it to be inferred.
func reportSkippedArchived(w io.Writer, m *repos.Manager) {
	archived, err := m.ListArchived()
	if err != nil {
		fmt.Fprintf(w, "note: could not list archived repos: %v\n", err)
		return
	}
	if len(archived) == 0 {
		return
	}
	names := make([]string, 0, len(archived))
	for _, a := range archived {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	fmt.Fprintf(w, "note: %d archived repo(s) not verified (--all covers active repos only): %s\n",
		len(names), strings.Join(names, ", "))
}

// toJSON converts a report to the wire shape.
//
// Every slice is initialised, never left nil: a nil slice marshals to `null`,
// not `[]`, and a consumer iterating report.issues would hit a type error on a
// CLEAN repo — the exact case that should be easiest to handle.
//
// maxIssues is deliberately NOT applied here. Truncating machine-readable
// output would hand a CI job a report that looks complete and is not; the flag
// is documented as affecting the text rendering only.
func toJSON(r store.IntegrityReport, pruned store.PruneResult) jsonReport {
	out := jsonReport{
		Repo:      r.Repo,
		CheckedAt: r.CheckedAt.UTC().Format("2006-01-02T15:04:05Z"),
		Branches:  emptyIfNil(r.Branches),
		Skipped:   emptyIfNil(r.Skipped),
		Clean:     r.IsClean(),
		Counts:    r.CountsByCategory(),
		Pruned:    pruned.Refs,
		Markers:   pruned.Markers,
	}
	out.Issues = make([]jsonIssue, 0, len(r.Issues))
	for _, i := range r.Issues {
		out.Issues = append(out.Issues, jsonIssue{
			Severity: i.Severity.String(),
			Category: i.Category,
			Branch:   i.Branch,
			Path:     i.Path,
			Commit:   i.Commit,
			Detail:   i.Detail,
		})
	}
	return out
}
