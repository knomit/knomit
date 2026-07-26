package main

import (
	"fmt"
	"sort"
	"time"

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
	ui            *ui
}

// renderFiles produces the complete set of owned files for one source commit:
// the OKF bundle plus the sync config. Rendering is a pure function of the
// source commit, which is what makes a re-sync of unchanged knowledge a no-op.
func renderFiles(req exportRequest) (map[string][]byte, okfsource.Snapshot, error) {
	u := req.ui
	u.Step("Reading", fmt.Sprintf("%s at %s", req.branch, shortSHA(req.head)))
	snap, err := okfsource.LoadWithProgress(req.repo.Storer, req.head, throttled(func(stage string, done int) {
		u.Update(fmt.Sprintf("%s at %s  %s %d", req.branch, shortSHA(req.head), stage, done))
	}))
	if err != nil {
		return nil, snap, err
	}
	u.Done(fmt.Sprintf("%d fact%s · %d event%s · %d retired",
		len(snap.Facts), plural(len(snap.Facts)),
		len(snap.Events), plural(len(snap.Events)), len(snap.Retired)))
	for _, w := range snap.Warnings {
		u.Note("%s", w)
	}

	u.Step("Rendering", "building the OKF bundle")
	bundle, skips := okf.Build(okf.RepoIdentity{ID: snap.RepoID}, snap.Facts, snap.Events, okf.RenderOpts{
		Ontology: snap.Ontology,
		Retired:  snap.Retired,
	})
	u.Done(fmt.Sprintf("%d document%s", len(bundle.Files), plural(len(bundle.Files))))
	if skips.Skipped > 0 {
		// A fact that cannot be mapped is knowledge missing from the published
		// bundle, so the COUNT is always stated — a corpus with hundreds of them
		// must not bury that headline under hundreds of lines, nor print the
		// reasons and leave the reader to total them up.
		u.Note("%d fact%s could not be mapped and %s NOT exported",
			skips.Skipped, plural(skips.Skipped), was(skips.Skipped))
		for i, r := range skips.Reasons {
			if i == maxSkipReasons {
				u.Note("… and %d more", len(skips.Reasons)-maxSkipReasons)
				break
			}
			u.Note("  %s", r)
		}
	}

	// Never commit a non-conformant bundle: the whole value of the export is
	// that a consumer can trust the format without re-validating.
	u.Step("Validating", "checking OKF "+okf.OKFVersion+" conformance")
	if err := okf.Validate(bundle); err != nil {
		u.Skip("failed")
		return nil, snap, fmt.Errorf("generated bundle is not conformant: %w", err)
	}
	u.Done("conformant with OKF " + okf.OKFVersion)

	files := make(map[string][]byte, len(bundle.Files)+1)
	for _, f := range bundle.Files {
		files[f.Path] = f.Content
	}

	cfg := Config{
		Branch:       req.branch,
		SyncedCommit: req.head.String(),
		ToolVersion:  version.String(),
		// Redact prevSource too: a config written by an older build may already
		// carry a credential, and rewriting it is the only chance to remove it.
		// safeURL, not redactURL: this value is COMMITTED, so an unparseable
		// URL — which redactURL passes through untouched — would put a token on
		// disk and then push it.
		Source: safeURL(req.prevSource),
	}
	if req.publishSource {
		cfg.Source = safeURL(req.source)
		if cfg.Source != req.source {
			u.Note("credentials stripped from the published source URL")
		}
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
	files, _, err := renderFiles(req)
	if err != nil {
		return false, err
	}

	u := req.ui
	u.Step("Writing", req.dir)
	changed, deleted, err := reconcile(req.dir, files)
	if err != nil {
		return false, err
	}
	// Report what CHANGED, not how many files the bundle contains. "3038
	// written" on a sync that altered 44 documents reads as "everything was
	// regenerated", which is what a reader will act on.
	u.Done(fmt.Sprintf("%d changed · %d removed  (of %d)", len(changed), deleted, len(files)))

	wt, err := req.repo.Worktree()
	if err != nil {
		return false, err
	}
	u.Step("Staging", "comparing against the index")
	stageProgress := throttledCount2(func(done, total int) {
		u.Update(fmt.Sprintf("%d/%d", done, total))
	})
	staged, err := stageOwned(req.repo, wt, files, changed, stageProgress)
	if err != nil {
		return false, err
	}
	u.Done(fmt.Sprintf("%d file%s", staged, plural(staged)))

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

	u.Step("Committing", req.branch)
	commit, err := wt.Commit(msg, &git.CommitOptions{Author: &sig, Committer: &sig})
	if err == git.ErrEmptyCommit {
		u.Skip("nothing changed — no commit")
		return false, nil
	}
	if err != nil {
		return false, err
	}
	u.Done(fmt.Sprintf("%s on %s", shortSHA(commit), req.branch))
	return true, nil
}

// shortSHA abbreviates a hash for display, as git does.
func shortSHA(h plumbing.Hash) string {
	s := h.String()
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// throttled wraps an okfsource.Progress so a terminal is redrawn at most every
// few dozen milliseconds. The callback fires per commit and per fact — tens of
// thousands of times on a large base — and redrawing at that rate costs more
// than the work being reported.
func throttled(fn func(stage string, done int)) okfsource.Progress {
	var last time.Time
	return func(stage string, done int) {
		if now := time.Now(); now.Sub(last) >= progressInterval {
			last = now
			fn(stage, done)
		}
	}
}

// throttledCount2 is throttled for a done/total counter stage.
func throttledCount2(fn func(done, total int)) func(int, int) {
	var last time.Time
	return func(done, total int) {
		if now := time.Now(); now.Sub(last) >= progressInterval {
			last = now
			fn(done, total)
		}
	}
}

// progressInterval is the redraw budget: fast enough to look live, slow enough
// to cost nothing.
const progressInterval = 60 * time.Millisecond

// maxSkipReasons is how many individual skips are named before the list is
// summarized, matching the cap okfsource uses for its own degradation reports.
const maxSkipReasons = 5

// was agrees a verb with a count, for the skip summary.
func was(n int) string {
	if n == 1 {
		return "was"
	}
	return "were"
}

// stageOwned makes the index match files for the OWNED paths, leaving every
// other index entry — a publisher's README.md, LICENSE, .github/ — untouched.
// Staging with `All` would sweep their uncommitted edits into an okf commit.
func stageOwned(repo *git.Repository, wt *git.Worktree, files map[string][]byte, forced []string, onProgress func(done, total int)) (int, error) {
	idx, err := repo.Storer.Index()
	if err != nil {
		return 0, err
	}
	indexed := make(map[string]plumbing.Hash, len(idx.Entries))
	for _, e := range idx.Entries {
		indexed[e.Name] = e.Hash
	}
	mustStage := make(map[string]bool, len(forced))
	for _, p := range forced {
		mustStage[p] = true
	}

	// Stage only what the index does not already record correctly. Comparing
	// the rendered document's blob hash against the index entry is what makes
	// the skip SAFE rather than merely fast: an equal hash means the index
	// already holds exactly this content, and reconcile has just guaranteed the
	// working file matches it too. Anything reconcile actually wrote is staged
	// unconditionally, so a hand-edited file cannot be skipped on a stale hash.
	var pending []string
	for _, rel := range sortedKeys(files) {
		if !mustStage[rel] && indexed[rel] == plumbing.ComputeHash(plumbing.BlobObject, files[rel]) {
			continue
		}
		pending = append(pending, rel)
	}

	// SkipStatus is load-bearing for speed, not a micro-optimisation. Plain
	// wt.Add recomputes the ENTIRE worktree Status() on every call, so staging
	// a bundle costs files × O(files): measured at 116s for a 1969-file corpus
	// versus 3.6s with SkipStatus, for a byte-identical index. It is safe here
	// because every path was just written by reconcile, so none is a
	// directory and none is missing — the cases Status would resolve.
	for i, rel := range pending {
		if err := wt.AddWithOptions(&git.AddOptions{Path: rel, SkipStatus: true}); err != nil {
			return 0, fmt.Errorf("stage %s: %w", rel, err)
		}
		if onProgress != nil {
			onProgress(i+1, len(pending))
		}
	}
	// Drop index entries under the owned roots that this render did not
	// produce. wt.Add cannot express a deletion for a path already gone from
	// disk, and without this a retired fact would stay committed forever.
	// Re-read the index: the staging above rewrote it.
	idx, err = repo.Storer.Index()
	if err != nil {
		return 0, err
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
	return len(pending), repo.Storer.SetIndex(idx)
}
