package fsck

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Format truncates the flat issue slice, so ordering decides what survives.
// Category-first ordering let an alphabetically-early category of warnings
// consume the whole budget and hide every error — schema-version sorts last of
// all categories, so a DIRTY migration was guaranteed invisible on a repo with
// 100 earlier findings.
func TestVerify_ErrorsSortAheadOfWarningsSoTruncationCannotHideThem(t *testing.T) {
	issues := []IntegrityIssue{
		{Severity: SeverityWarning, Category: CategoryDerivedTables, Detail: "skew"},
		{Severity: SeverityError, Category: CategorySchemaVersion, Detail: "DIRTY"},
		{Severity: SeverityWarning, Category: CategoryGeneratedRefs, Detail: "residue"},
		{Severity: SeverityError, Category: CategoryCommitLog, Detail: "orphan"},
	}
	sortIssues(issues)

	require.Equal(t, SeverityError, issues[0].Severity)
	require.Equal(t, SeverityError, issues[1].Severity)
	require.Equal(t, SeverityWarning, issues[2].Severity)

	// Budget for exactly the errors: both survive, both warnings are dropped.
	// Under category-first ordering "derived-tables" and "generated-refs" would
	// have taken these two slots and "schema-version" — last alphabetically —
	// would never print at all.
	r := IntegrityReport{Repo: "r", Branches: []string{"main"}, Issues: issues}
	out := r.Format(2)
	require.Contains(t, out, "DIRTY", "truncation must keep the errors")
	require.Contains(t, out, "orphan")
	require.NotContains(t, out, "residue", "warnings must be dropped before any error")
	require.NotContains(t, out, "skew")
	// The counts stay complete regardless of what the detail list dropped.
	require.Contains(t, out, "derived-tables       1")
}
