package embeddings

import (
	"fmt"
	"math"

	ort "github.com/yalue/onnxruntime_go"
)

// EmbedBatch tokenizes multiple texts, runs a single batched ONNX inference
// call, and returns one L2-normalised embedding vector per input text.
func (e *Embedder) EmbedBatch(texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) == 1 {
		v, err := e.Embed(texts[0])
		if err != nil {
			return nil, err
		}
		return [][]float32{v}, nil
	}

	batchSize := len(texts)

	// Tokenize all texts and find max sequence length.
	allIDs := make([][]int32, batchSize)
	allMasks := make([][]int32, batchSize)
	var maxSeqLen int
	for i, text := range texts {
		ids, mask, _ := e.tok.Encode(text)
		allIDs[i] = ids
		allMasks[i] = mask
		if len(ids) > maxSeqLen {
			maxSeqLen = len(ids)
		}
	}

	seqLen := int64(maxSeqLen)
	bs := int64(batchSize)
	shape := ort.NewShape(bs, seqLen)
	flatSize := bs * seqLen

	// Build flat int64 arrays padded to maxSeqLen.
	ids64 := make([]int64, flatSize)
	mask64 := make([]int64, flatSize)
	types64 := make([]int64, flatSize) // all zeros

	for i := 0; i < batchSize; i++ {
		offset := int64(i) * seqLen
		for j, id := range allIDs[i] {
			ids64[offset+int64(j)] = int64(id)
		}
		for j, m := range allMasks[i] {
			mask64[offset+int64(j)] = int64(m)
		}
		// Remaining positions stay 0 (padding); attention_mask 0 means ignore.
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

	outputs := []ort.Value{nil}
	if err := e.session.Run(
		[]ort.Value{idsTensor, maskTensor, typesTensor},
		outputs,
	); err != nil {
		return nil, fmt.Errorf("onnx run: %w", err)
	}
	defer outputs[0].Destroy()

	// Output shape is [batchSize, maxSeqLen, dims].
	outShape := outputs[0].GetShape()
	dims := int(outShape[2])
	outputTensor, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("unexpected output tensor type")
	}
	data := outputTensor.GetData() // len = batchSize * maxSeqLen * dims

	results := make([][]float32, batchSize)
	for i := 0; i < batchSize; i++ {
		actualLen := len(allIDs[i]) // actual (unpadded) sequence length
		pooled := make([]float32, dims)

		// Mean-pool over actual tokens only (not padding).
		rowOffset := i * maxSeqLen * dims
		for tok := 0; tok < actualLen; tok++ {
			tokOffset := rowOffset + tok*dims
			for d := 0; d < dims; d++ {
				pooled[d] += data[tokOffset+d]
			}
		}
		invN := float32(1.0 / float64(actualLen))
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

		results[i] = pooled
	}

	return results, nil
}
