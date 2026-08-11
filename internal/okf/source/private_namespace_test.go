package source

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestOKFExportSkipsPrivateNamespace pins the same rule
// TestLoad_SkipsPrivatePathsWithoutWarning already covers for a generic
// dot-directory (facts.go:44), anchored on the concrete case this whole
// design exists to protect: job state a job writes at .knomit/<area>/… (see
// internal/fact.PrivateRoot).
//
// The fixture parses perfectly as a fact — the skip must fire BEFORE the
// looksLikeFact check, or every hand-placed job-state file is reported as
// lost knowledge on every single export, which is exactly the false alarm
// TestLoad_SkipsPrivatePathsWithoutWarning was written to prevent.
func TestOKFExportSkipsPrivateNamespace(t *testing.T) {
	r := newFixtureRepo(t)
	h := commitFiles(t, r, "seed", "a+learn@agents.knomit.io", map[string]string{
		"kb/decisions/x/aaaaaaaa.md": factBody("Alpha", 0.9),
		".knomit/jobs/x.md":          factBody("Job state", 0.9),
	})

	snap, err := Load(r.Storer, h)
	require.NoError(t, err)
	require.Len(t, snap.Facts, 1)
	require.Equal(t, "kb/decisions/x/aaaaaaaa.md", snap.Facts[0].Fact.Path(),
		"job state under .knomit/ must not be exported")
	require.Empty(t, snap.Warnings,
		"job state under .knomit/ is not lost knowledge and must not be reported as unparseable")
}

// TestOKFHistorySkipsPrivateNamespace pins okfDeletionFromFile (history.go:458)
// and okfChangeFromFile (history.go:511) directly: both feed the authored-time
// and revision-history walk that Load's export relies on, and both must agree
// with okfReadFacts on which paths are knowledge — a job-state path under
// .knomit/ must never surface as a fact deletion or a fact revision.
func TestOKFHistorySkipsPrivateNamespace(t *testing.T) {
	contents := func() (string, error) { return factBody("Job state", 0.9), nil }

	_, ok := okfDeletionFromFile(".knomit/jobs/x.md", contents)
	require.False(t, ok, "a removed .knomit/ path must not be treated as fact retirement")

	_, ok = okfChangeFromFile(".knomit/jobs/x.md", true, contents)
	require.False(t, ok, "a changed .knomit/ path must not be treated as a fact revision")
}
