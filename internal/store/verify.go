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

// All check stubs return nil for now. Implementations land in tasks 1.2–1.8.
func (s *Service) checkGitReachability(_ context.Context, _ string) []IntegrityIssue {
	return nil
}
func (s *Service) checkCommitLogParity(_ context.Context, _ string) []IntegrityIssue {
	return nil
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
