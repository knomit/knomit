package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestRepoVerifyAcceptedTableIsNeverUsed: user ruling for PR 4. The accept
// list lives in control.db (per instance), injected as an AcceptList; the
// repo-database table verify_accepted (repo migration 000027) stays empty. No
// non-test source in the store may read or write it.
func TestRepoVerifyAcceptedTableIsNeverUsed(t *testing.T) {
	sqlUse := regexp.MustCompile(`(?i)(INTO|FROM|UPDATE|TABLE)\s+verify_accepted\b`)
	// The store's children (internal/sqlite, internal/sessions, internal/vcs,
	// where the accept list's type lives, and the siblings of git and migrate
	// such as fleetrec) are the store too. WALKED, not globbed: a glob at a
	// fixed depth silently drops every new child that sits elsewhere.
	var files []string
	dirs := 0
	err := filepath.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs++
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if dirs < 5 || len(files) < 100 {
		t.Fatalf("scanned %d directories and %d non-test Go files: the scan no longer reads the store and its children", dirs, len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue // prose may name the table
			}
			if m := sqlUse.FindString(line); m != "" {
				t.Errorf("%s:%d uses the repo-database verify_accepted table (%q): the accept list is control.db's", f, i+1, m)
			}
		}
	}
}
