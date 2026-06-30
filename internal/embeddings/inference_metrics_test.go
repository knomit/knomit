package embeddings

import (
	"testing"
	"time"
)

func TestObserveEmbedInference_RecordsHistogram(t *testing.T) {
	before := embedInferenceSeconds.Count()
	observeEmbedInference(time.Now().Add(-50 * time.Millisecond))
	if got := embedInferenceSeconds.Count() - before; got != 1 {
		t.Errorf("embed inference observations = %d, want 1", got)
	}
}
