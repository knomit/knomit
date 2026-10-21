package fsck

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Format's summary must survive truncation: a report capped at N lines still
// has to say how much it actually found.
func TestIntegrityReport_FormatTruncatesDetailNotCounts(t *testing.T) {
	r := IntegrityReport{
		Repo:     "r",
		Branches: []string{"main"},
		Issues: []IntegrityIssue{
			{Severity: SeverityError, Category: CategoryCommitLog, Detail: "one"},
			{Severity: SeverityError, Category: CategoryCommitLog, Detail: "two"},
			{Severity: SeverityError, Category: CategoryCommitLog, Detail: "three"},
		},
	}
	out := r.Format(1)
	require.Contains(t, out, "Issues: 3 (3 error, 0 warning)")
	require.Contains(t, out, "commit-log           3")
	require.Contains(t, out, "2 more")
	require.NotContains(t, out, "three")

	require.Contains(t, r.Format(0), "three", "0 means print everything")
}

// Skipped branches must be visible in the rendered report, because that line is
// the whole reason a per-branch warning is not raised for them.
func TestIntegrityReport_FormatNamesSkippedBranches(t *testing.T) {
	r := IntegrityReport{Repo: "r", Branches: []string{"main"}, Skipped: []string{"okf/main"}}
	out := r.Format(0)
	require.Contains(t, out, "Not indexed (parity not checked): okf/main")
	require.Contains(t, out, "Status: CLEAN")
}
