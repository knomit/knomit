package testenv

import (
	"context"
	"testing"

	"knomit/internal/store"
)

// AssertIntegrity runs Verify(Deep: true) against r's RepoInstance and fails
// the test if any Error-severity issues are found. Warnings are ignored
// (they don't affect IsClean).
func AssertIntegrity(t *testing.T, r *RepoHandle) {
	t.Helper()
	report, err := r.ri.Verify(context.Background(), store.VerifyOpts{Deep: true})
	if err != nil {
		t.Fatalf("verify on repo %q errored: %v", r.name, err)
	}
	if !report.IsClean() {
		t.Fatalf("integrity violations on repo %q:\n%s", r.name, report.String())
	}
}
