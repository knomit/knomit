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

const cloneUsage = "usage: knomit-okf clone [-b <branch>] [--publish-source] <kb-url> <dir>"

func runClone(args []string, out io.Writer) (rerr error) {
	fs := flag.NewFlagSet("clone", flag.ContinueOnError)
	fs.SetOutput(out)
	branch := fs.String("b", "", "source branch to export (default: the source's HEAD branch)")
	publishSource := fs.Bool("publish-source", false, "record the source URL in "+configFile+" so a stranger can sync the published repo")
	var auth authOpts
	registerAuthFlags(fs, &auth)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New(cloneUsage)
	}
	url, dir := fs.Arg(0), fs.Arg(1)

	if err := auth.resolve(); err != nil {
		return err
	}
	am, err := authFor(url, auth)
	if err != nil {
		return err
	}

	createdDir, err := ensureEmptyDir(dir)
	if err != nil {
		return err
	}
	// Leave nothing behind on failure. A half-initialised directory is not just
	// litter: ensureEmptyDir refuses a non-empty target, so the obvious retry —
	// the same command with a fixed token — would die with "<dir> is not empty"
	// and force a manual rm -rf. Auth failures make that the FIRST thing many
	// users meet.
	defer func() {
		if rerr == nil {
			return
		}
		if cerr := cleanupFailedClone(dir, createdDir); cerr != nil {
			fmt.Fprintf(out, "\n  ! could not clean up %s: %v\n    remove it before retrying\n",
				filepath.Clean(dir), cerr)
		}
	}()

	repo, err := git.PlainInit(dir, false)
	if err != nil {
		return fmt.Errorf("init %s: %w", dir, err)
	}

	u := newUI(out)
	u.Banner(version.String())

	u.Step("Fetching", safeURL(url))
	// clone is the only command that records where the knowledge comes from.
	if err := createSourceRemote(repo, url); err != nil {
		return err
	}
	if err := fetchSource(repo, url, am); err != nil {
		return explainFetchError(err, url, auth)
	}
	fetched, err := sourceBranches(repo)
	if err != nil {
		return err
	}
	u.Done(fmt.Sprintf("%d branch%s", len(fetched), pluralES(len(fetched))))

	name := *branch
	if name == "" {
		if name, err = defaultSourceBranch(repo, url, am); err != nil {
			return err
		}
	}
	head, err := resolveSourceBranch(repo, name)
	if err != nil {
		return err
	}

	// The output branch MIRRORS the source branch name, so one output repo can
	// carry every branch of one KB, each a faithful bundle of its source.
	if err := setHEADTo(repo, name); err != nil {
		return err
	}

	if _, err = export(exportRequest{
		repo: repo, dir: dir, branch: name, head: head,
		source: url, publishSource: *publishSource, ui: u,
	}); err != nil {
		return err
	}

	u.Finish("Cloned %s into %s", name, filepath.Clean(dir))
	u.Hint("Publish it:",
		"cd "+filepath.Clean(dir),
		"git remote add origin <your-remote-url>",
		"git push -u origin "+name)
	return nil
}

// ensureEmptyDir accepts a missing or empty directory and creates it. Refusing
// a non-empty one is deliberate: clone writes a whole repository, and silently
// merging into someone's existing directory is not recoverable.
//
// created reports whether the directory did not exist beforehand, which is what
// tells cleanupFailedClone whether removing it is ours to do.
func ensureEmptyDir(dir string) (created bool, err error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return true, os.MkdirAll(dir, 0o755)
	}
	if err != nil {
		return false, err
	}
	if len(entries) > 0 {
		return false, fmt.Errorf("%s is not empty", filepath.Clean(dir))
	}
	return false, nil
}

// cleanupFailedClone undoes a partial clone, restoring exactly what was there
// before: the directory goes only if this run created it, otherwise it is
// emptied back to the empty directory ensureEmptyDir accepted.
//
// The distinction matters. A user who ran `mkdir my-kb && knomit-okf clone …
// my-kb` still owns that directory; deleting it on a bad token would be us
// destroying something we did not make.
func cleanupFailedClone(dir string, created bool) error {
	if created {
		return os.RemoveAll(dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// setHEADTo points HEAD at a branch WITHOUT requiring it to exist. Committing
// from there mints a parentless (orphan) commit and creates the ref — which is
// how both `clone` and `sync -b <new>` start a bundle branch.
func setHEADTo(repo *git.Repository, branch string) error {
	return repo.Storer.SetReference(
		plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName(branch)),
	)
}
