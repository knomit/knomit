package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAssertNotOKFRef_PanicsOnOKF(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for okf/ branch")
		}
	}()
	assertNotOKFRef("okf/main")
}

func TestAssertNotOKFRef_PanicsOnEmpty(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for empty branch")
		}
	}()
	assertNotOKFRef("")
}

func TestAssertNotOKFRef_AllowsNormal(t *testing.T) {
	assertNotOKFRef("main")           // must not panic
	assertNotOKFRef("agent/host-123") // must not panic
}

func TestRefspecTouchesOKF(t *testing.T) {
	cases := map[string]bool{
		"+refs/heads/main:refs/heads/main":         false,
		"+refs/heads/agent/h:refs/heads/agent/h":   false,
		"+refs/heads/okf/main:refs/heads/okf/main": true,
		"refs/heads/okf/*:refs/heads/okf/*":        true,
	}
	for rs, want := range cases {
		if got := RefspecTouchesOKF(rs); got != want {
			t.Errorf("RefspecTouchesOKF(%q)=%v want %v", rs, got, want)
		}
	}
}

// TestNoUnguardedPushSites is a ref-safety net: it scans the internal/ tree's
// non-test .go files for occurrences of ".PushContext(" and asserts there is
// exactly one — the guarded call in remote_sync.go's remoteIndex.Push. If a
// future contributor adds a new push call site that bypasses
// remoteIndex.Push (and therefore bypasses assertNotOKFRef), this test fails,
// because okf/* refs must never reach a remote
// (kb/invariants/okf/refs-never-pushed).
//
// The test's working directory is the package directory internal/store, so
// "../../internal" locates the internal/ tree relative to that.
func TestNoUnguardedPushSites(t *testing.T) {
	root := "../../internal"
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("cannot locate internal/ tree at %q: %v", root, err)
	}

	const needle = ".PushContext("
	var (
		count int
		hits  []string
	)

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		n := strings.Count(string(content), needle)
		if n > 0 {
			count += n
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %q: %v", root, err)
	}

	if count != 1 {
		t.Fatalf("expected exactly 1 occurrence of %q in internal/ (non-test .go files), found %d in %v", needle, count, hits)
	}
}
