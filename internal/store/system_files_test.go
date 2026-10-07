package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestSystemFileAt pins the reader explain, REST GET and the refs gate share:
// exact case, a file only (a directory, or a path through a file, is "not
// found"), an unknown commit is "not found", a non-hash commit and a
// non-system path are ErrInvalidPath, and content is read only under the cap.
func TestSystemFileAt(t *testing.T) {
	ctx := context.Background()
	svc, err := Open(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	files := map[string]string{
		".knomit/skills/x/SKILL.md": "skill body\n",
		".knomit/big.txt":           strings.Repeat("b", 100),
	}
	if err := svc.InitRepo(ctx, files, "agent/test"); err != nil {
		t.Fatal(err)
	}
	sf := svc.SystemFiles()

	got, ok, err := sf.SystemFileAt(ctx, "agent/test", "", ".knomit/skills/x/SKILL.md", 1<<10)
	if err != nil || !ok || string(got.Content) != "skill body\n" || len(got.Blob) != 40 || got.Size != 11 || len(got.Commit) != 40 {
		t.Fatalf("exact path: %+v ok=%v err=%v", got, ok, err)
	}
	head := got.Commit

	for _, p := range []string{".knomit/skills/x/skill.md", ".knomit/skills/x", ".knomit/skills/x/SKILL.md/y", ".knomit/none"} {
		if _, ok, err := sf.SystemFileAt(ctx, "agent/test", "", p, 0); ok || err != nil {
			t.Errorf("%s: want not found, got ok=%v err=%v", p, ok, err)
		}
	}
	if _, ok, err := sf.SystemFileAt(ctx, "agent/test", strings.Repeat("0", 40), ".knomit/big.txt", 0); ok || err != nil {
		t.Errorf("unknown commit: want not found, got ok=%v err=%v", ok, err)
	}
	for _, c := range []struct{ commit, path string }{{"abc123", ".knomit/big.txt"}, {head, "kb/.drafts/x.md"}, {head, ".knomit/a/../b"}} {
		if _, _, err := sf.SystemFileAt(ctx, "agent/test", c.commit, c.path, 0); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("%+v: want ErrInvalidPath, got %v", c, err)
		}
	}

	big, ok, err := sf.SystemFileAt(ctx, "agent/test", head, ".knomit/big.txt", 99)
	if err != nil || !ok || !big.Truncated || big.Content != nil || big.Size != 100 {
		t.Fatalf("over the cap: %+v ok=%v err=%v", big, ok, err)
	}
	stat, ok, err := sf.SystemFileAt(ctx, "agent/test", head, ".knomit/big.txt", 0)
	if err != nil || !ok || stat.Truncated || stat.Content != nil || stat.Blob != big.Blob {
		t.Fatalf("stat only: %+v ok=%v err=%v", stat, ok, err)
	}
}
