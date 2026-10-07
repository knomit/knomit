package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// F24 M3: a set holding ONLY the template keys is not zero, and renders them
// after Run, Template before TemplateSource. Sabotage: leave Template or
// TemplateSource out of IsZero — the create's commit would be unstamped and
// nothing else would fail.
func TestTrailers_TemplateOnlySetIsStamped(t *testing.T) {
	for _, set := range []Trailers{{Template: "mission"}, {TemplateSource: "kb://abc@def"}} {
		require.False(t, set.IsZero(), "%+v must not be zero", set)
	}
	set := Trailers{Template: "mission", TemplateSource: "kb://034f37d5b4a5@" + strings.Repeat("a", 40), Run: "run-x"}
	got := appendTrailers("init: x", set)
	require.Equal(t, "init: x\n\nKnomit-Run: run-x\nKnomit-Template: mission\nKnomit-Template-Source: kb://034f37d5b4a5@"+strings.Repeat("a", 40)+"\n", got)
	require.Equal(t, "mission", TrailerValue(got, TrailerTemplate))

	// A ctx carrying a template set refuses an agent's trace on top of it,
	// exactly as it refuses one over a script's set.
	_, err := WithAgentTrace(WithTrailers(context.Background(), Trailers{Template: "m"}), Trailers{Trace: "t"})
	require.ErrorIs(t, err, ErrTraceOverride)
}

// WriteSystemTree keeps the case of every path, writes ONE commit signed by
// the store's signer and stamped with the ctx trailers, and refuses anything
// outside README.md and .knomit/ — before any object is written, so the
// branch does not move. Sabotage: lowercase the paths (SKILL.md and README.md
// assertions go red); drop the path guard (the kb/ and LICENSE rows go red).
func TestWriteSystemTree_ExactCaseOneSignedCommitNarrowPaths(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	before, err := svc.Triggers().UpstreamTip(ctx, "agent/a")
	require.NoError(t, err)

	for _, bad := range []map[string]string{
		{"kb/topic/x.md": "x"},
		{"LICENSE": "x"},
		{".gitmodules": "x"},
		{"readme.md": "x"},
		{"docs/README.md": "x"},
		{".knomit/": "x"},
		{"README.md": "a", ".knomit/x.md": "b", ".knomit/X.md": "c"},
	} {
		_, _, err := svc.WriteSystemTree(ctx, "agent/a", bad, "w", "created")
		require.ErrorIs(t, err, ErrSystemTreePath, "%v", bad)
	}
	tip, err := svc.Triggers().UpstreamTip(ctx, "agent/a")
	require.NoError(t, err)
	require.Equal(t, before, tip, "a refused write must not move the branch")

	files := map[string]string{
		"README.md":                         "# R\n",
		".knomit/ontology.yaml":             "id: x\n",
		".knomit/skills/work-task/SKILL.md": "S",
	}
	tctx := WithTrailers(ctx, Trailers{Template: "mission", TemplateSource: "kb://abc@def"})
	h, blobs, err := svc.WriteSystemTree(tctx, "agent/a", files, "init: from template mission", "created")
	require.NoError(t, err)
	require.Len(t, blobs, 3)
	hash := plumbing.NewHash(h)
	info, err := svc.Triggers().CommitInfo(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, []plumbing.Hash{before}, info.Parents, "ONE commit on top of the branch tip")
	require.Equal(t, "mission", TrailerValue(info.Message, TrailerTemplate))
	require.Equal(t, "kb://abc@def", TrailerValue(info.Message, TrailerTemplateSource))
	signer, err := svc.Triggers().CommitSignerOf(ctx, hash)
	require.NoError(t, err)
	require.Equal(t, svc.SignerFingerprint(), signer.Fingerprint)
	for p, want := range files {
		got, ok, err := svc.Triggers().BlobAt(ctx, hash, p)
		require.NoError(t, err)
		require.True(t, ok, "%s must exist with its exact case", p)
		require.Equal(t, want, got)
	}
	_, ok, err := svc.Triggers().BlobAt(ctx, hash, ".knomit/skills/work-task/skill.md")
	require.NoError(t, err)
	require.False(t, ok, "the path must not have been lowercased")
}

// TemplateFilesAt reads the commit's own tree under .knomit/templates/<name>/,
// keyed by the path each file lands at, and refuses by name: no folder, a
// path outside README.md and .knomit/, a case duplicate, too many files, too
// many bytes. TemplateFactsAt reads the direct *.md children of
// <root>/templates/<folder>/ only.
func TestTemplateFilesAt_ReadsAndRefuses(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	putRaw(t, svc, "main", ".knomit/templates/ok/README.md", "R")
	putRaw(t, svc, "main", ".knomit/templates/ok/.knomit/ontology.yaml", "O")
	putRaw(t, svc, "main", ".knomit/templates/ok/.knomit/skills/a/SKILL.md", "S")
	putRaw(t, svc, "main", ".knomit/templates/layout/README.md", "R")
	putRaw(t, svc, "main", ".knomit/templates/layout/kb/x/y.md", "F")
	putRaw(t, svc, "main", ".knomit/templates/root/LICENSE", "L")
	putRaw(t, svc, "main", ".knomit/templates/case/.knomit/a.md", "1")
	putRaw(t, svc, "main", ".knomit/templates/case/.knomit/A.md", "2")
	putRaw(t, svc, "main", ".knomit/templates/big/.knomit/big.bin", strings.Repeat("x", fact.TemplateMaxBytes+1))
	putRaw(t, svc, "main", "kb/templates/ok/aaaa1111.md", "fact")
	putRaw(t, svc, "main", "kb/templates/ok/files/readme/bbbb2222.md", "nested: not a candidate")
	tip := putRaw(t, svc, "main", "kb/templates/ok/notes.txt", "not md")

	got, err := svc.Templates().TemplateFilesAt(ctx, tip, "ok")
	require.NoError(t, err)
	var paths []string
	for _, f := range got {
		paths = append(paths, f.Path)
	}
	require.Equal(t, []string{".knomit/ontology.yaml", ".knomit/skills/a/SKILL.md", "README.md"}, paths)
	require.Equal(t, "S", string(got[1].Data))

	for name, want := range map[string]error{
		"absent": ErrTemplateNotFound,
		"layout": ErrTemplateLayout,
		"root":   ErrTemplateLayout,
		"case":   ErrTemplateLayout,
		"big":    ErrTemplateTooLarge,
		"../x":   ErrTemplateNotFound,
	} {
		_, err := svc.Templates().TemplateFilesAt(ctx, tip, name)
		require.True(t, errors.Is(err, want), "%s: want %v, got %v", name, want, err)
	}

	ok, err := svc.Templates().TemplateExistsAt(ctx, tip, "ok")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = svc.Templates().TemplateExistsAt(ctx, tip, "absent")
	require.NoError(t, err)
	require.False(t, ok)

	facts, err := svc.Templates().TemplateFactsAt(ctx, tip, "kb")
	require.NoError(t, err)
	require.Len(t, facts, 1)
	require.Equal(t, "ok", facts[0].Folder)
	require.Equal(t, "kb/templates/ok/aaaa1111.md", facts[0].Path)
}

// The limits are inclusive: exactly TemplateMaxFiles files and exactly
// TemplateMaxBytes bytes (summed over files) are accepted, one more of either
// is refused, never truncated (N4, #433 review).
// SABOTAGE: `>=` in either limit check → the "at" row of that limit red;
// allow one more → the "over" row red.
func TestTemplateFilesAt_LimitBoundaries(t *testing.T) {
	svc := newChangesService(t)
	ctx := context.Background()
	files := map[string]string{}
	for i := 0; i < fact.TemplateMaxFiles; i++ {
		files[fmt.Sprintf(".knomit/templates/files-at/.knomit/f%03d.md", i)] = "x"
		files[fmt.Sprintf(".knomit/templates/files-over/.knomit/f%03d.md", i)] = "x"
	}
	files[".knomit/templates/files-over/.knomit/one-more.md"] = "x"
	half := fact.TemplateMaxBytes / 2
	files[".knomit/templates/bytes-at/.knomit/a.bin"] = strings.Repeat("a", half)
	files[".knomit/templates/bytes-at/.knomit/b.bin"] = strings.Repeat("b", fact.TemplateMaxBytes-half)
	files[".knomit/templates/bytes-over/.knomit/a.bin"] = strings.Repeat("a", half)
	files[".knomit/templates/bytes-over/.knomit/b.bin"] = strings.Repeat("b", fact.TemplateMaxBytes-half+1)
	h, _, err := svc.Facts().BatchWriteFacts(ctx, "main", files, nil, "limits", "created")
	require.NoError(t, err)
	tip := plumbing.NewHash(h)

	got, err := svc.Templates().TemplateFilesAt(ctx, tip, "files-at")
	require.NoError(t, err, "exactly %d files is accepted", fact.TemplateMaxFiles)
	require.Len(t, got, fact.TemplateMaxFiles)
	_, err = svc.Templates().TemplateFilesAt(ctx, tip, "files-over")
	require.ErrorIs(t, err, ErrTemplateTooLarge)
	require.ErrorContains(t, err, "files")

	got, err = svc.Templates().TemplateFilesAt(ctx, tip, "bytes-at")
	require.NoError(t, err, "exactly %d bytes is accepted", fact.TemplateMaxBytes)
	total := 0
	for _, f := range got {
		total += len(f.Data)
	}
	require.Equal(t, fact.TemplateMaxBytes, total, "fixture: the files sum to the limit")
	_, err = svc.Templates().TemplateFilesAt(ctx, tip, "bytes-over")
	require.ErrorIs(t, err, ErrTemplateTooLarge)
	require.ErrorContains(t, err, "bytes")
}
