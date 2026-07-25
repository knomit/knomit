package main

import (
	"fmt"
	"io"
	"sort"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/okf"
	"knomit/internal/okfsource"
	"knomit/internal/version"
)

// exportRequest is one branch's export: read the source at head, render, and
// commit onto the output branch of the same name.
type exportRequest struct {
	repo   *git.Repository
	dir    string
	branch string // BOTH the source branch and the output branch — they mirror
	head   plumbing.Hash
	source string // the KB URL
	// publishSource records the URL in the committed config, so a stranger who
	// clones the published repo can sync it. Off by default: a private KB's
	// address must never travel just because someone exported it.
	publishSource bool
	prevSource    string // an already-published source: URL, preserved if any
	out           io.Writer
}

// renderFiles produces the complete set of owned files for one source commit:
// the OKF bundle plus the sync config. Rendering is a pure function of the
// source commit, which is what makes a re-sync of unchanged knowledge a no-op.
func renderFiles(req exportRequest) (map[string][]byte, okfsource.Snapshot, error) {
	snap, err := okfsource.Load(req.repo.Storer, req.head)
	if err != nil {
		return nil, snap, err
	}
	for _, w := range snap.Warnings {
		fmt.Fprintf(req.out, "warning: %s\n", w)
	}

	bundle, skips := okf.Build(okf.RepoIdentity{ID: snap.RepoID}, snap.Facts, snap.Events, okf.RenderOpts{
		Ontology: snap.Ontology,
		Retired:  snap.Retired,
	})
	if skips.Skipped > 0 {
		for _, r := range skips.Reasons {
			fmt.Fprintf(req.out, "skipped: %s\n", r)
		}
	}
	// Never commit a non-conformant bundle: the whole value of the export is
	// that a consumer can trust the format without re-validating.
	if err := okf.Validate(bundle); err != nil {
		return nil, snap, fmt.Errorf("generated bundle is not conformant: %w", err)
	}

	files := make(map[string][]byte, len(bundle.Files)+1)
	for _, f := range bundle.Files {
		files[f.Path] = f.Content
	}

	cfg := Config{
		Branch:       req.branch,
		SyncedCommit: req.head.String(),
		ToolVersion:  version.String(),
		Source:       req.prevSource,
	}
	if req.publishSource {
		cfg.Source = req.source
	}
	raw, err := marshalConfig(cfg)
	if err != nil {
		return nil, snap, err
	}
	files[configFile] = raw
	return files, snap, nil
}

// export renders, reconciles the working tree, stages the owned paths, and
// commits — but only if something actually changed. Returns whether a commit
// was made.
func export(req exportRequest) (bool, error) {
	files, snap, err := renderFiles(req)
	if err != nil {
		return false, err
	}

	written, deleted, err := reconcile(req.dir, files)
	if err != nil {
		return false, err
	}

	wt, err := req.repo.Worktree()
	if err != nil {
		return false, err
	}
	if err := stageOwned(req.repo, wt, files); err != nil {
		return false, err
	}

	// Timestamp the export from the SOURCE commit, never the clock, so the same
	// source commit always yields the same output commit — two people exporting
	// the same knowledge get byte-identical repositories.
	src, err := object.GetCommit(req.repo.Storer, req.head)
	if err != nil {
		return false, err
	}
	sig := object.Signature{
		Name:  "knomit-okf",
		Email: "okf@knomit.io",
		When:  src.Committer.When.UTC(),
	}
	msg := fmt.Sprintf("okf: sync %s\n\nsource-commit: %s\ntool-version: %s\n",
		req.branch, req.head.String(), version.String())

	_, err = wt.Commit(msg, &git.CommitOptions{Author: &sig, Committer: &sig})
	if err == git.ErrEmptyCommit {
		fmt.Fprintf(req.out, "no change to commit (%d facts, %d files)\n", len(snap.Facts), len(files))
		return false, nil
	}
	if err != nil {
		return false, err
	}

	fmt.Fprintf(req.out, "committed %s: %d facts, %d files (%d written, %d removed)\n",
		req.branch, len(snap.Facts), len(files), written, deleted)
	return true, nil
}

// stageOwned makes the index match files for the OWNED paths, leaving every
// other index entry — a publisher's README.md, LICENSE, .github/ — untouched.
// Staging with `All` would sweep their uncommitted edits into an okf commit.
func stageOwned(repo *git.Repository, wt *git.Worktree, files map[string][]byte) error {
	for _, rel := range sortedKeys(files) {
		if _, err := wt.Add(rel); err != nil {
			return fmt.Errorf("stage %s: %w", rel, err)
		}
	}
	// Drop index entries under the owned roots that this render did not
	// produce. wt.Add cannot express a deletion for a path already gone from
	// disk, and without this a retired fact would stay committed forever.
	idx, err := repo.Storer.Index()
	if err != nil {
		return err
	}
	kept := idx.Entries[:0]
	for _, e := range idx.Entries {
		if owns(e.Name) {
			if _, produced := files[e.Name]; !produced {
				continue
			}
		}
		kept = append(kept, e)
	}
	idx.Entries = kept
	sort.Slice(idx.Entries, func(i, j int) bool { return idx.Entries[i].Name < idx.Entries[j].Name })
	return repo.Storer.SetIndex(idx)
}
