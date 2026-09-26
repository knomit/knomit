package store

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// expNow is the fixed read clock every expiry test anchors on.
var expNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func writeExpiresFact(t *testing.T, svc *Service, branch, path, typ, expires string) {
	t.Helper()
	f := fact.NewFact(path)
	f.Title = "T " + path
	f.Body = "Body of " + path
	f.Type = fact.Type(typ)
	f.Domain = []string{"alpha"}
	f.Entities = []string{"Widget"}
	f.Refs = []string{}
	f.Confidence = 0.8
	f.Sources = 1
	f.Expires = expires
	body, err := fact.SerializeFact(f)
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(context.Background(), branch, f.Path(), body, "seed", "")
	require.NoError(t, err)
}

// expiresFixture: one expired hypothesis, two future ones, one undated fact.
func expiresFixture(t *testing.T) (*Service, string) {
	svc, branch := motifEnv(t)
	writeExpiresFact(t, svc, branch, "kb/alpha/past.md", "hypothesis", expNow.Add(-time.Hour).Format(time.RFC3339))
	writeExpiresFact(t, svc, branch, "kb/alpha/soon.md", "hypothesis", expNow.Add(24*time.Hour).In(time.FixedZone("x", 2*3600)).Format(time.RFC3339))
	writeExpiresFact(t, svc, branch, "kb/alpha/later.md", "observation", expNow.Add(10*24*time.Hour).Format(time.RFC3339))
	writeExpiresFact(t, svc, branch, "kb/alpha/never.md", "observation", "")
	return svc, branch
}

func searchPaths(t *testing.T, svc *Service, branch string, q SearchOptions) []string {
	t.Helper()
	if q.Now.IsZero() {
		q.Now = expNow
	}
	res, err := svc.fq.Search(context.Background(), branch, q)
	require.NoError(t, err)
	var out []string
	for _, r := range res {
		out = append(out, r.Path)
	}
	sort.Strings(out)
	return out
}

func recentPaths(t *testing.T, svc *Service, branch string, q SearchOptions) []string {
	t.Helper()
	if q.Now.IsZero() {
		q.Now = expNow
	}
	q.Limit = 50
	res, _, err := svc.fq.RecentFacts(context.Background(), branch, q)
	require.NoError(t, err)
	var out []string
	for _, r := range res {
		out = append(out, r.Path)
	}
	sort.Strings(out)
	return out
}

func boolp(b bool) *bool { return &b }

// TestSearch_ExpiryFilters pins every filter's semantics on BOTH shared-filter
// entry points (Search and RecentFacts), including the "expiring within a
// window" idiom and the treatment of a fact with no expires.
func TestSearch_ExpiryFilters(t *testing.T) {
	svc, branch := expiresFixture(t)
	all := []string{"kb/alpha/later.md", "kb/alpha/never.md", "kb/alpha/past.md", "kb/alpha/soon.md"}
	cases := []struct {
		name string
		q    SearchOptions
		want []string
	}{
		{"no filter hides nothing", SearchOptions{}, all},
		{"expired=true", SearchOptions{Expired: boolp(true)}, []string{"kb/alpha/past.md"}},
		{"expired=false includes the undated", SearchOptions{Expired: boolp(false)},
			[]string{"kb/alpha/later.md", "kb/alpha/never.md", "kb/alpha/soon.md"}},
		{"expires_before alone excludes the undated", SearchOptions{ExpiresBefore: expNow.Add(48 * time.Hour)},
			[]string{"kb/alpha/past.md", "kb/alpha/soon.md"}},
		{"expires_after alone excludes the undated", SearchOptions{ExpiresAfter: expNow},
			[]string{"kb/alpha/later.md", "kb/alpha/soon.md"}},
		{"window idiom: after=now, before=now+2d", SearchOptions{ExpiresAfter: expNow, ExpiresBefore: expNow.Add(48 * time.Hour)},
			[]string{"kb/alpha/soon.md"}},
		{"type still composes", SearchOptions{Expired: boolp(true), IncludeTypes: []string{"observation"}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, searchPaths(t, svc, branch, tc.q), "Search")
			require.Equal(t, tc.want, recentPaths(t, svc, branch, tc.q), "RecentFacts")
		})
	}
}

// TestSearch_ExpiredBoundaryIsInclusive: expires == now is expired.
func TestSearch_ExpiredBoundaryIsInclusive(t *testing.T) {
	svc, branch := motifEnv(t)
	writeExpiresFact(t, svc, branch, "kb/alpha/edge.md", "hypothesis", expNow.Format(time.RFC3339))
	require.Equal(t, []string{"kb/alpha/edge.md"}, searchPaths(t, svc, branch, SearchOptions{Expired: boolp(true)}))
	require.Empty(t, searchPaths(t, svc, branch, SearchOptions{Expired: boolp(true), Now: expNow.Add(-time.Second)}),
		"one second before its expiry the fact is not expired")
}

// TestSearch_ResultsCarryExpiresAsWritten: result rows carry the stored
// string (offset kept) on every scanner.
func TestSearch_ResultsCarryExpiresAsWritten(t *testing.T) {
	svc, branch := expiresFixture(t)
	ctx := context.Background()
	want := expNow.Add(24 * time.Hour).In(time.FixedZone("x", 2*3600)).Format(time.RFC3339)

	res, err := svc.fq.Search(ctx, branch, SearchOptions{Path: "kb/alpha/soon.md"})
	require.NoError(t, err)
	require.Len(t, res, 1)
	require.Equal(t, want, res[0].Expires)

	rec, _, err := svc.fq.RecentFacts(ctx, branch, SearchOptions{Path: "kb/alpha/soon.md", Limit: 5})
	require.NoError(t, err)
	require.Len(t, rec, 1)
	require.Equal(t, want, rec[0].Expires)

	got, err := svc.fq.GetByPath(ctx, branch, "kb/alpha/soon.md")
	require.NoError(t, err)
	require.Equal(t, want, got.Expires)

	got, err = svc.fq.GetByPath(ctx, branch, "kb/alpha/never.md")
	require.NoError(t, err)
	require.Equal(t, "", got.Expires)
}

// TestFactExpires_RebuildRepopulates: the bulk rebuild fills both columns
// from the blobs, not only the incremental upsert (the 000019 lesson). The
// columns are NULLed first, or the assertion would pass vacuously.
func TestFactExpires_RebuildRepopulates(t *testing.T) {
	svc, branch := expiresFixture(t)
	ctx := context.Background()
	_, err := svc.si.rh.db.ExecContext(ctx, `DELETE FROM fact_expires`)
	require.NoError(t, err)
	require.Empty(t, searchPaths(t, svc, branch, SearchOptions{Expired: boolp(true)}),
		"clearing must empty the side table, or the rebuild below proves nothing")

	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))

	require.Equal(t, []string{"kb/alpha/past.md"}, searchPaths(t, svc, branch, SearchOptions{Expired: boolp(true)}))
	var n int
	require.NoError(t, svc.si.rh.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM fact_expires`).Scan(&n))
	require.Equal(t, 3, n)
}

// TestFactExpires_UpgradeFromV5ForcesRebuild is the backfill: a repo indexed
// by a pre-F03 build (graph schema "5", no fact_expires rows even though a blob
// carries `expires:`) reports NeedsRebuild, and that rebuild — what repo open
// runs — fills the columns.
func TestFactExpires_UpgradeFromV5ForcesRebuild(t *testing.T) {
	svc, branch := expiresFixture(t)
	ctx := context.Background()
	// Precondition: at THIS binary's version the branch is current, so the
	// staleness below comes from the version alone.
	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))
	stale, err := svc.IndexManager().NeedsRebuild(ctx, branch)
	require.NoError(t, err)
	require.False(t, stale, "fixture: a freshly rebuilt branch must be current")

	_, err = svc.si.rh.db.ExecContext(ctx, `DELETE FROM fact_expires`)
	require.NoError(t, err)
	res, err := svc.si.rh.db.ExecContext(ctx, `UPDATE meta SET value = '5' WHERE key = ?`, schemaVersionKey(branch))
	require.NoError(t, err)
	n, _ := res.RowsAffected()
	require.EqualValues(t, 1, n, "fixture: the version row must exist to be downgraded")

	stale, err = svc.IndexManager().NeedsRebuild(ctx, branch)
	require.NoError(t, err)
	require.True(t, stale, "an index built at graph schema 5 must be rebuilt to pick up expires")

	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))
	require.Equal(t, []string{"kb/alpha/past.md"}, searchPaths(t, svc, branch, SearchOptions{Expired: boolp(true)}))
}

// TestHighlights_MarkExpired: highlights and Stats bypass newFactFilter.
// Nothing is hidden, so they need no filter, but an expired fact they return
// must carry the marker.
func TestHighlights_MarkExpired(t *testing.T) {
	// Highlights judge against the real clock (they take no SearchOptions),
	// so these dates are relative to it, not to expNow.
	svc, branch := motifEnv(t)
	real := time.Now().UTC()
	writeExpiresFact(t, svc, branch, "kb/alpha/past.md", "hypothesis", real.Add(-time.Hour).Format(time.RFC3339))
	writeExpiresFact(t, svc, branch, "kb/alpha/soon.md", "hypothesis", real.Add(24*time.Hour).Format(time.RFC3339))
	ctx := context.Background()

	check := func(hs []Highlight, where string) {
		byPath := map[string]Highlight{}
		for _, h := range hs {
			byPath[h.Path] = h
		}
		past, ok := byPath["kb/alpha/past.md"]
		require.True(t, ok, "%s: the expired fact is still highlighted (nothing hidden): %v", where, hs)
		require.True(t, past.Expired, where)
		require.NotEmpty(t, past.Expires, where)
		soon, ok := byPath["kb/alpha/soon.md"]
		require.True(t, ok, where)
		require.False(t, soon.Expired, where)
	}
	hs, _, err := svc.fq.Highlights(ctx, branch, "kb/alpha/", AxisRecent)
	require.NoError(t, err)
	check(hs, "Highlights")

	st, err := svc.fq.Stats(ctx, branch, "kb/alpha/", AxisRecent)
	require.NoError(t, err)
	check(st.Highlights, "Stats")
	require.Equal(t, 2, st.Types["hypothesis"], "Stats counts are unfiltered: both hypotheses, the expired one included")
}

// TestResolveDeadRefs_KeepsExpires: replay's dead-ref repair is the store
// path that PARSES and RE-SERIALIZES a fact (replay.go resolveDeadRefs). A
// dropped ref forces the rewrite; the fact's expires must survive it.
func TestResolveDeadRefs_KeepsExpires(t *testing.T) {
	svc, branch := motifEnv(t)
	f := fact.NewFact("kb/subject.md")
	f.Title, f.Body, f.Type = "Subject", "Body.", fact.Hypothesis
	f.Domain, f.Entities = []string{"alpha"}, []string{}
	f.Refs = []string{"kb/live.md", "kb/dead.md"}
	f.Confidence, f.Sources = 0.5, 1
	f.Expires = "2026-10-01T02:00:00+02:00"
	content, err := fact.SerializeFact(f)
	require.NoError(t, err)

	out, _, dropped, err := resolveDeadRefs(context.Background(), svc, branch, content, "kb/subject.md",
		map[string]bool{"kb/live.md": true}, map[string]bool{}, "")
	require.NoError(t, err)
	require.Equal(t, 1, dropped, "fixture: a ref must be dropped so the fact is re-serialized")
	require.NotEqual(t, content, out, "fixture: the rewrite path must have run")

	back, err := fact.ParseFact("kb/subject.md", out)
	require.NoError(t, err)
	require.Equal(t, "2026-10-01T02:00:00+02:00", back.Expires)
}

// TestFactExpires_RebuildKeepsSpacedKey: `expires : …` is valid YAML and the
// incremental path indexes it, so the rebuild's text prefilter must not miss
// it (it once matched only "expires:").
func TestFactExpires_RebuildKeepsSpacedKey(t *testing.T) {
	svc, branch := motifEnv(t)
	ctx := context.Background()
	body := "---\ntype: hypothesis\ndomain: [alpha]\nconfidence: 0.5\nsources: 1\nentities: []\nrefs: []\nexpires : \"2026-10-01T00:00:00Z\"\n---\n# Spaced\n\nbody\n"
	_, err := svc.Facts().WriteFact(ctx, branch, "kb/alpha/spaced.md", body, "seed", "")
	require.NoError(t, err)
	q := SearchOptions{Expired: boolp(true), Now: expNow}
	require.Equal(t, []string{"kb/alpha/spaced.md"}, searchPaths(t, svc, branch, q), "fixture: the incremental path indexes it")

	_, err = svc.si.rh.db.ExecContext(ctx, `DELETE FROM fact_expires`)
	require.NoError(t, err)
	require.NoError(t, svc.IndexManager().Rebuild(ctx, branch, nil))
	require.Equal(t, []string{"kb/alpha/spaced.md"}, searchPaths(t, svc, branch, q), "the rebuild agrees with the parser")
}
