package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
)

// twoRepoWriteBinding builds a lens over write repo A + read mount B, seeds one
// principle fact in each, and returns the binding plus both bare paths. Mirrors
// the query_federation_test fixtures but for the write path.
func twoRepoWriteBinding(t *testing.T) (b *repos.Binding, repoA, repoB *repos.RepoInstance, pathA, pathB string) {
	t.Helper()
	repoA, ctxA := fedRepo(t)
	repoB, ctxB := fedRepo(t)
	pathA = seedFedFact(t, ctxA, "seed-a", "mission/store", "Alpha", "store", nil)
	pathB = seedFedFact(t, ctxB, "seed-b", "mission/ui", "Bravo", "ui", nil)
	b = repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	return b, repoA, repoB, pathA, pathB
}

// updateVia runs UpdateHandler against an explicit binding.
func updateVia(t *testing.T, b *repos.Binding, args map[string]any) (*mcpgo.CallToolResult, string) {
	t.Helper()
	ctx := repos.WithBinding(context.Background(), b)
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	result, err := UpdateHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result, resultText(t, result)
}

// retractVia runs RetractHandler against an explicit binding.
func retractVia(t *testing.T, b *repos.Binding, args map[string]any) (*mcpgo.CallToolResult, string) {
	t.Helper()
	ctx := repos.WithBinding(context.Background(), b)
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	result, err := RetractHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result, resultText(t, result)
}

// readFactAt reads and parses a fact from ri's agent branch.
func readFactAt(t *testing.T, ri *repos.RepoInstance, path string) fact.Fact {
	t.Helper()
	s := storeIndices(ri)
	res, err := s.facts.ReadFact(context.Background(), "agent/test", path, nil)
	require.NoError(t, err)
	f, err := fact.ParseFact(path, res.Content)
	require.NoError(t, err)
	return f
}

// factExistsAt reports whether path exists on ri's agent branch.
func factExistsAt(t *testing.T, ri *repos.RepoInstance, path string) bool {
	t.Helper()
	s := storeIndices(ri)
	exists, err := s.facts.FactExists(context.Background(), "agent/test", path)
	require.NoError(t, err)
	return exists
}

// TestUpdate_QualifiedToWriteRepoEquivalentToBare: kb://<write-id>/<path> is
// exactly equivalent to the bare path (RFC §6.2) — the update lands, and the
// response echoes the BARE path (write-repo facts are unqualified everywhere).
func TestUpdate_QualifiedToWriteRepoEquivalentToBare(t *testing.T) {
	b, repoA, _, pathA, _ := twoRepoWriteBinding(t)

	qualified := qualifyPath(id12(repoA.ID()), pathA)
	result, text := updateVia(t, b, map[string]any{
		"file":        qualified,
		"moment_name": "bump",
		"updates":     map[string]any{"confidence": 0.42},
	})
	require.Falsef(t, result.IsError, "qualified-to-write update must succeed: %s", text)

	// The update actually landed on A's fact.
	require.Equal(t, 0.42, readFactAt(t, repoA, pathA).Confidence,
		"update via kb://<write-id>/ must mutate the write repo's fact")

	// Response "file" is the bare path, never kb://-qualified.
	var resp struct {
		File   string `json:"file"`
		Commit string `json:"commit"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.Equal(t, pathA, resp.File)
	require.NotContains(t, resp.File, kbScheme)
	require.NotEmpty(t, resp.Commit)
}

// TestUpdate_QualifiedToReadMountIsReadOnlyError: a qualified path to a read
// mount is a read-only-mount error, and the target fact is left untouched.
func TestUpdate_QualifiedToReadMountIsReadOnlyError(t *testing.T) {
	b, _, repoB, _, pathB := twoRepoWriteBinding(t)

	before := readFactAt(t, repoB, pathB)
	qualified := qualifyPath(id12(repoB.ID()), pathB)
	result, text := updateVia(t, b, map[string]any{
		"file":        qualified,
		"moment_name": "bump",
		"updates":     map[string]any{"confidence": 0.11},
	})
	require.True(t, result.IsError, "update to a read mount must be rejected")
	require.Contains(t, text, "read-only mount")
	require.Contains(t, text, id12(repoB.ID()))

	// B's fact is untouched — re-read to prove.
	require.Equal(t, before.Confidence, readFactAt(t, repoB, pathB).Confidence,
		"a rejected update must not mutate the read mount's fact")
}

// TestUpdate_UnmountedIDErrors: a qualified path to an unmounted repo fails with
// the shared not-mounted wording.
func TestUpdate_UnmountedIDErrors(t *testing.T) {
	b, _, _, _, _ := twoRepoWriteBinding(t)

	result, text := updateVia(t, b, map[string]any{
		"file":        qualifyPath("ffffffffffff", "kb/x.md"),
		"moment_name": "bump",
		"updates":     map[string]any{"confidence": 0.11},
	})
	require.True(t, result.IsError, "update to an unmounted ID must be rejected")
	require.Contains(t, text, "not mounted in this binding")
}

// TestRetract_QualifiedToWriteRepoEquivalentToBare: kb://<write-id>/<path> is
// exactly equivalent to bare — the retract lands (the fact is gone), and the
// response echoes the BARE path.
func TestRetract_QualifiedToWriteRepoEquivalentToBare(t *testing.T) {
	b, repoA, _, pathA, _ := twoRepoWriteBinding(t)
	require.True(t, factExistsAt(t, repoA, pathA), "precondition: fact exists")

	qualified := qualifyPath(id12(repoA.ID()), pathA)
	result, text := retractVia(t, b, map[string]any{
		"file":        qualified,
		"moment_name": "drop",
	})
	require.Falsef(t, result.IsError, "qualified-to-write retract must succeed: %s", text)

	require.False(t, factExistsAt(t, repoA, pathA),
		"retract via kb://<write-id>/ must delete the write repo's fact")

	var resp struct {
		File   string `json:"file"`
		Commit string `json:"commit"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.Equal(t, pathA, resp.File)
	require.NotContains(t, resp.File, kbScheme)
	require.NotEmpty(t, resp.Commit)
}

// TestRetract_QualifiedToReadMountIsReadOnlyError: a qualified retract to a read
// mount is a read-only-mount error, and the target fact still exists.
func TestRetract_QualifiedToReadMountIsReadOnlyError(t *testing.T) {
	b, _, repoB, _, pathB := twoRepoWriteBinding(t)

	qualified := qualifyPath(id12(repoB.ID()), pathB)
	result, text := retractVia(t, b, map[string]any{
		"file":        qualified,
		"moment_name": "drop",
	})
	require.True(t, result.IsError, "retract to a read mount must be rejected")
	require.Contains(t, text, "read-only mount")
	require.Contains(t, text, id12(repoB.ID()))

	require.True(t, factExistsAt(t, repoB, pathB),
		"a rejected retract must not delete the read mount's fact")
}

// TestRetract_UnmountedIDErrors: a qualified retract to an unmounted repo fails
// with the shared not-mounted wording.
func TestRetract_UnmountedIDErrors(t *testing.T) {
	b, _, _, _, _ := twoRepoWriteBinding(t)

	result, text := retractVia(t, b, map[string]any{
		"file":        qualifyPath("ffffffffffff", "kb/x.md"),
		"moment_name": "drop",
	})
	require.True(t, result.IsError, "retract to an unmounted ID must be rejected")
	require.Contains(t, text, "not mounted in this binding")
}
