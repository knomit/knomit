package main

import (
	"bytes"
	"database/sql"
	"knomit/internal/embeddings"
	"knomit/internal/embeddings/params"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"knomit/internal/synthesize"
)

// TestSuggestFloors_EmptyInput verifies that an empty bridge list returns (0,0).
func TestSuggestFloors_EmptyInput(t *testing.T) {
	coh, q := suggestFloors(nil)
	assert.Equal(t, 0.0, coh, "empty input: cohFloor should be 0")
	assert.Equal(t, 0.0, q, "empty input: qualityFloor should be 0")
}

// TestSuggestFloors_TwoClusters verifies that suggestFloors picks a cohFloor
// strictly between the grab-bag cluster (Coh ~0.05-0.10) and the cohesive
// cluster (Coh ~0.60-0.90), given Sep>=2 candidates in both groups.
func TestSuggestFloors_TwoClusters(t *testing.T) {
	bridges := []synthesize.ScoredBridge{
		// Grab-bags: low cohesion, cross-community (Sep>=2), not kept.
		{Token: "grab1", Comp: synthesize.BridgeComponents{Coh: 0.05, Sep: 3, Gap: 0.2, Spec: 0.3, Members: 3}, Q: 0.0, Kept: false},
		{Token: "grab2", Comp: synthesize.BridgeComponents{Coh: 0.08, Sep: 2, Gap: 0.1, Spec: 0.2, Members: 4}, Q: 0.0, Kept: false},
		{Token: "grab3", Comp: synthesize.BridgeComponents{Coh: 0.07, Sep: 4, Gap: 0.3, Spec: 0.4, Members: 2}, Q: 0.0, Kept: false},
		{Token: "grab4", Comp: synthesize.BridgeComponents{Coh: 0.10, Sep: 2, Gap: 0.2, Spec: 0.1, Members: 3}, Q: 0.0, Kept: false},
		// Cohesive: high cohesion, cross-community (Sep>=2), kept.
		{Token: "coh1", Comp: synthesize.BridgeComponents{Coh: 0.62, Sep: 2, Gap: 0.8, Spec: 0.7, Members: 2}, Q: 2.12, Kept: true},
		{Token: "coh2", Comp: synthesize.BridgeComponents{Coh: 0.75, Sep: 3, Gap: 0.9, Spec: 0.8, Members: 3}, Q: 2.45, Kept: true},
		{Token: "coh3", Comp: synthesize.BridgeComponents{Coh: 0.85, Sep: 2, Gap: 0.7, Spec: 0.6, Members: 2}, Q: 2.15, Kept: true},
		{Token: "coh4", Comp: synthesize.BridgeComponents{Coh: 0.90, Sep: 4, Gap: 0.85, Spec: 0.75, Members: 4}, Q: 2.50, Kept: true},
	}

	cohFloor, qualityFloor := suggestFloors(bridges)

	// cohFloor = p50 (median) of all Sep>=2 Coh values.
	// Sorted Sep>=2 Cohs: {0.05, 0.07, 0.08, 0.10, 0.62, 0.75, 0.85, 0.90}.
	// Median ≈ (0.10+0.62)/2 = 0.36 — squarely between the grab-bag cluster
	// (max 0.10) and the cohesive cluster (min 0.62).
	assert.Greater(t, cohFloor, 0.10, "cohFloor should be above the grab-bag cluster max")
	assert.Less(t, cohFloor, 0.62, "cohFloor should be below the cohesive cluster min")

	// qualityFloor = p25 of viable Q values. Grab-bags have Q=0 (excluded),
	// so viable Q = sorted{2.12, 2.15, 2.45, 2.50}. quantile p=0.25 interpolates
	// at idx 0.25*3=0.75 between 2.12 and 2.15 → 2.12 + 0.75*(2.15-2.12) = 2.1425.
	assert.InDelta(t, 2.1425, qualityFloor, 1e-9, "qualityFloor should be p25 of viable Q values")
}

// TestSuggestFloors_NoSep2Candidates verifies that if no candidates have Sep>=2,
// cohFloor returns 0.
func TestSuggestFloors_NoSep2Candidates(t *testing.T) {
	bridges := []synthesize.ScoredBridge{
		{Token: "a", Comp: synthesize.BridgeComponents{Coh: 0.5, Sep: 1, Gap: 0.5, Spec: 0.5, Members: 2}, Q: 0.0, Kept: false},
		{Token: "b", Comp: synthesize.BridgeComponents{Coh: 0.8, Sep: 0, Gap: 0.5, Spec: 0.5, Members: 2}, Q: 0.0, Kept: false},
	}
	cohFloor, _ := suggestFloors(bridges)
	assert.Equal(t, 0.0, cohFloor, "no Sep>=2 candidates → cohFloor should be 0")
}

// TestBridgesHelp verifies that "calibrate bridges --help" lists expected flags.
func TestBridgesHelp(t *testing.T) {
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"bridges", "--help"})

	err := cmd.Execute()
	require.NoError(t, err)

	out := buf.String()
	assert.Contains(t, out, "--db", "help should list --db flag")
	assert.Contains(t, out, "--branch", "help should list --branch flag")
	assert.Contains(t, out, "--effort", "help should list --effort flag")
	assert.Contains(t, out, "--kind", "help should list --kind flag")
}

// TestBridgesMissingDB verifies that running "calibrate bridges" without --db
// returns an error (required flag not set). No DB is opened.
func TestBridgesMissingDB(t *testing.T) {
	cmd := newRootCmd()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"bridges"})

	err := cmd.Execute()
	require.Error(t, err, "missing required --db should produce an error")
	assert.True(t,
		strings.Contains(err.Error(), "db") || strings.Contains(buf.String(), "db"),
		"error should mention 'db', got err=%q out=%q", err, buf.String(),
	)
}

// M-5. The command opens a store with no embedder, so the dedup threshold has
// to come from the corpus's recorded model identity. Unknown must stay
// unknown: params.Defaults() is nomic's geometry and every corpus in this
// campaign is embeddinggemma, so a silent default reports OverDedup 0.000 for
// a pair genuinely above its corpus's own gate.
func TestCorpusDedupThreshold_UnknownIsNotADefault(t *testing.T) {
	dir := t.TempDir()

	// No meta table at all.
	empty := filepath.Join(dir, "empty.db")
	db, err := sql.Open("sqlite3", empty)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE unrelated (x TEXT)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, known := corpusDedupThreshold(empty)
	require.False(t, known, "no meta table is unknown, not default")

	// A meta table naming a model nothing registers.
	unknown := filepath.Join(dir, "unknown.db")
	db, err = sql.Open("sqlite3", unknown)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE meta (key TEXT, value TEXT)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO meta VALUES ('embed_model_id','no-such-model')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, known = corpusDedupThreshold(unknown)
	require.False(t, known, "an unrecognised model is unknown, not default")

	// A recognised one resolves, and to the model's own value.
	real := filepath.Join(dir, "real.db")
	db, err = sql.Open("sqlite3", real)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE meta (key TEXT, value TEXT)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO meta VALUES ('embed_model_id','embeddinggemma')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	got, known := corpusDedupThreshold(real)
	require.True(t, known)
	m, err := embeddings.Lookup("embeddinggemma")
	require.NoError(t, err)
	require.Equal(t, m.Thresholds.Dedup, got)
	require.NotEqual(t, params.Defaults().Dedup, got,
		"precondition: this model's threshold must DIFFER from the default, or the "+
			"test cannot tell which one was used")
}
