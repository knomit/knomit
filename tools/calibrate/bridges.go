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

			// Quality config: start from config defaults, then apply overrides.
			defaults := config.Defaults().Discovery
			cfg := synthesize.QualityConfig{
				CohFloor:     defaults.CohFloor,
				QualityFloor: defaults.QualityFloor,
				WCoh:         defaults.WCoh,
				WGap:         defaults.WGap,
				WSpec:        defaults.WSpec,
				MaxMembers:   defaults.MaxMembers,
			}

			// Apply flag overrides when they were explicitly set.
			if v, err := f.GetFloat64("coh-floor"); err == nil && f.Changed("coh-floor") {
				cfg.CohFloor = v
			}
			if v, err := f.GetFloat64("quality-floor"); err == nil && f.Changed("quality-floor") {
				cfg.QualityFloor = v
			}
			if v, err := f.GetFloat64("w-coh"); err == nil && f.Changed("w-coh") {
				cfg.WCoh = v
			}
			if v, err := f.GetFloat64("w-gap"); err == nil && f.Changed("w-gap") {
				cfg.WGap = v
			}
			if v, err := f.GetFloat64("w-spec"); err == nil && f.Changed("w-spec") {
				cfg.WSpec = v
			}
			if v, err := f.GetInt("max-members"); err == nil && f.Changed("max-members") {
				cfg.MaxMembers = v
			}

			eff := synthesize.NormalizeEffort(synthesize.Effort(effortStr))
			if err := eff.Validate(); err != nil {
				return fmt.Errorf("--effort: %w", err)
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
	f.String("kind", "both", "bridge kind to enumerate (domain/entity/both)")
	f.Float64("resolution", 2.0, "Louvain resolution for clustering")
	f.Int("min-community", 2, "minimum community size for clustering")
	// Q-knob overrides (defaults come from config.Defaults().Discovery).
	f.Float64("coh-floor", 0, "cohesion floor override (default: config.Defaults)")
	f.Float64("quality-floor", 0, "quality floor override (default: config.Defaults)")
	f.Float64("w-coh", 0, "cohesion weight override (default: config.Defaults)")
	f.Float64("w-gap", 0, "gap weight override (default: config.Defaults)")
	f.Float64("w-spec", 0, "spec weight override (default: config.Defaults)")
	f.Int("max-members", 0, "max members override (default: config.Defaults)")
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
