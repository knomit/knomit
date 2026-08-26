package main

import (
	"context"
	"fmt"
	"os"

	"knomit/internal/fact"
	"knomit/internal/synthesize"
)

// bridges dumps every motif bridge candidate a corpus produces at one effort —
// SERVED AND DROPPED, with the drop cause — as JSON.
//
// This is Phase 4's measurement instrument, and it is deliberately a thin shell
// over synthesize.MotifComponentReport: the report drives the SHIPPED
// enumeration and scoring, so what is measured here is the engine that serves
// the bridges rather than a second implementation of it that agrees until it
// does not. A Python re-derivation would measure the re-derivation.
//
// Read-only. It plans no work, answers no item and writes nothing — and the
// corpus it opens is a lab copy, enforced by refuseLivePath in open().
func bridges(ctx context.Context, corpus, scratch, effortStr string) error {
	eff := synthesize.NormalizeEffort(synthesize.Effort(effortStr))
	if err := eff.Validate(); err != nil {
		return fmt.Errorf("-effort: %w", err)
	}

	svc, ri, branch, closeAll, err := open(ctx, corpus, scratch)
	if err != nil {
		return err
	}
	defer closeAll()

	rep, err := synthesize.MotifComponentReport(ctx, svc.Search(), svc.Motifs(), svc.Abstraction(),
		branch, fact.ID12(ri.ID()), eff, ri.ClusterResolution(), ri.ClusterMinCommunitySize(),
		synthesize.QualityConfigFromRepo(ri))
	if err != nil {
		return fmt.Errorf("motif component report: %w", err)
	}
	fmt.Fprintln(os.Stderr, rep.Summary())
	return emit(rep)
}
