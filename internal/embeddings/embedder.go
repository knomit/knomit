package embeddings

import (
	"fmt"
	"math"
	"os"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// ortOnce ensures InitializeEnvironment is called only once per process.
var ortOnce sync.Once
var ortInitErr error

// candidateLibraryPaths lists paths to try for the onnxruntime shared library
// when ORT_LIB_PATH is not set. Paths are relative-friendly or use well-known
// system locations; developer-specific absolute paths must not appear here.
var candidateLibraryPaths = []string{
	// Bundled alongside the knomit binary (macOS arm64).
	"lib/libonnxruntime.dylib",
	// Common Homebrew location (macOS).
	"/opt/homebrew/lib/libonnxruntime.dylib",
	// Common system location (Linux).
	"/usr/local/lib/libonnxruntime.so",
	"/usr/lib/libonnxruntime.so",
}

func initORT() error {
	ortOnce.Do(func() {
		// ORT_LIB_PATH env var takes priority.
		if p := os.Getenv("ORT_LIB_PATH"); p != "" {
			ort.SetSharedLibraryPath(p)
		} else {
			for _, candidate := range candidateLibraryPaths {
				if _, err := os.Stat(candidate); err == nil {
					ort.SetSharedLibraryPath(candidate)
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

// Embed tokenizes text, runs inference using nomic-embed-text-v1.5, mean-pools
// last_hidden_state over seq_len, and returns an L2-normalised float32 vector
// of dimension 768.
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

	// Output shape: [1, seqLen, 768] — pre-allocate.
	const dims = 768
	outputShape := ort.NewShape(1, seqLen, dims)
	outputTensor, err := ort.NewEmptyTensor[float32](outputShape)
	if err != nil {
		return nil, fmt.Errorf("new output tensor: %w", err)
	}
	defer outputTensor.Destroy()

	if err := e.session.Run(
		[]ort.Value{idsTensor, maskTensor, typesTensor},
		[]ort.Value{outputTensor},
	); err != nil {
		return nil, fmt.Errorf("onnx run: %w", err)
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
