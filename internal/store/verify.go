// Verify reports structural integrity issues across the store. It walks every
// branch ref, verifies git object reachability, parity between SQLite tables
// and git/tree state, and (with Deep) parses every fact for format errors.
//
// TODO(verify): orphan object scan for history-rewrite scenarios. Currently
// only walks objects reachable from refs; commits made unreachable by force
// pushes are not reported.
//
// Verify is read-only. It acquires per-branch read locks one branch at a time;
// the report is therefore not a snapshot of the whole repo at one instant.
// For definitive results on a busy repo, take the agent offline first.
package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

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
}

// Canonical issue category strings. Implementations of the per-category
// checks (tasks 1.2-1.8) MUST use exactly these constants when populating
// IntegrityIssue.Category — no string literals at the call sites.
const (
	CategoryGitReachability  = "git-reachability"
	CategoryCommitLog        = "commit-log"
	CategorySearchIndex      = "search-index"
	CategoryVectorDim        = "vector-dim"
	CategoryBranchesTable    = "branches-table"
	CategoryBranchFactsTable = "branch-facts-table"
	CategoryFactFormat       = "fact-format"
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
	Branches  []string
	Issues    []IntegrityIssue
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

// String formats the report as a multi-line human-readable summary.
func (r IntegrityReport) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Verify report for %q at %s\n", r.Repo, r.CheckedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "  Branches checked: %s\n", strings.Join(r.Branches, ", "))
	if r.IsStrictlyClean() {
		b.WriteString("  Status: CLEAN\n")
		return b.String()
	}
	fmt.Fprintf(&b, "  Issues: %d\n", len(r.Issues))
	for _, i := range r.Issues {
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
	return b.String()
}

// Verify walks the store and returns an integrity report. See package doc.
func (s *Service) Verify(ctx context.Context, opts VerifyOpts) (IntegrityReport, error) {
	report := IntegrityReport{CheckedAt: time.Now()}

	// Enumerate branches via git refs (storer-level), since the branches
	// SQLite table is one of the things we're going to verify.
	branches, err := s.listBranchRefsForVerify(ctx)
	if err != nil {
		return report, fmt.Errorf("verify: list branches: %w", err)
	}
	sort.Strings(branches)
	report.Branches = branches

	// Per-category check stubs — implementations land in subsequent tasks.
	// TODO(verify): once the stubs are implemented in tasks 1.2-1.8 and start
	// doing real I/O, add a `select { case <-ctx.Done(): return report, ctx.Err() }`
	// check between branches so cancellation can interrupt long verifies.
	for _, br := range branches {
		report.Issues = append(report.Issues, s.checkGitReachability(ctx, br)...)
		report.Issues = append(report.Issues, s.checkCommitLogParity(ctx, br)...)
		report.Issues = append(report.Issues, s.checkSearchIndexParity(ctx, br)...)
		report.Issues = append(report.Issues, s.checkVectorDim(ctx, br)...)
		report.Issues = append(report.Issues, s.checkBranchFactsParity(ctx, br)...)
		if opts.Deep {
			report.Issues = append(report.Issues, s.checkFactFormat(ctx, br)...)
		}
	}
	report.Issues = append(report.Issues, s.checkBranchesTable(ctx, branches)...)

	return report, nil
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

// deleteObjectForTest removes an object from the storer by hash. EXISTS ONLY
// FOR INTEGRITY-CHECK TESTS that need to corrupt state. Do not call from
// production code paths.
func (s *Service) deleteObjectForTest(hash string) error {
	return s.rh.gits.DeleteObjectForTest(plumbing.NewHash(hash))
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
		// Only follow first parent. Knomit branches are linear; merge commits
		// are not expected on agent branches today. If merge-to-main lands as
		// a real merge (not a fast-forward), this walk must visit all parents.
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

	// 1. Collect commits reachable on the branch chain.
	ref, err := s.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return nil // git-reachability already reported it
	}
	gitCommits := map[string]bool{}
	cur := ref.Hash()
	for !cur.IsZero() {
		if gitCommits[cur.String()] {
			break
		}
		gitCommits[cur.String()] = true
		commit, err := object.GetCommit(s.rh.gits, cur)
		if err != nil || len(commit.ParentHashes) == 0 {
			break
		}
		cur = commit.ParentHashes[0]
	}

	// 2. Collect distinct commit_log commit hashes visible on this branch.
	rows, err := s.rh.gits.DB().QueryContext(ctx,
		`SELECT DISTINCT cl.commit_hash FROM commit_log cl
		 JOIN branch_commits bc ON bc.commit_hash = cl.commit_hash
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

	// 3. Diff: commits in git not in log.
	for h := range gitCommits {
		if !logCommits[h] {
			issues = append(issues, IntegrityIssue{
				Severity: SeverityError, Category: CategoryCommitLog, Branch: branch, Commit: h,
				Detail: fmt.Sprintf("commit %s reachable from branch ref but missing from commit_log", h),
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
func (s *Service) checkSearchIndexParity(_ context.Context, _ string) []IntegrityIssue {
	return nil
}
func (s *Service) checkVectorDim(_ context.Context, _ string) []IntegrityIssue {
	return nil
}
func (s *Service) checkBranchFactsParity(_ context.Context, _ string) []IntegrityIssue {
	return nil
}
func (s *Service) checkBranchesTable(_ context.Context, _ []string) []IntegrityIssue {
	return nil
}
func (s *Service) checkFactFormat(_ context.Context, _ string) []IntegrityIssue {
	return nil
}
