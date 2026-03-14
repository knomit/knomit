package synthesize

import "testing"

func TestResolveProfile(t *testing.T) {
	tests := []struct {
		model          string
		wantName       string
		wantJSON       bool
		wantRetry      bool
		wantChunkBytes int
	}{
		{"qwen3:8b", "small", false, true, 50_000},
		{"qwen2.5:7b", "small", false, true, 50_000},
		{"llama3:3b", "small", false, true, 50_000},
		{"mistral:7b-instruct", "small", false, true, 50_000},
		{"claude-sonnet-4-20250514", "large", true, false, 100_000},
		{"gpt-4o", "large", true, false, 100_000},
		{"qwen3:32b", "large", true, false, 100_000},
		{"qwen3:72b", "large", true, false, 100_000},
		{"", "large", true, false, 100_000},
		{"qwen3", "large", true, false, 100_000},
		{"custom-model", "large", true, false, 100_000},
	}

	for _, tc := range tests {
		t.Run(tc.model, func(t *testing.T) {
			p := ResolveProfile(tc.model)
			if p.Name != tc.wantName {
				t.Errorf("Name: got %q, want %q", p.Name, tc.wantName)
			}
			if p.ForceJSON != tc.wantJSON {
				t.Errorf("ForceJSON: got %v, want %v", p.ForceJSON, tc.wantJSON)
			}
			if p.RetryOnPassive != tc.wantRetry {
				t.Errorf("RetryOnPassive: got %v, want %v", p.RetryOnPassive, tc.wantRetry)
			}
			if p.MaxChunkBytes != tc.wantChunkBytes {
				t.Errorf("MaxChunkBytes: got %d, want %d", p.MaxChunkBytes, tc.wantChunkBytes)
			}
		})
	}
}
