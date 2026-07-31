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
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/version"
)

const syncUsage = "usage: knomit-okf sync [-b <branch>] [--source <url>] [--publish-source]"

func runSync(args []string, dir string, out io.Writer) (rerr error) {
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

	repo, dir, err := openExport(dir)
	if err != nil {
		return err
	}
	// Searching upward found A repository; refuse unless it is one of ours.
	// Without this an accidental `sync --source <url>` run from inside an
	// unrelated checkout would render a bundle into someone's source tree.
	if !isExportRepo(repo, dir) {
		return fmt.Errorf("%s is a git repository but not a knomit-okf export (no %q remote, no %s)\n  hint: run knomit-okf clone <kb-url> <dir> to create one",
			filepath.Clean(dir), sourceRemote, configFile)
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
	created, rollback, err := checkoutOutputBranch(repo, dir, name)
	if err != nil {
		return err
	}
	// Creating an orphan branch moves HEAD and empties the index BEFORE the
	// export that justifies it has produced a single byte. A render failure —
	// an unreachable object, a non-conformant bundle, a bad token on a later
	// fetch — would otherwise strand the repository on an unborn branch with an
	// empty index and no word about how to get back.
	defer func() {
		if rerr == nil || rollback == nil {
			return
		}
		rollback()
		// Printed BEFORE the error itself, which main writes to stderr once this
		// returns — so it announces the failure rather than trailing it.
		u.Note("sync failed; left %s checked out as it was, nothing was committed", shortRef(repo))
	}()
	if !created {
		if cfg, err = readConfig(dir); err != nil {
			return err
		}
	} else {
		// A fresh orphan branch inherits nothing about WHAT was exported —
		// Branch and SyncedCommit belong to the branch we just left. The
		// published source URL is the exception: it says where this REPOSITORY
		// gets its knowledge, which is a property of the repo and not of one
		// branch. Dropping it stranded the new branch — the knomit-source
		// remote is local-only, so on a stranger's clone the committed field is
		// the only way back to the KB, and a bare `sync` there died with "no
		// knowledge-base URL". Carrying it publishes nothing new: it is already
		// committed on a sibling branch of the same repo, and an unpublished
		// (empty) source stays empty.
		cfg = Config{Source: cfg.Source}
	}

	// Nothing fetched, nothing pending, and the bundle was built by THIS build:
	// skip rendering entirely. This is the payoff of a deterministic mapper —
	// an unchanged source needs no work at all, rather than a full render whose
	// output happens to match.
	//
	// The release guard is what keeps the skip from becoming a stale cache. A
	// bundle is a function of the source commit AND the mapper, so a build
	// whose rendering changed must re-render even though the source has not
	// moved — otherwise the bundle stays at the old mapper's output until the
	// knowledge base happens to change, which may be never. Being wrong here is
	// cheap in one direction only: a needless render costs seconds and, because
	// the mapper is deterministic, produces no commit when the output matches.
	//
	// It compares the RELEASE (version.Version), not version.String(). The
	// latter embeds the build's git SHA, so it differs on every rebuild — a
	// developer's `go build` would re-render, rewrite tool_version, and commit
	// a bundle whose content nobody changed. That is precisely the "commits
	// record tool runs, not knowledge" failure this tool exists to avoid. The
	// residue is one re-export per RELEASE per branch, which is a true record:
	// this bundle was regenerated and re-validated under that release.
	upToDate := cfg.SyncedCommit == head.String() && releaseOf(cfg.ToolVersion) == version.Version
	if !created && upToDate {
		u.Step("Checking", name)
		// Status compares the index against HEAD's tree, so it reads objects and
		// loses the same race the export does. Guarded here too: this is the path
		// a scheduled `knomit-okf sync` takes on every run that has nothing to
		// do, which makes it the most-executed object read in the tool.
		var clean bool
		var recovered bool
		err = retryAfterRepack(repo, func() { recovered = true }, func() error {
			var e error
			clean, e = ownedPathsClean(repo)
			return e
		})
		if err != nil {
			return err
		}
		if clean {
			u.Skip(fmt.Sprintf("already up to date at %s", shortSHA(head)))
			noteIf(u, recovered)
			u.Finish("Nothing to do")
			return nil
		}
		u.Done("local bundle differs — re-rendering")
		noteIf(u, recovered)
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
// Returns whether the branch was newly created and, in that case, a rollback
// that undoes the preparation — see prepareOrphanBranch.
func checkoutOutputBranch(repo *git.Repository, dir, name string) (created bool, rollback func(), err error) {
	ref := plumbing.NewBranchReferenceName(name)

	if _, refErr := repo.Reference(ref, true); refErr == nil {
		head, headErr := repo.Reference(plumbing.HEAD, false)
		if headErr == nil && head.Type() == plumbing.SymbolicReference && head.Target() == ref {
			return false, nil, nil // already here
		}
		wt, err := repo.Worktree()
		if err != nil {
			return false, nil, err
		}
		// go-git refuses to switch branches while ANY tracked file differs from
		// the index, and says so as a bare "worktree contains unstaged changes"
		// that names nothing. The two causes deserve opposite treatment, so
		// separate them rather than passing Force and hoping.
		ownedDirty, publisherDirty, err := unstagedOwnership(wt)
		if err != nil {
			return false, nil, err
		}
		if len(publisherDirty) > 0 {
			return false, nil, fmt.Errorf(
				"cannot switch to %s: you have uncommitted changes to %s\n  hint: commit or stash them (git stash), then re-run",
				name, summarizePaths(publisherDirty, 5))
		}
		// Only bundle files differ, and export is about to rewrite every one of
		// them from the source commit — so putting them back as the index has
		// them loses nothing and clears the block. Restoring rather than
		// force-checking-out is what keeps the blast radius inside the owned
		// paths: a hard reset would also be entitled to a publisher's files.
		if err := restoreOwnedFromIndex(repo, dir, ownedDirty); err != nil {
			return false, nil, err
		}
		if err := wt.Checkout(&git.CheckoutOptions{Branch: ref}); err != nil {
			return false, nil, fmt.Errorf("checkout %s: %w", name, err)
		}
		return false, nil, nil
	}

	// The branch has no commit yet. HEAD may already point at it — an earlier
	// run stopped here, or someone ran `git checkout --orphan` — but that is the
	// same state this creates, so it gets the same preparation. Skipping it
	// there left whatever the previous branch had staged to be committed as the
	// new branch's first commit.
	rollback, err = prepareOrphanBranch(repo, name)
	if err != nil {
		return false, nil, err
	}
	return true, rollback, nil
}

// prepareOrphanBranch points HEAD at an unborn branch and empties the index, so
// the first commit holds this bundle and nothing inherited from the branch that
// was previously checked out.
//
// It does NOT delete the owned paths from the working tree. reconcile writes
// every bundle file and then prunes anything under the owned roots it did not
// write, so the result on disk is identical either way — and leaving the files
// alone means a failure between here and the commit costs the user nothing on
// disk. The returned rollback restores HEAD and the index, putting the
// repository back exactly as it was found.
func prepareOrphanBranch(repo *git.Repository, name string) (rollback func(), err error) {
	prevHEAD, _ := repo.Reference(plumbing.HEAD, false)
	idx, err := repo.Storer.Index()
	if err != nil {
		return nil, err
	}
	saved := make([]*index.Entry, len(idx.Entries))
	copy(saved, idx.Entries)

	if err := setHEADTo(repo, name); err != nil {
		return nil, err
	}
	idx.Entries = nil
	if err := repo.Storer.SetIndex(idx); err != nil {
		return nil, err
	}

	return func() {
		if prevHEAD != nil {
			_ = repo.Storer.SetReference(prevHEAD)
		}
		back, ierr := repo.Storer.Index()
		if ierr != nil {
			return
		}
		back.Entries = saved
		_ = repo.Storer.SetIndex(back)
	}, nil
}

// shortRef names whatever HEAD points at, for a message printed after a
// rollback. Best-effort: it is decoration on an error the user already has.
func shortRef(repo *git.Repository) string {
	head, err := repo.Reference(plumbing.HEAD, false)
	if err != nil || head.Type() != plumbing.SymbolicReference {
		return "the previous branch"
	}
	return head.Target().Short()
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
