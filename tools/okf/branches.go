package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"gopkg.in/yaml.v3"

	"knomit/internal/version"
)

const branchesUsage = "usage: knomit-okf branches [--source <url>] [--no-fetch]"

// branchRow is one line of the branches table: a source branch, the output
// branch exporting it (if any), and how far apart they are.
type branchRow struct {
	source  string // source branch name; "" when an output branch's source is gone
	output  string // output branch name; "" when not exported yet
	current bool   // the output branch is the one checked out
	status  string
	detail  string // the exported commit, abbreviated
}

func runBranches(args []string, dir string, out io.Writer) error {
	fs := flag.NewFlagSet("branches", flag.ContinueOnError)
	fs.SetOutput(out)
	source := fs.String("source", "", "override the KB URL for this run")
	noFetch := fs.Bool("no-fetch", false, "list from what is already fetched, without contacting the remote")
	var auth authOpts
	registerAuthFlags(fs, &auth)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(branchesUsage)
	}

	repo, err := git.PlainOpen(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w (run knomit-okf clone first)", filepath.Clean(dir), err)
	}

	u := newUI(out)
	u.Banner(version.String())

	// Fetching by default is the point: "behind by N" computed against stale
	// refs would be worse than not reporting it at all. --no-fetch is for
	// offline use, and says so in the output.
	if *noFetch {
		u.Step("Reading", "local refs only (--no-fetch)")
		u.Skip("skipped fetch — counts may be stale")
	} else {
		cfg, cerr := readConfig(dir)
		if cerr != nil {
			return cerr
		}
		url, uerr := resolveSourceURL(repo, cfg, *source)
		if uerr != nil {
			return uerr
		}
		if err := auth.resolve(); err != nil {
			return err
		}
		am, aerr := authFor(url, auth)
		if aerr != nil {
			return aerr
		}
		u.Step("Fetching", redactURL(url))
		if err := fetchSource(repo, url, am); err != nil {
			return err
		}
		fetched, ferr := sourceBranches(repo)
		if ferr != nil {
			return ferr
		}
		u.Done(fmt.Sprintf("%d branch%s", len(fetched), pluralES(len(fetched))))
	}

	rows, err := collectBranchRows(repo)
	if err != nil {
		return err
	}
	renderBranches(u, out, rows)
	return nil
}

// collectBranchRows joins the fetched source branches against the local output
// branches. The join key is each output branch's COMMITTED .knomit-okf.yaml,
// not its name: the config is what actually records which source a bundle
// tracks, and reading it per ref means no branch has to be checked out.
func collectBranchRows(repo *git.Repository) ([]branchRow, error) {
	sources, err := sourceBranches(repo)
	if err != nil {
		return nil, err
	}

	// source branch -> the output branch exporting it, and its synced commit.
	type exported struct {
		output string
		synced string
	}
	bySource := map[string]exported{}
	var orphanedOutputs []branchRow

	iter, err := repo.Branches()
	if err != nil {
		return nil, err
	}
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		cfg, ok, cerr := readConfigAtRef(repo, ref.Hash())
		if cerr != nil || !ok || cfg.Branch == "" {
			return nil // not an okf output branch
		}
		bySource[cfg.Branch] = exported{output: ref.Name().Short(), synced: cfg.SyncedCommit}
		return nil
	})
	if err != nil {
		return nil, err
	}

	head, _ := repo.Head()
	currentBranch := ""
	if head != nil && head.Name().IsBranch() {
		currentBranch = head.Name().Short()
	}

	var rows []branchRow
	seen := map[string]bool{}
	for _, src := range sources {
		seen[src] = true
		row := branchRow{source: src}
		exp, ok := bySource[src]
		if !ok {
			row.status = "not exported"
			row.detail = "—"
			rows = append(rows, row)
			continue
		}
		row.output = exp.output
		row.current = exp.output == currentBranch
		row.detail = shortHex(exp.synced)
		row.status = compareToSource(repo, src, exp.synced)
		rows = append(rows, row)
	}

	// Output branches whose source branch no longer exists upstream.
	for src, exp := range bySource {
		if seen[src] {
			continue
		}
		orphanedOutputs = append(orphanedOutputs, branchRow{
			source:  src,
			output:  exp.output,
			current: exp.output == currentBranch,
			detail:  shortHex(exp.synced),
			status:  "source branch gone",
		})
	}
	sort.Slice(orphanedOutputs, func(i, j int) bool { return orphanedOutputs[i].source < orphanedOutputs[j].source })
	return append(rows, orphanedOutputs...), nil
}

// compareToSource describes how the exported bundle relates to the source
// branch's current head.
func compareToSource(repo *git.Repository, sourceBranch, syncedCommit string) string {
	head, err := repo.Reference(plumbing.ReferenceName(sourceRefPrefix+sourceBranch), true)
	if err != nil {
		return "source branch gone"
	}
	if syncedCommit == "" {
		return "never synced"
	}
	if syncedCommit == head.Hash().String() {
		return "up to date"
	}

	synced, err := object.GetCommit(repo.Storer, plumbing.NewHash(syncedCommit))
	if err != nil {
		// The recorded commit is not in this repo — the source history was
		// rewritten, or the bundle came from a different KB.
		return "unknown (exported commit missing)"
	}
	headCommit, err := object.GetCommit(repo.Storer, head.Hash())
	if err != nil {
		return "unknown"
	}
	isAncestor, err := synced.IsAncestor(headCommit)
	if err != nil {
		return "unknown"
	}
	if !isAncestor {
		// The source moved in a way that does not contain what was exported.
		return "diverged — re-sync rewrites the bundle"
	}
	n := countNewCommits(headCommit, synced)
	return fmt.Sprintf("%d commit%s behind", n, plural(n))
}

// countNewCommits counts commits reachable from head but NOT from stop.
//
// It excludes every ancestor of stop up front rather than merely halting the
// walk at stop. Halting is not enough on a branching history: from a merge, the
// walk reaches the second parent and then descends into commits that are also
// ancestors of stop by another path, counting them as new. On the smallest
// such shape — A←B, A←C, M(B,C), stop=B — that reports 3 instead of 2.
func countNewCommits(head, stop *object.Commit) int {
	reachableFromStop := map[plumbing.Hash]bool{}
	stopIter := object.NewCommitPreorderIter(stop, nil, nil)
	_ = stopIter.ForEach(func(c *object.Commit) error {
		reachableFromStop[c.Hash] = true
		return nil
	})

	// seenExternal commits are neither yielded nor traversed past, so the walk
	// yields exactly the commits head adds over stop.
	n := 0
	iter := object.NewCommitPreorderIter(head, reachableFromStop, nil)
	_ = iter.ForEach(func(*object.Commit) error {
		n++
		return nil
	})
	return n
}

// readConfigAtRef reads .knomit-okf.yaml from a commit's TREE, so a branch can
// be inspected without checking it out.
func readConfigAtRef(repo *git.Repository, at plumbing.Hash) (Config, bool, error) {
	commit, err := object.GetCommit(repo.Storer, at)
	if err != nil {
		return Config{}, false, nil
	}
	f, err := commit.File(configFile)
	if err != nil {
		return Config{}, false, nil // no config ⇒ not an okf bundle branch
	}
	raw, err := f.Contents()
	if err != nil {
		return Config{}, false, err
	}
	var c Config
	if err := yaml.Unmarshal([]byte(raw), &c); err != nil {
		return Config{}, false, nil // unparseable ⇒ treat as not-a-bundle
	}
	return c, true, nil
}

func shortHex(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	if s == "" {
		return "—"
	}
	return s
}

// renderBranches prints the table. Columns are sized to the content so the
// long agent/<host>-<id> branch names knomit generates stay aligned.
func renderBranches(u *ui, out io.Writer, rows []branchRow) {
	if len(rows) == 0 {
		fmt.Fprintf(out, "\n  %s\n\n", u.dim("no source branches fetched"))
		return
	}

	nameW := len("BRANCH")
	expW := len("EXPORTED")
	for _, r := range rows {
		if n := len(r.source); n > nameW {
			nameW = n
		}
		if n := len(r.detail); n > expW {
			expW = n
		}
	}

	fmt.Fprintf(out, "\n  %s  %s  %s\n",
		u.dim(pad("BRANCH", nameW+2)), u.dim(pad("EXPORTED", expW)), u.dim("STATUS"))

	for _, r := range rows {
		marker := "  "
		if r.current {
			marker = u.green("* ")
		}
		name := r.source
		if r.output != "" && r.output != r.source {
			name += " → " + r.output
		}
		status := r.status
		switch {
		case strings.HasPrefix(status, "up to date"):
			status = u.green(status)
		case strings.Contains(status, "diverged"), strings.Contains(status, "gone"):
			status = u.paint("33", status) // yellow: needs a decision
		case status == "not exported":
			status = u.dim(status)
		}
		fmt.Fprintf(out, "  %s%s  %s  %s\n", marker, pad(name, nameW), pad(r.detail, expW), status)
	}

	fmt.Fprintf(out, "\n  %s\n\n", u.dim("export one with:  knomit-okf sync -b <branch>"))
}

func pad(s string, w int) string {
	if n := w - len([]rune(s)); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
