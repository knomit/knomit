package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The rule under test: this tool may OPEN a corpus only at a lab path. The
// user's real knowledge bases live under ~/.knomit/repos, and the annex writes
// motifs onto facts — so opening one in place would both mutate a real corpus
// and migrate its schema. `snapshot` deliberately touches a live home once, to
// checkpoint its WAL before a byte copy; that is a read and a flush, not an
// open-as-a-corpus, and it is out of this guard's scope on purpose.

func TestRefuseLivePath_RejectsInsideTheLiveReposRoot(t *testing.T) {
	home := t.TempDir()
	live := liveReposRoot(home)
	require.NoError(t, os.MkdirAll(live, 0o755))

	err := refuseLivePath(filepath.Join(live, "core.db"), home)
	require.Error(t, err)
	require.Contains(t, err.Error(), "live", "the error must name the rule it enforces")
}

func TestRefuseLivePath_RejectsTheRootItself(t *testing.T) {
	home := t.TempDir()
	live := liveReposRoot(home)
	require.NoError(t, os.MkdirAll(live, 0o755))

	require.Error(t, refuseLivePath(live, home))
}

func TestRefuseLivePath_AllowsASiblingLabDirectory(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(liveReposRoot(home), 0o755))
	lab := filepath.Join(home, ".knomit", "motif-phase4-lab", "corpora")
	require.NoError(t, os.MkdirAll(lab, 0o755))

	require.NoError(t, refuseLivePath(filepath.Join(lab, "merged.db"), home))
}

// A sibling whose NAME merely starts with the root's name. A string-prefix
// check refuses this, and refusing it would be a false positive that pushes
// the next person to weaken the guard rather than fix it.
func TestRefuseLivePath_AllowsAPathThatOnlyPREFIXMatchesTheRoot(t *testing.T) {
	home := t.TempDir()
	require.NoError(t, os.MkdirAll(liveReposRoot(home), 0o755))
	sibling := filepath.Join(home, ".knomit", "repos-lab")
	require.NoError(t, os.MkdirAll(sibling, 0o755))

	require.NoError(t, refuseLivePath(filepath.Join(sibling, "core.db"), home))
}

// THE MEDIUM THE VIOLATION WOULD ACTUALLY APPEAR IN (lesson 3). A lab path
// that is a symlink INTO the live root passes any lexical check and opens a
// real corpus. The guard has to resolve before it decides.
func TestRefuseLivePath_ResolvesSymlinksBeforeDeciding(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	home := t.TempDir()
	live := liveReposRoot(home)
	require.NoError(t, os.MkdirAll(live, 0o755))
	realDB := filepath.Join(live, "core.db")
	require.NoError(t, os.WriteFile(realDB, []byte("not really a db"), 0o644))

	lab := filepath.Join(home, ".knomit", "motif-phase4-lab")
	require.NoError(t, os.MkdirAll(lab, 0o755))
	link := filepath.Join(lab, "core.db")
	require.NoError(t, os.Symlink(realDB, link))

	// Precondition: the path is lexically outside the live root, so this test
	// fails for the right reason rather than by accident.
	require.False(t, strings.HasPrefix(link, live+string(filepath.Separator)),
		"precondition: the link path must be lexically outside the live root")

	require.Error(t, refuseLivePath(link, home),
		"a symlink into the live root must be refused")
}

// A lab path that does not exist yet must still be judged — the guard runs
// before the copy exists, and an unresolvable path is not a licence to proceed.
func TestRefuseLivePath_JudgesPathsThatDoNotExistYet(t *testing.T) {
	home := t.TempDir()
	live := liveReposRoot(home)
	require.NoError(t, os.MkdirAll(live, 0o755))

	require.Error(t, refuseLivePath(filepath.Join(live, "absent.db"), home),
		"a not-yet-created path inside the live root is still a live path")
	require.NoError(t, refuseLivePath(filepath.Join(home, "lab", "absent.db"), home),
		"a not-yet-created path outside it is still fine")
}
