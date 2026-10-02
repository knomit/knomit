package archtest

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The old name, built so this file does not contain it.
var retiredServerVar = "KNOMIT_" + "BASE_URL"

// KNOMIT_SERVER replaced the old variable everywhere, with no alias (user
// decision 2026-10-02: "No compatibility, nothing shipped yet. Rip and
// replace."). A leftover read would be a second, silent way to name the
// server — exactly the ambiguity the rename removes — and a leftover comment
// or doc would tell the next reader to set a variable nothing reads.
//
// Walks the module's tracked-content directories; .claude/ (plans and
// history), .git, node_modules and build output are skipped.
func TestNoRetiredServerVariable(t *testing.T) {
	root := moduleRoot(t)
	skip := map[string]bool{".git": true, ".claude": true, "node_modules": true, "dist": true, "build": true}
	var hits []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && skip[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 4<<20 {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		if strings.Contains(string(b), retiredServerVar) {
			rel, _ := filepath.Rel(root, p)
			hits = append(hits, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Fatalf("%s is gone (use KNOMIT_SERVER), yet these files still name it:\n  %s",
			retiredServerVar, strings.Join(hits, "\n  "))
	}
}

// Both server entry points hand the repos Manager their own address once
// their listeners are bound, or a recipe's `exec` child gets no
// KNOMIT_SERVER on that kind of install and its `kb` falls back to the
// lockfile — the bug KNOMIT_SERVER exists to remove. Which address is chosen
// is pinned by tests beside each (cmd/server_address_unix_test.go,
// tools/desktop/boot_*_test.go); this pins that each entry point passes it on.
func TestBothServerEntryPointsRecordTheirAddress(t *testing.T) {
	root := moduleRoot(t)
	for _, f := range []string{"cmd/serve.go", "tools/desktop/app.go"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), ".Manager().SetServerAddress(") {
			t.Errorf("%s does not hand its address to the repos Manager (Manager().SetServerAddress)", f)
		}
	}
}
