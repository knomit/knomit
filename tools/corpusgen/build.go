package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"knomit/internal/embeddings"
	"knomit/internal/fact"
	"knomit/internal/llm"
	"knomit/internal/mcp"
	"knomit/internal/repos"
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

--content-source real (default) requires the model to use its WebSearch tool
and ground every fact in a genuinely real, HTTP-verified citation — dropping
(and regenerating) any fact it can't verify. --content-source synthetic asks
the LLM to invent plausible content instead; build synthetic corpora only
deliberately, as a comparison point — they should not be used to draw
conclusions about the bridge tool's real-world readiness.

If --out already contains an incomplete build (MANIFEST.json with
complete=false), build RESUMES it automatically — continuing from the last
completed batch using that build's own saved parameters, ignoring any
conflicting flags on this invocation. This makes it safe to just re-run the
exact same command after an interruption (a killed process, a machine sleep
stalling a batch for hours, a session-limit error) instead of starting over
from zero and re-spending the LLM calls already paid for.

--diversity narrow assigns every fact one fixed --topic; --diversity broad
spreads facts round-robin across every real-world topic in the ontology
instead, which is the only profile that can produce facts NOT already
sharing a domain/entity tag by construction — see diversity.go's
buildBroadSlots doc comment for why that distinction matters for evaluating
keyword bridges specifically.

Facts are written through the real knomit_learn logic (internal/mcp.LearnHandler),
called in-process — the same dedup-merge, evidence-weight computation, and
agent-branch write behavior a real MCP knomit_learn call gets, not a
simplified direct write. This means --branch must be the repo's real agent
branch, not an arbitrary name: writes are only accepted on that branch.

--db <path> targets an arbitrary EXISTING .db file directly (instead of the
--out/<out>/core.db convention) and APPENDS --size more facts to it — for
growing a repo corpusgen didn't itself create (e.g. a copy of a
live-daemon-registered repo's file). This mode requires --branch to be
passed explicitly (the target repo's real agent branch — corpusgen has no
way to discover it automatically for a repo it didn't create) and does not
write a corpusgen MANIFEST.json (there is no corpusgen-native resume state
for a foreign repo; each --db run is its own batch of new facts).

Do NOT point --db at a file the live daemon currently has open — there is no
mechanism for the daemon to detect or reload an externally-modified repo
(Rescan explicitly skips repos it already has open); stop the daemon first,
run corpusgen against its file directly, then restart it.

Requires ORT_LIB_PATH (and DYLD_LIBRARY_PATH on macOS) pointed at the real
onnxruntime library, same as tools/calibrate:

  ORT_LIB_PATH=dist/lib/libonnxruntime.dylib DYLD_LIBRARY_PATH=dist/lib \
    go run ./tools/corpusgen build --out ~/knomit-corpora/narrow-100 \
      --size 100 --diversity narrow --ontology code --topic architecture

  ORT_LIB_PATH=dist/lib/libonnxruntime.dylib DYLD_LIBRARY_PATH=dist/lib \
    go run ./tools/corpusgen build --out ~/knomit-corpora/broad-100 \
      --size 100 --diversity broad --ontology default

  ORT_LIB_PATH=dist/lib/libonnxruntime.dylib DYLD_LIBRARY_PATH=dist/lib \
    go run ./tools/corpusgen build --db ~/.knomit/repos/real-broad-31.db \
      --branch agent/Alexs-MacBook-Air-6.local-60def18b \
      --size 50 --diversity broad --content-source real --ontology default`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runBuild,
	}

	f := cmd.Flags()
	f.String("out", "", "target directory for the new repo (required unless --db is set)")
	f.String("db", "", "path to an existing .db file to append to directly, instead of --out's <out>/core.db convention (requires --branch set explicitly)")
	f.Int("size", 100, "target fact count (--out mode) or additional fact count (--db append mode)")
	f.String("diversity", "narrow", `diversity profile: "narrow" (one fixed --topic) or "broad" (spans every real-world topic in the ontology, for testing whether keyword bridges find connections domain/entity tags don't already state)`)
	f.String("ontology", "code", `ontology preset: "default" (general 13-topic) or "code" (source-code)`)
	f.String("topic", "", "leaf topic to generate into (required for --diversity narrow; ignored for --diversity broad)")
	f.String("content-source", "real", `"real" (WebSearch-grounded, HTTP-verified citations) or "synthetic" (LLM-invented) — real is the default; only build synthetic deliberately for comparison`)
	f.Float64("shared-refs-rate", 0.05, "fraction of facts grouped into shared-external-ref clusters (synthetic) or shared-research-angle clusters (real)")
	f.Float64("keyword-overlap-rate", 0.05, "fraction of facts grouped into shared-keyword clusters")
	f.Int64("seed", 42, "seed governing every non-LLM-content structural choice")
	f.String("llm-model", "", "model passed to the claudecli adapter (empty = CLI default)")
	f.String("model-cache", "", "embedding model cache dir (default: ~/.knomit/models)")
	f.String("embed-model", "embeddinggemma", "embedding model id")
	f.Int("batch-size", 8, "facts generated per LLM call (real mode defaults lower unless set explicitly)")
	f.String("branch", "main", "branch to write facts onto (also the repo's agent branch — facts are only writable on this branch); --db append mode requires this passed explicitly")
	f.String("register", "", "if set, copy the finished corpus into the live knomit daemon's repos dir under this name and make it browsable (web UI/API) — failure here is a warning, not a build failure")
	f.String("daemon-url", "http://127.0.0.1:19278", "daemon base URL used by --register")

	return cmd
}

func runBuild(cmd *cobra.Command, args []string) error {
	f := cmd.Flags()
	out, _ := f.GetString("out")
	dbFlag, _ := f.GetString("db")
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

	if dbFlag == "" && out == "" {
		return fmt.Errorf("--out or --db is required")
	}

	// --- Resolve dbPath + manifestPath for the two targeting modes. ---
	var dbPath, manifestPath string
	if dbFlag != "" {
		dbPath = dbFlag
		// Co-located but uniquely named, not a shared "MANIFEST.json" in
		// dbPath's directory — that directory (e.g. ~/.knomit/repos/) may
		// hold many unrelated repos, and a fixed name there could collide
		// with or misread a different repo's manifest entirely.
		manifestPath = dbPath + ".manifest.json"
	} else {
		if err := os.MkdirAll(out, 0o755); err != nil {
			return fmt.Errorf("create --out dir: %w", err)
		}
		dbPath = filepath.Join(out, "core.db")
		manifestPath = filepath.Join(out, "MANIFEST.json")
	}

	// --- Resume/append detection: an incomplete prior build's own saved
	// parameters win over whatever was passed on this invocation, so
	// re-running the exact same command after an interruption just
	// continues it rather than needing the user to reconstruct the
	// original flags exactly. A db file with NO corpusgen manifest (only
	// possible via --db, pointed at a repo corpusgen didn't create) is a
	// third case — "foreign append" — that skips InitRepo and manifest
	// tracking entirely rather than erroring as inconsistent. ---
	prior, hadManifest, err := loadManifest(manifestPath)
	if err != nil {
		return fmt.Errorf("read existing manifest: %w", err)
	}
	_, dbExists := os.Stat(dbPath)
	dbPresent := dbExists == nil
	if hadManifest && !dbPresent {
		return fmt.Errorf("%q has a manifest but no db at %q — inconsistent state, remove the manifest and start fresh", manifestPath, dbPath)
	}
	foreignAppend := dbPresent && !hadManifest
	resuming := hadManifest && prior != nil && !prior.Complete
	if hadManifest && prior != nil && prior.Complete {
		fmt.Fprintf(os.Stderr, "%q already holds a complete corpus (%d facts) — nothing to do "+
			"(remove it to rebuild from scratch)\n", dbPath, prior.FactCount)
		if registerAs != "" {
			if err := registerWithDaemon(context.Background(), dbPath, registerAs, prior.Branch, daemonURL); err != nil {
				return fmt.Errorf("register: %w", err)
			}
			fmt.Fprintf(os.Stderr, "registered: repo %q now visible in the web UI/API\n", registerAs)
		}
		return nil
	}
	if resuming {
		size, diversity, ontologyPreset, topic = prior.Size, prior.Diversity, prior.Ontology, prior.Topic
		contentSource, sharedRefsRate, keywordOverlapRate = prior.ContentSource, prior.SharedRefsRate, prior.KeywordOverlapRate
		seed, batchSize, embedModelID, branch = prior.Seed, prior.BatchSize, prior.EmbedModel, prior.Branch
		fmt.Fprintf(os.Stderr, "resuming incomplete build in %q: %d/%d facts already written, "+
			"continuing from slot %d (using this build's own original parameters, ignoring any "+
			"conflicting flags passed just now)\n", dbPath, prior.FactCount, size, prior.NextSlotStart)
	}
	if foreignAppend && !f.Changed("branch") {
		return fmt.Errorf("--db %q has no corpusgen manifest (a repo corpusgen didn't create) — "+
			"pass --branch explicitly set to that repo's real agent branch (not the default %q); "+
			"corpusgen has no way to discover it automatically for a foreign repo", dbPath, branch)
	}

	if contentSource != "synthetic" && contentSource != "real" {
		return fmt.Errorf("--content-source must be \"synthetic\" or \"real\", got %q", contentSource)
	}
	// Real mode does genuine web searches per batch, which takes meaningfully
	// longer than invented content — default to a smaller batch unless the
	// caller explicitly overrode it (not applicable when resuming: batchSize
	// already came from the prior manifest above).
	if contentSource == "real" && !resuming && !f.Changed("batch-size") {
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

	ctx := context.Background()

	// --- Bootstrap (fresh) or open (resume) the repo. Same store.Open ->
	// InitRepo -> SetEmbedder sequence Manager.initLocal uses (lifecycle.go:296),
	// minus the server-lifecycle machinery (background sync, SSE observer) a
	// batch tool has no use for. ---
	svc, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("store.Open: %w", err)
	}
	defer svc.Close()

	writtenTotal := 0
	droppedTotal := 0
	resumeStart := 0
	if resuming {
		writtenTotal = prior.FactCount
		droppedTotal = prior.DroppedTotal
		resumeStart = prior.NextSlotStart
		if err := svc.OpenRepo(); err != nil {
			return fmt.Errorf("OpenRepo (resume): %w", err)
		}
	} else if foreignAppend {
		if err := svc.OpenRepo(); err != nil {
			return fmt.Errorf("OpenRepo (foreign append): %w", err)
		}
	} else {
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

	// --- Build the RepoInstance + Binding that LearnHandler needs, once,
	// up front. repos.NewTestInstanceWithDeps is documented "intended for
	// handler/integration tests in sibling packages... production code must
	// use Manager.openOne instead" — deliberately NOT what's used here:
	// Manager.openOne pulls in the full live-daemon lifecycle (background
	// sync, remote push/pull reconciliation, config/signer/keyPath deps
	// corpusgen has no use for and shouldn't fabricate) for a one-shot batch
	// writer that opens one repo, writes some facts, and exits. This is the
	// deliberate exception: no Manager, no background goroutines, just
	// LearnHandler's real dedup-merge/evidence-weight/agent-branch logic
	// running against a store this process already owns.
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name:         "corpusgen",
		AgentBranch:  branch,
		Svc:          svc,
		Ontology:     ontology,
		OntologyRoot: "kb",
		Embedder:     embedder,
	})
	binding := repos.NewBindingOfRepo(ri, branch)
	ctx = repos.WithBinding(ctx, binding)

	baseManifest := manifest{
		Size: size, Diversity: diversity, Ontology: ontologyPreset, Topic: topic,
		ContentSource: contentSource, Seed: seed, LLMProvider: "claudecli", LLMModel: llmModel,
		EmbedModel: embedModelID, BatchSize: batchSize, SharedRefsRate: sharedRefsRate,
		KeywordOverlapRate: keywordOverlapRate, Branch: branch, DBPath: dbPath,
	}
	checkpoint := func(nextSlotStart int, complete bool) error {
		if foreignAppend {
			// No corpusgen-native resume state for a repo corpusgen didn't
			// create — the facts themselves are already durably committed
			// by BatchWriteFacts inside LearnHandler; there's just nothing
			// to track here.
			return nil
		}
		m := baseManifest
		m.GeneratedAt = time.Now()
		m.FactCount = writtenTotal
		m.NextSlotStart = nextSlotStart
		m.DroppedTotal = droppedTotal
		m.Complete = complete
		return writeManifest(manifestPath, m)
	}
	if !resuming && !foreignAppend {
		// Checkpoint immediately after a successful fresh bootstrap so even a
		// kill during the very first batch leaves a valid, resumable state
		// (NextSlotStart=0 — equivalent to just restarting the loop).
		if err := checkpoint(0, false); err != nil {
			return fmt.Errorf("write initial manifest: %w", err)
		}
	}

	// --- Structural assignment (seeded, reproducible) ---
	// Real mode overprovisions slots since some facts will be dropped for
	// failing URL verification; the batch loop below stops early once the
	// target size is actually reached, so this is a ceiling, not a promise
	// every slot gets used. Regenerating the full deterministic slot array
	// from the same seed+params (rather than persisting it) is what lets a
	// resume just start the loop at resumeStart and get the identical
	// structure the interrupted run would have produced.
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

	var genErr error
batchLoop:
	for start := resumeStart; start < len(slots); start += batchSize {
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
			// whatever made it in so far, and leave NextSlotStart at this
			// batch's own start so a resume retries exactly this batch next.
			genErr = fmt.Errorf("generate batch [%d:%d): %w", start, end, err)
			fmt.Fprintf(os.Stderr, "%v (stopping; %d facts already written will still be saved; "+
				"re-run the same command to resume from slot %d)\n", genErr, writtenTotal, start)
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

		if len(batch) > 0 {
			// Incremental write: one learn call (one commit) per batch, not
			// one accumulated commit at the very end, so a later batch's
			// failure still leaves every already-generated fact durably
			// saved in the repo. Goes through the real knomit_learn logic
			// (dedup-merge, evidence-weight computation) rather than a
			// direct BatchWriteFacts — see the RepoInstance/Binding setup
			// above for why LearnHandler can be called in-process like this.
			var req mcpgo.CallToolRequest
			req.Params.Arguments = map[string]any{
				"moment_name": fmt.Sprintf("corpusgen: batch %d-%d", start, end-1),
				"facts":       factsFromBatch(batch, gen),
			}
			result, err := mcp.LearnHandler(embedder)(ctx, req)
			if err != nil {
				genErr = fmt.Errorf("learn batch [%d:%d): %w", start, end, err)
				break batchLoop
			}
			if result.IsError {
				genErr = fmt.Errorf("learn batch [%d:%d): %s", start, end, resultErrorText(result))
				break batchLoop
			}
			writtenTotal += len(batch)
		}
		// Checkpoint after every batch (success or a partial with drops but no
		// hard error) — not just at the very end — so a hard kill or crash
		// (not just a clean error return) still leaves an accurate resume
		// point. This is what makes --keep-usage-from-being-wasted actually
		// hold: without it, only the final write below would ever run, which
		// a SIGKILL or a machine sleep stall skips entirely.
		if err := checkpoint(end, false); err != nil {
			genErr = fmt.Errorf("checkpoint after batch [%d:%d): %w", start, end, err)
			break batchLoop
		}
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

	finalNextStart := len(slots)
	if genErr != nil {
		// Preserve whatever NextSlotStart the last successful checkpoint
		// recorded rather than jumping to the end — re-reading it keeps this
		// single code path authoritative instead of duplicating the batch
		// loop's bookkeeping.
		if m, ok, _ := loadManifest(manifestPath); ok && m != nil {
			finalNextStart = m.NextSlotStart
		}
	}
	if err := checkpoint(finalNextStart, genErr == nil); err != nil {
		return fmt.Errorf("write final manifest: %w", err)
	}

	fmt.Fprintf(os.Stderr, "done: %d facts written to %s\n", writtenTotal, dbPath)

	if registerAs != "" && writtenTotal > 0 {
		// registerWithDaemon checkpoints dbPath itself before copying it, so
		// no separate checkpoint is needed here.
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

// factsFromBatch converts a generated batch into the raw map shape
// knomit_learn's "facts" argument expects (see internal/mcp/learn.go's
// learnFactInput) — LearnHandler does its own path-building/serialization
// internally, so corpusgen no longer needs a separate buildFact step (the
// now-deleted factbuilder.go used to do this).
func factsFromBatch(batch []factSlot, gen []generatedContent) []any {
	out := make([]any, len(batch))
	for i, slot := range batch {
		g := gen[i]
		domain := g.Domain
		if domain == nil {
			domain = []string{}
		}
		entities := g.Entities
		if entities == nil {
			entities = []string{}
		}
		// Real mode: g.Refs carries the model's own (HTTP-verified — see
		// verify.go) citation URLs. Synthetic mode: g.Refs is always empty
		// (the synthetic prompt contract has no refs field), so this
		// reduces to the scripted SharedRefURL exactly as before — mirrors
		// the merge the deleted factbuilder.go used to do.
		refs := append([]string{}, g.Refs...)
		if slot.SharedRefURL != "" {
			refs = append(refs, slot.SharedRefURL)
		}
		out[i] = map[string]any{
			"topic":      slot.Topic,
			"category":   slot.Category,
			"kind":       string(slot.Kind),
			"type":       string(slot.Type),
			"title":      g.Title,
			"body":       g.Body,
			"domain":     domain,
			"entities":   entities,
			"confidence": slot.Confidence,
			"sources":    slot.Sources,
			"refs":       refs,
			// corpusgen-generated facts (including synthesis-typed ones) are
			// "authored" by this tool, not distilled/discovered — omitting
			// origin on a synthesis-typed fact defaults to "distilled" (see
			// knomit_learn's own origin field docs), which would misrepresent
			// how these facts came to exist.
			"origin": "authored",
		}
	}
	return out
}

// resultErrorText extracts the human-readable message from a LearnHandler
// error result (IsError=true) — handler errors are reported as TextContent
// inside the result, not as the Go error return (see mcpgo.CallToolResult's
// doc comment: tool errors belong in the result so an LLM caller can see and
// self-correct, not as an MCP protocol-level error).
func resultErrorText(result *mcpgo.CallToolResult) string {
	for _, c := range result.Content {
		if tc, ok := c.(mcpgo.TextContent); ok {
			return tc.Text
		}
	}
	return "unknown error"
}
