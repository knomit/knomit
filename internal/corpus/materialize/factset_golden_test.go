package materialize

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

var updateFactSetGolden = flag.Bool("update-factset-golden", false, "rewrite testdata/factset_golden")

var (
	goldenSHA    = regexp.MustCompile(`\b[0-9a-f]{40}\b`)
	goldenAnchor = regexp.MustCompile(`\bf-[0-9a-f]{16}\b`)
	goldenDate   = regexp.MustCompile(`"date":"\d{4}-\d{2}-\d{2}"`)
	goldenUnix   = regexp.MustCompile(`"d":\d+`)
)

// normalizeGolden replaces what differs between two runs of one fixture and
// nothing else: the cursor id, the repo id (a nonce in the init commit), every
// 40-hex hash (commits carry a timestamp; blobs are mapped too) and every
// anchor (FactHash includes the repo id), each to a placeholder numbered by
// first appearance so equal values stay equal, and the dates.
func normalizeGolden(s, id, repo string) string {
	s = strings.ReplaceAll(s, id, "ID")
	seen := map[string]string{}
	s = goldenSHA.ReplaceAllStringFunc(s, func(h string) string {
		if p, ok := seen[h]; ok {
			return p
		}
		p := fmt.Sprintf("H%d", len(seen)+1)
		seen[h] = p
		return p
	})
	s = strings.ReplaceAll(s, repo, "REPO")
	anchors := map[string]string{}
	s = goldenAnchor.ReplaceAllStringFunc(s, func(a string) string {
		if p, ok := anchors[a]; ok {
			return p
		}
		p := fmt.Sprintf("F%d", len(anchors)+1)
		anchors[a] = p
		return p
	})
	s = goldenDate.ReplaceAllString(s, `"date":"DATE"`)
	return goldenUnix.ReplaceAllString(s, `"d":0`)
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "factset_golden", name)
	if *updateFactSetGolden {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(got+"\n"), 0o644))
		return
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err, "run with -update-factset-golden to create it")
	require.Equal(t, string(want), got+"\n", "%s differs from the golden", name)
}

// Pre-split identity of a repo FactSet. The frozen row's JSON bytes and the
// first page of the F1 example (depth 3, unlimited references, 5 per page),
// normalized only for what changes between runs (ids, hashes, anchors,
// dates), against goldens GENERATED ON THE PRE-SEAM CODE (dev 8f34e391,
// before refactor/factset-pinned-seam) and unchanged since. A change that
// alters what a repo FactSet freezes or serves, or how, fails here even when
// it alters the seam and FactSetFirstPage alike (which
// TestFactSet_Pinned_RepoPathIsByteIdentical cannot see).
//
// Sabotage: make freezePinned always set WireRepo, and the frozen golden
// differs ("wire_repo").
func TestFactSet_RepoFrozenRowAndFirstPageGolden(t *testing.T) {
	isolateFactSetGlobals(t)
	f := newFactSetFixture(t)
	seedABC(t, f)
	sc := f.scope("o")
	req := f.request("kb/u/")
	req.Options.Depth, req.Options.MaxNodes, req.Limit = 3, MaxFactSetFacts, 5
	p, _, err := FactSetFirstPage(context.Background(), sc, req)
	require.NoError(t, err)
	id := mustID(t, *p.Cursor)
	row, err := sc.Sessions.GetFactSet(context.Background(), id)
	require.NoError(t, err)
	require.NotNil(t, row)
	repo := f.id12()
	checkGolden(t, "frozen.json", normalizeGolden(string(row.Frozen), id, repo))
	checkGolden(t, "first_page.json", normalizeGolden(mustJSON(t, p), id, repo))
}

// Pre-split identity of an UNPINNED repo FactSet replayed after its branch
// moved (the F10 scenario): every page after a commit changed a ref target
// and the cache was dropped, against a golden generated on the pre-seam code
// (dev 8f34e391). Unpinned planning reads LiveAt AT THE ANCHOR, so this pins
// that the home mount still carries the frozen anchor on a resume.
//
// Sabotage: drop `m.anchor = hs.anchor` in reader.homeMount, and the replay
// differs from the golden.
func TestFactSet_RepoUnpinnedReplayGolden(t *testing.T) {
	isolateFactSetGlobals(t)
	f := newFactSetFixture(t)
	ctx := context.Background()
	f.write(t, "kb/z/s.md", "S", "s body.")
	f.write(t, "kb/z/t.md", "T", "t body.", "kb/z/s.md")
	f.write(t, "kb/z/a.md", "A", "a body.", "kb/z/s.md")
	f.write(t, "kb/z/b.md", "B", "b body.")
	// The search lists newest commit first, then by path, and commit times are
	// whole seconds: v is written BEFORE u so u (the path-first one) is newer
	// or tied, and u lists first whether or not the writes straddle a second.
	f.write(t, "kb/u/v.md", "V", "v body.", "kb/z/a.md", "kb/z/s.md")
	f.write(t, "kb/u/u.md", "U", "u body.", "kb/z/t.md", "kb/z/a.md", "kb/z/b.md")
	_, err := f.svc.Facts().DeleteFact(ctx, "agent/test", "kb/z/t.md", "retract t")
	require.NoError(t, err)
	req := f.request("kb/u/")
	req.Options.Unpin, req.Limit = true, 1
	p := f.first(t, "o", req)
	id := mustID(t, *p.Cursor)
	f.write(t, "kb/z/a.md", "A", "a body, changed after creation.")
	f.write(t, "kb/z/b.md", "B", "b body, changed after creation.", "kb/z/s.md")
	DropFactSetCacheForTest()
	pages := []string{mustJSON(t, p)}
	for p.HasMore {
		p, err = FactSetNextPage(ctx, f.scope("o"), f.b, nil, nil, *p.Cursor, 0)
		require.NoError(t, err)
		pages = append(pages, mustJSON(t, p))
	}
	checkGolden(t, "unpinned_replay.json", normalizeGolden(strings.Join(pages, "\n"), id, f.id12()))
	require.Greater(t, len(pages), 3, "several pages")
}
