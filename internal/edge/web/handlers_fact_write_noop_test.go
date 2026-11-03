package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/domain/fact"
)

// putContent PUTs content at path on the fixture's request branch and returns
// the decoded answer's unchanged flag.
func putContent(t *testing.T, f *factWriteFixture, path, content string) (unchanged bool, present bool) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"content": content})
	require.NoError(t, err)
	rec := f.do(http.MethodPut, f.factURL(path), string(body))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	v, ok := out["unchanged"]
	if !ok {
		return false, false
	}
	b, isBool := v.(bool)
	require.True(t, isBool, "unchanged is a boolean: %v", v)
	return b, true
}

func (f *factWriteFixture) tip(branch string) string {
	f.t.Helper()
	h, err := f.svc.HeadCommit(context.Background(), branch)
	require.NoError(f.t, err)
	return h
}

// A PUT of the bytes the fact already holds commits nothing (#432): 200 with
// `unchanged: true`, and the request branch's tip does not move. A PUT that
// changes the fact commits, and its answer carries no `unchanged` at all.
//
// SABOTAGE: drop the blob-hash comparison from writeFileExact -> the second
// PUT makes an empty commit and the tip moves -> red. SABOTAGE: drop
// `view.Unchanged = res.Unchanged` from handleFactUpdate -> the answer lacks
// unchanged -> red.
func TestHandleFactUpdate_IdenticalPutIsUnchanged(t *testing.T) {
	f := newFactWriteFixture(t, "kb")
	const path = "kb/ai/noop.md"
	content := factText(t, path, "No-op", 0.9, nil)

	unchanged, present := putContent(t, f, path, content)
	require.False(t, present, "a write that commits says nothing about unchanged")
	require.False(t, unchanged)
	tip := f.tip(restWriteBranch)

	unchanged, _ = putContent(t, f, path, content)
	require.True(t, unchanged, "the same bytes again: nothing was written")
	require.Equal(t, tip, f.tip(restWriteBranch), "the branch tip did not move")

	changed := factText(t, path, "No-op", 0.5, nil)
	unchanged, present = putContent(t, f, path, changed)
	require.False(t, present)
	require.False(t, unchanged)
	require.NotEqual(t, tip, f.tip(restWriteBranch), "a changed PUT commits")
}

// The comparison is of the bytes the handler COMMITS, not the request's: a
// stored version with a BARE local ref (kb/seed.md) re-PUT verbatim is
// rewritten by the refs gate to kb://<id>/kb/seed.md, so it commits once;
// the same request again then compares equal to the canonical version
// already stored and is unchanged, although its bytes still differ from what
// is on disk.
//
// SABOTAGE: compare body.Content (the request) with the stored blob -> the
// first PUT reads as unchanged and the bare ref stays -> red.
func TestHandleFactUpdate_BareRefPutCommitsOnceThenUnchanged(t *testing.T) {
	f := newFactWriteFixture(t, "kb")
	const path = "kb/ai/bare.md"
	bare := factText(t, path, "Bare ref", 0.9, func(x *fact.Fact) { x.Refs = []string{"kb/seed.md"} })
	f.seed(restWriteBranch, path, bare)
	tip := f.tip(restWriteBranch)

	unchanged, _ := putContent(t, f, path, bare)
	require.False(t, unchanged, "canonicalising the bare ref changes the committed bytes")
	require.NotEqual(t, tip, f.tip(restWriteBranch))
	stored, ok := f.read(restWriteBranch, path)
	require.True(t, ok)
	require.NotEqual(t, bare, stored)
	sf, err := fact.ParseFact(path, stored)
	require.NoError(t, err)
	require.Len(t, sf.Refs, 1)
	require.True(t, strings.HasPrefix(sf.Refs[0], "kb://"), "the ref was canonicalised: %v", sf.Refs)

	tip = f.tip(restWriteBranch)
	unchanged, _ = putContent(t, f, path, bare)
	require.True(t, unchanged, "the canonical version is already stored")
	require.Equal(t, tip, f.tip(restWriteBranch))
}
