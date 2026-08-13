package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/repos"
	"knomit/internal/store"
)

func TestVerifyCmd_FlagsRegistered(t *testing.T) {
	c := verifyCmd()
	for _, name := range []string{
		"repo", "all", "deep", "json", "max-issues", "all-branches", "prune-generated-refs",
	} {
		require.NotNil(t, c.Flags().Lookup(name), "--%s must be registered", name)
	}
	require.Equal(t, "100", c.Flags().Lookup("max-issues").DefValue)
}

// The exit-code contract is the whole interface for anything automated. 1 must
// mean "the repo has integrity errors" and nothing else; a usage mistake used
// to exit 1 too, so a CI job could not tell a typo from a damaged corpus.
func TestVerifyCmd_HelpDocumentsExitCodes(t *testing.T) {
	c := verifyCmd()
	require.Contains(t, c.Long, "0   clean")
	require.Contains(t, c.Long, "1   integrity errors found")
	require.Contains(t, c.Long, "2   verify itself could not run")
	require.Equal(t, exitClean, 0)
	require.Equal(t, exitDirty, 1)
	require.Equal(t, exitFailed, 2)
}

// A missing --repo is a misuse of the CLI, not a finding about the corpus, so
// it must map to 2. Checked before any app boot, so no repo is opened.
func TestVerify_MissingRepoIsAUsageFailureNotADirtyRepo(t *testing.T) {
	c := verifyCmd()
	code, err := runVerify(c, verifyOpts{})
	require.Error(t, err)
	require.Equal(t, exitFailed, code, "a usage error must not be reported as integrity errors")
	require.Contains(t, err.Error(), "--repo is required")
}

// The help used to promise the OPPOSITE of what the code does — that verify
// locks one branch at a time and gives no snapshot. It takes every branch read
// lock up front and holds them, which means it blocks writers for the whole
// run. An operator who believes the old text fires this at a live agent.
func TestVerifyCmd_HelpDescribesLockingHonestly(t *testing.T) {
	c := verifyCmd()
	require.Contains(t, c.Long, "SNAPSHOT")
	require.Contains(t, c.Long, "BLOCKS")
	require.NotContains(t, c.Long, "one branch at a time")
	require.NotContains(t, c.Long, "does not produce an atomic snapshot")
}

// --all has always covered active repos only; the help now says so, because the
// silent version meant an archived knowledge base could rot unmentioned.
func TestVerifyCmd_HelpSaysAllMeansActive(t *testing.T) {
	c := verifyCmd()
	require.Contains(t, c.Long, "active repos")
	require.Contains(t, c.Long, "archived repo is not opened")
}

func TestReportSkippedArchived_SaysNothingWhenThereAreNone(t *testing.T) {
	var buf bytes.Buffer
	reportSkippedArchived(&buf, &repos.Manager{})
	// A zero-value Manager cannot reach control.db; either outcome is
	// acceptable, but it must never claim repos were skipped when none were.
	require.NotContains(t, buf.String(), "not verified (--all covers active repos only)")
}

// The JSON shape is a contract for anything consuming this from CI, so its
// field names are pinned rather than left to drift with the store struct.
func TestToJSON_ShapeIsStable(t *testing.T) {
	r := store.IntegrityReport{
		Repo:      "kb",
		CheckedAt: time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC),
		Branches:  []string{"main"},
		Skipped:   []string{"okf/main"},
		Issues: []store.IntegrityIssue{{
			Severity: store.SeverityWarning,
			Category: store.CategoryGeneratedRefs,
			Branch:   "okf/main",
			Detail:   "residue",
		}},
	}
	raw, err := json.Marshal(toJSON(r, store.PruneResult{Refs: []string{"okf/main"}, Markers: 2}))
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(raw, &got))
	for _, key := range []string{
		"repo", "checked_at", "branches_checked", "branches_not_indexed",
		"clean", "counts_by_category", "issues", "pruned_refs",
	} {
		require.Contains(t, got, key, "JSON field %q must be present", key)
	}
	require.Equal(t, true, got["clean"], "warnings alone must serialize as clean")

	issues := got["issues"].([]any)
	first := issues[0].(map[string]any)
	require.Equal(t, "WARN", first["severity"], "severity must serialize as a name, not an int")
	require.Equal(t, store.CategoryGeneratedRefs, first["category"])
}

// A clean report must not carry empty issue noise, so consumers can branch on
// the array being empty — and "empty" has to mean [], not null. A nil slice
// marshals to null, and a consumer looping over report["issues"] then fails on
// the one case that should be easiest: a healthy repo.
func TestToJSON_CleanReportEncodesEmptyArraysNotNull(t *testing.T) {
	out := toJSON(store.IntegrityReport{Repo: "kb", Branches: []string{"main"}}, store.PruneResult{})
	require.Empty(t, out.Issues)
	require.True(t, out.Clean)

	raw, err := json.Marshal(out)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"issues":[]`)
	require.Contains(t, string(raw), `"branches_not_indexed":[]`)
	require.NotContains(t, string(raw), `null`)
	require.False(t, strings.Contains(string(raw), `"pruned_refs"`),
		"pruned_refs is omitempty: absent when nothing was pruned")
}

// --max-issues truncates the human rendering only. Truncating JSON would hand a
// CI job a report that looks complete and is not.
func TestToJSON_IsNeverTruncated(t *testing.T) {
	r := store.IntegrityReport{Repo: "kb"}
	for i := range 150 {
		r.Issues = append(r.Issues, store.IntegrityIssue{
			Severity: store.SeverityError,
			Category: store.CategoryCommitLog,
			Detail:   fmt.Sprintf("issue %d", i),
		})
	}
	require.Len(t, toJSON(r, store.PruneResult{}).Issues, 150)
	require.Contains(t, verifyCmd().Flags().Lookup("max-issues").Usage, "--json is never truncated")
}
