package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/version"
)

const syncUsage = "usage: knomit-okf sync [-b <branch>] [--source <url>] [--publish-source]"

func runSync(args []string, dir string, out io.Writer) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(out)
	branch := fs.String("b", "", "source branch to export (default: the branch recorded in "+configFile+")")
	source := fs.String("source", "", "override the KB URL for this run")
	publishSource := fs.Bool("publish-source", false, "record the source URL in "+configFile)
	var auth authOpts
	registerAuthFlags(fs, &auth)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(syncUsage)
	}

	repo, err := git.PlainOpen(dir)
	if err != nil {
		return fmt.Errorf("open %s: %w (run knomit-okf clone first)", filepath.Clean(dir), err)
	}
	cfg, err := readConfig(dir)
	if err != nil {
		return err
	}

	url, err := resolveSourceURL(repo, cfg, *source)
	if err != nil {
		return err
	}
	if err := auth.resolve(); err != nil {
		return err
	}
	am, err := authFor(url, auth)
	if err != nil {
		return err
	}
	name := *branch
	if name == "" {
		name = cfg.Branch
	}
	if name == "" {
		if head, herr := repo.Head(); herr == nil && head.Name().IsBranch() {
			name = head.Name().Short()
		}
	}
	if name == "" {
		return errors.New("cannot determine which branch to sync: pass -b <branch>")
	}

	u := newUI(out)
	u.Banner(version.String())

	u.Step("Fetching", safeURL(url))
	if err := fetchSource(repo, url, am); err != nil {
		return explainFetchError(err, url, auth)
	}
	fetched, err := sourceBranches(repo)
	if err != nil {
		return err
	}
	u.Done(fmt.Sprintf("%d branch%s", len(fetched), pluralES(len(fetched))))

	head, err := resolveSourceBranch(repo, name)
	if err != nil {
		return err
	}

	// Move to the output branch for `name` BEFORE reading its config: each
	// output branch commits its own .knomit-okf.yaml, so the synced_commit that
	// matters is the target branch's, not whichever branch happens to be out.
	created, err := checkoutOutputBranch(repo, dir, name)
	if err != nil {
		return err
	}
	if !created {
		if cfg, err = readConfig(dir); err != nil {
			return err
		}
	} else {
		cfg = Config{}
	}

	// Nothing fetched, nothing pending: skip rendering entirely. This is the
	// payoff of a deterministic mapper — an unchanged source needs no work at
	// all, rather than a full render whose output happens to match.
	if !created && cfg.SyncedCommit == head.String() {
		u.Step("Checking", name)
		clean, err := ownedPathsClean(repo)
		if err != nil {
			return err
		}
		if clean {
			u.Skip(fmt.Sprintf("already up to date at %s", shortSHA(head)))
			u.Finish("Nothing to do")
			return nil
		}
		u.Done("local bundle differs — re-rendering")
	}

	committed, err := export(exportRequest{
		repo: repo, dir: dir, branch: name, head: head,
		source: url, publishSource: *publishSource, prevSource: cfg.Source, ui: u,
	})
	if err != nil {
		return err
	}
	if committed {
		u.Finish("Synced %s", name)
		u.Hint("Publish it:", "git push")
	} else {
		u.Finish("Already up to date")
	}
	return nil
}

// resolveSourceURL applies the documented precedence: an explicit --source
// beats the local git remote, which beats the committed config. The config
// field exists solely so a stranger who cloned a PUBLISHED repo can sync it;
// the remote is local and never travels.
func resolveSourceURL(repo *git.Repository, cfg Config, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if rm, err := repo.Remote(sourceRemote); err == nil && len(rm.Config().URLs) > 0 {
		return rm.Config().URLs[0], nil
	}
	if cfg.Source != "" {
		return cfg.Source, nil
	}
	return "", fmt.Errorf("no knowledge-base URL: pass --source <url>, add the %q git remote, or publish one with --publish-source", sourceRemote)
}

// checkoutOutputBranch puts the output branch for name into the working tree,
// creating it as an ORPHAN when it does not exist. Bundles for different source
// branches are unrelated snapshots, not descendants of one another, so a shared
// history would only produce meaningless diffs between them.
//
// Returns whether the branch was newly created.
func checkoutOutputBranch(repo *git.Repository, dir, name string) (created bool, err error) {
	ref := plumbing.NewBranchReferenceName(name)
	head, headErr := repo.Reference(plumbing.HEAD, false)
	if headErr == nil && head.Type() == plumbing.SymbolicReference && head.Target() == ref {
		if _, err := repo.Reference(ref, true); err == nil {
			return false, nil // already here
		}
		return true, nil // HEAD points at it but it has no commit yet
	}

	if _, err := repo.Reference(ref, true); err == nil {
		wt, err := repo.Worktree()
		if err != nil {
			return false, err
		}
		// go-git refuses to switch branches while ANY tracked file differs from
		// the index, and says so as a bare "worktree contains unstaged changes"
		// that names nothing. The two causes deserve opposite treatment, so
		// separate them rather than passing Force and hoping.
		ownedDirty, publisherDirty, err := unstagedOwnership(wt)
		if err != nil {
			return false, err
		}
		if len(publisherDirty) > 0 {
			return false, fmt.Errorf(
				"cannot switch to %s: you have uncommitted changes to %s\n  hint: commit or stash them (git stash), then re-run",
				name, summarizePaths(publisherDirty, 5))
		}
		// Only bundle files differ, and export is about to rewrite every one of
		// them from the source commit — so putting them back as the index has
		// them loses nothing and clears the block. Restoring rather than
		// force-checking-out is what keeps the blast radius inside the owned
		// paths: a hard reset would also be entitled to a publisher's files.
		if err := restoreOwnedFromIndex(repo, dir, ownedDirty); err != nil {
			return false, err
		}
		if err := wt.Checkout(&git.CheckoutOptions{Branch: ref}); err != nil {
			return false, fmt.Errorf("checkout %s: %w", name, err)
		}
		return false, nil
	}

	// New orphan branch: point HEAD at it, then clear the index and the owned
	// paths so the first commit contains this bundle and nothing inherited from
	// the branch that was previously checked out.
	if err := setHEADTo(repo, name); err != nil {
		return false, err
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		return false, err
	}
	idx.Entries = nil
	if err := repo.Storer.SetIndex(idx); err != nil {
		return false, err
	}
	for _, p := range ownedPaths {
		if err := os.RemoveAll(filepath.Join(dir, filepath.FromSlash(p))); err != nil {
			return false, err
		}
	}
	return true, nil
}

// unstagedOwnership splits the paths whose working file differs from the index
// into the ones this tool owns and the ones belonging to the publisher.
//
// It mirrors go-git's own definition of what blocks a checkout
// (Worktree.containsUnstagedChanges): a modification or deletion of a TRACKED
// file. An untracked addition is explicitly not one — go-git carries those
// across a branch switch — so a publisher's brand-new file neither blocks a
// sync nor shows up here.
func unstagedOwnership(wt *git.Worktree) (owned, publisher []string, err error) {
	st, err := wt.Status()
	if err != nil {
		return nil, nil, err
	}
	for p, s := range st {
		if s.Worktree == git.Unmodified || s.Worktree == git.Untracked {
			continue
		}
		if owns(p) {
			owned = append(owned, p)
		} else {
			publisher = append(publisher, p)
		}
	}
	// Sorted because Status is a map: an error message that reorders itself
	// between runs reads as a different error.
	sort.Strings(owned)
	sort.Strings(publisher)
	return owned, publisher, nil
}

// restoreOwnedFromIndex rewrites each path to the content the index holds for
// it. Every path passed in is an owned one — the caller has already refused to
// proceed when anything else was dirty — and the guard below keeps it that way.
func restoreOwnedFromIndex(repo *git.Repository, dir string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		return err
	}
	staged := make(map[string]plumbing.Hash, len(idx.Entries))
	for _, e := range idx.Entries {
		staged[e.Name] = e.Hash
	}
	for _, p := range paths {
		if !owns(p) {
			return fmt.Errorf("restore: %s is outside the owned paths %v", p, ownedPaths)
		}
		abs := filepath.Join(dir, filepath.FromSlash(p))
		h, tracked := staged[p]
		if !tracked {
			// Nothing in the index to restore to, so matching it means the file
			// should not be there.
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		blob, err := object.GetBlob(repo.Storer, h)
		if err != nil {
			return err
		}
		r, err := blob.Reader()
		if err != nil {
			return err
		}
		content, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(abs, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// summarizePaths lists at most max paths, noting how many were left out, so a
// broadly-dirty worktree names enough to act on without filling the terminal.
func summarizePaths(paths []string, max int) string {
	if len(paths) <= max {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:max], ", "), len(paths)-max)
}

// ownedPathsClean reports whether the working tree matches the index for the
// paths this tool manages. A publisher's own uncommitted edits are irrelevant
// to whether the BUNDLE needs regenerating, so they must not force one.
func ownedPathsClean(repo *git.Repository) (bool, error) {
	wt, err := repo.Worktree()
	if err != nil {
		return false, err
	}
	st, err := wt.Status()
	if err != nil {
		return false, err
	}
	for path, s := range st {
		if !owns(path) {
			continue
		}
		if s.Worktree != git.Unmodified || s.Staging != git.Unmodified {
			return false, nil
		}
	}
	return true, nil
}
