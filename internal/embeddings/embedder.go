package embeddings

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	tok "github.com/daulet/tokenizers"
	"github.com/rs/zerolog/log"
	ort "github.com/yalue/onnxruntime_go"

	"knomit/internal/retrieval"
)

// docBatchSize is how many documents share one ONNX session.Run in
// EmbedDocuments. Each run pads its texts to the longest in the chunk, so a
// modest batch keeps padding waste low while amortizing per-call overhead.
const docBatchSize = 32

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
}

// NewEmbedder loads the model+tokenizer for descriptor m from <cacheDir>/<id>/,
// downloading them first if missing.
func NewEmbedder(m Model, cacheDir string) (*Embedder, error) {
	if err := initORT(); err != nil {
		return nil, fmt.Errorf("onnxruntime init: %w", err)
	}
	modelPath, tokPath, err := EnsureModel(m, cacheDir)
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
	return &Embedder{model: m, sess: sess, tk: tk}, nil
}

func (e *Embedder) Dim() int   { return e.model.Dim }
func (e *Embedder) ID() string { return e.model.ID }
func (e *Embedder) Close()     { _ = e.sess.Destroy(); _ = e.tk.Close() }

// Thresholds returns this model's calibrated cosine cutoffs.
func (e *Embedder) Thresholds() retrieval.Thresholds { return e.model.Thresholds }

// EmbedQuery embeds a search query using the model's query template.
func (e *Embedder) EmbedQuery(text string) ([]float32, error) {
	vecs, err := e.embedBatch([]string{fillTemplate(e.model.QueryTemplate, "", text)})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

// EmbedDocument embeds a fact body (+ title) using the model's doc template.
func (e *Embedder) EmbedDocument(title, body string) ([]float32, error) {
	vecs, err := e.embedBatch([]string{e.docText(title, body)})
	if err != nil {
		return nil, err
	}
	return vecs[0], nil
}

// EmbedDocuments embeds (title, body) pairs, batching the ONNX inference so a
// full-corpus re-embed issues one session.Run per docBatchSize docs rather than
// one per doc.
func (e *Embedder) EmbedDocuments(titles, bodies []string) ([][]float32, error) {
	if len(titles) != len(bodies) {
		return nil, fmt.Errorf("EmbedDocuments: %d titles vs %d bodies", len(titles), len(bodies))
	}
	texts := make([]string, len(bodies))
	for i := range bodies {
		texts[i] = e.docText(titles[i], bodies[i])
	}
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += docBatchSize {
		end := min(i+docBatchSize, len(texts))
		vecs, err := e.embedBatch(texts[i:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
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

// embedBatch tokenizes texts, pads them to the longest in the batch, runs one
// ONNX inference, pools per the descriptor, and L2-normalizes each row.
func (e *Embedder) embedBatch(texts []string) ([][]float32, error) {
	n := len(texts)
	if n == 0 {
		return nil, nil
	}
	rowIDs := make([][]int64, n)
	rowMask := make([][]int64, n)
	maxLen := 0
	for i, text := range texts {
		rowIDs[i], rowMask[i] = e.encode(text)
		if len(rowIDs[i]) > maxLen {
			maxLen = len(rowIDs[i])
		}
	}
	if maxLen == 0 {
		maxLen = 1 // avoid a zero-length sequence dimension
	}

	// Flatten into [n, maxLen] row-major tensors; short rows are zero-padded
	// (pad token id 0 + attention_mask 0, so padding is ignored downstream).
	flatIDs := make([]int64, n*maxLen)
	flatMask := make([]int64, n*maxLen)
	for i := range n {
		copy(flatIDs[i*maxLen:], rowIDs[i])
		copy(flatMask[i*maxLen:], rowMask[i])
	}

	inputs, err := e.buildInputs(flatIDs, flatMask, int64(n), int64(maxLen))
	if err != nil {
		return nil, err
	}
	defer destroyAll(inputs)

	outs := []ort.Value{nil}
	if err := e.sess.Run(inputs, outs); err != nil {
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
