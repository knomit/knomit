package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"knomit/internal/embeddings"
	"knomit/internal/fact"
	"knomit/internal/llm"
	"knomit/internal/store"
)

// batchTimeout bounds a single generateBatch call. Real-content mode can
// take much longer per batch than synthetic (the model performs genuine web
// searches), but an unbounded context means one hung call blocks forever;
// this makes a stall fail (and, per the incremental-write design below,
// keep whatever was already written) instead of hanging indefinitely.
const batchTimeout = 8 * time.Minute

// realModeOverprovisionFactor accounts for facts real mode expects to drop
// during URL verification (verify.go) — generate somewhat more than the
// target size so a healthy verification-pass rate still lands at/near size.
const realModeOverprovisionFactor = 1.3

func newBuildCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build one real knomit repo at a given size and diversity profile",
		Long: `build creates a brand-new knomit repo (a single SQLite file, exactly like a
production repo) through the real write/embed path: a real ontology, real
fact validation, real ONNX embeddings, and a real Louvain-clusterable
SIMILAR_TO graph — populated with fact content at a deliberately chosen size
and topic-diversity profile.

--content-source synthetic (default) asks the LLM to invent plausible
content. --content-source real instead requires the model to use its
WebSearch tool and ground every fact in a genuinely real, HTTP-verified
citation — dropping (and regenerating) any fact it can't verify.

Requires ORT_LIB_PATH (and DYLD_LIBRARY_PATH on macOS) pointed at the real
onnxruntime library, same as tools/calibrate:

  ORT_LIB_PATH=dist/lib/libonnxruntime.dylib DYLD_LIBRARY_PATH=dist/lib \
    go run ./tools/corpusgen build --out ~/knomit-corpora/narrow-100 \
      --size 100 --diversity narrow --ontology code --topic architecture`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runBuild,
	}

	f := cmd.Flags()
	f.String("out", "", "target directory for the new repo (required)")
	f.Int("size", 100, "target fact count")
	f.String("diversity", "narrow", "diversity profile: narrow (only one implemented so far)")
	f.String("ontology", "code", `ontology preset: "default" (general 13-topic) or "code" (source-code)`)
	f.String("topic", "", "leaf topic to generate into (required for --diversity narrow)")
	f.String("content-source", "synthetic", `"synthetic" (LLM-invented) or "real" (WebSearch-grounded, HTTP-verified citations)`)
	f.Float64("shared-refs-rate", 0.05, "fraction of facts grouped into shared-external-ref clusters (synthetic) or shared-research-angle clusters (real)")
	f.Float64("keyword-overlap-rate", 0.05, "fraction of facts grouped into shared-keyword clusters")
	f.Int64("seed", 42, "seed governing every non-LLM-content structural choice")
	f.String("llm-model", "", "model passed to the claudecli adapter (empty = CLI default)")
	f.String("model-cache", "", "embedding model cache dir (default: ~/.knomit/models)")
	f.String("embed-model", "embeddinggemma", "embedding model id")
	f.Int("batch-size", 8, "facts generated per LLM call (real mode defaults lower unless set explicitly)")
	f.String("branch", "main", "branch to write facts onto (also the branch InitRepo sets as HEAD)")
	f.String("register", "", "if set, copy the finished corpus into the live knomit daemon's repos dir under this name and make it browsable (web UI/API) — failure here is a warning, not a build failure")
	f.String("daemon-url", "http://127.0.0.1:19278", "daemon base URL used by --register")
	_ = cmd.MarkFlagRequired("out")

	return cmd
}

func runBuild(cmd *cobra.Command, args []string) error {
	f := cmd.Flags()
	out, _ := f.GetString("out")
	size, _ := f.GetInt("size")
	diversity, _ := f.GetString("diversity")
	ontologyPreset, _ := f.GetString("ontology")
	topic, _ := f.GetString("topic")
	contentSource, _ := f.GetString("content-source")
	sharedRefsRate, _ := f.GetFloat64("shared-refs-rate")
	keywordOverlapRate, _ := f.GetFloat64("keyword-overlap-rate")
	seed, _ := f.GetInt64("seed")
	llmModel, _ := f.GetString("llm-model")
	modelCache, _ := f.GetString("model-cache")
	embedModelID, _ := f.GetString("embed-model")
	batchSize, _ := f.GetInt("batch-size")
	branch, _ := f.GetString("branch")
	registerAs, _ := f.GetString("register")
	daemonURL, _ := f.GetString("daemon-url")

	if contentSource != "synthetic" && contentSource != "real" {
		return fmt.Errorf("--content-source must be \"synthetic\" or \"real\", got %q", contentSource)
	}
	// Real mode does genuine web searches per batch, which takes meaningfully
	// longer than invented content — default to a smaller batch unless the
	// caller explicitly overrode it.
	if contentSource == "real" && !f.Changed("batch-size") {
		batchSize = 5
	}

	if modelCache == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("resolve default --model-cache: %w", err)
		}
		modelCache = filepath.Join(home, ".knomit", "models")
	}

	ontology, err := fact.OntologyByPreset(ontologyPreset)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("create --out dir: %w", err)
	}
	dbPath := filepath.Join(out, "core.db")

	ctx := context.Background()

	// --- Bootstrap a real repo: same sequence Manager.initLocal uses
	// (lifecycle.go:296), minus the server-lifecycle machinery (background
	// sync, SSE observer) a batch tool has no use for. ---
	svc, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("store.Open: %w", err)
	}
	defer svc.Close()

	ontologyYAML, err := ontology.Serialize()
	if err != nil {
		return fmt.Errorf("serialize ontology: %w", err)
	}
	// agentBranch == branch (both "main" by default): writes land directly on
	// the branch calibrate bridges reads, with no separate agent/main merge
	// step needed — this is a batch generator, not a simulated multi-agent
	// workflow.
	if err := svc.InitRepo(map[string]string{"domains/ontology.yaml": string(ontologyYAML)}, branch); err != nil {
		return fmt.Errorf("InitRepo: %w", err)
	}

	embedModel, err := embeddings.Lookup(embedModelID)
	if err != nil {
		return err
	}
	embedder, err := embeddings.NewEmbedder(ctx, embedModel, modelCache)
	if err != nil {
		return fmt.Errorf("NewEmbedder: %w", err)
	}
	defer embedder.Close()
	svc.SetEmbedder(embedder)

	// --- Structural assignment (seeded, reproducible) ---
	// Real mode overprovisions slots since some facts will be dropped for
	// failing URL verification; the batch loop below stops early once the
	// target size is actually reached, so this is a ceiling, not a promise
	// every slot gets used.
	genSize := size
	if contentSource == "real" {
		genSize = int(float64(size) * realModeOverprovisionFactor)
	}
	rng := rand.New(rand.NewSource(seed))
	slots, err := buildSlots(ontology, diversity, topic, genSize, contentSource, batchSize, sharedRefsRate, keywordOverlapRate, rng)
	if err != nil {
		return err
	}

	// --- Content generation (LLM, not reproducible byte-for-byte) ---
	adapter := llm.NewClaudeCLIAdapter(llmModel)
	if contentSource == "real" {
		adapter.SetAllowedTools([]string{"WebSearch"})
	}

	writtenTotal := 0
	droppedTotal := 0
	var genErr error
batchLoop:
	for start := 0; start < len(slots); start += batchSize {
		if writtenTotal >= size {
			break // real mode: target already reached, skip remaining overprovisioned slots
		}
		end := start + batchSize
		if end > len(slots) {
			end = len(slots)
		}
		batch := slots[start:end]
		fmt.Fprintf(os.Stderr, "generating facts %d-%d of %d...\n", start, end-1, len(slots))

		batchCtx, cancel := context.WithTimeout(ctx, batchTimeout)
		gen, err := generateBatch(batchCtx, adapter, ontology, batch, contentSource)
		cancel()
		if err != nil {
			// Don't lose everything already written: stop generating further
			// batches, but still fall through to Sync + manifest below with
			// whatever made it in so far. This is the exact failure mode that
			// lost an entire 1,200-fact run to a mid-run session-limit error
			// with zero facts persisted — see git history on this branch.
			genErr = fmt.Errorf("generate batch [%d:%d): %w", start, end, err)
			fmt.Fprintf(os.Stderr, "%v (stopping; %d facts already written will still be saved)\n", genErr, writtenTotal)
			break batchLoop
		}

		if contentSource == "real" {
			keptSlots, keptGen, dropped := verifyAndFilter(ctx, batch, gen)
			droppedTotal += dropped
			if dropped > 0 {
				fmt.Fprintf(os.Stderr, "  dropped %d/%d facts with no verifiable citation\n", dropped, len(batch))
			}
			batch, gen = keptSlots, keptGen
		}

		batchFiles := make(map[string]string, len(batch))
		for i, slot := range batch {
			path, content, err := buildFact(ontology, "kb", slot, gen[i])
			if err != nil {
				genErr = err
				break batchLoop
			}
			batchFiles[path] = content
		}
		if len(batchFiles) == 0 {
			continue
		}
		// Incremental write: one commit per batch, not one accumulated
		// commit at the very end, so a later batch's failure still leaves
		// every already-generated fact durably saved in the repo.
		msg := fmt.Sprintf("corpusgen: batch %d-%d", start, end-1)
		if _, _, err := svc.Facts().BatchWriteFacts(ctx, branch, batchFiles, nil, msg, "corpusgen"); err != nil {
			genErr = fmt.Errorf("BatchWriteFacts [%d:%d): %w", start, end, err)
			break batchLoop
		}
		writtenTotal += len(batchFiles)
	}

	if contentSource == "real" && writtenTotal < size && genErr == nil {
		fmt.Fprintf(os.Stderr, "warning: reached end of overprovisioned slots with only %d/%d facts kept "+
			"(%d dropped for failing verification) — consider a higher --shared-refs-rate overprovision "+
			"or checking network/search reliability\n", writtenTotal, size, droppedTotal)
	}

	// One terminal Sync, not per-fact/per-batch — Sync's documented behavior
	// (missing meta.last_commit -> full rebuild) makes this sufficient
	// regardless of how many commits the writes landed as. Skipping this
	// step is the single easiest mistake to make here — it produces a .db
	// that opens fine but yields "no bridge candidates found" from
	// calibrate bridges (looks broken, is actually just un-indexed).
	if writtenTotal > 0 {
		fmt.Fprintln(os.Stderr, "syncing index (embedding + graph build)...")
		if err := svc.IndexManager().Sync(ctx, branch); err != nil {
			return fmt.Errorf("index sync: %w", err)
		}
	}

	if err := writeManifest(out, manifest{
		GeneratedAt:        time.Now(),
		Size:               size,
		Diversity:          diversity,
		Ontology:           ontologyPreset,
		Topic:              topic,
		Seed:               seed,
		LLMProvider:        "claudecli",
		LLMModel:           llmModel,
		EmbedModel:         embedModelID,
		BatchSize:          batchSize,
		SharedRefsRate:     sharedRefsRate,
		KeywordOverlapRate: keywordOverlapRate,
		FactCount:          writtenTotal,
		DBPath:             dbPath,
	}); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	fmt.Fprintf(os.Stderr, "done: %d facts written to %s\n", writtenTotal, dbPath)

	if registerAs != "" && writtenTotal > 0 {
		// Flush the WAL into the main file first — Checkpoint's documented
		// purpose is exactly "before file-level copy" (internal/store/service.go),
		// which is exactly what registerWithDaemon does next.
		if err := svc.Checkpoint(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: checkpoint before registration failed: %v\n", err)
		}
		fmt.Fprintf(os.Stderr, "registering %q with daemon at %s...\n", registerAs, daemonURL)
		if err := registerWithDaemon(ctx, dbPath, registerAs, branch, daemonURL); err != nil {
			fmt.Fprintf(os.Stderr, "warning: registration failed (corpus is still complete and usable via calibrate bridges): %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "registered: repo %q now visible in the web UI/API\n", registerAs)
		}
	}

	if genErr != nil {
		return fmt.Errorf("generation stopped early (%d facts were still saved): %w", writtenTotal, genErr)
	}
	return nil
}
