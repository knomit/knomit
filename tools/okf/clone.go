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
)

const cloneUsage = "usage: knomit-okf clone [-b <branch>] [--publish-source] <kb-url> <dir>"

func runClone(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("clone", flag.ContinueOnError)
	fs.SetOutput(out)
	branch := fs.String("b", "", "source branch to export (default: the source's HEAD branch)")
	publishSource := fs.Bool("publish-source", false, "record the source URL in "+configFile+" so a stranger can sync the published repo")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New(cloneUsage)
	}
	url, dir := fs.Arg(0), fs.Arg(1)

	if err := ensureEmptyDir(dir); err != nil {
		return err
	}
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		return fmt.Errorf("init %s: %w", dir, err)
	}

	fmt.Fprintf(out, "fetching %s\n", url)
	if err := fetchSource(repo, url); err != nil {
		return err
	}

	name := *branch
	if name == "" {
		if name, err = defaultSourceBranch(repo, url); err != nil {
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

	_, err = export(exportRequest{
		repo: repo, dir: dir, branch: name, head: head,
		source: url, publishSource: *publishSource, out: out,
	})
	return err
}

// ensureEmptyDir accepts a missing or empty directory and creates it. Refusing
// a non-empty one is deliberate: clone writes a whole repository, and silently
// merging into someone's existing directory is not recoverable.
func ensureEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return os.MkdirAll(dir, 0o755)
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s is not empty", filepath.Clean(dir))
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
