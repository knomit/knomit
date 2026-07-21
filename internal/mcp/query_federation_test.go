package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/retrieval"
	"knomit/internal/store"
)

// seedFedMany writes n policy-typed principle facts into the repo bound to ctx
// in one learn moment. Titles share titlePrefix; bodies get distinct lengths
// (bodyPrefix + i 'x's) so the length-based mock embedder yields distinct
// vectors and dedup does not collapse them. Used to force multi-page federated
// result sets with per-mount-identifiable rows.
func seedFedMany(t *testing.T, ctx context.Context, n int, titlePrefix, bodyPrefix, domain string) {
	t.Helper()
	facts := make([]any, n)
	for i := range n {
		facts[i] = map[string]any{
			"topic":      "principles",
			"category":   "mission/store",
			"title":      fmt.Sprintf("%s %04d", titlePrefix, i),
			"body":       bodyPrefix + strings.Repeat("x", i),
			"kind":       "pragmatic",
			"type":       "policy",
			"domain":     []any{domain},
			"confidence": 0.8,
			"sources":    1,
			"entities":   []any{"designer"},
			"refs":       []any{},
		}
	}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"moment_name": "seed-fed-" + titlePrefix, "facts": facts}
	result, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Falsef(t, result.IsError, "seed failed: %s", resultText(t, result))
}

// seedFedFact writes one principle-shaped fact (kind=pragmatic, type=policy,
// entities=[designer]) with the given title/domain/refs into the repo bound to
// ctx, returning the kb-relative path it was written to.
func seedFedFact(t *testing.T, ctx context.Context, moment, category, title, domain string, refs []string) string {
	t.Helper()
	refsAny := make([]any, len(refs))
	for i, r := range refs {
		refsAny[i] = r
	}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{
		"moment_name": moment,
		"facts": []any{
			map[string]any{
				"topic":      "principles",
				"category":   category,
				"title":      title,
				"body":       "designer authored " + title + ".",
				"kind":       "pragmatic",
				"type":       "policy",
				"domain":     []any{domain},
				"confidence": 0.8,
				"sources":    1,
				"entities":   []any{"designer"},
				"refs":       refsAny,
			},
		},
	}
	result, err := LearnHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Falsef(t, result.IsError, "seed failed: %s", resultText(t, result))
	path := findPathWithPrefix(resultText(t, result), "kb/principles/"+category+"/")
	require.NotEmpty(t, path, "could not locate written fact path in %s", resultText(t, result))
	return path
}

// fedRepo builds a fresh code-ontology repo and a context bound to it (for
// seeding via LearnHandler).
func fedRepo(t *testing.T) (*repos.RepoInstance, context.Context) {
	t.Helper()
	ri := newLearnTestRepo(t, fact.CodeOntology())
	return ri, repos.WithRepoInstance(context.Background(), ri)
}

// queryVia runs QueryHandler against an explicit binding.
func queryVia(t *testing.T, b *repos.Binding, args map[string]any) (*mcpgo.CallToolResult, string) {
	t.Helper()
	ctx := repos.WithBinding(context.Background(), b)
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	result, err := QueryHandler()(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	return result, resultText(t, result)
}

// factByTitle finds the result row with the given title.
func factByTitle(t *testing.T, resp queryResponse, title string) factOutput {
	t.Helper()
	for _, f := range resp.Facts {
		if f.Title == title {
			return f
		}
	}
	t.Fatalf("no result row titled %q in %+v", title, resp.Facts)
	return factOutput{}
}

// TestQueryFederation_QualifiedPaths: a lens over write repo A + read repo B
// returns A rows with bare paths and B rows kb://-qualified, with refs untouched.
func TestQueryFederation_QualifiedPaths(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, ctxB := fedRepo(t)

	pathA := seedFedFact(t, ctxA, "seed-a", "mission/store", "Alpha", "store", []string{"kb/decisions/a/ref.md"})
	pathB := seedFedFact(t, ctxB, "seed-b", "mission/ui", "Bravo", "ui", []string{"kb/decisions/b/ref.md"})

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	result, text := queryVia(t, b, map[string]any{"type": []any{"policy"}})
	require.Falsef(t, result.IsError, "query failed: %s", text)

	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.Len(t, resp.Facts, 2, "both mounts' facts must appear: %s", text)

	rowA := factByTitle(t, resp, "Alpha")
	rowB := factByTitle(t, resp, "Bravo")

	// A is the write repo → bare path, no scheme.
	require.Equal(t, pathA, rowA.File)
	require.NotContains(t, rowA.File, kbScheme)
	// B is a foreign read mount → kb://<id12(B)>/<path>.
	require.Equal(t, qualifyPath(id12(repoB.ID()), pathB), rowB.File)

	// Refs are returned exactly as stored — never rewritten to qualified form.
	require.Equal(t, []string{"kb/decisions/a/ref.md"}, rowA.Frontmatter.Refs)
	require.Equal(t, []string{"kb/decisions/b/ref.md"}, rowB.Frontmatter.Refs)
}

// TestQueryFederation_EmptyWriteMount drives the real Search fan-out where the
// write mount (A) matches zero facts while a foreign read mount (B) returns
// rows. The query must succeed, return only B's rows (each kb://-qualified to
// B), and not error — an empty write list must neither shrink the fused set to
// nothing nor fail the call. fuseRRF's empty-list handling is unit-tested; this
// pins the same behavior through the live goroutine fan-out.
func TestQueryFederation_EmptyWriteMount(t *testing.T) {
	repoA, _ := fedRepo(t)
	repoB, ctxB := fedRepo(t)
	pathB := seedFedFact(t, ctxB, "seed-b", "mission/ui", "Bravo", "ui", nil)

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	result, text := queryVia(t, b, map[string]any{"type": []any{"policy"}})
	require.Falsef(t, result.IsError, "a query with an empty write mount must succeed: %s", text)

	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.Lenf(t, resp.Facts, 1, "only the non-empty mount's rows may appear: %s", text)
	require.Equal(t, "Bravo", resp.Facts[0].Title)
	require.Equal(t, qualifyPath(id12(repoB.ID()), pathB), resp.Facts[0].File,
		"the B row must be kb://-qualified to repo B")
}

// TestQueryFederation_EmptyReadMount is the mirror of the above: the foreign
// read mount (B) matches zero facts while the write mount (A) returns rows. The
// query must succeed and return only A's rows, bare (never kb://-qualified) —
// an empty foreign list must not perturb the write mount's bare-path output.
func TestQueryFederation_EmptyReadMount(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, _ := fedRepo(t)
	pathA := seedFedFact(t, ctxA, "seed-a", "mission/store", "Alpha", "store", nil)

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	result, text := queryVia(t, b, map[string]any{"type": []any{"policy"}})
	require.Falsef(t, result.IsError, "a query with an empty read mount must succeed: %s", text)

	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.Lenf(t, resp.Facts, 1, "only the non-empty mount's rows may appear: %s", text)
	require.Equal(t, "Alpha", resp.Facts[0].Title)
	require.Equal(t, pathA, resp.Facts[0].File)
	require.NotContains(t, resp.Facts[0].File, kbScheme, "the A (write mount) row must be bare")
}

// TestQueryFederation_LensOfOneUnchanged: a lens-of-one produces byte-for-byte
// the same output as a direct single-repo query — no kb:// anywhere.
func TestQueryFederation_LensOfOneUnchanged(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	seedFedFact(t, ctxA, "seed-1", "mission/store", "One", "store", nil)
	seedFedFact(t, ctxA, "seed-2", "mission/ui", "Two", "ui", nil)

	// Direct single-repo path (synthesized lens-of-one).
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"type": []any{"policy"}}
	direct, err := QueryHandler()(ctxA, req)
	require.NoError(t, err)
	directText := resultText(t, direct)

	// Explicit lens-of-one binding over the same repo.
	b := repos.NewBindingForTest(repoA, repos.ReadTarget{RI: repoA, Branch: "agent/test"})
	_, lensText := queryVia(t, b, map[string]any{"type": []any{"policy"}})

	require.Equal(t, directText, lensText, "lens-of-one must be byte-for-byte identical to a direct query")
	require.NotContains(t, lensText, kbScheme, "lens-of-one output must never be kb://-qualified")
}

// TestQueryFederation_QualifiedPathFilterRestrictsMount: a kb://-qualified path
// filter restricts the query to that single mount; every returned row is
// qualified to it.
func TestQueryFederation_QualifiedPathFilterRestrictsMount(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, ctxB := fedRepo(t)
	seedFedFact(t, ctxA, "seed-a", "mission/store", "Alpha", "store", nil)
	pathB := seedFedFact(t, ctxB, "seed-b", "mission/ui", "Bravo", "ui", nil)

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	filter := qualifyPath(id12(repoB.ID()), "kb/")
	result, text := queryVia(t, b, map[string]any{"path": filter})
	require.Falsef(t, result.IsError, "query failed: %s", text)

	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.NotEmpty(t, resp.Facts, "the qualified mount's facts must be returned")
	for _, f := range resp.Facts {
		require.Truef(t, strings.HasPrefix(f.File, qualifyPath(id12(repoB.ID()), "")),
			"every row must be qualified to repo B: %s", f.File)
		require.Equal(t, "Bravo", f.Title, "only B's facts may appear")
	}
	require.Equal(t, qualifyPath(id12(repoB.ID()), pathB), resp.Facts[0].File)
}

// TestQueryFederation_UnmountedPathFilterErrors: a qualified filter naming a repo
// not in the binding fails loudly.
func TestQueryFederation_UnmountedPathFilterErrors(t *testing.T) {
	repoA, _ := fedRepo(t)
	repoB, _ := fedRepo(t)
	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	result, text := queryVia(t, b, map[string]any{"path": qualifyPath("aaaaaaaaaaaa", "kb/")})
	require.True(t, result.IsError, "unmounted qualified filter must error")
	require.Contains(t, text, "not mounted")
}

// TestQueryFederation_PagesAcrossMounts: a fused result set larger than one
// page walks the cursor to exhaustion; every B (foreign-mount) row stays
// kb://-qualified on every page including resumed ones, every A row stays bare,
// the union equals the full fused set, and has_more/cursor behave as today.
func TestQueryFederation_PagesAcrossMounts(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, ctxB := fedRepo(t)
	const perMount = 15 // 30 total > defaultPageSize (20) → forces a resumed page
	seedFedMany(t, ctxA, perMount, "Alpha", "alpha body ", "store")
	seedFedMany(t, ctxB, perMount, "Bravo", "bravo body ", "ui")

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	qualPrefix := qualifyPath(id12(repoB.ID()), "")

	seen := map[string]bool{}
	qualified, bare := 0, 0
	collect := func(facts []factOutput) {
		for _, f := range facts {
			require.Falsef(t, seen[f.File], "row %s returned twice across pages", f.File)
			seen[f.File] = true
			require.Greater(t, f.Score, 0.0, "score must be present on every page (incl. resumed)")
			if strings.HasPrefix(f.File, kbScheme) {
				require.Truef(t, strings.HasPrefix(f.File, qualPrefix), "B row must be qualified to repo B: %s", f.File)
				require.Truef(t, strings.HasPrefix(f.Title, "Bravo"), "qualified row must carry B's own title: %s", f.Title)
				qualified++
			} else {
				require.Truef(t, strings.HasPrefix(f.Title, "Alpha"), "bare row must carry A's own title: %s", f.Title)
				bare++
			}
		}
	}

	result, text := queryVia(t, b, map[string]any{"type": []any{"policy"}})
	require.Falsef(t, result.IsError, "query failed: %s", text)
	var first queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &first))
	require.Len(t, first.Facts, defaultPageSize, "first page must be a full page")
	require.True(t, first.HasMore, "more results remain")
	require.NotNil(t, first.Cursor, "cursor must be returned while more remain")
	collect(first.Facts)

	cursor := *first.Cursor
	for {
		result, text := queryVia(t, b, map[string]any{"cursor": cursor})
		require.Falsef(t, result.IsError, "resume failed: %s", text)
		var page queryResponse
		require.NoError(t, json.Unmarshal([]byte(text), &page))
		collect(page.Facts)
		if !page.HasMore {
			require.Nil(t, page.Cursor, "cursor must be nil once drained")
			break
		}
		require.NotNil(t, page.Cursor)
		cursor = *page.Cursor
	}

	require.Len(t, seen, 2*perMount, "every seeded fact must appear exactly once across pages")
	require.Equal(t, perMount, qualified, "every B row must be kb://-qualified")
	require.Equal(t, perMount, bare, "every A row must be bare")
}

// TestQueryFederation_ResumeHydratesFromCorrectMount: on a RESUMED page, a
// foreign-mount (B) row must carry B's own title and body — proving each item
// is re-read from its own mount's store, not the write store. A B-qualified rel
// path does not exist in A's store, so a mis-routed row would vanish; its
// presence with B's content is the routing proof.
func TestQueryFederation_ResumeHydratesFromCorrectMount(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, ctxB := fedRepo(t)
	const perMount = 15
	seedFedMany(t, ctxA, perMount, "Alpha", "alpha-marker body ", "store")
	seedFedMany(t, ctxB, perMount, "Bravo", "bravo-marker body ", "ui")

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	qualPrefix := qualifyPath(id12(repoB.ID()), "")

	result, text := queryVia(t, b, map[string]any{"type": []any{"policy"}, "include_body": true})
	require.Falsef(t, result.IsError, "query failed: %s", text)
	var first queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &first))
	require.NotNil(t, first.Cursor, "multi-page result must return a cursor")

	sawQualified, sawBare := false, false
	cursor := *first.Cursor
	for {
		result, text := queryVia(t, b, map[string]any{"cursor": cursor, "include_body": true})
		require.Falsef(t, result.IsError, "resume failed: %s", text)
		var page queryResponse
		require.NoError(t, json.Unmarshal([]byte(text), &page))
		for _, f := range page.Facts {
			if strings.HasPrefix(f.File, qualPrefix) {
				sawQualified = true
				require.Truef(t, strings.HasPrefix(f.Title, "Bravo"), "resumed B row must carry B's title: %s", f.Title)
				require.Contains(t, f.Body, "bravo-marker", "resumed B row must carry B's own body")
				require.NotContains(t, f.Body, "alpha-marker", "B row body must not be read from A's store")
			} else {
				sawBare = true
				require.Truef(t, strings.HasPrefix(f.Title, "Alpha"), "resumed A row must carry A's title: %s", f.Title)
				require.Contains(t, f.Body, "alpha-marker", "resumed A row must carry A's own body")
			}
		}
		if !page.HasMore {
			break
		}
		cursor = *page.Cursor
	}
	require.True(t, sawQualified, "a B (qualified) row must hydrate on a resumed page — proving mount routing")
	require.True(t, sawBare, "an A (bare) row must hydrate on a resumed page")
}

// TestQueryFederation_VanishedMountExpiresSession: a cursor minted over lens(A,B)
// resumed under a same-named binding that no longer mounts B must fail with the
// exact expiry message (RFC §7.3) — the frozen view no longer exists.
func TestQueryFederation_VanishedMountExpiresSession(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, ctxB := fedRepo(t)
	const perMount = 15 // guarantees B rows land in the snapshot ([20:30] under RRF)
	seedFedMany(t, ctxA, perMount, "Alpha", "alpha body ", "store")
	seedFedMany(t, ctxB, perMount, "Bravo", "bravo body ", "ui")

	full := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	result, text := queryVia(t, full, map[string]any{"type": []any{"policy"}})
	require.Falsef(t, result.IsError, "query failed: %s", text)
	var first queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &first))
	require.NotNil(t, first.Cursor, "multi-page query must return a cursor")

	// Same write repo → same binding name, so the binding-name and branch checks
	// pass and the resume reaches ByID, where B's absence is detected.
	shrunk := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
	)
	require.Equal(t, full.Name(), shrunk.Name(), "binding names must match to reach the ByID check")

	result, text = queryVia(t, shrunk, map[string]any{"cursor": *first.Cursor})
	require.True(t, result.IsError, "a vanished mount at resume must fail")
	require.Contains(t, text, "session expired or not found",
		"vanished-mount rejection must be byte-identical to real expiry")
}

// TestQueryFederation_RecentMergesByTimestamp: sort=recent over a lens(A,B)
// federates by a k-way committed_at merge (RFC §7.1 — timestamp merge, NOT RRF).
// Commits are interleaved across A and B with a sleep between each so every fact
// lands in a distinct committed_at second (committed_at is Unix-second
// resolution — cf. TestRecentFacts_* in internal/store). Commit order over
// wall-clock time is Alpha 0 (A), Bravo 0 (B), Alpha 1 (A), Bravo 1 (B), so the
// strict committed_at-DESC order is Bravo 1, Alpha 1, Bravo 0, Alpha 0 — an
// interleaving no per-mount concatenation could produce. limit=2 forces the
// 4-row merged set to page across the cursor; the strict order must hold across
// every page, with B rows kb://-qualified and A rows bare.
func TestQueryFederation_RecentMergesByTimestamp(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, ctxB := fedRepo(t)

	seedFedFact(t, ctxA, "seed-a0", "mission/store", "Alpha 0", "store", nil)
	time.Sleep(1100 * time.Millisecond)
	seedFedFact(t, ctxB, "seed-b0", "mission/ui", "Bravo 0", "ui", nil)
	time.Sleep(1100 * time.Millisecond)
	seedFedFact(t, ctxA, "seed-a1", "mission/store", "Alpha 1", "store", nil)
	time.Sleep(1100 * time.Millisecond)
	seedFedFact(t, ctxB, "seed-b1", "mission/ui", "Bravo 1", "ui", nil)

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/test"},
	)
	qualPrefix := qualifyPath(id12(repoB.ID()), "")
	want := []string{"Bravo 1", "Alpha 1", "Bravo 0", "Alpha 0"}

	var gotTitles []string
	checkRow := func(f factOutput) {
		if strings.HasPrefix(f.Title, "Bravo") {
			require.Truef(t, strings.HasPrefix(f.File, qualPrefix), "B row must be kb://-qualified: %s", f.File)
		} else {
			require.NotContainsf(t, f.File, kbScheme, "A row must be bare: %s", f.File)
		}
		gotTitles = append(gotTitles, f.Title)
	}

	result, text := queryVia(t, b, map[string]any{"sort": "recent", "limit": 2})
	require.Falsef(t, result.IsError, "recent query failed: %s", text)
	var first queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &first))
	require.Lenf(t, first.Facts, 2, "first page must hold limit=2 rows: %s", text)
	require.NotNil(t, first.Cursor, "more rows remain → cursor must be returned")
	for _, f := range first.Facts {
		checkRow(f)
	}

	cursor := *first.Cursor
	for {
		result, text := queryVia(t, b, map[string]any{"cursor": cursor})
		require.Falsef(t, result.IsError, "resume failed: %s", text)
		var page queryResponse
		require.NoError(t, json.Unmarshal([]byte(text), &page))
		for _, f := range page.Facts {
			checkRow(f)
		}
		if !page.HasMore {
			require.Nil(t, page.Cursor, "cursor must be nil once drained")
			break
		}
		require.NotNil(t, page.Cursor)
		cursor = *page.Cursor
	}

	require.Equal(t, want, gotTitles,
		"sort=recent must return the strict global committed_at-DESC order across all pages")
}

// TestQueryFederation_MountErrorFailsLoud: any mount whose Search errors fails
// the whole call (RFC §9.1). We force the error with a read mount pinned to a
// branch that does not exist in that repo's store, so its Search returns
// ErrBranchNotFound — a lens must never silently shrink its read set.
func TestQueryFederation_MountErrorFailsLoud(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	repoB, _ := fedRepo(t)
	seedFedFact(t, ctxA, "seed-a", "mission/store", "Alpha", "store", nil)

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: repoB, Branch: "agent/does-not-exist"},
	)
	result, text := queryVia(t, b, map[string]any{"type": []any{"policy"}})
	require.True(t, result.IsError, "a failing mount must fail the whole query")
	require.Contains(t, text, "search error")
}

// TestQueryFederation_PanickingMountFailsLoud pins the fix for the fan-out
// panic-recovery bug: a mount whose store is unavailable (svc == nil, e.g. an
// archive/shutdown race) makes storeIndices return a zero mcpStore whose index
// fields are nil interfaces, so the per-mount goroutine's sm.search.Search call
// panics. A bare fan-out goroutine's panic is NOT recovered by net/http (only
// the request goroutine is), so without recovery this crashes the whole process.
// The recovery must instead route the panic into the mount's error slot so it
// flows through the "any mount error fails the whole query" path (RFC §9.1) — a
// lens must never silently shrink its read set — yielding a tool error, not a
// crash. Covers the relevance (Search) fan-out site.
func TestQueryFederation_PanickingMountFailsLoud(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	seedFedFact(t, ctxA, "seed-a", "mission/store", "Alpha", "store", nil)

	// A bare test instance with no Svc → WithRead passes svc == nil → storeIndices
	// yields a zero mcpStore, so any index call on it panics.
	nosvc := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "nosvc", AgentBranch: "agent/test"})

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: nosvc, Branch: "agent/test"},
	)
	result, text := queryVia(t, b, map[string]any{"type": []any{"policy"}})
	require.True(t, result.IsError, "a panicking mount must fail the whole query, not crash")
	require.Contains(t, text, "search error")
	require.Contains(t, text, "panicked", "the panic must surface as an error, not be swallowed")
	require.Contains(t, text, "nosvc", "the error must name the offending mount")
}

// TestQueryFederation_RecentPanickingMountFailsLoud is the sort=recent twin of
// TestQueryFederation_PanickingMountFailsLoud: the RecentFacts fan-out site must
// recover a nil-svc mount's panic into its error slot rather than crashing.
func TestQueryFederation_RecentPanickingMountFailsLoud(t *testing.T) {
	repoA, ctxA := fedRepo(t)
	seedFedFact(t, ctxA, "seed-a", "mission/store", "Alpha", "store", nil)

	nosvc := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{Name: "nosvc", AgentBranch: "agent/test"})

	b := repos.NewBindingForTest(repoA,
		repos.ReadTarget{RI: repoA, Branch: "agent/test"},
		repos.ReadTarget{RI: nosvc, Branch: "agent/test"},
	)
	result, text := queryVia(t, b, map[string]any{"sort": "recent"})
	require.True(t, result.IsError, "a panicking mount must fail the whole recent query, not crash")
	require.Contains(t, text, "recent error")
	require.Contains(t, text, "panicked", "the panic must surface as an error, not be swallowed")
	require.Contains(t, text, "nosvc", "the error must name the offending mount")
}

// rankedFedEmbedder is a content-addressed embedder for federation ORDER tests.
// It dispatches on marker substrings shared by a fact's title/body and the
// query text (the same trick as store.rankedEmbedder), so relevance can be
// dialed independently of body length or commit time:
//   - a "match-target" doc and the "match-target" query embed identically → cosine 1.0
//   - a "weak-target" doc embeds to cosine 0.8 with that query (0.8/√(0.8²+0.6²))
//
// Both sit above the 0.40 recall floor (retrieval.Defaults), so both survive
// the search while "match" strictly outranks "weak".
type rankedFedEmbedder struct{}

func (rankedFedEmbedder) vec(text string) []float32 {
	out := make([]float32, 768)
	switch {
	case strings.Contains(text, "match-target"):
		out[0] = 1
	case strings.Contains(text, "weak-target"):
		out[0], out[1] = 0.8, 0.6
	default:
		out[2] = 1
	}
	return out
}

func (e rankedFedEmbedder) EmbedQuery(text string) ([]float32, error) { return e.vec(text), nil }
func (e rankedFedEmbedder) EmbedDocument(title, body string) ([]float32, error) {
	return e.vec(title + " " + body), nil
}
func (e rankedFedEmbedder) EmbedDocuments(titles, bodies []string) ([][]float32, error) {
	out := make([][]float32, len(titles))
	for i := range titles {
		out[i] = e.vec(titles[i] + " " + bodies[i])
	}
	return out, nil
}
func (rankedFedEmbedder) Dim() int                         { return 768 }
func (rankedFedEmbedder) ID() string                       { return "ranked-fed" }
func (rankedFedEmbedder) Thresholds() retrieval.Thresholds { return retrieval.Defaults() }

// rankedFedRepo builds a code-ontology repo wired with rankedFedEmbedder (so
// text search yields a deterministic relevance ranking) and a seeding context.
func rankedFedRepo(t *testing.T) (*repos.RepoInstance, context.Context) {
	t.Helper()
	dir := t.TempDir()
	svc, err := store.Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })
	emb := rankedFedEmbedder{}
	svc.SetEmbedder(emb)
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/test"))
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "test",
		AgentBranch:  "agent/test",
		Svc:          svc,
		Ontology:     fact.CodeOntology(),
		OntologyRoot: "kb",
		Embedder:     emb,
	})
	return ri, repos.WithRepoInstance(context.Background(), ri)
}

// TestQueryFederation_RecentWithTextPreservesRelevanceOrder pins the fix for the
// I-1 finding: sort=recent + a text query must federate by rank fusion, NOT by a
// global committed_at merge, so the store's relevance ordering
// (store.recentFactsSearch, guarded by TestRecentFacts_WithQuery_SortsByRelevanceNotDate)
// survives federation — even for a lens-of-one, where the pre-phase output was
// byte-for-byte relevance order. The older fact is the strong match; the newer
// fact is the weak match. A global timestamp merge would surface the newer weak
// fact first (the bug); relevance order (this assertion) surfaces the older
// strong match first.
func TestQueryFederation_RecentWithTextPreservesRelevanceOrder(t *testing.T) {
	ri, ctx := rankedFedRepo(t)

	// Older commit = the STRONG match; newer commit = the WEAK match.
	seedFedFact(t, ctx, "seed-strong", "mission/store", "match-target alpha strong", "store", nil)
	time.Sleep(1100 * time.Millisecond) // distinct committed_at seconds (Unix-second resolution)
	seedFedFact(t, ctx, "seed-weak", "mission/ui", "weak-target beta", "ui", nil)

	b := repos.NewBindingForTest(ri, repos.ReadTarget{RI: ri, Branch: "agent/test"})
	result, text := queryVia(t, b, map[string]any{"sort": "recent", "text": "match-target alpha"})
	require.Falsef(t, result.IsError, "recent+text query failed: %s", text)

	var resp queryResponse
	require.NoError(t, json.Unmarshal([]byte(text), &resp))
	require.Lenf(t, resp.Facts, 2, "both facts (strong + weak, both above recall floor) must appear: %s", text)
	require.Equal(t, "match-target alpha strong", resp.Facts[0].Title,
		"older-but-stronger match must come first — relevance order, not recency")
	require.Equal(t, "weak-target beta", resp.Facts[1].Title,
		"newer weak match must come last despite its later commit")
	require.NotContains(t, resp.Facts[0].File, kbScheme, "lens-of-one rows must be bare (never kb://-qualified)")
}
