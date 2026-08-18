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
//
// kb/.drafts/x.md is an ISOLATING companion fixture, not redundant with the
// .knomit/ one above: .knomit/jobs/x.md never reaches fact.IsPrivatePath at
// all, because okfReadFacts's ontology-root prefix check
// (`strings.HasPrefix(f.Name, okfOntologyRoot+"/")`) already rejects it —
// .knomit sits at the repo root, outside kb/. kb/.drafts/x.md starts with
// "kb/" and ends in ".md", so it clears that prefix check and IsPrivatePath is
// the only thing standing between it and export. Keep both: .knomit/ pins the
// real requirement (job state must never be exported); kb/.drafts/ pins the
// guard that would actually catch a regression in it — deleting the
// IsPrivatePath call in facts.go would still pass a .knomit/-only test.
func TestOKFExportSkipsPrivateNamespace(t *testing.T) {
	r := newFixtureRepo(t)
	h := commitFiles(t, r, "seed", "a+learn@agents.knomit.io", map[string]string{
		"kb/decisions/x/aaaaaaaa.md": factBody("Alpha", 0.9),
		".knomit/jobs/x.md":          factBody("Job state", 0.9),
		"kb/.drafts/x.md":            factBody("Draft", 0.9),
	})

	snap, err := Load(r.Storer, h)
	require.NoError(t, err)
	require.Len(t, snap.Facts, 1)
	require.Equal(t, "kb/decisions/x/aaaaaaaa.md", snap.Facts[0].Fact.Path(),
		"job state under .knomit/ and a draft under kb/.drafts/ must not be exported")
	require.Empty(t, snap.Warnings,
		"neither must be reported as lost/unparseable knowledge")
}

// TestOKFHistorySkipsPrivateNamespace pins okfDeletionFromFile (history.go:458)
// and okfChangeFromFile (history.go:511) directly: both feed the authored-time
// and revision-history walk that Load's export relies on, and both must agree
// with okfReadFacts on which paths are knowledge — a job-state path under
// .knomit/ must never surface as a fact deletion or a fact revision.
//
// kb/.drafts/x.md is the ISOLATING companion for the same reason as in
// TestOKFExportSkipsPrivateNamespace: both functions reject
// ".knomit/jobs/x.md" via their own `!strings.HasPrefix(name,
// okfOntologyRoot+"/")` check before ever consulting fact.IsPrivatePath, so a
// .knomit/-only assertion here would keep passing even if the IsPrivatePath
// call were deleted from both functions. kb/.drafts/x.md clears the prefix
// check, so IsPrivatePath is the only thing that can make it return false.
func TestOKFHistorySkipsPrivateNamespace(t *testing.T) {
	jobContents := func() (string, error) { return factBody("Job state", 0.9), nil }
	draftContents := func() (string, error) { return factBody("Draft", 0.9), nil }

	_, ok := okfDeletionFromFile(".knomit/jobs/x.md", jobContents)
	require.False(t, ok, "a removed .knomit/ path must not be treated as fact retirement")
	_, ok = okfDeletionFromFile("kb/.drafts/x.md", draftContents)
	require.False(t, ok, "a removed kb/.drafts/ path must not be treated as fact retirement")

	_, ok = okfChangeFromFile(".knomit/jobs/x.md", true, jobContents)
	require.False(t, ok, "a changed .knomit/ path must not be treated as a fact revision")
	_, ok = okfChangeFromFile("kb/.drafts/x.md", true, draftContents)
	require.False(t, ok, "a changed kb/.drafts/ path must not be treated as a fact revision")
}
