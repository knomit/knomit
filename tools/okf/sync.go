package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

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
