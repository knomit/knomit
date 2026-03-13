package embeddings

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"

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

// Embedder tokenizes text, runs ONNX inference using nomic-embed-text-v1.5,
// mean-pools over sequence length, and L2-normalises to unit vector.
type Embedder struct {
	session *ort.DynamicAdvancedSession
	tok     *Tokenizer
}

// NewEmbedder creates a new ONNX-based sentence embedder.
// The ONNX Runtime shared library is located automatically.
// Set ORT_LIB_PATH to override the library path
// (e.g. ORT_LIB_PATH=/usr/local/lib/libonnxruntime.so).
func NewEmbedder(modelPath, tokenizerPath string) (*Embedder, error) {
	if err := initORT(); err != nil {
		return nil, fmt.Errorf("onnxruntime init: %w", err)
	}

	tok, err := LoadTokenizer(tokenizerPath)
	if err != nil {
		return nil, fmt.Errorf("load tokenizer: %w", err)
	}

	inputNames := []string{"input_ids", "attention_mask", "token_type_ids"}
	outputNames := []string{"last_hidden_state"}

	session, err := ort.NewDynamicAdvancedSession(modelPath, inputNames, outputNames, nil)
	if err != nil {
		return nil, fmt.Errorf("create onnx session: %w", err)
	}

	return &Embedder{session: session, tok: tok}, nil
}

// Embed tokenizes text, runs inference, mean-pools last_hidden_state over
// seq_len, and returns an L2-normalised float32 vector. The embedding
// dimension is determined by the model (e.g. 384 for MiniLM, 768 for nomic).
func (e *Embedder) Embed(text string) ([]float32, error) {
	inputIDs, attentionMask, _ := e.tok.Encode(text)
	seqLen := int64(len(inputIDs))

	shape := ort.NewShape(1, seqLen)

	// Convert int32 slices to int64 for the model.
	// token_type_ids are all zeros (single-segment input).
	ids64 := make([]int64, seqLen)
	mask64 := make([]int64, seqLen)
	types64 := make([]int64, seqLen) // all zeros
	for i := int64(0); i < seqLen; i++ {
		ids64[i] = int64(inputIDs[i])
		mask64[i] = int64(attentionMask[i])
	}

	idsTensor, err := ort.NewTensor(shape, ids64)
	if err != nil {
		return nil, fmt.Errorf("new input_ids tensor: %w", err)
	}
	defer idsTensor.Destroy()

	maskTensor, err := ort.NewTensor(shape, mask64)
	if err != nil {
		return nil, fmt.Errorf("new attention_mask tensor: %w", err)
	}
	defer maskTensor.Destroy()

	typesTensor, err := ort.NewTensor(shape, types64)
	if err != nil {
		return nil, fmt.Errorf("new token_type_ids tensor: %w", err)
	}
	defer typesTensor.Destroy()

	// Let ONNX Runtime allocate the output so the embedding dimension is
	// determined by the model rather than hardcoded.
	outputs := []ort.Value{nil}
	if err := e.session.Run(
		[]ort.Value{idsTensor, maskTensor, typesTensor},
		outputs,
	); err != nil {
		return nil, fmt.Errorf("onnx run: %w", err)
	}
	defer outputs[0].Destroy()

	// Output shape is [1, seqLen, dims].
	outShape := outputs[0].GetShape()
	dims := int(outShape[2])
	outputTensor, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("unexpected output tensor type")
	}

	// Mean-pool over seq_len dimension.
	data := outputTensor.GetData() // len = seqLen * dims
	pooled := make([]float32, dims)
	for tok := int64(0); tok < seqLen; tok++ {
		for d := 0; d < dims; d++ {
			pooled[d] += data[tok*int64(dims)+int64(d)]
		}
	}
	invN := float32(1.0 / float64(seqLen))
	for d := range pooled {
		pooled[d] *= invN
	}

	// L2 normalise.
	var norm float64
	for _, v := range pooled {
		norm += float64(v) * float64(v)
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for d := range pooled {
			pooled[d] = float32(float64(pooled[d]) / norm)
		}
	}

	return pooled, nil
}

// Close destroys the ONNX session. The global ort environment is shared and
// managed separately; callers that need to fully clean up the environment
// should call ort.DestroyEnvironment() themselves.
func (e *Embedder) Close() {
	e.session.Destroy()
}
