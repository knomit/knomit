package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// TestSynthFactFromResult_PropagatesOrigin is the regression guard for the
// first-run idempotency leak: hypothesizeStart's watermark-empty branch built
// each synthesis seed from a search result but never copied Origin, so a
// discovered-origin synthesis fact slipped past the §7 idempotency filter in
// the bridge engine (which excludes only origin=discovered) and seeded its own
// backward discovery. Origin MUST survive the projection.
func TestSynthFactFromResult_PropagatesOrigin(t *testing.T) {
	mk := func(origin string) store.SearchResult {
		var r store.SearchResult
		r.Path = "kb/x/p.md"
		r.Title = "P"
		r.Body = "body"
		r.Type = string(fact.Synthesis)
		r.Domain = []string{"auth"}
		r.Entities = []string{"shared"}
		r.Confidence = 0.9
		r.Sources = 1
		r.Origin = origin
		return r
	}

	discovered := synthFactFromResult(mk("discovered"))
	require.Equal(t, fact.Discovered, discovered.Origin,
		"discovered-origin synthesis facts must carry Origin so bridge seeding can exclude them (§7 idempotency)")

	authored := synthFactFromResult(mk("authored"))
	require.Equal(t, fact.Authored, authored.Origin)

	// The other index-sourced fields must survive the projection too.
	require.Equal(t, fact.Synthesis, discovered.Type)
	require.Equal(t, []string{"auth"}, discovered.Domain)
	require.Equal(t, []string{"shared"}, discovered.Entities)
	require.Equal(t, 0.9, discovered.Confidence)
	require.Equal(t, 1, discovered.Sources)
}
