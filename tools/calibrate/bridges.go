package main

import (
	"context"
	"fmt"
	"io"
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
			poolStr, _ := f.GetString("pool")

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
			keywordMinBodyDF, _ := f.GetInt("keyword-min-body-df")
			keywordMinBodyDFRatio, _ := f.GetFloat64("keyword-min-body-df-ratio")
			keywordMaxBodyDFRatio, _ := f.GetFloat64("keyword-max-body-df-ratio")
			keywordMaxBodyDFCap, _ := f.GetInt("keyword-max-body-df-cap")
			cfg := synthesize.QualityConfig{
				CohFloor:              cohFloorFlag,
				QualityFloor:          qualityFloorFlag,
				WCoh:                  wCoh,
				WGap:                  wGap,
				WSpec:                 wSpec,
				MaxMembers:            maxMembers,
				KeywordMinBodyDF:      keywordMinBodyDF,
				KeywordMinBodyDFRatio: keywordMinBodyDFRatio,
				KeywordMaxBodyDFRatio: keywordMaxBodyDFRatio,
				KeywordMaxBodyDFCap:   keywordMaxBodyDFCap,
			}

			eff := synthesize.NormalizeEffort(synthesize.Effort(effortStr))
			if err := eff.Validate(); err != nil {
				return fmt.Errorf("--effort: %w", err)
			}
			kind := synthesize.BridgeKindFromString(kindStr)

			// --pool defaults to "both": production always has both
			// knomit_review's (forward) and knomit_hypothesize's (backward)
			// pools available, and testing only one gave a false picture of
			// readiness for six sessions before this changed — see
			// .claude/plans/yake-forward-vs-backward-pool-report.md. Pass
			// --pool backward or --pool forward explicitly to skip the other
			// for fast iterative threshold tuning (forward-pool runs cost
			// ~10-20x longer than backward on a real corpus).
			var pools []synthesize.BridgeReportPool
			var labels []string
			switch poolStr {
			case "", "both":
				pools = []synthesize.BridgeReportPool{synthesize.PoolBackward, synthesize.PoolForward}
				labels = []string{
					"BACKWARD (knomit_hypothesize's real input — type=synthesis only)",
					"FORWARD (knomit_review's real input — all epistemic types)",
				}
			case "backward":
				pools = []synthesize.BridgeReportPool{synthesize.PoolBackward}
				labels = []string{"BACKWARD (knomit_hypothesize's real input — type=synthesis only)"}
			case "forward":
				pools = []synthesize.BridgeReportPool{synthesize.PoolForward}
				labels = []string{"FORWARD (knomit_review's real input — all epistemic types)"}
			default:
				return fmt.Errorf("--pool: must be \"backward\", \"forward\", or \"both\", got %q", poolStr)
			}

			// Open the index without an embedder — the scoring path is embedder-free.
			svc, err := store.Open(dbPath)
			if err != nil {
				return fmt.Errorf("open index %q: %w", dbPath, err)
			}
			defer svc.Close()

			idx := svc.Search()
			ctx := context.Background()
			out := cmd.OutOrStdout()

			for i, pool := range pools {
				if len(pools) > 1 {
					fmt.Fprintf(out, "=== %s ===\n", labels[i])
				}
				if err := runBridgeReport(ctx, idx, out, branch, kind, eff, resolution, minCommunity, cfg, pool); err != nil {
					return err
				}
				if len(pools) > 1 && i < len(pools)-1 {
					fmt.Fprintln(out)
				}
			}

			return nil
		},
	}

	f := cmd.Flags()
	f.String("db", "", "path to knomit index DB (required)")
	f.String("branch", "main", "branch name to query")
	f.String("effort", "medium", "discovery effort level (normal/medium/high)")
	f.String("kind", "both", "bridge kind to enumerate (domain/entity/both)")
	f.String("pool", "both", "which production seed pool(s) to mirror: \"both\" (default — backward and forward, sequentially), \"backward\" (IncludeTypes=synthesis, hypothesize.go's real input), or \"forward\" (IncludeKinds=epistemic, review.go's real input)")
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
	f.Int("keyword-min-body-df", dd.KeywordMinBodyDF, "fixed floor for keyword body-DF lower bound (0 = ratio-only)")
	f.Float64("keyword-min-body-df-ratio", dd.KeywordMinBodyDFRatio, "keyword body-DF lower bound as fraction of pool size, combined with the floor via max() (0 = fixed floor only)")
	f.Float64("keyword-max-body-df-ratio", dd.KeywordMaxBodyDFRatio, "keyword body-DF upper bound as fraction of pool size (0 = disabled)")
	f.Int("keyword-max-body-df-cap", dd.KeywordMaxBodyDFCap, "absolute ceiling on the keyword body-DF upper bound regardless of pool size (0 = uncapped)")
	_ = cmd.MarkFlagRequired("db")

	return cmd
}

// runBridgeReport runs BridgeComponentReport against a single pool and
// prints its per-bridge table, aggregate stats, and suggested floors to out.
// Factored out of newBridgesCmd's RunE so it can be called once (a single
// --pool value) or twice (the default "both") without duplicating the
// printing logic.
func runBridgeReport(
	ctx context.Context,
	idx store.SearchIndex,
	out io.Writer,
	branch string,
	kind synthesize.BridgeKind,
	eff synthesize.Effort,
	resolution float64,
	minCommunity int,
	cfg synthesize.QualityConfig,
	pool synthesize.BridgeReportPool,
) error {
	report, err := synthesize.BridgeComponentReport(ctx, idx, branch, kind, eff, resolution, minCommunity, cfg, pool)
	if err != nil {
		return fmt.Errorf("bridge component report: %w", err)
	}

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
