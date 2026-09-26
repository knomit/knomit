package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// TestMergeFacts_ExpiresIsTheWinnersOwn: a dedup merge never invents,
// inherits or pools an expiry — the surviving identity keeps its own value.
// Mixed-type rows included: the winner's TYPE and its EXPIRES travel together.
func TestMergeFacts_ExpiresIsTheWinnersOwn(t *testing.T) {
	cases := []struct {
		name                 string
		newType, exType      fact.Type
		newExpires, exExpire string
		newWins              bool
		want                 string
	}{
		{"new wins, keeps its own", fact.Hypothesis, fact.Hypothesis, "2026-10-01T00:00:00Z", "2027-01-01T00:00:00Z", true, "2026-10-01T00:00:00Z"},
		{"existing wins, keeps its own", fact.Hypothesis, fact.Hypothesis, "2026-10-01T00:00:00Z", "2027-01-01T00:00:00Z", false, "2027-01-01T00:00:00Z"},
		{"existing wins with none: incoming not inherited", fact.Hypothesis, fact.Hypothesis, "2026-10-01T00:00:00Z", "", false, ""},
		{"new wins with none: existing not inherited", fact.Hypothesis, fact.Hypothesis, "", "2027-01-01T00:00:00Z", true, ""},
		{"mixed: observation winner does not take the hypothesis's date", fact.Hypothesis, fact.Observation, "2026-10-01T00:00:00Z", "", false, ""},
		{"mixed: observation winner keeps its own", fact.Observation, fact.Hypothesis, "2028-01-01T00:00:00Z", "2026-10-01T00:00:00Z", true, "2028-01-01T00:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nf := fact.NewFact("kb/alpha/new.md")
			nf.Title, nf.Body, nf.Type, nf.Sources, nf.Expires = "New", "New body", tc.newType, 1, tc.newExpires
			ex := fact.NewFact("kb/alpha/existing.md")
			ex.Title, ex.Body, ex.Type, ex.Sources, ex.Expires = "Existing", "Existing body", tc.exType, 1, tc.exExpire
			nf.Confidence, ex.Confidence = 0.5, 0.9
			if tc.newWins {
				nf.Confidence, ex.Confidence = 0.9, 0.5
			}
			require.Equal(t, tc.newWins, newFactWins(nf, ex), "fixture must produce the intended winner")
			merged := mergeFacts(nf, ex, testLocalID)
			require.Equal(t, tc.want, merged.Expires)
			if tc.newWins {
				require.Equal(t, tc.newType, merged.Type)
			} else {
				require.Equal(t, tc.exType, merged.Type)
			}
		})
	}
}

func expiresLearnReq(title, body, typ string, confidence float64, expires string) mcpgo.CallToolRequest {
	item := map[string]any{
		"topic": "decisions", "category": "weighting",
		"title": title, "body": body, "type": typ,
		"domain": []any{"global"}, "confidence": confidence, "sources": 1,
		"entities": []any{"designer"}, "refs": []any{},
	}
	if expires != "" {
		item["expires"] = expires
	}
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"moment_name": "expires", "facts": []any{item}}
	return req
}

func TestLearnHandler_ExpiresPersistsAndIsValidated(t *testing.T) {
	svc, ctx, emb := newPrinciplesTestRepo(t)

	res, err := LearnHandler(emb)(ctx, expiresLearnReq("Dated claim", "Settles by October.", "hypothesis", 0.6, "2026-10-01T00:00:00Z"))
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	got, err := svc.Facts().ReadFact(context.Background(), "agent/test", mergedFactPath(t, res), nil)
	require.NoError(t, err)
	require.Contains(t, got.Content, "\nexpires: \"2026-10-01T00:00:00Z\"\n")

	res, err = LearnHandler(emb)(ctx, expiresLearnReq("Undated claim", "Date-only is refused.", "hypothesis", 0.6, "2026-10-01"))
	require.NoError(t, err)
	require.True(t, res.IsError, "date-only expires must be refused")
	require.Contains(t, resultText(t, res), "invalid expires")
}

// TestLearnHandler_DedupExistingWins_SaysExpiresNotApplied: when the merge
// keeps the EXISTING fact, the caller's explicit expires does not land, and
// the result says so instead of dropping it silently.
func TestLearnHandler_DedupExistingWins_SaysExpiresNotApplied(t *testing.T) {
	svc, ctx, emb := newPrinciplesTestRepo(t)
	bg := context.Background()

	const targetLen = 120
	existingTitle := "Weighted synthesis"
	existingBody := lenMatched(t, existingTitle, "the corpus already holds this claim", targetLen)
	_, err := svc.Facts().WriteFact(bg, "agent/test", weightedExistingPath,
		weightedFactContent(t, existingTitle, existingBody, 0.95, 0), "seed", "test")
	require.NoError(t, err)

	incomingTitle := "Weighted synthesis restated"
	incomingBody := lenMatched(t, incomingTitle, "the same claim, said again", targetLen)
	res, err := LearnHandler(emb)(ctx, expiresLearnReq(incomingTitle, incomingBody, "synthesis", 0.5, "2026-10-01T00:00:00Z"))
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	require.Equal(t, weightedExistingPath, mergedFactPath(t, res), "fixture: must merge INTO the existing fact")

	merged, err := svc.Facts().ReadFact(bg, "agent/test", weightedExistingPath, nil)
	require.NoError(t, err)
	require.Contains(t, merged.Content, existingTitle+"\n", "fixture: the EXISTING fact must have won")
	require.NotContains(t, merged.Content, "expires", "existing fact kept: the incoming expires is not applied")

	text := resultText(t, res)
	require.Contains(t, text, fmt.Sprintf("existing fact kept; expires not applied; set it with knomit_update on %s", weightedExistingPath))
}

// TestUpdateHandler_ExpiresSetChangeClear: set, change, leave (omitted), and
// clear with an explicit "".
func TestUpdateHandler_ExpiresSetChangeClear(t *testing.T) {
	svc, ctx, emb := newPrinciplesTestRepo(t)
	seed, err := LearnHandler(emb)(ctx, expiresLearnReq("Dated claim", "Settles later.", "hypothesis", 0.6, ""))
	require.NoError(t, err)
	require.False(t, seed.IsError, resultText(t, seed))
	path := mergedFactPath(t, seed)

	read := func() string {
		r, err := svc.Facts().ReadFact(context.Background(), "agent/test", path, nil)
		require.NoError(t, err)
		return r.Content
	}
	update := func(updates map[string]any) *mcpgo.CallToolResult {
		var req mcpgo.CallToolRequest
		req.Params.Arguments = map[string]any{"file": path, "moment_name": "exp", "updates": updates}
		res, err := UpdateHandler()(ctx, req)
		require.NoError(t, err)
		return res
	}

	require.False(t, update(map[string]any{"expires": "2026-10-01T00:00:00Z"}).IsError)
	require.Contains(t, read(), "expires: \"2026-10-01T00:00:00Z\"")

	require.False(t, update(map[string]any{"expires": "2027-01-01T00:00:00+01:00"}).IsError)
	require.Contains(t, read(), "expires: \"2027-01-01T00:00:00+01:00\"")

	require.False(t, update(map[string]any{"confidence": 0.7}).IsError)
	require.Contains(t, read(), "expires: \"2027-01-01T00:00:00+01:00\"", "omitting expires leaves it unchanged")

	res := update(map[string]any{"expires": "next week"})
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "invalid expires")

	require.False(t, update(map[string]any{"expires": ""}).IsError)
	require.NotContains(t, read(), "expires", "explicit empty string clears it")
}

func queryFacts(t *testing.T, ctx context.Context, args map[string]any) (queryResponse, *mcpgo.CallToolResult) {
	t.Helper()
	var req mcpgo.CallToolRequest
	req.Params.Arguments = args
	res, err := QueryHandler()(ctx, req)
	require.NoError(t, err)
	var out queryResponse
	if !res.IsError {
		require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &out))
	}
	return out, res
}

// seedDated learns one fact per (category, expires) pair; distinct categories
// keep learn's category-scoped dedup from merging them.
func seedDated(t *testing.T) (context.Context, map[string]string) {
	t.Helper()
	_, ctx, emb := newPrinciplesTestRepo(t)
	now := time.Now().UTC()
	dates := map[string]string{
		"past":  now.Add(-48 * time.Hour).Format(time.RFC3339),
		"soon":  now.Add(24 * time.Hour).Format(time.RFC3339),
		"later": now.Add(30 * 24 * time.Hour).Format(time.RFC3339),
		"never": "",
	}
	paths := map[string]string{}
	for name, exp := range dates {
		req := expiresLearnReq("Claim "+name, "Body for the "+name+" claim.", "hypothesis", 0.6, exp)
		req.Params.Arguments.(map[string]any)["facts"].([]any)[0].(map[string]any)["category"] = "dated/" + name
		res, err := LearnHandler(emb)(ctx, req)
		require.NoError(t, err)
		require.False(t, res.IsError, resultText(t, res))
		paths[name] = mergedFactPath(t, res)
	}
	return ctx, paths
}

func filesOf(r queryResponse) map[string]factOutput {
	m := map[string]factOutput{}
	for _, f := range r.Facts {
		m[f.File] = f
	}
	return m
}

// TestQuery_ExpiryFiltersAndMarker: nothing is hidden by default, expired rows
// are marked, and each filter keeps exactly the rows its documented semantics
// say — including what happens to a fact with no expires.
func TestQuery_ExpiryFiltersAndMarker(t *testing.T) {
	ctx, p := seedDated(t)
	base := map[string]any{"path": "kb/decisions/dated/", "limit": 20}
	with := func(kv ...any) map[string]any {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}

	all, _ := queryFacts(t, ctx, with())
	got := filesOf(all)
	require.Len(t, got, 4, "no filter: nothing hidden")
	require.True(t, got[p["past"]].Expired)
	require.NotEmpty(t, got[p["past"]].Frontmatter.Expires)
	for _, n := range []string{"soon", "later", "never"} {
		require.False(t, got[p[n]].Expired, n)
	}
	require.Empty(t, got[p["never"]].Frontmatter.Expires)

	keys := func(r queryResponse) []string {
		var out []string
		for f := range filesOf(r) {
			for n, path := range p {
				if path == f {
					out = append(out, n)
				}
			}
		}
		sort.Strings(out)
		return out
	}
	now := time.Now().UTC()
	r, _ := queryFacts(t, ctx, with("expired", true))
	require.Equal(t, []string{"past"}, keys(r))
	r, _ = queryFacts(t, ctx, with("expired", false))
	require.Equal(t, []string{"later", "never", "soon"}, keys(r), "expired=false INCLUDES the undated")
	r, _ = queryFacts(t, ctx, with("expires_before", now.Add(7*24*time.Hour).Format(time.RFC3339)))
	require.Equal(t, []string{"past", "soon"}, keys(r), "expires_before alone EXCLUDES the undated")
	r, _ = queryFacts(t, ctx, with("expires_after", now.Format(time.RFC3339), "expires_before", now.Add(7*24*time.Hour).Format(time.RFC3339)))
	require.Equal(t, []string{"soon"}, keys(r), "window idiom")

	// An expiry-only query (no other filter) is a valid query.
	r, res := queryFacts(t, ctx, map[string]any{"expired": true})
	require.False(t, res.IsError, resultText(t, res))
	require.Contains(t, filesOf(r), p["past"])

	_, res = queryFacts(t, ctx, with("expires_before", "2026-10-01"))
	require.True(t, res.IsError)
	require.Contains(t, resultText(t, res), "expires_before must be an RFC 3339 timestamp")
}

// TestQuery_CursorPagesKeepTheQueryClock: every page of a cursor marks rows
// against the instant the snapshot was built, recorded per row.
func TestQuery_CursorPagesKeepTheQueryClock(t *testing.T) {
	ctx, p := seedDated(t)
	first, _ := queryFacts(t, ctx, map[string]any{"path": "kb/decisions/dated/", "limit": 1, "sort": "recent"})
	require.NotNil(t, first.Cursor, "fixture: must page")
	seen := filesOf(first)
	cur := first.Cursor
	for cur != nil {
		next, res := queryFacts(t, ctx, map[string]any{"cursor": *cur, "limit": 1})
		require.False(t, res.IsError, resultText(t, res))
		for k, v := range filesOf(next) {
			seen[k] = v
		}
		cur = next.Cursor
	}
	require.Len(t, seen, 4)
	require.True(t, seen[p["past"]].Expired, "a resumed page marks expired rows")
	require.False(t, seen[p["soon"]].Expired)

	// The clock a resumed page uses is the recorded one, not page-serve time.
	asOf := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	require.Equal(t, asOf, snapshotClock(pagedRowState{AsOf: asOf.Unix()}).UTC())
	f := fact.NewFact("kb/a/b.md")
	f.Expires = "2026-06-01T00:00:00Z"
	require.False(t, buildFactOutputFromFact(f, "kb/a/b.md", "c", 1, 0, false, asOf).Expired,
		"at the snapshot's clock (January) a June expiry has not passed, whatever today is")
	require.True(t, buildFactOutputFromFact(f, "kb/a/b.md", "c", 1, 0, false, asOf.AddDate(1, 0, 0)).Expired)
}

// TestExplain_MarksExpired: by-path reads carry the value and the marker.
func TestExplain_MarksExpired(t *testing.T) {
	ctx, p := seedDated(t)
	var req mcpgo.CallToolRequest
	req.Params.Arguments = map[string]any{"file": p["past"]}
	res, err := ExplainHandler()(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	var out struct {
		Facts []explainFactEntry `json:"facts"`
	}
	require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &out))
	require.NotEmpty(t, out.Facts)
	require.True(t, out.Facts[0].Expired)
	require.NotEmpty(t, out.Facts[0].Expires)
}

// pinMCPClock fixes this package's clock for one test.
func pinMCPClock(t *testing.T, at time.Time) {
	t.Helper()
	prev := timeNow
	timeNow = func() time.Time { return at }
	t.Cleanup(func() { timeNow = prev })
}

// TestQuery_FirstPageFilterAndMarkerShareOneClock pins the one-clock contract
// on the FIRST page (served from search results, not from a snapshot) at the
// boundary: expires == now is both selected by expired=true AND marked; one
// second earlier it is neither. A marker or filter reading any other clock —
// time.Now(), or q.Now skewed — fails one of the two halves.
func TestQuery_FirstPageFilterAndMarkerShareOneClock(t *testing.T) {
	_, ctx, emb := newPrinciplesTestRepo(t)
	at := time.Date(2031, 5, 4, 3, 2, 1, 0, time.UTC)
	req := expiresLearnReq("Edge claim", "Due exactly at the pinned clock.", "hypothesis", 0.6, at.Format(time.RFC3339))
	req.Params.Arguments.(map[string]any)["facts"].([]any)[0].(map[string]any)["category"] = "dated/edge"
	res, err := LearnHandler(emb)(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	path := mergedFactPath(t, res)

	pinMCPClock(t, at)
	r, _ := queryFacts(t, ctx, map[string]any{"path": "kb/decisions/dated/edge/", "expired": true})
	require.Nil(t, r.Cursor, "fixture: one page, served from search results")
	got := filesOf(r)
	require.Contains(t, got, path, "at expires == now the filter selects it")
	require.True(t, got[path].Expired, "and the marker, from the same clock, agrees")

	pinMCPClock(t, at.Add(-time.Second))
	r, _ = queryFacts(t, ctx, map[string]any{"path": "kb/decisions/dated/edge/", "expired": true})
	require.Empty(t, r.Facts, "one second before, the filter excludes it")
	r, _ = queryFacts(t, ctx, map[string]any{"path": "kb/decisions/dated/edge/"})
	require.Contains(t, filesOf(r), path)
	require.False(t, filesOf(r)[path].Expired, "and the unfiltered row is not marked")
}

// TestExplain_SummaryNodesMarkExpired: an expired fact cited by the root shows
// up as a SUMMARY node, and the marker must be on it too.
func TestExplain_SummaryNodesMarkExpired(t *testing.T) {
	_, ctx, emb := newPrinciplesTestRepo(t)
	at := time.Date(2031, 5, 4, 3, 2, 1, 0, time.UTC)
	cited := expiresLearnReq("Cited prediction", "It was due.", "hypothesis", 0.6, at.Add(-time.Hour).Format(time.RFC3339))
	cited.Params.Arguments.(map[string]any)["facts"].([]any)[0].(map[string]any)["category"] = "dated/cited"
	res, err := LearnHandler(emb)(ctx, cited)
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	citedPath := mergedFactPath(t, res)

	root := expiresLearnReq("Root claim", "Rests on the prediction.", "observation", 0.8, "")
	item := root.Params.Arguments.(map[string]any)["facts"].([]any)[0].(map[string]any)
	item["category"] = "dated/root"
	item["refs"] = []any{citedPath}
	res, err = LearnHandler(emb)(ctx, root)
	require.NoError(t, err)
	require.False(t, res.IsError, resultText(t, res))
	rootPath := mergedFactPath(t, res)

	pinMCPClock(t, at)
	var summary *explainFactEntry
	args := map[string]any{"file": rootPath}
	for i := 0; i < 10 && summary == nil; i++ {
		var req mcpgo.CallToolRequest
		req.Params.Arguments = args
		res, err := ExplainHandler()(ctx, req)
		require.NoError(t, err)
		require.False(t, res.IsError, resultText(t, res))
		var out struct {
			Cursor  string             `json:"cursor"`
			Facts   []explainFactEntry `json:"facts"`
			HasMore bool               `json:"has_more"`
		}
		require.NoError(t, json.Unmarshal([]byte(resultText(t, res)), &out))
		for i := range out.Facts {
			// Summary paths are wire paths; for the write repo that is the bare path.
			if out.Facts[i].Summary && strings.HasSuffix(out.Facts[i].Path, citedPath) {
				summary = &out.Facts[i]
			}
		}
		if !out.HasMore {
			break
		}
		args = map[string]any{"file": rootPath, "cursor": out.Cursor}
	}
	require.NotNil(t, summary, "fixture: the cited fact must appear as a summary node")
	require.True(t, summary.Expired, "the summary node carries the marker")
	require.Equal(t, at.Add(-time.Hour).Format(time.RFC3339), summary.Expires)
}
