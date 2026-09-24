package skills

import (
	"io/fs"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// fullSrcRef matches a src:// ref in the full form a skill template shows as
// an EXAMPLE of what to write. Placeholder forms (<repo-id>, <commit>) do not
// match and are not checked.
var fullSrcRef = regexp.MustCompile(`src://([0-9a-f]{12})/([^@\s` + "`" + `'"]+)@([0-9a-f]{40}):([0-9a-f]{40})`)

// knomit#249 review. The example ref in knomit-remember and knomit-decided
// cited internal/store/service.go with the blob of internal/store/git/storer.go,
// shipped in the very skills that tell agents to compute refs with git rather
// than copy them. An example is copied more than any other ref in the corpus,
// so every full-form src ref in a template must name the blob that
// `git rev-parse <commit>:<path>` gives in THIS repo.
//
// A ref citing another repo, or a commit this clone does not have (a shallow
// CI checkout), is skipped rather than failed: the check can only speak for
// objects it can see.
func TestTemplates_SrcRefExamplesNameTheRealBlob(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	root, err := exec.Command("git", "rev-list", "--max-parents=0", "HEAD").Output()
	if err != nil {
		t.Skipf("not in a git checkout: %v", err)
	}
	roots := strings.Fields(string(root))

	checked := 0
	err = fs.WalkDir(FS, Root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return werr
		}
		body, rerr := fs.ReadFile(FS, p)
		if rerr != nil {
			return rerr
		}
		for _, m := range fullSrcRef.FindAllStringSubmatch(string(body), -1) {
			ref, repo, path, commit, blob := m[0], m[1], m[2], m[3], m[4]
			if !ownRepo(repo, roots) {
				continue
			}
			if exec.Command("git", "cat-file", "-e", commit+"^{commit}").Run() != nil {
				t.Logf("%s: commit %s not in this clone, skipped", p, commit[:12])
				continue
			}
			out, gerr := exec.Command("git", "rev-parse", commit+":"+path).Output()
			if gerr != nil {
				t.Errorf("%s: %s cites %s, which does not exist at %s", p, ref, path, commit[:12])
				continue
			}
			if got := strings.TrimSpace(string(out)); got != blob {
				t.Errorf("%s: %s names blob %s, but git rev-parse %s:%s = %s",
					p, ref, blob[:12], commit[:12], path, got)
			}
			checked++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("checked %d example ref(s)", checked)
}

func ownRepo(id string, roots []string) bool {
	for _, r := range roots {
		if strings.HasPrefix(r, id) {
			return true
		}
	}
	return false
}
