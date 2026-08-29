package embeddings

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	tok "github.com/daulet/tokenizers"
	"github.com/rs/zerolog/log"
	ort "github.com/yalue/onnxruntime_go"

	"knomit/internal/embeddings/params"
)

// ortOnce ensures InitializeEnvironment is called only once per process.
var ortOnce sync.Once
var ortInitErr error

// mustExePath returns the directory containing the running executable.
func mustExePath() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return filepath.Dir(exe)
	}
	return filepath.Dir(resolved)
}

// libCandidates returns ORT shared library paths to try, in priority order.
func libCandidates(exeDir string) []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			filepath.Join(exeDir, "lib", "libonnxruntime.dylib"),
			"/opt/homebrew/lib/libonnxruntime.dylib",
		}
	case "linux":
		return []string{
			filepath.Join(exeDir, "lib", "libonnxruntime.so"),
			"/usr/local/lib/libonnxruntime.so",
			"/usr/lib/libonnxruntime.so",
		}
	case "windows":
		return []string{
			filepath.Join(exeDir, "lib", "onnxruntime.dll"),
		}
	default:
		return nil
	}
}

func initORT() error {
	ortOnce.Do(func() {
		if p := os.Getenv("ORT_LIB_PATH"); p != "" {
			ort.SetSharedLibraryPath(p)
		} else {
			exeDir := mustExePath()
			for _, c := range libCandidates(exeDir) {
				if _, err := os.Stat(c); err == nil {
					ort.SetSharedLibraryPath(c)
					break
				}
			}
		}
		ortInitErr = ort.InitializeEnvironment()
	})
	return ortInitErr
}

// Embedder runs one model's ONNX graph in-process and returns vectors.
type Embedder struct {
	model Model
	sess  *ort.DynamicAdvancedSession
	tk    *tok.Tokenizer
	// maxBatchTokens is the padded-token budget for one session.Run. Always
	// positive for an Embedder built by NewEmbedder; see DefaultMaxBatchTokens.
	maxBatchTokens int
	// batchSem caps simultaneous BATCH inferences. Nil means unbounded.
	// Single-shot paths deliberately bypass it.
	batchSem batchSem
	// firstRunRSS logs process RSS immediately before the first inference —
	// the only moment non-embedding memory is measurable, since the ONNX arena
	// retains its high-water mark and idle RSS afterwards equals peak RSS.
	firstRunRSS sync.Once
}

// Option customizes an Embedder at construction.
type Option func(*Embedder)

// WithMaxBatchTokens sets the padded-token budget for one session.Run.
//
// The caller is responsible for passing a positive value; ResolveBudget
// produces one from the config value and the detected memory ceiling.
//
// SUPERSEDES an earlier decision that the offline tools should skip derivation
// to stay deterministic across machines. They no longer do: a memory ceiling is
// a property of the MACHINE, not of the entry point, and a tool running
// 16384-token batches on a 2 GiB host is the exact shape the derivation exists
// to clamp. Batch-shape numerics sit inside the cosine >= 0.9999 envelope that
// TestEmbedDocumentsBatchMatchesSingle already accepts, so calibration is not
// meaningfully machine-dependent through this path. The tools still skip the
// SERIALIZATION gate, which is a separate option — they run one batch at a time
// and never contend, so it would cost them nothing and buy nothing.
func WithMaxBatchTokens(n int) Option {
	return func(e *Embedder) { e.maxBatchTokens = n }
}

// WithBatchConcurrency caps how many BATCH inferences may run at once; 0 leaves
// them unbounded. One shared Embedder plus per-branch locking otherwise lets
// concurrent runs overlap and their peaks ADD — and because the ONNX arena
// retains its high-water mark, an overshoot is not a spike but a permanent
// floor on resident memory.
//
// This is NOT a per-process memory bound. Single-row inference bypasses the gate
// (see below), so it is unbounded in count and sits outside the guarantee.
//
// Unbounded by default, and that default is deliberate rather than lazy.
//
// On cost, stated as what was actually measured. An earlier version of this
// comment attributed the numbers below to batch WIDTH; that was wrong — the
// benchmark hardcoded its budget, so both runs used identical 4x2048 batches
// and only the WORKER COUNT differed. Corrected reading, 8-core host:
//
//	4 concurrent workers vs fully serialized:  54.66s -> 75.04s  (37%)
//	2 concurrent workers vs fully serialized:  60.71s -> 61.78s  ( 1.8%)
//
// Note the 1.8% above did NOT reproduce: the same configuration on a single
// clean build measured 1.36, so treat that figure as history rather than data.
//
// Batch WIDTH does matter, and it was finally measured properly by holding
// workers and total work fixed while varying only the shape (results-4, one
// build, the harness printing rows/budget/batches):
//
//	2 batches of 4x2048 ( 910 MiB)  57.71s concurrent vs 78.54s serialized
//	1 batch  of 8x2048 (1820 MiB)  65.42s concurrent vs 65.25s serialized
//
// A wide batch already saturates the cores through ORT intra-op parallelism, so
// bounding concurrency costs nothing there; a narrow one leaves headroom that
// concurrency exploits. This is the same claim an earlier version of this
// comment made and had withdrawn as unmeasured — the earlier evidence was a
// hardcoded budget that produced identical shapes in both arms. It is restated
// here because THIS pair measured it, not because the old wording was right.
//
// The app enables it only when the derived budget shows the machine actually
// constrained us — which is also the narrow-batch, expensive end of that range,
// so this is a deliberate trade rather than a free guarantee. The offline tools
// run one batch at a time and never contend, so the gate would cost them
// nothing and buy nothing.
//
// Single-row inference (EmbedQuery, EmbedDocument) bypasses the gate whether or
// not it is enabled, so interactive search never queues behind a rebuild batch.
func WithBatchConcurrency(n int) Option {
	return func(e *Embedder) {
		if n > 0 {
			e.batchSem = newBatchSem(n)
			return
		}
		e.batchSem = nil
	}
}

// NewEmbedder loads the model+tokenizer for descriptor m from <cacheDir>/<id>/,
// downloading them first if missing. ctx bounds those downloads — cancelling it
// aborts a fetch in progress, which is what keeps a dead mirror from hanging
// server boot (embeddings are mandatory, so this runs before the server listens).
func NewEmbedder(ctx context.Context, m Model, cacheDir string, opts ...Option) (*Embedder, error) {
	if err := initORT(); err != nil {
		return nil, fmt.Errorf("onnxruntime init: %w", err)
	}
	modelPath, tokPath, err := EnsureModel(ctx, m, cacheDir)
	if err != nil {
		return nil, err
	}
	tk, err := tok.FromFile(tokPath)
	if err != nil {
		return nil, fmt.Errorf("load tokenizer: %w", err)
	}
	sess, err := ort.NewDynamicAdvancedSession(modelPath, m.ONNXInputs, m.ONNXOutputs, nil)
	if err != nil {
		_ = tk.Close()
		return nil, fmt.Errorf("create onnx session: %w", err)
	}
	e := &Embedder{model: m, sess: sess, tk: tk, maxBatchTokens: DefaultMaxBatchTokens}
	for _, opt := range opts {
		opt(e)
	}
	// Hold the "always positive" field invariant by construction rather than by
	// convention at the call site. A non-positive budget reaching the packer
	// degrades to one row per inference for the process lifetime — a silent
	// ~30x slowdown, not a failure — and the ways to get one are all plausible:
	// a tool flag, or a future memory resolver returning 0 on an unreadable
	// /sys. Clamp here so no caller has to remember.
	if e.maxBatchTokens <= 0 {
		e.maxBatchTokens = DefaultMaxBatchTokens
	}
	return e, nil
}

func (e *Embedder) Dim() int   { return e.model.Dim }
func (e *Embedder) ID() string { return e.model.ID }
func (e *Embedder) Close()     { _ = e.sess.Destroy(); _ = e.tk.Close() }

// Thresholds returns this model's calibrated cosine cutoffs.
func (e *Embedder) Thresholds() params.Thresholds { return e.model.Thresholds }

// EmbedQuery embeds a search query using the model's query template. ctx is a
// pre-flight checkpoint only: sess.Run cannot be interrupted, so cancelling
// after this check does not stop the inference (see store.Embedder's doc).
func (e *Embedder) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vecs, err := e.embedBatch([]string{fillTemplate(e.model.QueryTemplate, "", text)})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

// EmbedDocument embeds a fact body (+ title) using the model's doc template.
// ctx is a pre-flight checkpoint only, as in EmbedQuery.
func (e *Embedder) EmbedDocument(ctx context.Context, title, body string) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vecs, err := e.embedBatch([]string{e.docText(title, body)})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

// EmbedDocuments embeds (title, body) pairs, batching the ONNX inference so a
// full-corpus re-embed issues one session.Run per maxBatchTokens of padded
// input rather than one per doc. ctx is checked before each batch, so
// cancelling a re-embed costs at most one in-flight batch rather than the
// remainder of the corpus.
func (e *Embedder) EmbedDocuments(ctx context.Context, titles, bodies []string) ([][]float32, error) {
	if len(titles) != len(bodies) {
		return nil, fmt.Errorf("EmbedDocuments: %d titles vs %d bodies", len(titles), len(bodies))
	}
	// Checked before tokenizing, not just inside embedInBatches: encodeAll is
	// evaluated as an argument expression, so without this a cancelled caller
	// would pay a full tokenization pass (and any truncation warnings) before
	// the error came back.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	texts := make([]string, len(bodies))
	for i := range bodies {
		texts[i] = e.docText(titles[i], bodies[i])
	}
	return embedInBatches(ctx, e.encodeAll(texts), e.maxBatchTokens, e.batchSem, e.runRows)
}

// EmbedShortStrings embeds bare short strings — fact titles, and from Phase 2
// of the motif work, motif names and name+definition strings — through the
// model's ShortStringTemplate. Never the query or document template: see the
// descriptor's comment for the measurement that settled this.
//
// ctx is checked before each batch, exactly as in EmbedDocuments.
func (e *Embedder) EmbedShortStrings(ctx context.Context, texts []string) ([][]float32, error) {
	// Checked before tokenizing, for the reason given in EmbedDocuments.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rendered := make([]string, len(texts))
	for i, t := range texts {
		rendered[i] = e.shortStringText(t)
	}
	return embedInBatches(ctx, e.encodeAll(rendered), e.maxBatchTokens, e.batchSem, e.runRows)
}

// shortStringText renders one short string through the model's descriptor. The
// string fills the {content} slot; {title} is empty, which for the title-hack
// template is not reached at all.
func (e *Embedder) shortStringText(s string) string {
	return fillTemplate(e.model.ShortStringTemplate, "", s)
}

// docText renders a document for embedding. When the model's DocTemplate has a
// {title} slot the title fills it; otherwise the model has no separate notion of
// title, so we fall back to the historical behavior of prepending the title to
// the body so its signal is not lost.
func (e *Embedder) docText(title, body string) string {
	if strings.Contains(e.model.DocTemplate, "{title}") {
		return fillTemplate(e.model.DocTemplate, title, body)
	}
	content := body
	if title != "" {
		content = title + " " + body
	}
	return fillTemplate(e.model.DocTemplate, "", content)
}

// encode tokenizes one text into int64 id/mask slices, truncating to the
// model's MaxTokens cap (with a warning) so an oversized input cannot exceed the
// graph's max position embeddings.
func (e *Embedder) encode(text string) (ids, mask []int64) {
	enc := e.tk.EncodeWithOptions(text, true, tok.WithReturnAttentionMask())
	n := len(enc.IDs)
	if max := e.model.MaxTokens; max > 0 && n > max {
		log.Warn().Str("model", e.model.ID).Int("tokens", n).Int("max_tokens", max).
			Msg("embeddings: input exceeds model max tokens; truncating (tail dropped)")
		n = max
	}
	ids = make([]int64, n)
	mask = make([]int64, n)
	for i := 0; i < n; i++ {
		ids[i] = int64(enc.IDs[i])
		mask[i] = int64(enc.AttentionMask[i])
	}
	return ids, mask
}

// embedBatch tokenizes texts and runs them as a single batch. The single-shot
// paths (EmbedQuery, EmbedDocument) use it directly; the batched paths encode
// once through encodeAll instead, so packing can read token lengths without
// tokenizing twice.
func (e *Embedder) embedBatch(texts []string) ([][]float32, error) {
	return e.runRows(e.encodeAll(texts))
}

// encodeAll tokenizes every text into a row of ids and mask, each truncated to
// the model's MaxTokens.
//
// This holds the whole input tokenized at once, which is inherent to
// length-aware packing: the packer cannot group by width without knowing every
// width first. The cost is bounded by the caller's request size, and the ids
// are the model input anyway — the alternative is tokenizing twice.
func (e *Embedder) encodeAll(texts []string) []encodedRow {
	rows := make([]encodedRow, len(texts))
	for i, text := range texts {
		ids, mask := e.encode(text)
		rows[i] = encodedRow{ids: ids, mask: mask}
	}
	return rows
}

// runRows pads rows to the longest in the batch, runs one ONNX inference, pools
// per the descriptor, and L2-normalizes each row.
func (e *Embedder) runRows(rows []encodedRow) ([][]float32, error) {
	e.firstRunRSS.Do(logRSSBeforeFirstInference)
	n := len(rows)
	if n == 0 {
		return nil, nil
	}
	maxLen := 0
	for _, r := range rows {
		if len(r.ids) > maxLen {
			maxLen = len(r.ids)
		}
	}
	if maxLen == 0 {
		maxLen = 1 // avoid a zero-length sequence dimension
	}

	// Flatten into [n, maxLen] row-major tensors; short rows are zero-padded
	// (pad token id 0 + attention_mask 0, so padding is ignored downstream).
	flatIDs := make([]int64, n*maxLen)
	flatMask := make([]int64, n*maxLen)
	for i, r := range rows {
		copy(flatIDs[i*maxLen:], r.ids)
		copy(flatMask[i*maxLen:], r.mask)
	}

	inputs, err := e.buildInputs(flatIDs, flatMask, int64(n), int64(maxLen))
	if err != nil {
		return nil, err
	}
	defer destroyAll(inputs)

	outs := []ort.Value{nil}
	start := time.Now()
	err = e.sess.Run(inputs, outs)
	observeEmbedInference(start)
	if err != nil {
		return nil, fmt.Errorf("onnx run: %w", err)
	}
	defer outs[0].Destroy() //nolint:errcheck

	t, ok := outs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("unexpected output type from model %q", e.model.ID)
	}
	data := t.GetData()
	shape := t.GetShape()

	out := make([][]float32, n)
	if e.model.Pooling == PoolNone {
		// Graph emits one pre-pooled [n, Dim] vector per row.
		dim := e.model.Dim
		if len(data) < n*dim {
			return nil, fmt.Errorf("model %q: output has %d floats, expected at least %d", e.model.ID, len(data), n*dim)
		}
		for i := range n {
			vec := make([]float32, dim)
			copy(vec, data[i*dim:(i+1)*dim])
			l2normalize(vec)
			out[i] = vec
		}
		return out, nil
	}

	// Token-level output [n, maxLen, dim]: pool each row over its own tokens.
	if len(shape) < 3 {
		return nil, fmt.Errorf("model %q: expected rank-3 token output for pooling, got shape %v", e.model.ID, shape)
	}
	dim := int(shape[len(shape)-1])
	rowLen := maxLen * dim
	if len(data) < n*rowLen {
		return nil, fmt.Errorf("model %q: output has %d floats, expected %d (%d rows × %d)", e.model.ID, len(data), n*rowLen, n, rowLen)
	}
	for i := range n {
		rowData := data[i*rowLen : (i+1)*rowLen]
		maskRow := flatMask[i*maxLen : (i+1)*maxLen]
		var vec []float32
		switch e.model.Pooling {
		case PoolMean:
			vec = poolMean(rowData, maskRow, maxLen, dim)
		case PoolCLS:
			vec = poolCLS(rowData, dim)
		case PoolLastToken:
			vec = poolLastToken(rowData, maskRow, maxLen, dim)
		default:
			return nil, fmt.Errorf("model %q: unsupported pooling %d", e.model.ID, e.model.Pooling)
		}
		l2normalize(vec)
		out[i] = vec
	}
	return out, nil
}

// buildInputs constructs the int64 tensors named by the descriptor. token_type_ids
// (if requested) are all-zero.
func (e *Embedder) buildInputs(ids, mask []int64, batch, seqLen int64) ([]ort.Value, error) {
	shape := ort.NewShape(batch, seqLen)
	vals := make([]ort.Value, 0, len(e.model.ONNXInputs))
	for _, name := range e.model.ONNXInputs {
		var data []int64
		switch name {
		case "input_ids":
			data = ids
		case "attention_mask":
			data = mask
		case "token_type_ids":
			data = make([]int64, len(ids)) // zeros
		default:
			destroyAll(vals)
			return nil, fmt.Errorf("unsupported ONNX input %q for model %q", name, e.model.ID)
		}
		tn, err := ort.NewTensor(shape, data)
		if err != nil {
			destroyAll(vals)
			return nil, err
		}
		vals = append(vals, tn)
	}
	return vals, nil
}

func destroyAll(vals []ort.Value) {
	for _, v := range vals {
		if v != nil {
			_ = v.Destroy()
		}
	}
}
