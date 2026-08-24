package main

import (
	"context"
	"fmt"
	"sort"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"knomit/internal/config"
	"knomit/internal/store"
	"knomit/internal/synthesize"
)

func newBridgesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bridges --db <repo.db> [flags]",
		Short: "Run bridge quality scorer and print component report + suggested floors",
		Long: `bridges opens a knomit index read-only (no ONNX required), runs
synthesize.BridgeComponentReport, and prints:
  1. A per-bridge component table (token, kind, members, coh, sep, gap, spec, Q, kept).
  2. Aggregates: mean Q, median Q, count Kept, total candidates.
  3. Suggested cohFloor and qualityFloor derived from the score distribution.

The scorer reads precomputed SIMILAR_TO edges, Louvain clusters, derivation
paths, and token frequencies — no embedding model is loaded or needed.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			f := cmd.Flags()
			dbPath, _ := f.GetString("db")
			branch, _ := f.GetString("branch")
			effortStr, _ := f.GetString("effort")
			kindStr, _ := f.GetString("kind")
			resolution, _ := f.GetFloat64("resolution")
			minCommunity, _ := f.GetInt("min-community")

			// Quality config: the Q-knob flag defaults are registered from
			// config.Defaults().Discovery (see flag registration below), so
			// reading each value directly yields the config default when unset
			// and the override when set — no f.Changed() guards needed.
			cohFloorFlag, _ := f.GetFloat64("coh-floor")
			qualityFloorFlag, _ := f.GetFloat64("quality-floor")
			wCoh, _ := f.GetFloat64("w-coh")
			wGap, _ := f.GetFloat64("w-gap")
			wSpec, _ := f.GetFloat64("w-spec")
			maxMembers, _ := f.GetInt("max-members")
			cfg := synthesize.QualityConfig{
				CohFloor:     cohFloorFlag,
				QualityFloor: qualityFloorFlag,
				WCoh:         wCoh,
				WGap:         wGap,
				WSpec:        wSpec,
				MaxMembers:   maxMembers,
			}

			eff := synthesize.NormalizeEffort(synthesize.Effort(effortStr))
			if err := eff.Validate(); err != nil {
				return fmt.Errorf("--effort: %w", err)
			}
			// MOTIF is a SIBLING report, not a kind of this one (Phase-4 Q3).
			// The two enumerate different populations over different pools with
			// different engines, and 8ad54ee8 is precisely an aggregate whose
			// population was not the production population, surviving because
			// nothing forced the population into the output. Folding a second
			// population behind a flag on one function is how that recurs.
			if kindStr == "motif" {
				return runMotifReport(cmd, dbPath, branch, effortStr, resolution, minCommunity, cfg)
			}
			kind := synthesize.BridgeKindFromString(kindStr)

			// Open the index without an embedder — the scoring path is embedder-free.
			svc, err := store.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open index %q: %w", dbPath, err)
			}
			defer svc.Close()

			idx := svc.Search()
			ctx := context.Background()

			report, err := synthesize.BridgeComponentReport(ctx, idx, branch, kind, eff, resolution, minCommunity, cfg)
			if err != nil {
				return fmt.Errorf("bridge component report: %w", err)
			}

			out := cmd.OutOrStdout()
			// POPULATION FIRST, before any number. A figure that travels
			// without the population it was computed over is how 8ad54ee8
			// happened: a suggested floor derived from unreshaped candidates,
			// read as if it described the production ones.
			fmt.Fprintf(out, "POPULATION: %s bridge candidates over live SYNTHESIS facts on %q, "+
				"scored UNRESHAPED (production applies cohFloor AFTER reshapeCohesiveSubset, "+
				"so the suggested floor below is a lower bound, not the production floor)\n\n",
				kindStr, branch)
			if len(report) == 0 {
				fmt.Fprintln(out, "no bridge candidates found")
				return nil
			}

			// Per-bridge table.
			tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "TOKEN\tKIND\tMEMBERS\tCOH\tSEP\tGAP\tSPEC\tQ\tKEPT")
			for _, b := range report {
				kept := "no"
				if b.Kept {
					kept = "yes"
				}
				fmt.Fprintf(tw, "%s\t%s\t%d\t%.3f\t%d\t%.3f\t%.3f\t%.3f\t%s\n",
					b.Token,
					string(b.Kind),
					b.Comp.Members,
					b.Comp.Coh,
					b.Comp.Sep,
					b.Comp.Gap,
					b.Comp.Spec,
					b.Q,
					kept,
				)
			}
			tw.Flush()

			// Aggregate stats.
			var sumQ float64
			var qs []float64
			kept := 0
			for _, b := range report {
				sumQ += b.Q
				qs = append(qs, b.Q)
				if b.Kept {
					kept++
				}
			}
			meanQ := sumQ / float64(len(report))
			medianQ := quantile(sortedCopy(qs), 0.5)

			fmt.Fprintf(out, "\ncandidates=%d  kept=%d  meanQ=%.3f  medianQ=%.3f\n",
				len(report), kept, meanQ, medianQ)

			// Suggested floors.
			cohFloor, qualityFloor := suggestFloors(report)
			fmt.Fprintf(out, "\nSUGGESTED-FLOORS  coh-floor=%.3f  quality-floor=%.3f\n",
				cohFloor, qualityFloor)

			return nil
		},
	}

	f := cmd.Flags()
	f.String("db", "", "path to knomit index DB (required)")
	f.String("branch", "main", "branch name to query")
	f.String("effort", "medium", "discovery effort level (normal/medium/high)")
	f.String("kind", "both", "bridge kind to enumerate (domain/entity/both/motif)")
	f.Float64("resolution", 2.0, "Louvain resolution for clustering")
	f.Int("min-community", 2, "minimum community size for clustering")
	// Q-knob overrides: register with config.Defaults().Discovery values as the
	// cobra defaults so --help shows the real defaults and an unset flag yields
	// the config default when read directly in RunE.
	dd := config.Defaults().Discovery
	f.Float64("coh-floor", dd.CohFloor, "cohesion floor override")
	f.Float64("quality-floor", dd.QualityFloor, "quality floor override")
	f.Float64("w-coh", dd.WCoh, "cohesion weight override")
	f.Float64("w-gap", dd.WGap, "gap weight override")
	f.Float64("w-spec", dd.WSpec, "specificity weight override")
	f.Int("max-members", dd.MaxMembers, "max members override")
	_ = cmd.MarkFlagRequired("db")

	return cmd
}

// suggestFloors derives suggested quality thresholds from a bridge score
// distribution. Returns interpretable starting points that can be refined by
// eye against the printed table.
//
// cohFloor heuristic: p50 (median) of Coh values among cross-community
// candidates (Sep>=2). For a typical bimodal distribution — grab-bags cluster
// near 0.05-0.10, cohesive bridges cluster near 0.6-0.9 — the median falls
// in the gap between the two clusters, giving a natural gate point. Calibrate
// the exact percentile per corpus once the table is printed.
//
// qualityFloor heuristic: p25 of Q values among candidates that passed the
// cohesion+separation gates (Kept=true or Q>0). This is the lower tail of
// viable bridges; raise it to filter borderline cases.
//
// Returns (0,0) for empty input or when the filtered subsets are empty.
func suggestFloors(bridges []synthesize.ScoredBridge) (cohFloor, qualityFloor float64) {
	if len(bridges) == 0 {
		return 0, 0
	}

	// Collect Coh values for cross-community candidates (Sep>=2).
	var cohVals []float64
	for _, b := range bridges {
		if b.Comp.Sep >= 2 {
			cohVals = append(cohVals, b.Comp.Coh)
		}
	}
	if len(cohVals) > 0 {
		sort.Float64s(cohVals)
		cohFloor = quantile(cohVals, 0.50) // p50: lands in the gap between clusters
	}

	// Collect Q values for candidates that are viable (Kept or Q>0).
	var qVals []float64
	for _, b := range bridges {
		if b.Kept || b.Q > 0 {
			qVals = append(qVals, b.Q)
		}
	}
	if len(qVals) > 0 {
		sort.Float64s(qVals)
		qualityFloor = quantile(qVals, 0.25) // p25: lower tail of viable bridges
	}

	return cohFloor, qualityFloor
}

// sortedCopy returns a sorted copy of vs (does not modify the input).
func sortedCopy(vs []float64) []float64 {
	cp := make([]float64, len(vs))
	copy(cp, vs)
	sort.Float64s(cp)
	return cp
}

// runMotifReport prints the motif axis's component report.
//
// Separate from the entity/domain path deliberately (Phase-4 Q3): different
// population, different pool, different engine. It states all three in its own
// header rather than inheriting a sentence written about another axis.
func runMotifReport(cmd *cobra.Command, dbPath, branch, effortStr string,
	resolution float64, minCommunity int, cfg synthesize.QualityConfig) error {
	eff := synthesize.NormalizeEffort(synthesize.Effort(effortStr))
	if err := eff.Validate(); err != nil {
		return fmt.Errorf("--effort: %w", err)
	}
	svc, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open index %q: %w", dbPath, err)
	}
	defer svc.Close()

	rep, err := synthesize.MotifComponentReport(cmd.Context(), svc.Search(), svc.Motifs(),
		svc.Abstraction(), branch, eff, resolution, minCommunity, cfg)
	if err != nil {
		return fmt.Errorf("motif component report: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "POPULATION: %s\n\n", rep.Population)
	fmt.Fprintf(out, "%s\n", rep.Summary())
	if !rep.ActivationActive {
		fmt.Fprintf(out, "\nAXIS INACTIVE: %d recurring motif(s) — below the activation floor. "+
			"Nothing below is a statement about this corpus's bridges; the axis did not run.\n",
			rep.SeedDF2Clusters)
	}
	fmt.Fprintf(out, "activation population: %d recurring motif(s), %d bridgeable pair(s)\n\n",
		rep.SeedDF2Clusters, rep.SeedBridgeablePairs)

	if len(rep.Candidates) == 0 {
		fmt.Fprintln(out, "no motif bridge candidates found")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TOKEN\tLANE\tMEMBERS\tCOH\tSEP\tGAP\tSPEC\tSEEDCOS\tEJACC\tOVERDUP\tQ\tDISPOSITION")
	for _, b := range rep.Candidates {
		// One column, every state named. It read "yes" for candidates absent
		// from the served line — suppression happens after the scorer's
		// verdict, so "kept" and "served" are different questions and the
		// table now answers both (review finding M-4).
		verdict := "served"
		switch {
		case !b.Kept:
			verdict = "dropped: " + b.Cause
		case !b.Served:
			verdict = "kept, no slot (budget)"
		}
		seedCos, overDup := "n/a", "n/a"
		if b.Comp.Novelty.VectorsRead {
			seedCos = fmt.Sprintf("%.3f", b.Comp.Novelty.SeedCos)
			overDup = fmt.Sprintf("%.3f", b.Comp.Novelty.OverDedup)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%.3f\t%d\t%.3f\t%.3f\t%s\t%.3f\t%s\t%.3f\t%s\n",
			b.Token, b.Lane, len(b.Members), b.Comp.Coh, b.Comp.Sep, b.Comp.Gap, b.Comp.Spec,
			seedCos, b.Comp.Novelty.EntityJaccard, overDup, b.Q, verdict)
	}
	tw.Flush()

	fmt.Fprintf(out, "\nserved: %d near, %d far (budgets %d/%d)\n",
		len(rep.NearServed), len(rep.FarServed), rep.NearBudget, rep.FarBudget)
	fmt.Fprintln(out, "\nNOTE: OVERDUP counts member pairs at or above the dedup cosine, which "+
		"catches VERBATIM duplicates only (90d69628). A zero does NOT mean the members say "+
		"different things.")
	for _, l := range rep.HealthLines {
		fmt.Fprintf(out, "  health: %s\n", l)
	}
	return nil
}
