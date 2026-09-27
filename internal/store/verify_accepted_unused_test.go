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
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
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
