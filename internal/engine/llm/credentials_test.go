package llm

import (
	"context"
	"errors"
	"testing"
)

// A Gemini adapter with no key in the environment fails with an error that IS
// ErrNoCredentials, through NewAdapter's wrapping, so internal/app can log it
// as the configuration it is rather than as a fault.
//
// SABOTAGE S3c (the Gemini error does not wrap the sentinel) → red.
func TestNewAdapter_MissingGeminiKeyIsErrNoCredentials(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "")
	t.Setenv("GOOGLE_AI_API_KEY", "")
	_, err := NewAdapter(context.Background(), "gemini", "gemini-2.5-flash")
	if !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("want ErrNoCredentials, got %v", err)
	}
}
