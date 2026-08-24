package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// liveReposRoot is where the user's real knowledge bases live.
func liveReposRoot(home string) string {
	return filepath.Join(home, ".knomit", "repos")
}

// refuseLivePath rejects any corpus path that resolves inside the live repos
// root.
//
// WHY THIS EXISTS AT ALL. This tool opens a corpus and runs real review
// sessions against it — sessions that write motifs onto facts and that migrate
// the schema on open. Both are fine on a lab copy and neither is acceptable on
// a real knowledge base. The annex's rule ("copies only, never a live KB") was
// held by discipline; Phase 4 holds it with a check, because Phase 4 is the
// phase that reads numbers off these corpora and a number taken from a mutated
// corpus is worse than no number.
//
// SCOPE, deliberately narrow: this guards OPENING a corpus. `snapshot` still
// touches a live home exactly once, to flush its WAL before a byte copy — a
// read and a checkpoint, no session, no migration, and documented at its own
// call site. Widening this guard to cover that would break the only supported
// way to make a lab copy in the first place.
//
// RESOLVE, THEN DECIDE. A lab path can be a symlink into the live root, and a
// lexical check waves that through — so symlinks are evaluated first, on both
// sides, for the longest existing ancestor of a path that does not exist yet
// (the guard runs before a copy is made). Comparison is separator-aware, so a
// sibling merely NAMED like the root (`repos-lab`) is not caught by it: a false
// positive here teaches the next reader to weaken the check, which is how a
// guard stops guarding.
func refuseLivePath(path, home string) error {
	live, err := resolveDeepest(liveReposRoot(home))
	if err != nil {
		return fmt.Errorf("resolve live repos root: %w", err)
	}
	target, err := resolveDeepest(path)
	if err != nil {
		return fmt.Errorf("resolve corpus path %q: %w", path, err)
	}
	if target == live || strings.HasPrefix(target, live+string(filepath.Separator)) {
		return fmt.Errorf(
			"refusing to open %q: it resolves to %q, inside the live knowledge-base root %q. "+
				"Phase-4 measurement runs on COPIES only — take a snapshot and point -scratch at it",
			path, target, live)
	}
	return nil
}

// resolveDeepest returns an absolute, symlink-resolved form of path. When path
// does not exist, it resolves the deepest ancestor that does and re-appends the
// missing tail — so a path that has not been created yet is still judged
// against where it WOULD land, rather than being waved through for not
// existing.
func resolveDeepest(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	cur := filepath.Clean(abs)
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the root without finding anything that exists; nothing
			// was resolvable, so the lexical form is the best answer there is.
			return filepath.Join(cur, rest), nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
