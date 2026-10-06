package recipes

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"knomit/internal/fact"
)

// fakeTB records what dirAt does with a missing checkout: Fatal (wanted) or
// Skip (never). Fatal stops the call the way testing.T does, by unwinding.
type fakeTB struct {
	testing.TB
	fatal   string
	skipped bool
}

type stop struct{}

func (f *fakeTB) Helper() {}
func (f *fakeTB) Fatal(args ...any) {
	f.fatal = fmt.Sprint(args...)
	panic(stop{})
}
func (f *fakeTB) Fatalf(format string, args ...any) {
	f.fatal = fmt.Sprintf(format, args...)
	panic(stop{})
}
func (f *fakeTB) Skip(...any)          { f.skipped = true; panic(stop{}) }
func (f *fakeTB) Skipf(string, ...any) { f.skipped = true; panic(stop{}) }
func (f *fakeTB) SkipNow()             { f.skipped = true; panic(stop{}) }

func runDirAt(root string) (f *fakeTB, dir string) {
	f = &fakeTB{}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(stop); !ok {
				panic(r)
			}
		}
	}()
	return f, dirAt(f, root)
}

// TestDir_MissingOrEmptyFails: a missing checkout, and an empty one (what a
// clone without `--recurse-submodules` leaves), FAIL with the command that
// fixes it. They never skip.
//
// SABOTAGE: make dirAt call t.Skip instead of t.Fatal → red; drop FixHint
// from Check's message → red.
func TestDir_MissingOrEmptyFails(t *testing.T) {
	missing := t.TempDir()
	empty := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(empty, filepath.FromSlash(SubmodulePath)), 0o755))
	for name, root := range map[string]string{"missing": missing, "empty": empty} {
		t.Run(name, func(t *testing.T) {
			f, dir := runDirAt(root)
			require.False(t, f.skipped, "a missing checkout must never skip")
			require.Empty(t, dir)
			require.Contains(t, f.fatal, "git submodule update --init")
		})
	}

	ok := t.TempDir()
	onto := filepath.Join(ok, filepath.FromSlash(SubmodulePath), sentinel)
	require.NoError(t, os.MkdirAll(filepath.Dir(onto), 0o755))
	require.NoError(t, os.WriteFile(onto, []byte("id: x\n"), 0o644))
	f, dir := runDirAt(ok)
	require.Empty(t, f.fatal)
	require.Equal(t, filepath.Join(ok, filepath.FromSlash(SubmodulePath)), dir)
}

// TestDir_ThisRepo: the real checkout is present, and it is the submodule.
func TestDir_ThisRepo(t *testing.T) {
	dir := Dir(t)
	require.Equal(t, filepath.FromSlash(SubmodulePath), lastTwo(dir))
	_, err := os.Stat(filepath.Join(MissionTemplate(t), ManifestFile))
	require.NoError(t, err, "the mission template carries its manifest")
}

func lastTwo(p string) string {
	return filepath.Join(filepath.Base(filepath.Dir(p)), filepath.Base(p))
}

// TestRecipesKB_OntologyLoads: knomit-recipes' own ontology loads in knomit
// as a NEW ontology, with no diagnostics at all, and has F24's five topics.
//
// SABOTAGE: add an unknown key or a bad repository attribute to the
// checkout's .knomit/ontology.yaml → red.
func TestRecipesKB_OntologyLoads(t *testing.T) {
	raw, err := os.ReadFile(Path(t, ".knomit/ontology.yaml"))
	require.NoError(t, err)
	o, diags := fact.ValidateOntologyYAML(raw)
	require.Empty(t, diags)
	_, err = fact.ParseNewOntology(raw)
	require.NoError(t, err)
	require.Equal(t, []string{"gotchas", "howto", "patterns", "reference", "templates"}, o.TopicNames())

	m, err := os.ReadFile(filepath.Join(MissionTemplate(t), ".knomit", "ontology.yaml"))
	require.NoError(t, err)
	_, err = fact.ParseNewOntology(m)
	require.NoError(t, err, "the mission template's ontology loads as a new one")
}

// TestCIChecksOutSubmodules: every actions/checkout step in the workflows and
// local actions sets `submodules: true`, so every CI job (every OS, the
// desktop jobs included) has the knomit-recipes checkout the tests read.
//
// SABOTAGE: drop `submodules: true` from one checkout → red.
func TestCIChecksOutSubmodules(t *testing.T) {
	root, err := moduleRoot()
	require.NoError(t, err)
	var files []string
	for _, g := range []string{".github/workflows/*.yml", ".github/workflows/*.yaml", ".github/actions/*/action.yml"} {
		m, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(g)))
		require.NoError(t, err)
		files = append(files, m...)
	}
	checkouts := 0
	var missing []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		require.NoError(t, err)
		lines := strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n")
		for i, l := range lines {
			if !strings.Contains(l, "uses: actions/checkout@") {
				continue
			}
			checkouts++
			// The step's own lines: those indented deeper than its `- uses:`.
			ind := len(l) - len(strings.TrimLeft(l, " -"))
			ok := false
			for _, n := range lines[i+1:] {
				if strings.TrimSpace(n) == "" {
					continue
				}
				if len(n)-len(strings.TrimLeft(n, " ")) < ind || strings.HasPrefix(strings.TrimLeft(n, " "), "- ") {
					break
				}
				if strings.TrimSpace(n) == "submodules: true" {
					ok = true
				}
			}
			if !ok {
				rel, _ := filepath.Rel(root, f)
				missing = append(missing, fmt.Sprintf("%s:%d", filepath.ToSlash(rel), i+1))
			}
		}
	}
	require.Greater(t, checkouts, 10, "the scan found the workflows' checkouts")
	require.Empty(t, missing, "these checkouts do not set `submodules: true`")
}

// TestNoExamplesPaths: the mission template and the claude-session recipe
// moved to knomit-recipes. No tracked file outside third_party/ and .claude/
// names their old folders.
//
// SABOTAGE: write the old mission folder's path into any doc → red.
func TestNoExamplesPaths(t *testing.T) {
	root, err := moduleRoot()
	require.NoError(t, err)
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is needed to list tracked files: %v", err)
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	require.NoError(t, err)
	// Built from parts so this file does not name them itself.
	old := []string{"examples" + "/mission", "examples" + "/recipes"}
	var hits []string
	seen := 0
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" || strings.HasPrefix(p, "third_party/") || strings.HasPrefix(p, ".claude/") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			continue // a gitlink or a file deleted in the work tree
		}
		seen++
		for _, o := range old {
			if strings.Contains(string(b), o) {
				hits = append(hits, p+": "+o)
			}
		}
	}
	require.Greater(t, seen, 100, "the walk read the repository's files")
	sort.Strings(hits)
	require.Empty(t, hits, "these files still name the old template folders; point them at knomit-recipes")
}
