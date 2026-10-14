package fact

import (
	"strings"
	"testing"
)

// D18/P1b: {host} is the persisted host label VERBATIM. A hostname that ends
// in "-<8 hex>" is not mangled (the old dispatcher trimmed such a tail as if
// it were the fp8), and {fp8} no longer exists: it is an unknown placeholder.
func TestSubstitutePlaceholders_HostIsVerbatimAndFp8IsGone(t *testing.T) {
	id := TriggerIdentity{Agent: "opaque-1", Host: "ci-0badcafe"}
	got, err := substitutePlaceholders("claims/{host}/{agent}/*.md", id)
	if err != nil {
		t.Fatal(err)
	}
	if got != "claims/ci-0badcafe/opaque-1/*.md" {
		t.Fatalf("got %q", got)
	}
	if _, err := substitutePlaceholders("claims/{fp8}/*.md", id); err == nil || !strings.Contains(err.Error(), "unknown placeholder {fp8}") {
		t.Fatalf("{fp8} must be unknown now, err = %v", err)
	}
}
