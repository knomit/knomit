package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// synthesisFactBody builds a serialized epistemic synthesis fact body for
// tests. Synthesis facts have origin=distilled when no explicit value is
// written (the legacy default applied by ParseFact), so this is the canary
// for the type-aware backfill.
func synthesisFactBody(title string) string {
	f := fact.NewFact("placeholder.md")
	f.Title = title
	f.Body = "synthesis body"
	f.Type = fact.Synthesis
	f.Origin = fact.Distilled
	f.Confidence = 0.9
	f.Sources = 1
	f.Domain = []string{"test"}
	out, err := fact.SerializeFact(f)
	if err != nil {
		panic(err)
	}
	return out
}

// TestFactsOriginColumnBackfill regresses the origin storage backbone: writing
// a synthesis fact and an observation fact through WriteFact must surface
// origin=distilled and origin=authored respectively in the facts table. This
// asserts the indexer + parser wiring agree on the type-aware default and the
// migration's backfill UPDATE doesn't drift from ParseFact's rule.
func TestFactsOriginColumnBackfill(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(filepath.Join(dir, "k.db"))
	require.NoError(t, err)
	defer svc.Close()
	require.NoError(t, svc.InitRepo(map[string]string{}, "agent/a"))

	ctx := context.Background()
	const synthPath = "kb/synth/s.md"
	const obsPath = "kb/obs/o.md"
	_, err = svc.Facts().WriteFact(ctx, "agent/a", synthPath,
		synthesisFactBody("S"), "add synth", "")
	require.NoError(t, err)
	_, err = svc.Facts().WriteFact(ctx, "agent/a", obsPath,
		testFactBody("O", 0.9, nil), "add obs", "")
	require.NoError(t, err)

	db := svc.rh.db
	var got string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT origin FROM facts WHERE path = ?`, synthPath).Scan(&got))
	require.Equal(t, "distilled", got, "synthesis origin must surface as distilled")

	require.NoError(t, db.QueryRowContext(ctx, `SELECT origin FROM facts WHERE path = ?`, obsPath).Scan(&got))
	require.Equal(t, "authored", got, "observation origin must surface as authored")
}
