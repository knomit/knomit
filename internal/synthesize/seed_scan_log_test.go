package synthesize

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// PR #126, HIGH-1 + MEDIUM-3. Fix (b)'s deliverable is the LOG LINE, and the
// line had no coverage at all: seed_scan_fields_test.go pins the seedScan
// struct, but nothing pinned what StartSession actually emits.
//
// Two distinct gaps, both closed here.
//
// HIGH-1 — the label VALUES were pinned by nothing. Every assertion compared
// scan.Path against the constants seedScanFull/seedScanIncremental, so
// swapping the constants' values ("full" ⇄ "incremental") was invisible BY
// CONSTRUCTION and left the suite fully green. The log could then mislabel
// which path ran — the exact diagnostic #121 needed — with no test failing.
//
// So the assertions below name "full" and "incremental" as STRING LITERALS at
// the assertion site. Reaching them through the constants would re-create the
// same non-assertion in a new shape: a test that reaches the thing under test
// through the thing under test pins nothing.
//
// MEDIUM-3 — the three field BINDINGS were pinned by nothing either. Swapping
// or inverting them at the log call left the suite green, so the line could
// report the wrong scoped flag or the watermark under the wrong key.
//
// Asserting field NAMES as well as values is deliberate: an operator greps for
// these names, and #121's diagnosis was reconstructed by arithmetic precisely
// because they did not exist.
func TestSeedScanLogLine_CarriesScopedPathAndWatermark(t *testing.T) {
	ctx := context.Background()
	const branch = "agent/test"

	t.Run("unscoped incremental run", func(t *testing.T) {
		r, svc := newPhaseTestReviewer(t)
		writeKindFact(t, svc, branch, "kb/technology/base.md", fact.Epistemic, fact.Observation)
		head, err := svc.Branches().HeadCommit(ctx, branch)
		require.NoError(t, err)
		require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", branch, head))
		writeKindFact(t, svc, branch, "kb/technology/b.md", fact.Epistemic, fact.Observation)

		line := captureSeedScanLine(t, func() { _, _ = r.StartSession(ctx) })

		require.Equal(t, "incremental", line["scan_path"],
			`the literal is asserted here on purpose: comparing against `+
				`seedScanIncremental would pass even with the constants' values swapped`)
		require.Equal(t, false, line["scoped"])
		require.Equal(t, head, line["watermark"])
	})

	t.Run("scoped run is a full scan", func(t *testing.T) {
		r, svc := newPhaseTestReviewer(t)
		writeKindFact(t, svc, branch, "kb/technology/base.md", fact.Epistemic, fact.Observation)
		head, err := svc.Branches().HeadCommit(ctx, branch)
		require.NoError(t, err)
		require.NoError(t, svc.Pipeline().SetPipelineWatermark(ctx, "review", branch, head))
		r.p.scope = ScopeFilter{Domain: []string{"technology"}}

		line := captureSeedScanLine(t, func() { _, _ = r.StartSession(ctx) })

		require.Equal(t, "full", line["scan_path"],
			"literal, for the same reason as above")
		require.Equal(t, true, line["scoped"],
			"the scoped flag is the field that told #121's forensics which calls "+
				"had lost their scope")
		require.Equal(t, head, line["watermark"],
			"reported on the scoped path too: its VALUE is the diagnostic, not "+
				"whether this call consulted it")
	})
}

// captureSeedScanLine runs fn with the global logger redirected, and returns
// the decoded "pipeline: seed scan" line.
//
// It asserts the line was emitted at all: a capture helper that returned an
// empty map when nothing was logged would make every assertion above vacuous —
// which is the failure mode this whole test exists to close.
func captureSeedScanLine(t *testing.T, fn func()) map[string]any {
	t.Helper()

	var buf bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&buf).Level(zerolog.InfoLevel)
	t.Cleanup(func() { log.Logger = prev })

	fn()

	for _, raw := range bytes.Split(buf.Bytes(), []byte("\n")) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}
		if entry["message"] == "pipeline: seed scan" {
			return entry
		}
	}
	t.Fatal("no 'pipeline: seed scan' line was logged — every assertion on it " +
		"would otherwise be vacuous")
	return nil
}
