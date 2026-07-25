package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

// sourceRemote is the git remote holding the KB. It is created by `clone`,
// lives only in .git/config, and therefore never travels to a publisher's
// remote — which is why a private KB's address stays private by default.
const sourceRemote = "knomit-source"

// sourceRefspec fetches every source branch into a PRIVATE ref namespace.
// refs/knomit-okf/source/* is outside refs/heads/*, so git's default push
// refspec never publishes it and the source history is never checked out —
// the whole reason one directory can be both the export and its own source.
const sourceRefspec = "+refs/heads/*:refs/knomit-okf/source/*"

// sourceRefPrefix is where sourceRefspec lands.
const sourceRefPrefix = "refs/knomit-okf/source/"

// ownedPaths are the only entries knomit-okf manages. It writes them
// completely and deletes anything under them it did not write. Everything else
// in the repo — README.md, LICENSE, .github/ — belongs to the publisher and is
// never touched. A naive "clean the working tree" would destroy their work.
var ownedPaths = []string{"index.md", "log.md", "kb", "views", configFile}

// owns reports whether a repo-relative path is one this tool manages.
func owns(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, p := range ownedPaths {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// reconcile makes the owned paths in dir exactly match files, deleting any
// stale entry beneath them. Returns the number of files written and deleted.
//
// Deleting is not optional: overlaying files can never remove them, so a
// retired fact's document would stay published forever, contradicting the
// views/retired.md in the same bundle.
func reconcile(dir string, files map[string][]byte) (written, deleted int, err error) {
	// Write first, then prune whatever survives that we did not write.
	for _, rel := range sortedKeys(files) {
		if !owns(rel) {
			return written, deleted, fmt.Errorf("reconcile: %s is outside the owned paths %v", rel, ownedPaths)
		}
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return written, deleted, err
		}
		if err := os.WriteFile(abs, files[rel], 0o644); err != nil {
			return written, deleted, err
		}
		written++
	}

	// Prune: walk only the owned roots, so a publisher's files are never even
	// visited, let alone removed.
	for _, root := range ownedPaths {
		absRoot := filepath.Join(dir, filepath.FromSlash(root))
		info, statErr := os.Lstat(absRoot)
		if statErr != nil {
			continue // nothing there
		}
		if !info.IsDir() {
			if _, keep := files[root]; !keep {
				if err := os.Remove(absRoot); err != nil {
					return written, deleted, err
				}
				deleted++
			}
			continue
		}
		var stale []string
		err := filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, relErr := filepath.Rel(dir, p)
			if relErr != nil {
				return relErr
			}
			if _, keep := files[filepath.ToSlash(rel)]; !keep {
				stale = append(stale, p)
			}
			return nil
		})
		if err != nil {
			return written, deleted, err
		}
		for _, p := range stale {
			if err := os.Remove(p); err != nil {
				return written, deleted, err
			}
			deleted++
		}
		if err := pruneEmptyDirs(absRoot); err != nil {
			return written, deleted, err
		}
	}
	return written, deleted, nil
}

// pruneEmptyDirs removes directories left empty by the prune above, so a
// retired category does not linger as an empty tree. root itself is removed
// when it ends up empty.
func pruneEmptyDirs(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Deepest first, so a parent sees its children already gone.
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			if err := os.Remove(d); err != nil {
				return err
			}
		}
	}
	return nil
}

func sortedKeys(m map[string][]byte) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// createSourceRemote records the KB URL as the knomit-source remote. Only
// `clone` does this: it is the one command whose job includes deciding where a
// repository's knowledge comes from. Everything else fetches WITHOUT touching
// the stored configuration — see fetchSource.
func createSourceRemote(repo *git.Repository, url string) error {
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name:  sourceRemote,
		URLs:  []string{url},
		Fetch: []config.RefSpec{config.RefSpec(sourceRefspec)},
	}); err != nil {
		return fmt.Errorf("create remote %s: %w", sourceRemote, err)
	}
	return nil
}

// fetchSource fetches every source branch from url into the private namespace,
// WITHOUT reading or writing the repository's remote configuration.
//
// The independence from stored config is the point: `--source` is documented
// as a one-off override, so it must not silently repoint the knomit-source
// remote for every future run. A user who has genuinely moved their knowledge
// base changes it deliberately, with `git remote set-url`.
func fetchSource(repo *git.Repository, url string) error {
	// An anonymous remote — constructed per call, never persisted to config.
	rm := git.NewRemote(repo.Storer, &config.RemoteConfig{
		Name:  sourceRemote,
		URLs:  []string{url},
		Fetch: []config.RefSpec{config.RefSpec(sourceRefspec)},
	})

	// Prune, because refs/knomit-okf/source/* mirrors upstream branches exactly
	// as remote-tracking refs do. Without it a branch deleted upstream lingers
	// locally forever: `branches` would report it as live and `sync -b` would
	// happily export a branch the knowledge base no longer has. The bundle
	// branches are a separate commit chain and are unaffected.
	err := rm.Fetch(&git.FetchOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(sourceRefspec)},
		Tags:     git.NoTags,
		Prune:    true,
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return fmt.Errorf("fetch %s: %w", url, err)
	}
	return nil
}

// sourceBranches lists the branch names fetched into the private namespace.
func sourceBranches(repo *git.Repository) ([]string, error) {
	iter, err := repo.References()
	if err != nil {
		return nil, err
	}
	var out []string
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().String()
		if strings.HasPrefix(name, sourceRefPrefix) {
			out = append(out, strings.TrimPrefix(name, sourceRefPrefix))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// resolveSourceBranch returns the source head for branch, with an error that
// lists what WAS fetched — the failure is almost always a typo or a branch that
// does not exist upstream, and guessing would export the wrong knowledge.
func resolveSourceBranch(repo *git.Repository, branch string) (plumbing.Hash, error) {
	ref, err := repo.Reference(plumbing.ReferenceName(sourceRefPrefix+branch), true)
	if err == nil {
		return ref.Hash(), nil
	}
	available, lerr := sourceBranches(repo)
	if lerr != nil || len(available) == 0 {
		return plumbing.ZeroHash, fmt.Errorf("source branch %q not found (nothing was fetched from %s)", branch, sourceRemote)
	}
	return plumbing.ZeroHash, fmt.Errorf("source branch %q not found; fetched branches: %s",
		branch, strings.Join(available, ", "))
}

// defaultSourceBranch resolves the source's own HEAD branch, falling back to
// "main"/"master" and finally to the sole fetched branch. Mirroring the
// source's default is what makes `clone` need no -b in the common case.
func defaultSourceBranch(repo *git.Repository, url string) (string, error) {
	if name, err := remoteHeadBranch(repo, url); err == nil && name != "" {
		if _, err := repo.Reference(plumbing.ReferenceName(sourceRefPrefix+name), true); err == nil {
			return name, nil
		}
	}
	available, err := sourceBranches(repo)
	if err != nil {
		return "", err
	}
	for _, pref := range []string{"main", "master"} {
		for _, b := range available {
			if b == pref {
				return b, nil
			}
		}
	}
	if len(available) == 1 {
		return available[0], nil
	}
	if len(available) == 0 {
		return "", fmt.Errorf("no branches were fetched from %s", url)
	}
	return "", fmt.Errorf("cannot infer the source's default branch; pass -b (fetched: %s)",
		strings.Join(available, ", "))
}

// remoteHeadBranch asks the remote which branch its HEAD points at.
func remoteHeadBranch(repo *git.Repository, url string) (string, error) {
	rm := git.NewRemote(repo.Storer, &config.RemoteConfig{Name: sourceRemote, URLs: []string{url}})
	refs, err := rm.List(&git.ListOptions{})
	if err != nil {
		return "", err
	}
	for _, ref := range refs {
		if ref.Name() == plumbing.HEAD && ref.Type() == plumbing.SymbolicReference {
			return ref.Target().Short(), nil
		}
	}
	return "", fmt.Errorf("remote advertises no symbolic HEAD")
}
