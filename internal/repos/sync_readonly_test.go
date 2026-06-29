package repos

import "testing"

func TestPushAllowed(t *testing.T) {
	if !pushAllowed(false) {
		t.Fatal("push must be allowed when not read-only")
	}
	if pushAllowed(true) {
		t.Fatal("push must be skipped in read-only mode")
	}
}
