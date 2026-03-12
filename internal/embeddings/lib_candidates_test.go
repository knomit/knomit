package embeddings

import (
	"runtime"
	"testing"
)

func TestLibCandidates(t *testing.T) {
	candidates := libCandidates("/usr/local/bin")

	if len(candidates) == 0 {
		t.Fatal("expected at least one candidate path")
	}

	// First candidate should be exe-relative
	if candidates[0] != "/usr/local/bin/lib/libonnxruntime.dylib" &&
		candidates[0] != "/usr/local/bin/lib/libonnxruntime.so" &&
		candidates[0] != "/usr/local/bin/lib/onnxruntime.dll" {
		t.Fatalf("first candidate should be exe-relative, got %q", candidates[0])
	}

	switch runtime.GOOS {
	case "darwin":
		found := false
		for _, c := range candidates {
			if c == "/opt/homebrew/lib/libonnxruntime.dylib" {
				found = true
			}
		}
		if !found {
			t.Fatal("expected Homebrew fallback on darwin")
		}
	case "linux":
		found := false
		for _, c := range candidates {
			if c == "/usr/local/lib/libonnxruntime.so" {
				found = true
			}
		}
		if !found {
			t.Fatal("expected /usr/local/lib fallback on linux")
		}
	}
}
