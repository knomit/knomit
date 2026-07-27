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

	// Validate the credential flags on BOTH paths. --no-fetch needs none of
	// them, but a command line that contradicts itself is wrong whether or not
	// this run would have used it, and accepting it here trains the habit that
	// then fails on the fetching path.
	if err := auth.validate(); err != nil {
		return err
	}

	repo, dir, err := openExport(dir)
	if err != nil {
		return err
	}
	if !isExportRepo(repo, dir) {
		return fmt.Errorf("%s is a git repository but not a knomit-okf export (no %q remote, no %s)\n  hint: run knomit-okf clone <kb-url> <dir> to create one",
			filepath.Clean(dir), sourceRemote, configFile)
	}

	u := newUI(out)
	u.Banner(version.String())

	// Fetching by default is the point: "behind by N" computed against stale
	// refs would be worse than not reporting it at all. --no-fetch is for
	// offline use, and says so in the output.
	if *noFetch {
		u.Step("Reading", "local refs only (--no-fetch)")
		u.Skip("skipped fetch — counts may be stale")
		if auth.specified() {
			u.Note("credential flags ignored — --no-fetch contacts no remote")
		}
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
		u.Step("Fetching", safeURL(url))
		if err := fetchSource(repo, url, am); err != nil {
			return explainFetchError(err, url, auth)
		}
		fetched, ferr := sourceBranches(repo)
		if ferr != nil {
			return ferr
		}
		u.Done(fmt.Sprintf("%d branch%s", len(fetched), pluralES(len(fetched))))
	}

	// Announced, because comparing walks each exported branch's history against
	// its source and that is the one stage here that can take real time on a
	// large base. A silent wait is the shape of a hang.
	u.Step("Comparing", "exported bundles against their source branches")
	// This walks history and reads a config blob out of every output branch's
	// tree, so it loses the same race an export does — and it is the command
	// most likely to be run seconds after the git commit that starts a repack.
	var rows []branchRow
	var recovered bool
	err = retryAfterRepack(repo,
		// A flag, not a Note: this runs INSIDE the open Comparing stage, and a
		// Note closes whatever stage is open — which would delete the very line
		// this stage exists to print. Pinned by
		// TestUI_ANoteInsideAnOpenStageTakesItsResultWithIt.
		func() { recovered = true },
		func() error {
			var e error
			rows, e = collectBranchRows(repo)
			return e
		})
	if err != nil {
		return err
	}
	u.Done(fmt.Sprintf("%d branch%s", len(rows), pluralES(len(rows))))
	// Not repackedRerunNote: the comparison ran twice inside one stage, so
	// nothing was printed twice and claiming otherwise would send a reader
	// looking for output that is not there.
	noteIf(u, recovered)

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
		// rivals are the OTHER output branches claiming the same source, held
		// so the collision is reported rather than resolved by map-iteration
		// order. Two branches claim one source after a `git branch -c` of an
		// output branch; picking a winner silently would make the table say
		// something different on each run.
		rivals []string
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
		name := ref.Name().Short()
		prev, claimed := bySource[cfg.Branch]
		if !claimed {
			bySource[cfg.Branch] = exported{output: name, synced: cfg.SyncedCommit}
			return nil
		}
		// Keep the lexicographically first as the representative so the row is
		// stable across runs, and record every claimant.
		e := prev
		e.rivals = append(e.rivals, prev.output, name)
		if name < prev.output {
			e.output, e.synced = name, cfg.SyncedCommit
		}
		bySource[cfg.Branch] = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Deduplicate and order the rival lists: they accumulated pairwise above.
	for src, e := range bySource {
		if len(e.rivals) == 0 {
			continue
		}
		uniq := map[string]bool{}
		var names []string
		for _, n := range e.rivals {
			if !uniq[n] {
				uniq[n] = true
				names = append(names, n)
			}
		}
		sort.Strings(names)
		e.rivals = names
		bySource[src] = e
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
		if len(exp.rivals) > 1 {
			// Surfaced, not resolved: which bundle is authoritative is the
			// user's call, and `sync -b` would overwrite only one of them.
			row.status = fmt.Sprintf("exported by %d branches (%s) — %s",
				len(exp.rivals), strings.Join(exp.rivals, ", "), row.status)
		}
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
	n, exact := countNewCommits(headCommit, synced, maxBehindCount)
	if !exact {
		// A bound reported as a plain number would be a lie the user cannot see.
		// "N+" says what was actually established: at least this many, and
		// counting stopped there.
		return fmt.Sprintf("%d+ commits behind", n)
	}
	return fmt.Sprintf("%d commit%s behind", n, plural(n))
}

// maxBehindCount bounds the counting walk. The exact number stops being
// actionable long before this — past a few dozen the answer is "re-sync" either
// way — and an unbounded walk over years of history is the one place this
// command can sit silently for a long time.
const maxBehindCount = 1000

// countNewCommits counts commits reachable from head but NOT from stop, up to
// limit. exact reports whether the count finished; when false, n is limit and
// is a true LOWER bound, which is why the caller may render it as "N+".
//
// It excludes every ancestor of stop up front rather than merely halting the
// walk at stop. Halting is not enough on a branching history: from a merge, the
// walk reaches the second parent and then descends into commits that are also
// ancestors of stop by another path, counting them as new. On the smallest
// such shape — A←B, A←C, M(B,C), stop=B — that reports 3 instead of 2.
//
// Only the COUNTING walk is bounded. The exclusion set must stay complete —
// truncating it would let ancestors of stop be counted as new, turning "N+"
// into an over-count rather than a lower bound — but it loads commit objects
// only, no trees, which is what makes it affordable to walk in full.
func countNewCommits(head, stop *object.Commit, limit int) (n int, exact bool) {
	reachableFromStop := map[plumbing.Hash]bool{}
	stopIter := object.NewCommitPreorderIter(stop, nil, nil)
	_ = stopIter.ForEach(func(c *object.Commit) error {
		reachableFromStop[c.Hash] = true
		return nil
	})

	// Commits in the ignore set are neither yielded nor traversed past, so the
	// walk yields exactly the commits head adds over stop.
	exact = true
	iter := object.NewCommitPreorderIter(head, reachableFromStop, nil)
	_ = iter.ForEach(func(*object.Commit) error {
		if n >= limit {
			exact = false
			return object.ErrCanceled
		}
		n++
		return nil
	})
	return n, exact
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

	// Size the columns over the RENDERED name, not r.source: a row whose output
	// branch differs prints "source → output", which is wider and would push
	// STATUS out of alignment if the column were sized over the source alone.
	// Width is counted in runes throughout — knomit branch names carry the "→"
	// separator, and byte length would over-pad every row containing one.
	names := make([]string, len(rows))
	nameW := runeLen("BRANCH")
	expW := runeLen("EXPORTED")
	for i, r := range rows {
		names[i] = r.source
		if r.output != "" && r.output != r.source {
			names[i] += " → " + r.output
		}
		if n := runeLen(names[i]); n > nameW {
			nameW = n
		}
		if n := runeLen(r.detail); n > expW {
			expW = n
		}
	}

	fmt.Fprintf(out, "\n  %s  %s  %s\n",
		u.dim(pad("BRANCH", nameW+2)), u.dim(pad("EXPORTED", expW)), u.dim("STATUS"))

	for i, r := range rows {
		marker := "  "
		if r.current {
			marker = u.green("* ")
		}
		name := names[i]
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

// runeLen is a column width in printable characters. Branch names and the "→"
// separator are multi-byte, so byte length would over-pad and skew the table.
func runeLen(s string) int { return len([]rune(s)) }

func pad(s string, w int) string {
	if n := w - runeLen(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
