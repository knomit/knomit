package embeddings

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	tok "github.com/daulet/tokenizers"
	ort "github.com/yalue/onnxruntime_go"
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
		return nil, fmt.Errorf("create onnx session: %w", err)
	}
	return &Embedder{model: m, sess: sess, tk: tk}, nil
}

func (e *Embedder) Dim() int   { return e.model.Dim }
func (e *Embedder) ID() string { return e.model.ID }
func (e *Embedder) Close()     { _ = e.sess.Destroy(); _ = e.tk.Close() }

// EmbedQuery embeds a search query using the model's query template.
func (e *Embedder) EmbedQuery(text string) ([]float32, error) {
	return e.embedRaw(fillTemplate(e.model.QueryTemplate, "", text))
}

// EmbedDocument embeds a fact body (+ title) using the model's doc template.
func (e *Embedder) EmbedDocument(title, body string) ([]float32, error) {
	return e.embedRaw(fillTemplate(e.model.DocTemplate, title, body))
}

// embedRaw tokenizes, runs inference, pools per the descriptor, and L2-normalizes.
func (e *Embedder) embedRaw(text string) ([]float32, error) {
	enc := e.tk.EncodeWithOptions(text, true, tok.WithReturnAttentionMask())
	seqLen := len(enc.IDs)
	ids := make([]int64, seqLen)
	mask := make([]int64, seqLen)
	for i := range enc.IDs {
		ids[i] = int64(enc.IDs[i])
		mask[i] = int64(enc.AttentionMask[i])
	}
	inputs, err := e.buildInputs(ids, mask, 1, int64(seqLen))
	if err != nil {
		return nil, err
	}
	defer destroyAll(inputs)

	outs := []ort.Value{nil}
	if err := e.sess.Run(inputs, outs); err != nil {
		return nil, fmt.Errorf("onnx run: %w", err)
	}
	defer outs[0].Destroy() //nolint:errcheck

	t := outs[0].(*ort.Tensor[float32])
	shape := t.GetShape()
	data := t.GetData()
	var vec []float32
	switch e.model.Pooling {
	case PoolNone:
		vec = make([]float32, e.model.Dim)
		copy(vec, data[:e.model.Dim])
	case PoolMean:
		vec = poolMean(data, mask, seqLen, int(shape[2]))
	case PoolCLS:
		vec = poolCLS(data, int(shape[2]))
	case PoolLastToken:
		vec = poolLastToken(data, mask, seqLen, int(shape[2]))
	}
	l2normalize(vec)
	return vec, nil
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
