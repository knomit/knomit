// Verify reports structural integrity issues across the store: git object
// reachability, parity between SQLite tables and git/tree state, database-level
// soundness, and (with Deep) fact format.
//
// # WHICH BRANCHES ARE CHECKED
//
// Parity checks run ONLY for branches the index actually maintains, which is
// the set carrying a meta.last_commit:<branch> row. That is narrower than
// refs/heads/*, deliberately: repoBuilder.setupIndex maintains the agent branch
// plus upstreamMain and nothing else, so every other local ref — another
// machine's agent branch arriving by fetch, or a generated ref left by a
// removed feature — has no SQLite rows and never will. Demanding parity for
// those refs is demanding an invariant the system never promised; doing it
// produced 8128 errors on a healthy five-repo home, every one of them false.
// Unmaintained refs are reported once each, as warnings, so they stay visible
// without failing the run.
//
// # LOCKING
//
// Verify is read-only, and it is a SNAPSHOT. It acquires the read lock on every
// branch it is about to check UP FRONT and holds all of them for the whole run,
// so no writer can advance a ref or mutate branch_facts mid-report and no torn
// state can appear. The cost is the other side of that coin: for its whole
// duration Verify BLOCKS EVERY WRITER on every branch it covers, and a
// concurrent index Rebuild (which takes the write side of the same per-branch
// RWMutex) blocks Verify. It is not a cheap probe to fire at a live agent —
// budget for it, or take the repo offline.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"knomit/internal/fact"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Severity classifies an integrity issue.
type Severity int

const (
	SeverityError Severity = iota
	SeverityWarning
)

func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "ERROR"
	case SeverityWarning:
		return "WARN"
	default:
		return "?"
	}
}

// VerifyOpts controls which checks Verify runs.
type VerifyOpts struct {
	// Deep enables the fact-format check (parses every .md as a Fact).
	// Slow on large repos; off by default.
	Deep bool
	// AllBranches runs the parity checks against every refs/heads/* ref rather
	// than only the branches the index maintains. Off by default, and off is
	// the answer you want: see the package doc. It exists so a developer
	// chasing "why does this branch have no rows" can ask for the old
	// behaviour deliberately, and so tests can pin it.
	AllBranches bool
}

// Canonical issue category strings. Every check MUST use exactly these
// constants when populating IntegrityIssue.Category — no string literals at the
// call sites.
const (
	CategoryGitReachability    = "git-reachability"
	CategoryCommitLog          = "commit-log"
	CategoryCommitParents      = "commit-parents"
	CategoryFactsCoherence     = "facts-coherence"
	CategoryEmbeddingsCoverage = "embeddings-coverage"
	CategoryEmbeddingIdentity  = "embedding-identity"
	CategoryBranchesTable      = "branches-table"
	CategoryFactFormat         = "fact-format"
	CategoryGraphCoherence     = "graph-coherence"
	CategoryDerivedTables      = "derived-tables"
	CategoryDatabase           = "database"
	CategorySchemaVersion      = "schema-version"
	CategoryOrphanObjects      = "orphan-objects"
	CategoryGeneratedRefs      = "generated-refs"
)

// IntegrityIssue is a single finding from Verify.
type IntegrityIssue struct {
	Severity Severity
	Category string // one of the Category* constants above
	Branch   string // "" if not branch-scoped
	Path     string // "" if not path-scoped
	Commit   string // "" if not commit-scoped
	Detail   string // human-readable
}

// IntegrityReport collects all issues from a Verify run.
type IntegrityReport struct {
	// Repo is populated by the RepoInstance.Verify wrapper from the
	// repository's registered name. Service.Verify itself leaves it empty
	// because Service has no notion of a logical repo name — only the
	// containing manager does.
	Repo      string
	CheckedAt time.Time
	// Branches are the branches whose parity was CHECKED — the maintained set.
	Branches []string
	// Skipped are refs/heads/* refs that exist but carry no index, and so were
	// not parity-checked. Reported rather than silently dropped: "verify found
	// nothing wrong" must not be able to mean "verify looked at nothing".
	Skipped []string
	Issues  []IntegrityIssue
}

// CountsByCategory returns issue counts keyed by category, for a summary that
// stays readable when the detail runs to thousands of lines.
func (r IntegrityReport) CountsByCategory() map[string]int {
	out := make(map[string]int, len(r.Issues))
	for _, i := range r.Issues {
		out[i.Category]++
	}
	return out
}

// IsClean returns true iff there are no Error-severity issues.
// Warnings (e.g. malformed fact YAML) do not affect cleanliness.
func (r IntegrityReport) IsClean() bool {
	for _, i := range r.Issues {
		if i.Severity == SeverityError {
			return false
		}
	}
	return true
}

// IsStrictlyClean returns true iff there are no issues at all (no errors, no warnings).
func (r IntegrityReport) IsStrictlyClean() bool { return len(r.Issues) == 0 }

// String formats the report as a multi-line human-readable summary, printing
// every issue. Equivalent to Format(0).
func (r IntegrityReport) String() string { return r.Format(0) }

// Format renders the report, listing at most maxIssues findings (0 = all). The
// per-category summary is always printed in full, so a truncated report still
// says how much it found — a raw tail of 1600 identical lines tells an operator
// less than six counts do.
func (r IntegrityReport) Format(maxIssues int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Verify report for %q at %s\n", r.Repo, r.CheckedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "  Branches checked: %s\n", strings.Join(r.Branches, ", "))
	if len(r.Skipped) > 0 {
		fmt.Fprintf(&b, "  Not indexed (parity not checked): %s\n", strings.Join(r.Skipped, ", "))
	}
	if r.IsStrictlyClean() {
		b.WriteString("  Status: CLEAN\n")
		return b.String()
	}

	counts := r.CountsByCategory()
	cats := make([]string, 0, len(counts))
	for c := range counts {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	errs := 0
	for _, i := range r.Issues {
		if i.Severity == SeverityError {
			errs++
		}
	}
	fmt.Fprintf(&b, "  Issues: %d (%d error, %d warning)\n", len(r.Issues), errs, len(r.Issues)-errs)
	for _, c := range cats {
		fmt.Fprintf(&b, "    %-20s %d\n", c, counts[c])
	}

	shown := r.Issues
	if maxIssues > 0 && len(shown) > maxIssues {
		shown = shown[:maxIssues]
	}
	for _, i := range shown {
		fmt.Fprintf(&b, "    [%s] %s", i.Severity, i.Category)
		if i.Branch != "" {
			fmt.Fprintf(&b, " branch=%s", i.Branch)
		}
		if i.Path != "" {
			fmt.Fprintf(&b, " path=%s", i.Path)
		}
		if i.Commit != "" {
			fmt.Fprintf(&b, " commit=%s", i.Commit)
		}
		fmt.Fprintf(&b, " — %s\n", i.Detail)
	}
	if len(shown) < len(r.Issues) {
		fmt.Fprintf(&b, "    … %d more (use --max-issues 0 for all)\n", len(r.Issues)-len(shown))
	}
	return b.String()
}

// Verify walks the store and returns an integrity report. See package doc.
func (s *Service) Verify(ctx context.Context, opts VerifyOpts) (IntegrityReport, error) {
	report := IntegrityReport{CheckedAt: time.Now()}

	// Enumerate branches via git refs (storer-level), since the branches
	// SQLite table is one of the things we're going to verify.
	allBranches, err := s.listBranchRefsForVerify(ctx)
	if err != nil {
		return report, fmt.Errorf("verify: list branches: %w", err)
	}
	sort.Strings(allBranches)

	// Split into the branches the index maintains and the rest. Parity is only
	// an invariant for the former — see the package doc.
	checked, skipped := allBranches, []string(nil)
	if !opts.AllBranches {
		indexed, ierr := s.indexedBranches(ctx)
		if ierr != nil {
			return report, fmt.Errorf("verify: list indexed branches: %w", ierr)
		}
		checked, skipped = nil, nil
		for _, br := range allBranches {
			if indexed[br] {
				checked = append(checked, br)
			} else {
				skipped = append(skipped, br)
			}
		}
	}
	report.Branches = checked
	report.Skipped = skipped

	// Acquire the per-branch read lock on every branch we're about to
	// check and hold it for the whole Verify run. This gives us a
	// consistent snapshot: no writer can advance a ref, mutate
	// branch_facts, or rewrite graph nodes while any check is running,
	// so torn mid-write states (e.g. HEAD advanced but branch_facts not
	// yet updated) cannot appear in the report. Writers serialize on the
	// write side of the same RWMutex via lockBranch, so they block until
	// Verify completes. This is the read-locking behavior promised in
	// the package doc.
	//
	// Locked over `checked` only. A skipped branch is not read by any check
	// below, and locking it would extend the writer stall to refs this run
	// never looks at.
	for _, br := range checked {
		unlock := s.rh.lockBranchRead(br)
		defer unlock()
	}

	// Read once, compared per branch below. Inside the lock window, so it is
	// one query rather than one per commit on every branch.
	//
	// On failure the comparison is SKIPPED, not run against the empty map. An
	// unreadable commit_parents makes every non-root commit look like it lost
	// its parent edges, so a single transient query error would manufacture one
	// ERROR per commit — thousands on a real branch, and a permanent exit 1.
	// That is the exact failure this whole change removes; it must not come
	// back through the error path.
	commitParents, cpErr := s.loadCommitParents(ctx)
	if cpErr != nil {
		report.Issues = append(report.Issues, IntegrityIssue{
			Severity: SeverityError, Category: CategoryCommitParents,
			Detail: fmt.Sprintf("read commit_parents: %v (per-commit comparison skipped)", cpErr),
		})
	}

	// Per-category checks. Each check runs against the frozen snapshot
	// held by the read locks above.
	for _, br := range checked {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		report.Issues = append(report.Issues, s.checkGitReachability(ctx, br)...)
		report.Issues = append(report.Issues, s.checkCommitLogParity(ctx, br)...)
		if cpErr == nil {
			report.Issues = append(report.Issues, s.checkCommitParents(ctx, br, commitParents)...)
		}
		report.Issues = append(report.Issues, s.checkFactsCoherence(ctx, br)...)
		if opts.Deep {
			report.Issues = append(report.Issues, s.checkFactFormat(ctx, br)...)
		}
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	report.Issues = append(report.Issues, s.checkBranchesTable(ctx, checked, allBranches)...)
	report.Issues = append(report.Issues, s.checkCommitLogOrphans(ctx)...)
	report.Issues = append(report.Issues, s.checkEmbeddingsCoverage(ctx)...)
	report.Issues = append(report.Issues, s.checkEmbeddingIdentity(ctx)...)
	report.Issues = append(report.Issues, s.checkGraphCoherence(ctx)...)
	report.Issues = append(report.Issues, s.checkDerivedTables(ctx)...)
	report.Issues = append(report.Issues, s.checkDatabase(ctx)...)
	report.Issues = append(report.Issues, s.checkSchemaVersion(ctx)...)
	report.Issues = append(report.Issues, s.checkOrphanObjects(ctx)...)
	report.Issues = append(report.Issues, s.reportUnindexedBranches(allBranches)...)

	sortIssues(report.Issues)
	return report, nil
}

// sortIssues puts the report in a stable order. Several checks iterate Go maps,
// so without this two runs over an unchanged repo emit the same findings in
// different orders and the reports cannot be diffed.
// Severity leads the ordering, not category. Format truncates the flat slice at
// maxIssues, so ordering by category first lets one alphabetically-early
// category of warnings eat the whole budget and hide every error behind it —
// schema-version sorts last of all categories, so a DIRTY migration would be
// guaranteed invisible on any repo with 100 earlier findings. Errors first
// means truncation can only ever drop the least severe findings.
func sortIssues(issues []IntegrityIssue) {
	sort.SliceStable(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		if a.Severity != b.Severity {
			return a.Severity < b.Severity // SeverityError == 0, so errors lead
		}
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		if a.Branch != b.Branch {
			return a.Branch < b.Branch
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Commit != b.Commit {
			return a.Commit < b.Commit
		}
		return a.Detail < b.Detail
	})
}

// indexedBranches returns the set of branches whose SQLite parity is an
// invariant — the ones the index maintains.
//
// Three sources, unioned, because no single one is complete:
//
//  1. meta.last_commit:<branch>, written by the index every time it syncs a
//     branch. This is the primary signal and the only one that covers the
//     upstream branch. Deliberately NOT meta.graph_schema_version:<branch>,
//     which looks like the same signal and is not: migration 000016 backfilled
//     the old global schema-version row onto every branch known at the time, so
//     an unindexed branch can carry one. On a live home that mistake reads
//     another machine's never-indexed agent branch as maintained and then
//     reports all 1366 of its commits as missing from branch_commits.
//
//  2. HEAD's target. A freshly created repo has NO meta rows at all — the first
//     last_commit row appears only after the first write — so source 1 alone
//     returns the empty set for a new repo and verify would check nothing and
//     report CLEAN. That silent skip is the same class of bug as the false
//     positives this function exists to remove, pointing the other way.
//
//  3. meta.agent_branch_owner, when set: the branch this database records as
//     the one it writes. Empty on a database that has never completed a boot,
//     which is why it cannot be the only source either.
//
// What the union deliberately does NOT include: any other refs/heads/* ref.
// Another machine's agent branch arriving by fetch has a `branches` row and no
// index, so the `branches` table is not usable as a fourth source.
func (s *Service) indexedBranches(ctx context.Context) (map[string]bool, error) {
	const prefix = "last_commit:"
	rows, err := s.rh.gits.DB().QueryContext(ctx,
		`SELECT key FROM meta WHERE key LIKE ? || '%'`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		if br, ok := strings.CutPrefix(key, prefix); ok && br != "" {
			out[br] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Errors here are not fatal: a detached HEAD or an unstamped database is a
	// legitimate state, and source 1 still stands on its own.
	if head, herr := s.rh.DefaultBranch(ctx); herr == nil && head != "" {
		out[head] = true
	}
	if owner, oerr := s.rh.AgentBranchOwner(ctx); oerr == nil && owner != "" {
		out[owner] = true
	}
	return out, nil
}

// reportUnindexedBranches raises an issue only for GENERATED refs, not for
// every unchecked branch.
//
// It is fed ALL branch names, not just the skipped ones, so that
// --all-branches cannot suppress it. Under that flag generated refs move into
// the checked set and produce hard parity errors; without this the one message
// naming --prune-generated-refs would vanish exactly when an operator went
// looking for why those refs have no rows.
//
// The transparency requirement — "CLEAN" must never quietly mean "nothing was
// examined" — is met by IntegrityReport.Skipped, which Format always prints.
// An unindexed branch is the normal, healthy state for a local `main` with no
// origin and for another machine's agent branch arriving by fetch, so raising a
// warning per branch would put a permanent warning on ordinary repos. A check
// that cries wolf on healthy input is the defect this whole pass exists to
// remove; it would be an odd thing to reintroduce in the fix.
//
// Generated okf/* refs are different in kind: residue from the server-side OKF
// export removed in bf9becbe, which nothing creates any more and which has an
// actual repair. Those get a warning naming it.
func (s *Service) reportUnindexedBranches(skipped []string) []IntegrityIssue {
	var issues []IntegrityIssue
	for _, br := range skipped {
		if !isGeneratedRef(br) {
			continue
		}
		issues = append(issues, IntegrityIssue{
			Severity: SeverityWarning, Category: CategoryGeneratedRefs, Branch: br,
			Detail: "generated ref left by the removed server-side OKF export; " +
				"nothing creates these now — remove with --prune-generated-refs",
		})
	}
	return issues
}

// generatedRefPrefix is the branch-name prefix the removed server-side OKF
// export wrote under refs/heads/. Nothing creates these any more (the producer
// went in bf9becbe, and TestVerify_NoGeneratedRefs pins that), so a ref matching
// it on a live home is residue from before that commit.
//
// Deliberately narrow. knomit-okf's own fetched source history lives at
// refs/knomit-okf/source/*, OUTSIDE refs/heads, and is live, deliberate and not
// matched here — see kb/invariants/okf/source-refs-stay-local.
const generatedRefPrefix = "okf/"

// isGeneratedRef reports whether a BRANCH NAME (no refs/heads/ prefix) is
// export residue.
func isGeneratedRef(branch string) bool {
	return strings.HasPrefix(branch, generatedRefPrefix)
}

// PruneResult reports what a prune actually removed — not what it considered.
type PruneResult struct {
	// Refs are the branch names whose refs were deleted, sorted. On a partial
	// failure this holds the ones that were already gone, so a caller can print
	// it without claiming a still-present ref was removed.
	Refs []string
	// Markers is the number of okf:marker:* kv rows deleted. Separate from Refs
	// because markers are keyed by source branch and can outlive every ref.
	Markers int64
}

// Empty reports whether the prune removed nothing at all.
func (p PruneResult) Empty() bool { return len(p.Refs) == 0 && p.Markers == 0 }

// PruneGeneratedRefs deletes the generated okf/* branch refs and their
// okf:marker:* bookkeeping rows, reporting what it removed. It is the
// repair for the residue reportUnindexedBranches warns about.
//
// Not called by Verify. Verify stays read-only, and this runs only when the
// operator asks for it by name (`--prune-generated-refs`), because it deletes
// git refs and there is no undo short of a reflog knomit does not keep.
//
// Scope is exactly the refs isGeneratedRef matches under refs/heads/*, so it
// cannot touch a real branch, and cannot touch refs/knomit-okf/source/*.
func (s *Service) PruneGeneratedRefs(ctx context.Context) (PruneResult, error) {
	var out PruneResult

	iter, err := s.rh.gits.IterReferences()
	if err != nil {
		return out, fmt.Errorf("prune: iterate refs: %w", err)
	}
	var targets []plumbing.ReferenceName
	if err := iter.ForEach(func(ref *plumbing.Reference) error {
		if name, ok := strings.CutPrefix(ref.Name().String(), "refs/heads/"); ok && isGeneratedRef(name) {
			targets = append(targets, ref.Name())
		}
		return nil
	}); err != nil {
		return out, fmt.Errorf("prune: collect refs: %w", err)
	}

	for _, ref := range targets {
		// Under the same per-branch write lock a normal ref update takes, so a
		// concurrent read of that branch cannot see it half-removed.
		branch, _ := strings.CutPrefix(ref.String(), "refs/heads/")
		unlock := s.rh.lockBranch(branch)
		err := s.rh.gits.RemoveReference(ref)
		unlock()
		if err != nil {
			// Refs is what was ACTUALLY deleted, never what was merely found.
			// Returning the full candidate list here would have the CLI print
			// "pruned generated ref: X" for a ref still sitting in the store.
			sort.Strings(out.Refs)
			return out, fmt.Errorf("prune: remove %s: %w", ref, err)
		}
		out.Refs = append(out.Refs, branch)
	}
	sort.Strings(out.Refs)

	// The markers are keyed by SOURCE branch, not by the generated ref, so they
	// are removed by prefix rather than by the ref names above — and they can
	// outlive the refs, so this runs even when no ref matched. The row count is
	// returned rather than swallowed: a prune that quietly deleted rows in a
	// repo it reported nothing about is an invisible write.
	res, err := s.rh.gits.DB().ExecContext(ctx,
		`DELETE FROM kv WHERE key LIKE 'okf:marker:%'`)
	if err != nil {
		return out, fmt.Errorf("prune: delete okf markers: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil {
		out.Markers = n
	}
	return out, nil
}

// listBranchRefsForVerify enumerates refs/heads/* directly from the storer.
// We deliberately bypass the branches SQLite cache so the branches-table check
// can compare cache vs. truth.
func (s *Service) listBranchRefsForVerify(_ context.Context) ([]string, error) {
	iter, err := s.rh.gits.IterReferences()
	if err != nil {
		return nil, err
	}
	var out []string
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		if name, ok := strings.CutPrefix(ref.Name().String(), "refs/heads/"); ok {
			out = append(out, name)
		}
		return nil
	})
	return out, err
}

// DeleteObjectForTest removes a git object (blob, tree, or commit) from the
// storer by hash. EXISTS ONLY for integrity-check tests that need to corrupt
// the store. Do not call from production code paths.
//
// Exported (vs. the previous lowercase form) so the testenv DSL's
// CorruptObject helper in a different package can call it.
func (s *Service) DeleteObjectForTest(hash string) error {
	return s.rh.gits.DeleteObjectForTest(plumbing.NewHash(hash))
}

// RawDBForTest returns the underlying *sql.DB handle. EXISTS ONLY for
// integrity-check tests that need to tamper with SQLite rows directly —
// deleting commit_log entries, inserting ghost rows, corrupting
// facts.blob_hash, etc. Do not call from production code paths.
func (s *Service) RawDBForTest() *sql.DB {
	return s.rh.gits.DB()
}

// FetchRefspecsForTest returns the fetch refspecs configured on the named git
// remote, or nil when the remote (or the repository) is absent. EXISTS ONLY so
// tests in other packages can assert that an origin change actually rewrote the
// git config — the refspec is what decides which branch the next Sync fetches,
// and it is otherwise invisible from outside this package (go-git's config lives
// in the SQLite-backed storer, not a .git/config file on disk).
//
// Without it, deleting the ConfigureRemote call from an upstream-change path
// leaves the stored branch correct and the fetch silently tracking the OLD
// branch — see TestPatchOriginUpstream_PersistsToControlDB.
func (s *Service) FetchRefspecsForTest(remote string) []string {
	if s.rh.repo == nil {
		return nil
	}
	cfg, err := s.rh.repo.Config()
	if err != nil {
		return nil
	}
	rc, ok := cfg.Remotes[remote]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(rc.Fetch))
	for _, rs := range rc.Fetch {
		out = append(out, string(rs))
	}
	return out
}

// RemoteURLsForTest returns the URLs configured on the named git remote, or nil
// when the remote (or the repository) is absent. EXISTS ONLY as the URL half of
// FetchRefspecsForTest: an origin write that re-points the git remote and then
// fails to make that durable has to be observed as a URL, because the refspecs
// are unchanged when only the url moved.
//
// See TestSetOrigin_FailedPersistRestoresTheGitRemote — without this, a repo
// left fetching a url nothing records looks identical to one that was never
// touched.
func (s *Service) RemoteURLsForTest(remote string) []string {
	if s.rh.repo == nil {
		return nil
	}
	cfg, err := s.rh.repo.Config()
	if err != nil {
		return nil
	}
	rc, ok := cfg.Remotes[remote]
	if !ok {
		return nil
	}
	return append([]string(nil), rc.URLs...)
}

// RawWriteForTest commits raw content to a path on the given branch,
// bypassing fact.ParseFact validation. EXISTS ONLY for integrity-check
// tests that need to inject deliberately malformed fact content (e.g. to
// exercise the deep fact-format check). The normal WriteFact path rejects
// invalid YAML at write time, which this escape hatch skips.
//
// The commit is created with the standard author/committer signatures and
// is signed. commit_log and the index are synced so the post-state is
// structurally consistent — only the FACT FORMAT is broken.
func (s *Service) RawWriteForTest(ctx context.Context, branch, path, content, message string) (string, error) {
	commitHash, _, err := s.fi.writeFile(ctx, branch, path, content, message, "raw-write-test")
	if err != nil {
		return "", err
	}
	return commitHash, nil
}

// checkGitReachability walks the commit chain from the branch ref to the root.
// For every commit it verifies the tree object exists and recursively that
// every tree entry resolves to an existing blob or subtree. Reports any
// missing object as a git-reachability Error naming the offending hash and
// (when known) the path that referenced it.
func (s *Service) checkGitReachability(_ context.Context, branch string) []IntegrityIssue {
	var issues []IntegrityIssue
	ref, err := s.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryGitReachability, Branch: branch,
			Detail: fmt.Sprintf("ref not found: %v", err),
		}}
	}

	// Walk commit chain to root.
	cur := ref.Hash()
	visited := map[string]bool{}
	for !cur.IsZero() {
		if visited[cur.String()] {
			break // safety: should never happen on a DAG without cycles
		}
		visited[cur.String()] = true

		commit, err := object.GetCommit(s.rh.gits, cur)
		if err != nil {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryGitReachability, Branch: branch, Commit: cur.String(),
				Detail: fmt.Sprintf("commit not found: %v", err),
			})
			break
		}

		// Walk this commit's tree.
		issues = append(issues, s.walkTreeReachable(branch, cur.String(), commit.TreeHash, "")...)

		if len(commit.ParentHashes) == 0 {
			break
		}
		// Only follow first parent. Agent branches CAN now contain merge
		// commits (introduced by the steady-state merge-based reconcile that
		// pulls main into the agent), so this walk no longer sees the full
		// commit DAG — but that's still correct for *this* check, which only
		// asserts tree/blob reachability. Trees and blobs from main's side
		// of the merge are reachable through the merge commit's TreeHash
		// (already covered by walkTreeReachable above). Commit-log parity
		// (which DOES need to see every commit) is checked separately by
		// checkCommitLogParity, which walks all parents.
		cur = commit.ParentHashes[0]
	}
	return issues
}

// walkTreeReachable recursively walks a tree and reports any unreachable
// blobs or subtrees. pathPrefix is the path accumulated so far; "" at root.
func (s *Service) walkTreeReachable(branch, commit string, treeHash plumbing.Hash, pathPrefix string) []IntegrityIssue {
	tree, err := object.GetTree(s.rh.gits, treeHash)
	if err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryGitReachability, Branch: branch, Commit: commit, Path: pathPrefix,
			Detail: fmt.Sprintf("tree %s not found: %v", treeHash, err),
		}}
	}
	var issues []IntegrityIssue
	for _, e := range tree.Entries {
		childPath := e.Name
		if pathPrefix != "" {
			childPath = pathPrefix + "/" + e.Name
		}
		switch e.Mode {
		case filemode.Dir:
			issues = append(issues, s.walkTreeReachable(branch, commit, e.Hash, childPath)...)
		default:
			// Verify the blob object exists. HasEncodedObject is a COUNT(*)
			// query — much cheaper than EncodedObject which loads blob data.
			if err := s.rh.gits.HasEncodedObject(e.Hash); err != nil {
				issues = append(issues, IntegrityIssue{
					Severity: SeverityError, Category: CategoryGitReachability,
					Branch: branch, Commit: commit, Path: childPath,
					Detail: fmt.Sprintf("blob %s not found: %v", e.Hash, err),
				})
			}
		}
	}
	return issues
}

// deleteCommitLogRowForTest removes commit_log rows for the given commit hash.
// Test-only escape hatch for integrity-check tests.
func (s *Service) deleteCommitLogRowForTest(commitHash string) error {
	_, err := s.rh.gits.DB().Exec(`DELETE FROM commit_log WHERE commit_hash = ?`, commitHash)
	return err
}

// checkCommitLogParity verifies that the commit_log SQLite rows for the branch
// are exactly the set of commits reachable from the branch head. Reports gaps
// (commits in git but not in commit_log) and orphans (commit_log rows for
// commits not on this branch's chain, via branch_commits).
func (s *Service) checkCommitLogParity(ctx context.Context, branch string) []IntegrityIssue {
	var issues []IntegrityIssue

	// 1. Collect every commit reachable on the branch chain. Merge
	// commits have multiple parents and branch_commits records
	// visibility via every edge, so a first-parent-only walk would
	// flag the non-first-parent side as "unreachable" even though
	// it's genuinely in the ancestry. Walk the full parent DAG.
	ref, err := s.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return nil // git-reachability already reported it
	}
	gitCommits := map[string]bool{}
	stack := []plumbing.Hash{ref.Hash()}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur.IsZero() || gitCommits[cur.String()] {
			continue
		}
		gitCommits[cur.String()] = true
		commit, err := object.GetCommit(s.rh.gits, cur)
		if err != nil {
			continue
		}
		stack = append(stack, commit.ParentHashes...)
	}

	// 2. Collect distinct commit hashes visible on this branch via
	// branch_commits. Previously this joined branch_commits ↔ commit_log,
	// which produced false parity gaps for legitimate no-op commits (a
	// write whose new blob equals the parent's blob at the same path has
	// zero tree changes, so changedFilesInCommit returns an empty slice
	// and CommitLogSync records only the branch_commits visibility row
	// without any commit_log path entries). Branch visibility is the
	// authoritative invariant — every reachable commit must have a
	// branch_commits row — and commit_log is a path-indexed projection
	// that legitimately has no rows for no-op commits.
	rows, err := s.rh.gits.DB().QueryContext(ctx,
		`SELECT bc.commit_hash FROM branch_commits bc
		 JOIN branches b ON b.id = bc.branch_id
		 WHERE b.name = ?`, branch)
	if err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryCommitLog, Branch: branch,
			Detail: fmt.Sprintf("query commit_log: %v", err),
		}}
	}
	defer rows.Close()
	logCommits := map[string]bool{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return []IntegrityIssue{{Severity: SeverityError, Category: CategoryCommitLog, Branch: branch, Detail: fmt.Sprintf("scan: %v", err)}}
		}
		logCommits[h] = true
	}
	if err := rows.Err(); err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryCommitLog, Branch: branch,
			Detail: fmt.Sprintf("iterate commit_log: %v", err),
		}}
	}

	// 3. Diff: commits in git not visible on this branch (via branch_commits).
	for h := range gitCommits {
		if !logCommits[h] {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryCommitLog, Branch: branch, Commit: h,
				Detail: fmt.Sprintf("commit %s reachable from branch ref but no branch_commits row", h),
			})
		}
	}
	// 4. Diff: commits claimed visible on this branch but not reachable from its ref.
	for h := range logCommits {
		if !gitCommits[h] {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryCommitLog, Branch: branch, Commit: h,
				Detail: fmt.Sprintf("branch_commits row %s not reachable from branch ref", h),
			})
		}
	}
	return issues
}

// checkCommitLogOrphans reports commit_log rows whose commit is not in the
// object store at all.
//
// checkCommitLogParity is named for commit_log and does not read it: it moved
// onto branch_commits, because a legitimate no-op commit (a write whose blob
// equals the parent's at the same path) produces zero commit_log rows and the
// old join reported that as a gap. Correct, but it left the table the category
// is named after with no coverage whatsoever.
//
// Orphans are the direction that has no such exception. A commit_log row for a
// commit no object exists for is drift under any policy about no-op commits,
// and it is what a partially-applied history rewrite leaves behind. The other
// direction (a commit with no commit_log rows) stays unchecked ON PURPOSE —
// that is exactly the no-op case.
//
// Repo-global, not per branch: commit_log is keyed by (commit_hash, path) with
// no branch dimension.
func (s *Service) checkCommitLogOrphans(ctx context.Context) []IntegrityIssue {
	rows, err := s.rh.gits.DB().QueryContext(ctx,
		`SELECT DISTINCT commit_hash FROM commit_log`)
	if err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryCommitLog,
			Detail: fmt.Sprintf("query commit_log: %v", err),
		}}
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return []IntegrityIssue{{
				Severity: SeverityError, Category: CategoryCommitLog,
				Detail: fmt.Sprintf("scan commit_log: %v", err),
			}}
		}
		hashes = append(hashes, h)
	}
	if err := rows.Err(); err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryCommitLog,
			Detail: fmt.Sprintf("iterate commit_log: %v", err),
		}}
	}

	var issues []IntegrityIssue
	for _, h := range hashes {
		if _, err := object.GetCommit(s.rh.gits, plumbing.NewHash(h)); err != nil {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryCommitLog, Commit: h,
				Detail: "commit_log rows reference a commit that is not in the object store",
			})
		}
	}
	return issues
}

// deleteBranchFactsRowForTest removes a branch_facts row for (branch, path).
// Test-only escape hatch for integrity-check tests.
func (s *Service) deleteBranchFactsRowForTest(branch, path string) error {
	_, err := s.rh.gits.DB().Exec(
		`DELETE FROM branch_facts
		 WHERE branch_id = (SELECT id FROM branches WHERE name = ?) AND path = ?`,
		branch, path)
	return err
}

// corruptFactsBlobHashForTest overwrites the blob_hash column on a facts row
// so it no longer matches the tree's blob hash at HEAD. Test-only escape hatch.
func (s *Service) corruptFactsBlobHashForTest(factID int64, newBlobHash string) error {
	_, err := s.rh.gits.DB().Exec(
		`UPDATE facts SET blob_hash = ? WHERE id = ?`, newBlobHash, factID)
	return err
}

// checkFactsCoherence verifies the three-way triangle for every branch:
//
//	branch_facts ↔ facts ↔ tree blob_hash
//
// For every .md file under kb/ at the branch's HEAD tree, there must be a
// branch_facts row linking to a facts row whose path matches and whose
// blob_hash equals the tree's blob for that path. For every branch_facts row
// on this branch, the referenced facts row must exist and its path must match.
func (s *Service) checkFactsCoherence(ctx context.Context, branch string) []IntegrityIssue {
	var issues []IntegrityIssue

	// Tree contents at HEAD (path -> blob_hash).
	treePaths, treeBlobs, err := s.rh.ListAllWithHash(ctx, branch)
	if err != nil {
		return nil // git-reachability reports this.
	}
	treeMap := make(map[string]string, len(treePaths))
	for i, p := range treePaths {
		// Same predicate the indexer admits paths by, so "what Verify expects"
		// and "what the index holds" cannot drift — and a custom ontology_root
		// is honoured by both rather than only by the writer.
		if !s.rh.isFactPath(p) {
			continue
		}
		treeMap[p] = treeBlobs[i]
	}

	// branch_facts rows for this branch joined against facts.
	rows, err := s.rh.gits.DB().QueryContext(ctx,
		`SELECT bf.path, bf.fact_id, f.path, f.blob_hash
		 FROM branch_facts bf
		 LEFT JOIN facts f ON f.id = bf.fact_id
		 WHERE bf.branch_id = (SELECT id FROM branches WHERE name = ?)`, branch)
	if err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryFactsCoherence, Branch: branch,
			Detail: fmt.Sprintf("query branch_facts: %v", err),
		}}
	}
	defer rows.Close()
	type bfRow struct {
		factID   int64
		factPath sql.NullString
		factBlob sql.NullString
	}
	branchFactsMap := make(map[string]bfRow)
	for rows.Next() {
		var bfPath string
		var row bfRow
		if err := rows.Scan(&bfPath, &row.factID, &row.factPath, &row.factBlob); err != nil {
			return []IntegrityIssue{{
				Severity: SeverityError, Category: CategoryFactsCoherence, Branch: branch,
				Detail: fmt.Sprintf("scan branch_facts: %v", err),
			}}
		}
		branchFactsMap[bfPath] = row
	}
	if err := rows.Err(); err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryFactsCoherence, Branch: branch,
			Detail: fmt.Sprintf("iterate branch_facts: %v", err),
		}}
	}

	// 1. Every tree file has a branch_facts row with correct blob_hash.
	for path, blob := range treeMap {
		bf, ok := branchFactsMap[path]
		if !ok {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryFactsCoherence, Branch: branch, Path: path,
				Detail: "fact present in tree at HEAD but no branch_facts row",
			})
			continue
		}
		if !bf.factPath.Valid || !bf.factBlob.Valid {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryFactsCoherence, Branch: branch, Path: path,
				Detail: fmt.Sprintf("branch_facts.fact_id=%d refers to missing facts row", bf.factID),
			})
			continue
		}
		if bf.factPath.String != path {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryFactsCoherence, Branch: branch, Path: path,
				Detail: fmt.Sprintf("branch_facts.path=%q but facts.path=%q for fact_id=%d", path, bf.factPath.String, bf.factID),
			})
		}
		if bf.factBlob.String != blob {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryFactsCoherence, Branch: branch, Path: path,
				Detail: fmt.Sprintf("facts.blob_hash=%s does not match tree blob %s", bf.factBlob.String, blob),
			})
		}
	}

	// 2. Every branch_facts row corresponds to a file in the tree.
	for path := range branchFactsMap {
		if _, ok := treeMap[path]; !ok {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryFactsCoherence, Branch: branch, Path: path,
				Detail: "branch_facts row for path not present in tree at HEAD (ghost)",
			})
		}
	}

	return issues
}

// deleteEmbeddingForTest removes a facts_vec row for the given facts.id.
// Test-only escape hatch.
func (s *Service) deleteEmbeddingForTest(factID int64) error {
	_, err := s.rh.gits.DB().Exec(`DELETE FROM facts_vec WHERE rowid = ?`, factID)
	return err
}

// checkEmbeddingsCoverage verifies, when an embedder is configured, that
// every facts row has a facts_vec row and vice versa. Skipped entirely when
// no embedder is configured because upsert only populates facts_vec when
// Service.SetEmbedder was called.
//
// This check is not branch-scoped — facts_vec is keyed by facts.id which is
// branch-agnostic under knomit's COW fact model. It is called once per
// Verify run, not per branch.
func (s *Service) checkEmbeddingsCoverage(ctx context.Context) []IntegrityIssue {
	if s.rh.getEmbedder() == nil {
		return nil
	}
	var issues []IntegrityIssue

	// Direction 1: facts rows without facts_vec rows.
	rows, err := s.rh.gits.DB().QueryContext(ctx,
		`SELECT f.id, f.path FROM facts f
		 WHERE f.id NOT IN (SELECT rowid FROM facts_vec)`)
	if err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryEmbeddingsCoverage,
			Detail: fmt.Sprintf("query facts missing embeddings: %v", err),
		}}
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			return append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryEmbeddingsCoverage,
				Detail: fmt.Sprintf("scan facts row: %v", err),
			})
		}
		issues = append(issues, IntegrityIssue{
			Severity: SeverityError, Category: CategoryEmbeddingsCoverage, Path: path,
			Detail: fmt.Sprintf("facts row id=%d has no facts_vec entry", id),
		})
	}
	if err := rows.Err(); err != nil {
		return append(issues, IntegrityIssue{
			Severity: SeverityError, Category: CategoryEmbeddingsCoverage,
			Detail: fmt.Sprintf("iterate facts: %v", err),
		})
	}

	// Direction 2: facts_vec rows without facts rows (trigger regression detection).
	orphanRows, err := s.rh.gits.DB().QueryContext(ctx,
		`SELECT rowid FROM facts_vec WHERE rowid NOT IN (SELECT id FROM facts)`)
	if err != nil {
		return append(issues, IntegrityIssue{
			Severity: SeverityError, Category: CategoryEmbeddingsCoverage,
			Detail: fmt.Sprintf("query facts_vec orphans: %v", err),
		})
	}
	defer orphanRows.Close()
	for orphanRows.Next() {
		var id int64
		if err := orphanRows.Scan(&id); err != nil {
			return append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryEmbeddingsCoverage,
				Detail: fmt.Sprintf("scan facts_vec row: %v", err),
			})
		}
		issues = append(issues, IntegrityIssue{
			Severity: SeverityError, Category: CategoryEmbeddingsCoverage,
			Detail: fmt.Sprintf("facts_vec rowid=%d has no corresponding facts row (trigger regression)", id),
		})
	}
	if err := orphanRows.Err(); err != nil {
		return append(issues, IntegrityIssue{
			Severity: SeverityError, Category: CategoryEmbeddingsCoverage,
			Detail: fmt.Sprintf("iterate facts_vec: %v", err),
		})
	}

	return issues
}

// checkEmbeddingIdentity reports derived state written under a different
// embedding model than the one now configured. checkEmbeddingsCoverage counts
// rows and so cannot see this: every facts row can have a facts_vec row and
// every vector still be from the wrong model, which makes similarity search
// quietly meaningless rather than broken.
func (s *Service) checkEmbeddingIdentity(ctx context.Context) []IntegrityIssue {
	emb := s.rh.getEmbedder()
	if emb == nil {
		return nil
	}
	storedID, err := s.si.persistedEmbedModelID(ctx)
	if err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryEmbeddingIdentity,
			Detail: fmt.Sprintf("read meta.embed_model_id: %v", err),
		}}
	}
	storedDim, err := s.si.persistedEmbedDim(ctx)
	if err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryEmbeddingIdentity,
			Detail: fmt.Sprintf("read meta.embed_dim: %v", err),
		}}
	}
	// Unset means nothing has ever been embedded under a recorded identity;
	// checkEmbeddingsCoverage owns the "rows are missing" story, and reporting
	// it a second time here would just double-count.
	if storedID == "" && storedDim == 0 {
		return nil
	}

	var issues []IntegrityIssue
	if storedID != "" && storedID != emb.ID() {
		issues = append(issues, IntegrityIssue{
			Severity: SeverityError, Category: CategoryEmbeddingIdentity,
			Detail: fmt.Sprintf(
				"facts_vec was written by embedding model %q but %q is configured; "+
					"similarity results are meaningless until the index is rebuilt",
				storedID, emb.ID()),
		})
	}
	if storedDim != 0 && storedDim != emb.Dim() {
		issues = append(issues, IntegrityIssue{
			Severity: SeverityError, Category: CategoryEmbeddingIdentity,
			Detail: fmt.Sprintf(
				"facts_vec holds %d-dimensional vectors but the configured model emits %d",
				storedDim, emb.Dim()),
		})
	}
	return issues
}

// checkCommitParents verifies the third leg of the commit index. commit_log
// (per-path) and branch_commits (visibility) are checked elsewhere;
// commit_parents holds the DAG edges, and drift there is invisible to both —
// history walks silently take the wrong shape.
func (s *Service) checkCommitParents(ctx context.Context, branch string, stored map[string][]string) []IntegrityIssue {
	ref, err := s.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return nil // git-reachability already reported it
	}

	var issues []IntegrityIssue
	seen := map[string]bool{}
	stack := []plumbing.Hash{ref.Hash()}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur.IsZero() || seen[cur.String()] {
			continue
		}
		seen[cur.String()] = true
		commit, err := object.GetCommit(s.rh.gits, cur)
		if err != nil {
			continue // git-reachability owns this
		}
		stack = append(stack, commit.ParentHashes...)

		want := make([]string, 0, len(commit.ParentHashes))
		for _, p := range commit.ParentHashes {
			want = append(want, p.String())
		}
		got := stored[cur.String()]
		// A root commit has no parents and therefore no rows: absence is
		// correct, not a gap. Only a MISMATCH is reported.
		if len(want) == 0 && len(got) == 0 {
			continue
		}
		if !slices.Equal(want, got) {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryCommitParents, Branch: branch, Commit: cur.String(),
				Detail: fmt.Sprintf("commit_parents has %v but the commit object has %v", got, want),
			})
		}
	}
	return issues
}

// loadCommitParents reads the whole commit_parents table once, keyed by commit
// and ordered by parent_order.
//
// One query for the repo, not one per commit. The per-commit form issued
// thousands of round trips on a real repo — all of them inside the window where
// Verify holds every branch's read lock and therefore blocks every writer. The
// table is small (two hashes and an int per parent edge) and the walk visits
// most of it anyway, so reading it whole is cheaper in both time and lock-hold
// than querying per commit, and it is shared across branches.
func (s *Service) loadCommitParents(ctx context.Context) (map[string][]string, error) {
	rows, err := s.rh.gits.DB().QueryContext(ctx,
		`SELECT commit_hash, parent_hash FROM commit_parents ORDER BY commit_hash, parent_order`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var commit, parent string
		if err := rows.Scan(&commit, &parent); err != nil {
			return nil, err
		}
		out[commit] = append(out[commit], parent)
	}
	return out, rows.Err()
}

// checkDerivedTables verifies the three fact_* projections that domain and
// entity search read. Nothing else checks them, and their failure mode is the
// quiet one: the facts are all present and correct, and searching by domain
// returns nothing.
//
// The two directions differ in severity ON PURPOSE.
//
// Orphan rows and a fact present in one table but not the other are structural
// and are ERRORS. A (fact_id, domain) set difference between fact_domains and
// fact_domain_tokens is a WARNING, because the two columns legitimately hold
// different STRINGS: both writers insert canonicalizeDomain(d), but that
// function gained a de-hyphenize step after some rows were written, and only
// the token side was ever repopulated. Comparing the strings and calling the
// difference corruption reports healthy repos as broken — which is the exact
// bug this whole pass exists to remove.
func (s *Service) checkDerivedTables(ctx context.Context) []IntegrityIssue {
	var issues []IntegrityIssue

	orphans := []struct{ table, col string }{
		{"fact_domains", "fact_id"},
		{"fact_domain_tokens", "fact_id"},
		{"fact_entities", "fact_id"},
	}
	for _, o := range orphans {
		var n int
		if err := s.rh.gits.DB().QueryRowContext(ctx, fmt.Sprintf(
			`SELECT COUNT(*) FROM %s WHERE %s NOT IN (SELECT id FROM facts)`, o.table, o.col),
		).Scan(&n); err != nil {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryDerivedTables,
				Detail: fmt.Sprintf("query %s orphans: %v", o.table, err),
			})
			continue
		}
		if n > 0 {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryDerivedTables,
				Detail: fmt.Sprintf("%s has %d row(s) referencing a missing facts row", o.table, n),
			})
		}
	}

	// A fact indexed for domain filtering but not for token containment (or the
	// reverse) is half-searchable. Compared at fact_id granularity, which is
	// immune to the canonicalization skew above.
	for _, d := range []struct{ have, missing string }{
		{"fact_domains", "fact_domain_tokens"},
		{"fact_domain_tokens", "fact_domains"},
	} {
		var n int
		if err := s.rh.gits.DB().QueryRowContext(ctx, fmt.Sprintf(
			`SELECT COUNT(*) FROM (SELECT DISTINCT fact_id FROM %s
			  WHERE fact_id NOT IN (SELECT fact_id FROM %s))`, d.have, d.missing),
		).Scan(&n); err != nil {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryDerivedTables,
				Detail: fmt.Sprintf("compare %s to %s: %v", d.have, d.missing, err),
			})
			continue
		}
		if n > 0 {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryDerivedTables,
				Detail: fmt.Sprintf("%d fact(s) present in %s but absent from %s", n, d.have, d.missing),
			})
		}
	}

	var skew int
	if err := s.rh.gits.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM (
		   SELECT fact_id, domain FROM fact_domains
		   EXCEPT
		   SELECT fact_id, domain FROM fact_domain_tokens)`).Scan(&skew); err != nil {
		return append(issues, IntegrityIssue{
			Severity: SeverityError, Category: CategoryDerivedTables,
			Detail: fmt.Sprintf("compare domain strings: %v", err),
		})
	}
	if skew > 0 {
		issues = append(issues, IntegrityIssue{
			Severity: SeverityWarning, Category: CategoryDerivedTables,
			Detail: fmt.Sprintf(
				"%d (fact_id, domain) pair(s) in fact_domains have no fact_domain_tokens row for "+
					"the same string — canonicalizeDomain drift, so domain filtering and token "+
					"containment disagree for these facts; a rebuild re-canonicalizes both", skew),
		})
	}
	return issues
}

// checkDatabase runs SQLite's own structural checks. Every parity check in this
// file assumes the pages underneath are readable and the declared foreign keys
// hold; this is the check that stops that assumption being silent.
func (s *Service) checkDatabase(ctx context.Context) []IntegrityIssue {
	var issues []IntegrityIssue

	rows, err := s.rh.gits.DB().QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryDatabase,
			Detail: fmt.Sprintf("quick_check: %v", err),
		}}
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryDatabase,
				Detail: fmt.Sprintf("scan quick_check: %v", err),
			})
		}
		if line != "ok" {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryDatabase,
				Detail: "quick_check: " + line,
			})
		}
	}
	// Checked, not assumed: a cursor that dies mid-iteration (cancellation, or
	// an I/O error on exactly the damaged page this check exists to find) ends
	// the loop with zero rows. Without this the integrity checker would swallow
	// its own read failure and report the database sound.
	rerr := rows.Err()
	rows.Close()
	if rerr != nil {
		return append(issues, IntegrityIssue{
			Severity: SeverityError, Category: CategoryDatabase,
			Detail: fmt.Sprintf("iterate quick_check: %v", rerr),
		})
	}

	// foreign_key_check reports violations even when PRAGMA foreign_keys is
	// OFF for the connection — which is the case that matters, since a cascade
	// that never ran is exactly how orphan rows survive.
	fkRows, err := s.rh.gits.DB().QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return append(issues, IntegrityIssue{
			Severity: SeverityError, Category: CategoryDatabase,
			Detail: fmt.Sprintf("foreign_key_check: %v", err),
		})
	}
	defer fkRows.Close()
	violations := map[string]int{}
	for fkRows.Next() {
		var table string
		var rowid sql.NullInt64
		var parent string
		var fkid int
		if err := fkRows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryDatabase,
				Detail: fmt.Sprintf("scan foreign_key_check: %v", err),
			})
		}
		violations[table+" -> "+parent]++
	}
	if err := fkRows.Err(); err != nil {
		return append(issues, IntegrityIssue{
			Severity: SeverityError, Category: CategoryDatabase,
			Detail: fmt.Sprintf("iterate foreign_key_check: %v", err),
		})
	}
	for pair, n := range violations {
		issues = append(issues, IntegrityIssue{
			Severity: SeverityError, Category: CategoryDatabase,
			Detail: fmt.Sprintf("foreign key violated: %s (%d row(s))", pair, n),
		})
	}
	return issues
}

// checkSchemaVersion reports a database left mid-migration. golang-migrate sets
// dirty=1 before applying a step and clears it after; a dirty row means a
// migration died part-way and every check above is reading a schema that is
// neither the old shape nor the new one.
func (s *Service) checkSchemaVersion(ctx context.Context) []IntegrityIssue {
	var version int64
	var dirty bool
	err := s.rh.gits.DB().QueryRowContext(ctx,
		`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty)
	if err == sql.ErrNoRows {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategorySchemaVersion,
			Detail: "schema_migrations is empty: this database was never stamped by the migrator",
		}}
	}
	if err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategorySchemaVersion,
			Detail: fmt.Sprintf("read schema_migrations: %v", err),
		}}
	}
	if dirty {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategorySchemaVersion,
			Detail: fmt.Sprintf(
				"schema_migrations is DIRTY at version %d: a migration failed part-way and the "+
					"schema is in neither the old nor the new shape", version),
		}}
	}
	return nil
}

// checkOrphanObjects counts git objects unreachable from any ref. This is the
// scan the package TODO asked for: a force push or a dropped branch leaves the
// old commits, trees and blobs in the objects table, where they are invisible
// to every other check and count only as growth.
//
// Reported as a WARNING and as a COUNT, not per object. Unreachable objects are
// not corruption — git accumulates them normally — so failing a run over them
// would be as wrong as the false positives this pass removes.
//
// NOT branch-scoped, which is why it takes no branch list: it roots from EVERY
// ref via IterReferences, including refs/remotes and the refs/knomit/*
// bookkeeping refs, so nothing legitimately retained is counted. Scoping it to
// the maintained branches would report everything the other refs hold as
// garbage.
//
// Note for callers: knomit has no git-object GC, so a count here is currently
// informational only. PruneGeneratedRefs makes it RISE, because the export
// commits its refs held become unreachable the moment the refs go.
func (s *Service) checkOrphanObjects(ctx context.Context) []IntegrityIssue {
	reachable := map[string]bool{}

	iter, err := s.rh.gits.IterReferences()
	if err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryOrphanObjects,
			Detail: fmt.Sprintf("iterate refs: %v", err),
		}}
	}
	var roots []plumbing.Hash
	if err := iter.ForEach(func(ref *plumbing.Reference) error {
		if !ref.Hash().IsZero() {
			roots = append(roots, ref.Hash())
		}
		return nil
	}); err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryOrphanObjects,
			Detail: fmt.Sprintf("collect ref roots: %v", err),
		}}
	}

	var markTree func(h plumbing.Hash)
	markTree = func(h plumbing.Hash) {
		if h.IsZero() || reachable[h.String()] {
			return
		}
		reachable[h.String()] = true
		tree, err := object.GetTree(s.rh.gits, h)
		if err != nil {
			return
		}
		for _, e := range tree.Entries {
			if e.Mode == filemode.Dir {
				markTree(e.Hash)
				continue
			}
			reachable[e.Hash.String()] = true
		}
	}

	stack := roots
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur.IsZero() || reachable[cur.String()] {
			continue
		}
		commit, err := object.GetCommit(s.rh.gits, cur)
		if err != nil {
			// Not a commit (an annotated tag or a ref straight at a tree/blob).
			// Mark it reachable so it is never counted as an orphan, and move on.
			reachable[cur.String()] = true
			continue
		}
		reachable[cur.String()] = true
		markTree(commit.TreeHash)
		stack = append(stack, commit.ParentHashes...)
	}

	rows, err := s.rh.gits.DB().QueryContext(ctx, `SELECT DISTINCT hash FROM objects`)
	if err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryOrphanObjects,
			Detail: fmt.Sprintf("query objects: %v", err),
		}}
	}
	defer rows.Close()
	orphans := 0
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return []IntegrityIssue{{
				Severity: SeverityError, Category: CategoryOrphanObjects,
				Detail: fmt.Sprintf("scan objects: %v", err),
			}}
		}
		if !reachable[h] {
			orphans++
		}
	}
	if err := rows.Err(); err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryOrphanObjects,
			Detail: fmt.Sprintf("iterate objects: %v", err),
		}}
	}
	if orphans == 0 {
		return nil
	}
	return []IntegrityIssue{{
		Severity: SeverityWarning, Category: CategoryOrphanObjects,
		Detail: fmt.Sprintf(
			"%d git object(s) unreachable from any ref (history rewrite or dropped branch residue); "+
				"they cost space only", orphans),
	}}
}

// checkBranchesTable verifies the branches SQLite table against the git refs,
// in two directions with deliberately different scopes.
//
// Direction 1 (a git ref must have a correct branches row) runs over the
// MAINTAINED branches only. A ref the index does not maintain is not required
// to have a row — an unindexed ref having no branches row is the normal
// consequence of not indexing it, and demanding one produced two errors per
// repo for refs left behind by a feature that no longer exists.
//
// Direction 2 (a branches row must have a git ref) runs over ALL refs. A row
// pointing at a branch that no longer exists anywhere is drift under any
// scoping, and scoping this direction to maintained branches would make the
// check unable to see the rows it is most needed for.
//
// Called once per Verify, not per branch.
func (s *Service) checkBranchesTable(ctx context.Context, checked, allBranches []string) []IntegrityIssue {
	var issues []IntegrityIssue

	rows, err := s.rh.gits.DB().QueryContext(ctx, `SELECT name, git_ref FROM branches`)
	if err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryBranchesTable,
			Detail: fmt.Sprintf("query branches: %v", err),
		}}
	}
	defer rows.Close()

	tableRows := make(map[string]string)
	for rows.Next() {
		var name, gitRef string
		if err := rows.Scan(&name, &gitRef); err != nil {
			return []IntegrityIssue{{
				Severity: SeverityError, Category: CategoryBranchesTable,
				Detail: fmt.Sprintf("scan branches: %v", err),
			}}
		}
		tableRows[name] = gitRef
	}
	if err := rows.Err(); err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryBranchesTable,
			Detail: fmt.Sprintf("iterate branches: %v", err),
		}}
	}

	gitSet := make(map[string]bool, len(allBranches))
	for _, b := range allBranches {
		gitSet[b] = true
	}

	// Direction 1: every MAINTAINED git ref has a matching branches row with
	// the correct git_ref.
	for _, b := range checked {
		ref, ok := tableRows[b]
		if !ok {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryBranchesTable, Branch: b,
				Detail: "git ref exists but no row in branches table",
			})
			continue
		}
		expected := "refs/heads/" + b
		if ref != expected {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryBranchesTable, Branch: b,
				Detail: fmt.Sprintf("branches.git_ref=%q does not match expected %q", ref, expected),
			})
		}
	}

	// Direction 2: every branches row corresponds to a git ref.
	for name := range tableRows {
		if !gitSet[name] {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryBranchesTable, Branch: name,
				Detail: "branches table row exists but no git ref",
			})
		}
	}

	return issues
}

// checkFactFormat parses every .md file under kb/ at the branch's HEAD tree
// via fact.ParseFact. Files that fail to parse produce Warning-severity
// issues — they represent data problems (bad YAML, missing title), not
// structural corruption. Only runs when opts.Deep is true.
func (s *Service) checkFactFormat(ctx context.Context, branch string) []IntegrityIssue {
	var issues []IntegrityIssue

	treePaths, _, err := s.rh.ListAllWithHash(ctx, branch)
	if err != nil {
		return nil // git-reachability reports this.
	}

	for _, p := range treePaths {
		if !s.rh.isFactPath(p) {
			continue
		}
		content, err := s.rh.readFile(ctx, branch, p)
		if err != nil {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityWarning, Category: CategoryFactFormat, Branch: branch, Path: p,
				Detail: fmt.Sprintf("read failed: %v", err),
			})
			continue
		}
		if _, err := fact.ParseFact(p, content); err != nil {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityWarning, Category: CategoryFactFormat, Branch: branch, Path: p,
				Detail: fmt.Sprintf("parse failed: %v", err),
			})
		}
	}

	return issues
}

// deleteGraphFactNodeForTest removes a Fact node from the graph for the given
// (path, blob_hash). Test-only escape hatch for integrity tests. Incident
// edges, labels and properties cascade (ON DELETE CASCADE), which is what
// Cypher's DETACH DELETE did.
func (s *Service) deleteGraphFactNodeForTest(path, blobHash string) error {
	_, err := s.rh.gits.DB().Exec(`
		DELETE FROM nodes
		WHERE id IN (
			SELECT nl.node_id
			FROM node_labels nl
			JOIN node_props_text p ON p.node_id = nl.node_id
			JOIN property_keys kp ON kp.id = p.key_id AND kp.key = 'path'
			JOIN node_props_text b ON b.node_id = nl.node_id
			JOIN property_keys kb ON kb.id = b.key_id AND kb.key = 'blob_hash'
			WHERE nl.label = ? AND p.value = ? AND b.value = ?
		)`, NodeFact, path, blobHash)
	return err
}

// checkGraphCoherence verifies bidirectional parity between the facts
// SQLite table and the LIVE Fact nodes in the property graph (those with
// deleted != true). Every facts row must have a live Fact node keyed by
// (path, blob_hash), and every live Fact node must have a facts row.
//
// The graph model is intentionally a permanent temporal graph:
//   - Soft-deleted Fact nodes (deleted = true) persist forever after
//     graphDeleteFact runs, preserving lineage for DERIVED_FROM walks.
//
// This check explicitly scopes itself to LIVE Fact nodes. Soft-deleted
// nodes are out of scope.
//
// This check is global (not branch-scoped) because facts and graph Fact
// nodes have no branch dimension — both are deduplicated by (path, blob_hash)
// via the COW model.
//
// TODO(verify): extend to Entity, Domain, OntologyNode parity once the
// basic Fact-node check is stable.
func (s *Service) checkGraphCoherence(ctx context.Context) []IntegrityIssue {
	var issues []IntegrityIssue

	// 1. Collect all facts from the relational table.
	sqlRows, err := s.rh.gits.DB().QueryContext(ctx, `SELECT path, blob_hash FROM facts`)
	if err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryGraphCoherence,
			Detail: fmt.Sprintf("query facts: %v", err),
		}}
	}
	defer sqlRows.Close()
	sqlSet := make(map[string]string) // key "path|blob_hash" -> path
	for sqlRows.Next() {
		var path, blob string
		if err := sqlRows.Scan(&path, &blob); err != nil {
			return []IntegrityIssue{{
				Severity: SeverityError, Category: CategoryGraphCoherence,
				Detail: fmt.Sprintf("scan facts: %v", err),
			}}
		}
		sqlSet[path+"|"+blob] = path
	}
	if err := sqlRows.Err(); err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryGraphCoherence,
			Detail: fmt.Sprintf("iterate facts: %v", err),
		}}
	}

	// 2. Collect LIVE Fact nodes (deleted != 'true') straight from the EAV
	// tables. `deleted` is now plain TEXT ('true'/'false'), so the comparison
	// is a direct one; a node with no `deleted` property at all counts as live,
	// matching the property's false default.
	graphRows, err := s.rh.gits.DB().QueryContext(ctx, `
		SELECT p.value, b.value
		FROM node_labels nl
		JOIN node_props_text p ON p.node_id = nl.node_id
		JOIN property_keys kp ON kp.id = p.key_id AND kp.key = 'path'
		JOIN node_props_text b ON b.node_id = nl.node_id
		JOIN property_keys kb ON kb.id = b.key_id AND kb.key = 'blob_hash'
		LEFT JOIN property_keys kd ON kd.key = 'deleted'
		LEFT JOIN node_props_text d ON d.node_id = nl.node_id AND d.key_id = kd.id
		WHERE nl.label = ? AND (d.value IS NULL OR d.value != 'true')`, NodeFact)
	if err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryGraphCoherence,
			Detail: fmt.Sprintf("query live graph Fact nodes: %v", err),
		}}
	}
	defer graphRows.Close()
	graphSet := make(map[string]bool)
	for graphRows.Next() {
		var path, blob sql.NullString
		if err := graphRows.Scan(&path, &blob); err != nil {
			return []IntegrityIssue{{
				Severity: SeverityError, Category: CategoryGraphCoherence,
				Detail: fmt.Sprintf("scan graph Fact node: %v", err),
			}}
		}
		if !path.Valid || !blob.Valid {
			continue
		}
		graphSet[path.String+"|"+blob.String] = true
	}
	if err := graphRows.Err(); err != nil {
		return []IntegrityIssue{{
			Severity: SeverityError, Category: CategoryGraphCoherence,
			Detail: fmt.Sprintf("iterate graph Fact nodes: %v", err),
		}}
	}

	// 3. Direction 1: facts row exists but no live Fact node.
	// This fires if the facts-row was inserted but graphSyncFact failed
	// (logged as a warning in search_crud.go) OR if the Fact node was
	// incorrectly soft-deleted while the facts row is still live.
	for key, path := range sqlSet {
		if !graphSet[key] {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryGraphCoherence, Path: path,
				Detail: fmt.Sprintf("facts row %s has no live graph Fact node (missing or soft-deleted)", key),
			})
		}
	}

	// 4. Direction 2: live Fact node exists but no facts row.
	// Historical soft-deleted nodes are already filtered out by the cypher
	// query above, so hitting this branch means a Fact node still has
	// deleted = false but the corresponding facts row is gone — the delete
	// path ran partially (facts DELETE succeeded, graphDeleteFact soft-delete
	// did not run or was reverted).
	for key := range graphSet {
		if _, ok := sqlSet[key]; !ok {
			parts := strings.SplitN(key, "|", 2)
			path := ""
			if len(parts) > 0 {
				path = parts[0]
			}
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryGraphCoherence, Path: path,
				Detail: fmt.Sprintf("live graph Fact node %s has no facts row (delete path left node live)", key),
			})
		}
	}

	return issues
}
