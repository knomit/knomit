// Command motifannex drives the Phase-2 GATE annex: real review sessions over
// COPIES of live corpora, with a real embedder, so the motif passes can be
// measured on corpora of genuinely different character.
//
// It never touches a live home. Every run copies the corpus database into a
// scratch directory and works there; the source is opened read-only to copy
// and never again.
//
// The LLM half is work-stealing, exactly as in production: this tool DUMPS the
// work items a session produced and APPLIES answers supplied as JSON. The
// model answering them is whatever agent is driving the tool — which is the
// same arrangement knomit_review uses against a live repo.
//
// Usage:
//
//	motifannex snapshot  -corpus <name>            copy a corpus and report its shape
//	motifannex session   -corpus <name>            run one session, dump its items
//	motifannex answer    -corpus <name> -in a.json apply answers, report health
//	motifannex report    -corpus <name>            vocabulary/coverage/health snapshot
//	motifannex bridges   -corpus <name> -effort h  motif bridge candidates, served and dropped
//	motifannex namedef   -corpus <name>            name / name+def cosine ladders, centered and not
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"knomit/internal/config"
	"knomit/internal/embeddings"
	"knomit/internal/fact"
	"knomit/internal/repos"
	"knomit/internal/store"
	"knomit/internal/synthesize"
)

// corpora maps the annex's corpus names to their live database ids.
var corpora = map[string]string{
	"knomit-kb":           "3HhAGPyhxhweCgIRPYdxvYMROAe",
	"core":                "3HhAGRep1sec4EUtxRGxmwgwiAh",
	"agentic-engineering": "3HhAGVQUHeeYpvvxXXCb7Zs3WCL",
	"knomit-io-kb":        "3HhAGT67HoGHpnSaScYXsbNEH6o",
	// Derived corpora: built by `merge`, never copied from a live home.
	"merged":        "",
	"core.pristine": "",
}

func main() {
	if len(os.Args) < 2 {
		fatal(fmt.Errorf("usage: motifannex <snapshot|session|answer|report|bridges|namedef> -corpus <name>"))
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	corpus := fs.String("corpus", "", "corpus name")
	in := fs.String("in", "", "answers JSON (answer)")
	effort := fs.String("effort", "high", "discovery effort for `bridges` (normal|medium|high)")
	scratch := fs.String("scratch", defaultScratch(), "working directory for copies")
	_ = fs.Parse(os.Args[2:])

	if *corpus == "" && cmd != "merge" {
		*corpus = "merged"
	}
	if *corpus == "" {
		fatal(fmt.Errorf("-corpus is required"))
	}
	id, ok := corpora[*corpus]
	if !ok {
		fatal(fmt.Errorf("unknown corpus %q", *corpus))
	}

	ctx := context.Background()
	switch cmd {
	case "snapshot":
		if id == "" {
			fatal(fmt.Errorf("%s is derived, not snapshotted", *corpus))
		}
		fatal(snapshot(ctx, *corpus, id, *scratch))
	case "session":
		fatal(session(ctx, *corpus, *scratch))
	case "plan":
		fatal(plan(ctx, *corpus, *scratch))
	case "apply":
		fatal(apply(ctx, *corpus, *scratch, *in))
	case "answer":
		fatal(answer(ctx, *corpus, *scratch, *in))
	case "merge":
		fatal(merge(ctx, *scratch))
	case "prunebase":
		fatal(prunebase(ctx, *scratch))
	case "report":
		fatal(report(ctx, *corpus, *scratch))
	case "bridges":
		fatal(bridges(ctx, *corpus, *scratch, *effort))
	case "namedef":
		fatal(namedef(ctx, *corpus, *scratch))
	default:
		fatal(fmt.Errorf("unknown command %q", cmd))
	}
}

func defaultScratch() string {
	return filepath.Join(os.TempDir(), "knomit-motif-annex")
}

func copyPath(scratch, corpus string) string {
	return filepath.Join(scratch, corpus+".db")
}

// pristinePath holds the corpus as it stood before the last plan run.
func pristinePath(scratch, corpus string) string {
	return filepath.Join(scratch, corpus+".pristine.db")
}

// snapshot copies a live corpus into the scratch directory.
//
// Copy, never open-in-place: these are the user's real knowledge bases, and
// the annex writes motifs onto facts. Opening a live home would also migrate
// its schema, which is a change no measurement is worth.
//
// CHECKPOINT FIRST. The stores run in WAL mode, so a live corpus's most recent
// writes can be sitting in a `-wal` sidecar that a file copy of the `.db` does
// not take. Service.Checkpoint exists for exactly this ("before file-level
// copy") and was not being called, so a snapshot could silently omit the
// newest facts — a measurement reading low with nothing to say it had.
//
// HONEST NOTE ON EARLIER RUNS: annex snapshots taken before this fix may be
// missing an uncheckpointed WAL tail. The direction is knowable even though the
// amount is not — a snapshot can only LOSE recent facts, never invent them, so
// coverage and vocabulary figures from those runs are floors rather than
// point estimates. It does not change any gate conclusion in the direction that
// would matter (more facts can only add candidates), but the numbers should be
// read as such rather than as exact.
func snapshot(ctx context.Context, corpus, id, scratch string) error {
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	src := filepath.Join(home, ".knomit", "repos", id+".db")

	// Open the live home ONLY to flush its WAL, and close it again before the
	// copy. This is the one moment the annex touches a real home, it takes no
	// write of its own, and skipping it would be the silent-truncation failure
	// this tool exists to measure around.
	if err := checkpointLiveHome(src); err != nil {
		return fmt.Errorf("checkpoint %s before copy: %w", corpus, err)
	}

	dst := copyPath(scratch, corpus)
	if err := copyFile(src, dst); err != nil {
		return fmt.Errorf("copy %s: %w", corpus, err)
	}
	fmt.Printf("copied %s -> %s (WAL checkpointed first)\n", src, dst)
	return report(ctx, corpus, scratch)
}

// checkpointLiveHome flushes a live corpus's WAL into its .db file so a
// file-level copy is self-contained.
//
// Reported rather than ignored on failure: a snapshot that could not checkpoint
// is a snapshot that may be short of facts, and the whole point of the annex is
// that its inputs are what they claim to be.
func checkpointLiveHome(path string) error {
	svc, err := store.Open(path)
	if err != nil {
		return err
	}
	defer svc.Close()
	return svc.Checkpoint()
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// open builds a store + repo instance over the COPY, with the real embedder.
//
// The lab guard runs FIRST — before the stat, and before store.Open, because
// store.Open migrates the schema of whatever it is handed. See refuseLivePath.
func open(ctx context.Context, corpus, scratch string) (*store.Service, *repos.RepoInstance, string, func(), error) {
	home, _ := os.UserHomeDir()
	path := copyPath(scratch, corpus)
	if err := refuseLivePath(path, home); err != nil {
		return nil, nil, "", nil, err
	}
	if _, err := os.Stat(path); err != nil {
		return nil, nil, "", nil, fmt.Errorf("no snapshot for %s — run `snapshot` first: %w", corpus, err)
	}
	svc, err := store.Open(path)
	if err != nil {
		return nil, nil, "", nil, err
	}
	// The corpora were embedded with this model (meta.embed_model_id); the
	// annex must use the SAME one or every stored vector is unreadable and
	// every similarity is nonsense.
	model, err := embeddings.Lookup("embeddinggemma")
	if err != nil {
		svc.Close()
		return nil, nil, "", nil, err
	}
	emb, err := embeddings.NewEmbedder(ctx, model, filepath.Join(home, ".knomit", "models"))
	if err != nil {
		svc.Close()
		return nil, nil, "", nil, fmt.Errorf("embedder: %w", err)
	}
	svc.SetEmbedder(emb)
	// The git layer lives in the same database (objects table) but must be
	// opened explicitly; without it every history read hits a nil repository.
	if err := svc.OpenRepo(); err != nil {
		emb.Close()
		svc.Close()
		return nil, nil, "", nil, fmt.Errorf("open repo: %w", err)
	}
	branch, err := branchOf(ctx, svc)
	if err != nil {
		svc.Close()
		return nil, nil, "", nil, err
	}
	ri := repos.NewTestInstanceWithDeps(repos.TestInstanceConfig{
		Name: corpus, AgentBranch: branch, Svc: svc, OntologyRoot: "kb", Embedder: emb,
		Quality: productionQuality(),
	})
	return svc, ri, branch, func() { emb.Close(); svc.Close() }, nil
}

// productionQuality gives the instance the SHIPPED bridge-quality knobs.
//
// WITHOUT IT THE TOOL MEASURES AN ENGINE THAT CANNOT EMIT. A bare test
// instance leaves MaxMembers at zero, and a zero member cap gates out every
// bridge candidate there is — so a `bridges` run reports "0 near, 0 far" on
// every corpus and it reads as a finding about the corpora. It is not; it is a
// finding about the harness. Phase-3's rulings-3 already caught this once in
// the test harness (deviation #3) and TestInstanceConfig.Quality's own doc
// comment warns about it in as many words; this tool walked into it anyway,
// which is why the values are now READ rather than re-typed.
//
// Read from config.Defaults() rather than restated: the Phase-3 review noted
// that the test harness's re-typed copies could drift from the real defaults
// and only a sibling test pinned them. A measurement tool that drifts from
// production configuration is measuring a different engine, quietly.
func productionQuality() *repos.TestQualityConfig {
	d := config.Defaults().Discovery
	return &repos.TestQualityConfig{
		CohFloor:     d.CohFloor,
		QualityFloor: d.QualityFloor,
		WCoh:         d.WCoh,
		WGap:         d.WGap,
		WSpec:        d.WSpec,
		MaxMembers:   d.MaxMembers,
	}
}

// branchOf finds the branch carrying the most facts — the corpus's own agent
// branch, whose name differs per machine and per key.
func branchOf(ctx context.Context, svc *store.Service) (string, error) {
	branches, err := svc.Branches().ListBranches(ctx)
	if err != nil {
		return "", err
	}
	best, bestN := "", -1
	for _, b := range branches {
		res, err := svc.FactQuery().Search(ctx, b.Name, store.SearchOptions{Limit: 100_000})
		if err != nil {
			continue
		}
		if len(res) > bestN {
			best, bestN = b.Name, len(res)
		}
	}
	if best == "" {
		return "", fmt.Errorf("no branches with facts")
	}
	fmt.Fprintf(os.Stderr, "branch %q (%d facts)\n", best, bestN)
	return best, nil
}

func fatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// ── session / step ────────────────────────────────────────────────────────
//
// The real serve loop, exactly as knomit_review drives it: StartSession
// returns the first item, and answering one returns the next. The annex does
// NOT enumerate the queue out of band — measuring a shortcut would measure
// something the product does not do.

type servedItem struct {
	Corpus    string   `json:"corpus"`
	SessionID string   `json:"session_id"`
	ItemID    int64    `json:"item_id"`
	Type      string   `json:"type"`
	Prompt    string   `json:"prompt,omitempty"`
	Facts     string   `json:"facts,omitempty"`
	Done      bool     `json:"done,omitempty"`
	Health    []string `json:"health,omitempty"`
}

func serve(res *synthesize.ReviewResult, corpus string) servedItem {
	out := servedItem{Corpus: corpus, SessionID: res.SessionID, Health: res.Health, Done: res.Done}
	if res.Item != nil {
		out.ItemID = res.Item.ID
		out.Type = res.Item.Type
		out.Prompt = res.Item.Prompt
		out.Facts = string(res.Item.Facts)
	}
	return out
}

func emit(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

// motifStep reports whether a step type is one the annex measures.
func motifStep(t string) bool {
	switch t {
	case "motif_alias", "motif_define", "motif_backfill":
		return true
	}
	return false
}

// emptyFor is the empty-but-valid response for an ordinary step type.
//
// The annex drains prune/distill/discover/reflect rather than answering them.
// Answering substantively would spend real judgement measuring passes this
// annex is not about, AND would mutate the corpus — merging facts, writing
// syntheses — which confounds every motif measurement taken afterwards.
func emptyFor(stepType string) string {
	switch stepType {
	case "prune":
		return `{"decisions":[],"merges":[]}`
	case "distill":
		return `{"synthesize":[],"retract":[]}`
	case "reflect":
		return `{"reasoning":"annex: not measured","reinforce":[],"propose":[]}`
	case "discover":
		return `{"proposals":[]}`
	}
	return `{}`
}

// drain answers ordinary items automatically until a MOTIF item is served or
// the session ends, so the driving model spends its turns only on the passes
// under measurement.
func drain(ctx context.Context, rv *synthesize.Reviewer, res *synthesize.ReviewResult, corpus string) (*synthesize.ReviewResult, error) {
	const guard = 400 // a session that never drains is a bug, not a long queue
	for i := 0; i < guard; i++ {
		if res.Done || res.Item == nil || motifStep(res.Item.Type) {
			return res, nil
		}
		// A PAGED item must be read to its final page before it can be
		// answered — the completion token proves the whole cluster was seen,
		// and the engine rejects an answer without it. The annex is not
		// measuring these passes, but it still has to satisfy that contract to
		// drain them: paging through is the only honest way past, and faking a
		// token would be defeating a guard rather than obeying it.
		token := ""
		if res.Item.MoreAvailable {
			var perr error
			token, perr = pageToEnd(ctx, rv, res.SessionID, res.Item.ID, res.Item.Pages)
			if perr != nil {
				return nil, perr
			}
		}
		next, err := rv.ContinueSessionForItemPaged(ctx, res.SessionID, emptyFor(res.Item.Type), res.Item.ID, token)
		if err != nil {
			return nil, fmt.Errorf("drain %s item %d: %w", res.Item.Type, res.Item.ID, err)
		}
		res = next
	}
	return nil, fmt.Errorf("drain exceeded %d items", guard)
}

// plan runs a throwaway session and collects EVERY motif item it produces.
//
// Two phases are necessary because the session database is deleted on every
// store.Open ("a session cursor from a previous run is dead anyway"), so a
// session cannot outlive the process. plan discovers the items; apply re-runs
// the session and answers them. The corpus does not change in between, so the
// same items are produced — and apply asserts that rather than assuming it.
func plan(ctx context.Context, corpus, scratch string) error {
	// Save a pristine copy FIRST. Draining a session is not free — answering
	// items advances the review watermark, so the next session sees a
	// different seed set and may plan different work. Measured: the first
	// plan/apply attempt produced a backfill item during plan and NONE during
	// apply, because the plan run had already consumed the dirty set.
	//
	// apply restores this copy before re-running, so it faces exactly the
	// corpus plan faced and produces exactly the items plan reported.
	if err := copyFile(copyPath(scratch, corpus), pristinePath(scratch, corpus)); err != nil {
		return fmt.Errorf("save pristine: %w", err)
	}
	if err := rewindWatermark(ctx, corpus, scratch); err != nil {
		return err
	}
	_, ri, _, closeAll, err := open(ctx, corpus, scratch)
	if err != nil {
		return err
	}
	defer closeAll()

	rv := synthesize.NewReviewerWithOptions(ri, nil, synthesize.EffortMedium, synthesize.ScopeFilter{})
	res, err := rv.StartSession(ctx)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	out := sessionPlan{Corpus: corpus, Health: res.Health}
	for {
		res, err = drain(ctx, rv, res, corpus)
		if err != nil {
			return err
		}
		if res.Done || res.Item == nil {
			break
		}
		out.Items = append(out.Items, dumpedItem{
			StepType: res.Item.Type,
			Facts:    string(res.Item.Facts),
		})
		// Advance past it with an empty answer; this session is discarded.
		res, err = rv.ContinueSessionForItem(ctx, res.SessionID, skipFor(res.Item.Type), res.Item.ID)
		if err != nil {
			return fmt.Errorf("advance %s: %w", res.Item.Type, err)
		}
	}
	return emit(out)
}

type dumpedItem struct {
	StepType string `json:"step_type"`
	Facts    string `json:"facts"`
}

type sessionPlan struct {
	Corpus string       `json:"corpus"`
	Health []string     `json:"health"`
	Items  []dumpedItem `json:"items"`
}

// skipFor is the empty-but-valid response for a MOTIF step type.
func skipFor(stepType string) string {
	switch stepType {
	case "motif_alias":
		return `{"verdicts":[]}`
	case "motif_define":
		return `{"definitions":[]}`
	case "motif_backfill":
		return `{"assignments":[]}`
	}
	return emptyFor(stepType)
}

// apply re-runs the session and answers each motif item from the answers file,
// keyed by step type — a session plans at most one item of each motif type.
func apply(ctx context.Context, corpus, scratch, in string) error {
	// Rewind to the state plan saw, so the same items are produced.
	if _, err := os.Stat(pristinePath(scratch, corpus)); err == nil {
		if err := copyFile(pristinePath(scratch, corpus), copyPath(scratch, corpus)); err != nil {
			return fmt.Errorf("restore pristine: %w", err)
		}
	}
	if err := rewindWatermark(ctx, corpus, scratch); err != nil {
		return err
	}
	raw, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	var answers map[string]json.RawMessage
	if err := json.Unmarshal(raw, &answers); err != nil {
		return err
	}

	_, ri, _, closeAll, err := open(ctx, corpus, scratch)
	if err != nil {
		return err
	}
	defer closeAll()

	rv := synthesize.NewReviewerWithOptions(ri, nil, synthesize.EffortMedium, synthesize.ScopeFilter{})
	res, err := rv.StartSession(ctx)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	answered := map[string]bool{}
	for {
		res, err = drain(ctx, rv, res, corpus)
		if err != nil {
			return err
		}
		if res.Done || res.Item == nil {
			break
		}
		body, ok := answers[res.Item.Type]
		if !ok || answered[res.Item.Type] {
			body = json.RawMessage(skipFor(res.Item.Type))
		}
		answered[res.Item.Type] = true
		res, err = rv.ContinueSessionForItem(ctx, res.SessionID, string(body), res.Item.ID)
		if err != nil {
			return fmt.Errorf("answer %s: %w", res.Item.Type, err)
		}
	}
	for k := range answers {
		if !answered[k] {
			fmt.Fprintf(os.Stderr, "WARNING: no %s item appeared; that answer was unused\n", k)
		}
	}
	return report(ctx, corpus, scratch)
}

// pageToEnd walks a paged item to its last page and returns the completion
// token, which only the final page carries.
func pageToEnd(ctx context.Context, rv *synthesize.Reviewer, sessionID string, itemID int64, pages int) (string, error) {
	if pages < 1 {
		pages = 1
	}
	for p := 1; p <= pages; p++ {
		res, err := rv.PageItem(ctx, sessionID, itemID, p)
		if err != nil {
			return "", fmt.Errorf("page %d of item %d: %w", p, itemID, err)
		}
		if res.Item != nil && res.Item.CompletionToken != "" {
			return res.Item.CompletionToken, nil
		}
	}
	return "", fmt.Errorf("item %d: no completion token after %d pages", itemID, pages)
}

func session(ctx context.Context, corpus, scratch string) error {
	_, ri, _, closeAll, err := open(ctx, corpus, scratch)
	if err != nil {
		return err
	}
	defer closeAll()
	rv := synthesize.NewReviewerWithOptions(ri, nil, synthesize.EffortMedium, synthesize.ScopeFilter{})
	res, err := rv.StartSession(ctx)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	res, err = drain(ctx, rv, res, corpus)
	if err != nil {
		return err
	}
	return emit(serve(res, corpus))
}

// ── answer ────────────────────────────────────────────────────────────────

type answerFile struct {
	SessionID string          `json:"session_id"`
	ItemID    int64           `json:"item_id"`
	Response  json.RawMessage `json:"response"`
	// Skip answers the item with an empty acknowledgement, for step types the
	// annex is not measuring (prune/distill/discover/reflect). The queue has to
	// drain for the session to advance, and answering them substantively would
	// be measuring passes this annex is not about — and spending real judgement
	// on them.
	Skip bool `json:"skip,omitempty"`
}

func answer(ctx context.Context, corpus, scratch, in string) error {
	if in == "" {
		return fmt.Errorf("-in is required")
	}
	raw, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	var af answerFile
	if err := json.Unmarshal(raw, &af); err != nil {
		return err
	}
	_, ri, _, closeAll, err := open(ctx, corpus, scratch)
	if err != nil {
		return err
	}
	defer closeAll()

	body := string(af.Response)
	if af.Skip {
		body = skipBodyFor(af)
	}
	rv := synthesize.NewReviewerWithOptions(ri, nil, synthesize.EffortMedium, synthesize.ScopeFilter{})
	res, err := rv.ContinueSessionForItem(ctx, af.SessionID, body, af.ItemID)
	if err != nil {
		return fmt.Errorf("continue: %w", err)
	}
	res, err = drain(ctx, rv, res, corpus)
	if err != nil {
		return err
	}
	return emit(serve(res, corpus))
}

// skipBodyFor returns the empty-but-valid response for a step type, so a pass
// the annex does not measure can be drained without inventing content for it.
func skipBodyFor(af answerFile) string {
	switch strings.TrimSpace(string(af.Response)) {
	case "prune":
		return `{"decisions":[],"merges":[]}`
	case "distill":
		return `{"synthesize":[],"retract":[]}`
	case "reflect":
		return `{"reasoning":"annex skip","reinforce":[],"propose":[]}`
	case "discover":
		return `{"proposals":[]}`
	case "motif_alias":
		return `{"verdicts":[]}`
	case "motif_define":
		return `{"definitions":[]}`
	case "motif_backfill":
		return `{"assignments":[]}`
	}
	return `{}`
}

// ── report ────────────────────────────────────────────────────────────────

type corpusReport struct {
	Corpus       string  `json:"corpus"`
	Branch       string  `json:"branch"`
	AuthoredLive int     `json:"authored_live"`
	WithMotifs   int     `json:"with_motifs"`
	Coverage     float64 `json:"coverage"`
	Clusters     int     `json:"clusters"`
	Recurring    int     `json:"recurring_df2plus"`
	Recurrence   float64 `json:"recurrence_rate"`
	MintToLink   float64 `json:"mint_to_link"`
	NeedDefine   int     `json:"clusters_needing_definition"`
	// BridgeablePairs is the CEILING on motif bridging — see bridgeablePairs.
	// Reported next to Recurring because the two answer different questions:
	// Recurring counts CLUSTERS that could bridge, this counts the PAIRS they
	// could bridge, and one heavily-shared motif separates them sharply.
	BridgeablePairs int      `json:"bridgeable_pairs"`
	TopMotifs       []string `json:"top_motifs"`
}

func report(ctx context.Context, corpus, scratch string) error {
	svc, _, branch, closeAll, err := open(ctx, corpus, scratch)
	if err != nil {
		return err
	}
	defer closeAll()

	r := corpusReport{Corpus: corpus, Branch: branch}
	if with, total, err := svc.Motifs().MotifCoverage(ctx, branch); err == nil {
		r.WithMotifs, r.AuthoredLive = with, total
		if total > 0 {
			r.Coverage = float64(with) / float64(total)
		}
	}
	if vh, err := svc.Motifs().VocabularyHealth(ctx, branch); err == nil {
		r.Clusters, r.Recurring = vh.Clusters, vh.Recurring
		r.Recurrence, r.MintToLink = vh.RecurrenceRate(), vh.MintToLinkRatio()
	}
	if need, err := svc.Motifs().ClustersNeedingDefinition(ctx, branch); err == nil {
		r.NeedDefine = len(need)
	}
	if cs, err := svc.Motifs().Clusters(ctx, branch); err == nil {
		dfs := make([]int, 0, len(cs))
		for _, c := range cs {
			dfs = append(dfs, c.DF)
		}
		r.BridgeablePairs = bridgeablePairs(dfs)
		for i, c := range cs {
			if i >= 20 {
				break
			}
			def := ""
			if d, ok, _ := svc.Motifs().Definition(ctx, branch, c.ClusterKey); ok {
				def = " — " + d
			}
			r.TopMotifs = append(r.TopMotifs, fmt.Sprintf("%s (df %d)%s", c.CanonicalID, c.DF, def))
		}
	}
	return emit(r)
}

// rewindWatermark clears the review watermark so the next session sees the
// whole corpus as unreviewed.
//
// THIS IS A HARNESS INTERVENTION AND IT SIMULATES SOMETHING REAL. A session
// with an empty seed pool completes IMMEDIATELY — before Plan runs, so before
// any motif work is planned (pipeline.go: "An empty seed pool completes the
// session immediately"). Motif backfill therefore progresses only on corpora
// with NEW activity: a corpus holding 250 unmotifed facts and taking no new
// writes will never backfill one of them, because its own backlog does not
// make a session non-empty.
//
// Rewinding simulates a corpus that keeps receiving writes, which is the
// condition backfill was designed for. It does NOT paper over the coupling —
// that is reported as a finding in its own right, because a reader of the
// annex needs to know coverage grows with ACTIVITY and not with backlog.
func rewindWatermark(ctx context.Context, corpus, scratch string) error {
	svc, _, branch, closeAll, err := open(ctx, corpus, scratch)
	if err != nil {
		return err
	}
	defer closeAll()
	return svc.Pipeline().SetPipelineWatermark(ctx, "review", branch, "")
}

// ── merge ─────────────────────────────────────────────────────────────────
//
// Builds the SEEDED merged corpus: agentic-engineering (carrying whatever
// motifs its own run assigned) as the base, with core's facts written in on
// top. This is the centrepiece the gate turns on — whether a shared motif
// forms across two genuinely different subject domains, and whether the pairs
// it produces are carries a working agent would want.
//
// Two rules, both from the ruling:
//
//   - The base is a COPY. Neither live home is ever opened.
//   - Which corpus a fact came from is recorded in a SIDECAR map, never in a
//     fact field. A source tag inside the fact would be a subject word the
//     strip could see and, worse, a term motifs could form around — the
//     merged corpus would then measure its own construction.
//
// Facts are written with ONE BatchWriteFacts call, so core's mutual internal
// refs resolve against each other (the write path checks a batch as a unit).
// What cannot resolve is dropped, transitively, and counted: those are refs to
// facts that were NEVER written in core — a past agent citing work it never
// did. The count is reported rather than buried, because it is the merged
// corpus's one construction-time distortion.
// mergeSampleSize/Seed: a proportional, reproducible sample of the merged-in
// corpus. Size chosen so the sweep reaches the dominant subject region within a
// bounded run; the seed so two runs measure the same corpus.
const (
	mergeSampleSize = 500
	mergeSampleSeed = 20260823
)

func merge(ctx context.Context, scratch string) error {
	base := copyPath(scratch, "agentic-engineering")
	dst := copyPath(scratch, "merged")
	if err := copyFile(base, dst); err != nil {
		return fmt.Errorf("seed merged from agentic-engineering: %w", err)
	}
	if err := copyFile(base, pristinePath(scratch, "merged")); err != nil {
		return err
	}

	// Read core from its PRISTINE copy — the one untouched by any session.
	srcSvc, _, srcBranch, srcClose, err := open(ctx, "core.pristine", scratch)
	if err != nil {
		return fmt.Errorf("open core: %w", err)
	}
	defer srcClose()

	coreFacts, err := srcSvc.FactQuery().Search(ctx, srcBranch, store.SearchOptions{Limit: 100_000})
	if err != nil {
		return err
	}

	dstSvc, _, dstBranch, dstClose, err := open(ctx, "merged", scratch)
	if err != nil {
		return err
	}
	defer dstClose()

	agFacts, err := dstSvc.FactQuery().Search(ctx, dstBranch, store.SearchOptions{Limit: 100_000})
	if err != nil {
		return err
	}
	present := map[string]bool{}
	for _, f := range agFacts {
		present[f.Path] = true
	}
	fmt.Printf("base: %d agentic-engineering facts\n", len(agFacts))

	// Read every core fact's FULL SOURCE. Not the indexed body — that is the
	// husk mistake backfill already paid for once.
	type srcFact struct {
		path    string
		content string
		refs    []string
	}
	all := make([]srcFact, 0, len(coreFacts))
	collided := 0
	for _, f := range coreFacts {
		if present[f.Path] {
			collided++
			continue
		}
		rec, err := srcSvc.Facts().ReadFact(ctx, srcBranch, f.Path, nil)
		if err != nil {
			continue
		}
		parsed, err := fact.ParseFact(f.Path, rec.Content)
		if err != nil {
			continue
		}
		var local []string
		for _, r := range parsed.Refs {
			// localRepoID "" so kb://<id>/ classifies FOREIGN — those point at
			// another corpus and are never checked locally. Only bare
			// repo-relative paths are the local gate's business.
			if c := fact.ClassifyRef(r, ""); c.Kind == fact.RefLocalFact {
				local = append(local, c.Path)
			}
		}
		all = append(all, srcFact{path: f.Path, content: rec.Content, refs: local})
	}

	// Drop a fact only if a ref of its own genuinely fails to resolve — asked
	// with knomit's TEMPORAL predicate, FactExistsAt, which walks back past
	// retractions to the last valid blob.
	//
	// The first version of this tool tested ref targets for membership in the
	// set of LIVE paths. That is the same live-index read that produced a
	// four-times-too-high damage figure for this corpus: "absent from the live
	// index" is also what a RETRACTED or SUPERSEDED fact looks like, and refs
	// resolve at the referrer's commit, not at HEAD. It dropped 99 facts where
	// the honest number is 21.
	resolves := map[string]bool{}
	dropped := map[string]bool{}
	for _, f := range all {
		for _, r := range f.refs {
			if _, seen := resolves[r]; !seen {
				ok, err := srcSvc.FactQuery().FactExistsAt(ctx, srcBranch, r, "")
				if err != nil {
					return fmt.Errorf("FactExistsAt %s: %w", r, err)
				}
				resolves[r] = ok
			}
			if !resolves[r] {
				dropped[f.path] = true
			}
		}
	}
	nResolve := 0
	for _, ok := range resolves {
		if ok {
			nResolve++
		}
	}
	fmt.Printf("refs: %d distinct local targets, %d resolve via FactExistsAt, %d do not\n",
		len(resolves), nResolve, len(resolves)-nResolve)
	fmt.Printf("facts dropped for a genuinely unresolvable ref: %d\n", len(dropped))

	// PROPORTIONAL fixed-seed sample. The sweep is oldest-fact-id first, which
	// in a freshly built corpus is path order, so an unsampled core would spend
	// ~38 sessions in its alphabetically-first regions before reaching
	// kb/technology — 80.7% of the corpus and the part that carries mechanisms.
	// A proportional sample preserves the composition while putting technology
	// within reach of a bounded run.
	//
	// Fixed seed: the sample must be the same on a rerun, or no two runs of
	// this annex measure the same corpus.
	eligible := make([]srcFact, 0, len(all))
	for _, f := range all {
		if !dropped[f.path] {
			eligible = append(eligible, f)
		}
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].path < eligible[j].path })
	rng := rand.New(rand.NewSource(mergeSampleSeed))
	perm := rng.Perm(len(eligible))
	keep := mergeSampleSize
	if keep > len(eligible) {
		keep = len(eligible)
	}
	sampled := make([]srcFact, 0, keep)
	for _, i := range perm[:keep] {
		sampled = append(sampled, eligible[i])
	}

	files := map[string]string{}
	for _, f := range sampled {
		files[f.path] = f.content
	}
	fmt.Printf("core: %d facts, %d path collisions skipped, %d dropped for unresolvable refs, %d eligible, %d SAMPLED and written (seed %d)\n",
		len(coreFacts), collided, len(dropped), len(eligible), len(files), mergeSampleSeed)

	if _, _, err := dstSvc.Facts().BatchWriteFacts(ctx, dstBranch, files, nil,
		"annex: merge core into agentic-engineering", "create"); err != nil {
		return fmt.Errorf("batch write: %w", err)
	}

	// The sidecar. Path -> source corpus, outside the facts entirely.
	sources := map[string]string{}
	for p := range present {
		sources[p] = "agentic-engineering"
	}
	for p := range files {
		sources[p] = "core"
	}
	blob, err := json.MarshalIndent(sources, "", " ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(scratch, "merged.sources.json"), blob, 0o644); err != nil {
		return err
	}
	fmt.Printf("sidecar: %d paths -> source corpus\n", len(sources))
	return report(ctx, "merged", scratch)
}

// prunebase drops the base corpus's UNMOTIFED facts from `merged`.
//
// Backfill sweeps oldest-fact-id first, deterministically, so a corpus that
// keeps its base's 208 motif-free facts spends ~26 sessions on them before it
// ever offers a fact from the corpus that was merged in. That order is right
// for a real corpus and fatal for this measurement: the merged run exists to
// ask whether a fact from one domain reuses a motif minted in another, and it
// cannot ask that until it reaches the second domain.
//
// So the seeded corpus is the base's MOTIF-BEARING facts — the vocabulary
// being seeded — plus everything merged in. A base fact with no motif carries
// no vocabulary; it can only consume a backfill slot. Dropping it costs the
// measurement nothing and buys it the question.
//
// Deleted through BatchWriteFacts, so history stays consistent and the dedup
// pass that walks it does not trip on a fact the index forgot.
func prunebase(ctx context.Context, scratch string) error {
	blob, err := os.ReadFile(filepath.Join(scratch, "merged.sources.json"))
	if err != nil {
		return fmt.Errorf("sidecar: %w (run `merge` first)", err)
	}
	var sources map[string]string
	if err := json.Unmarshal(blob, &sources); err != nil {
		return err
	}
	svc, _, branch, closeFn, err := open(ctx, "merged", scratch)
	if err != nil {
		return err
	}
	defer closeFn()

	facts, err := svc.FactQuery().Search(ctx, branch, store.SearchOptions{Limit: 100_000})
	if err != nil {
		return err
	}
	var drop []string
	kept, fromBase := 0, 0
	for _, f := range facts {
		if sources[f.Path] != "agentic-engineering" {
			continue
		}
		fromBase++
		if len(f.Motifs) == 0 {
			drop = append(drop, f.Path)
			continue
		}
		kept++
	}
	fmt.Printf("base facts %d: keeping %d motif-bearing, dropping %d motif-free\n", fromBase, kept, len(drop))
	if len(drop) == 0 {
		return report(ctx, "merged", scratch)
	}
	if _, _, err := svc.Facts().BatchWriteFacts(ctx, branch, nil, drop,
		"annex: drop motif-free base facts from the seeded corpus", "delete"); err != nil {
		return fmt.Errorf("batch delete: %w", err)
	}
	for _, p := range drop {
		delete(sources, p)
	}
	out, err := json.MarshalIndent(sources, "", " ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(scratch, "merged.sources.json"), out, 0o644); err != nil {
		return err
	}
	return report(ctx, "merged", scratch)
}
