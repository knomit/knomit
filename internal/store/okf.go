package store

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/okf"
)

const okfOntologyRoot = "kb"

// okfOntologyFile is the ontology committed in the repo tree. Reading it at the
// SOURCE COMMIT (rather than from the live repo instance) keeps the bundle a
// pure function of that commit — the same determinism guarantee the facts get.
const okfOntologyFile = "domains/ontology.yaml"

// okfOntologyDoc reads and flattens the authored ontology at sourceSHA. A
// missing or unparseable ontology is not an error: the bundle is still fully
// conformant without descriptions, so it degrades to an empty doc.
func (s *Service) okfOntologyDoc(sourceSHA plumbing.Hash) okf.OntologyDoc {
	// Mirror how the repo itself resolves its ontology (repos/builder.go):
	// a committed domains/ontology.yaml wins, otherwise the embedded default,
	// which is what the repo is actually being validated against. The default
	// is compiled in, so it is stable for a given build.
	ont := fact.DefaultOntology()
	if commit, err := object.GetCommit(s.rh.gits, sourceSHA); err == nil {
		if f, err := commit.File(okfOntologyFile); err == nil {
			if content, err := f.Contents(); err == nil {
				parsed, perr := fact.ParseOntology([]byte(content))
				if perr != nil {
					s.rh.logOKFSkip("-", "ontology parse: "+perr.Error())
				} else {
					ont = parsed
				}
			}
		}
	}
	if ont == nil {
		return okf.OntologyDoc{}
	}
	doc := okf.OntologyDoc{
		Name:        strings.TrimSpace(ont.Name),
		Description: strings.TrimSpace(ont.Description),
		Nodes:       map[string]string{},
	}
	var walk func(prefix string, nodes map[string]*fact.OntologyNode)
	walk = func(prefix string, nodes map[string]*fact.OntologyNode) {
		for name, n := range nodes {
			if n == nil {
				continue
			}
			key := name
			if prefix != "" {
				key = prefix + "/" + name
			}
			if d := strings.TrimSpace(n.Description); d != "" {
				doc.Nodes[key] = d
			}
			walk(key, n.Children)
		}
	}
	walk("", ont.Topics)
	return doc
}

// okfReadFacts enumerates every fact blob under kb/ in the tree at sourceSHA,
// parses it, and stamps each with its authoring time from the history walk.
// It reads the git tree directly (not the derived index) so the result is a
// pure function of the source commit.
func (s *Service) okfReadFacts(ctx context.Context, sourceSHA plumbing.Hash) ([]okf.FactInput, error) {
	hist, err := s.okfHistory(ctx, sourceSHA)
	if err != nil {
		return nil, err
	}

	commit, err := object.GetCommit(s.rh.gits, sourceSHA)
	if err != nil {
		return nil, fmt.Errorf("okf: get source commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("okf: source tree: %w", err)
	}

	var facts []okf.FactInput
	err = tree.Files().ForEach(func(f *object.File) error {
		if !strings.HasPrefix(f.Name, okfOntologyRoot+"/") || !strings.HasSuffix(f.Name, ".md") {
			return nil
		}
		content, err := f.Contents()
		if err != nil {
			return fmt.Errorf("okf: read %s: %w", f.Name, err)
		}
		parsed, err := fact.ParseFact(f.Name, content)
		if err != nil {
			// Non-fact markdown under kb/ (e.g. a stray README or the kb.md
			// manifest) is skipped: it is simply not a fact to export.
			return nil
		}
		ts := hist.Authored[f.Name]
		if ts.IsZero() {
			ts = commit.Committer.When // fallback: the exported commit's time
		}
		facts = append(facts, okf.FactInput{
			Fact:      parsed,
			Timestamp: ts,
			Revisions: hist.Revisions[f.Name],
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return facts, nil
}

// okfHistoryResult is one pass over the commit DAG: the changelog entries, the
// per-path authoring times, and the per-path revision lists. All three come
// from the same walk, so they are returned together rather than recomputed.
type okfHistoryResult struct {
	Events    []okf.LogEntry
	Authored  map[string]time.Time
	Revisions map[string][]okf.Revision
}

// okfHistory walks commits from sourceSHA (bounded), producing log entries, a
// path→authoring-time map, and each path's revision list. Authoring time is
// the OLDEST commit that touched a path; an Update entry is emitted for each
// later commit that modified it. Deterministic per sourceSHA. Bounded to avoid
// unbounded walks on huge DAGs — on a history longer than the bound, a fact's
// oldest revisions are simply absent from the History section.
func (s *Service) okfHistory(ctx context.Context, sourceSHA plumbing.Hash) (okfHistoryResult, error) {
	const maxCommits = 5000

	root, err := object.GetCommit(s.rh.gits, sourceSHA)
	if err != nil {
		return okfHistoryResult{}, fmt.Errorf("okf: get source commit: %w", err)
	}

	authored := map[string]time.Time{} // path -> earliest touch time
	revisions := map[string][]okf.Revision{}
	var events []okf.LogEntry

	iter := object.NewCommitPreorderIter(root, nil, nil)
	seenCommits := 0
	err = iter.ForEach(func(c *object.Commit) error {
		if seenCommits >= maxCommits {
			return object.ErrCanceled
		}
		seenCommits++

		changed, err := okfChangedFactPaths(s, c)
		if err != nil {
			return err
		}
		for _, ch := range changed {
			if prev, seen := authored[ch.path]; !seen || c.Committer.When.Before(prev) {
				authored[ch.path] = c.Committer.When
			}
			kind := "Update"
			if ch.created {
				kind = "Creation"
			}
			events = append(events, okf.LogEntry{
				Date:  c.Committer.When,
				Kind:  kind,
				Title: ch.title,
				Path:  ch.path,
			})
			revisions[ch.path] = append(revisions[ch.path], okf.Revision{
				Date:       c.Committer.When,
				Operation:  okfOperationLabel(c.Author.Email),
				Confidence: ch.confidence,
				Title:      ch.title,
				BodyDigest: ch.bodyDigest,
				RefCount:   ch.refCount,
			})
		}
		return nil
	})
	if err != nil && err != object.ErrCanceled {
		return okfHistoryResult{}, err
	}

	// Normalize to exactly one Creation per path: the earliest Creation-marked
	// event. A path's Creation is decided by the diff (a file absent from the
	// parent tree), not by timestamp equality — commits sharing a wall-second
	// would otherwise both look "earliest" and both be labelled Creation. Any
	// remaining events (later touches, or a rare create/delete/recreate) are
	// Updates.
	creationAt := map[string]time.Time{} // path -> time of its Creation event
	for _, e := range events {
		if e.Kind != "Creation" {
			continue
		}
		if t, ok := creationAt[e.Path]; !ok || e.Date.Before(t) {
			creationAt[e.Path] = e.Date
		}
	}
	for i := range events {
		t, ok := creationAt[events[i].Path]
		if !(ok && events[i].Kind == "Creation" && events[i].Date.Equal(t)) {
			events[i].Kind = "Update"
		}
	}
	// The preorder walk starts at the tip and moves toward the root, so each
	// path's revisions accumulated newest-first above. renderHistory's mapper
	// contract requires oldest-first input — it relies on caller order to
	// break same-timestamp ties chronologically — so reverse every slice here.
	for _, rs := range revisions {
		for i, j := 0, len(rs)-1; i < j; i, j = i+1, j-1 {
			rs[i], rs[j] = rs[j], rs[i]
		}
	}

	return okfHistoryResult{Events: events, Authored: authored, Revisions: revisions}, nil
}

type okfChange struct {
	path    string
	title   string
	created bool // the path was absent from the parent tree (an Insert)

	// Snapshot of the fact AT THIS REVISION, for the History deltas.
	confidence float64
	bodyDigest string
	refCount   int
}

// okfChangedFactPaths returns the kb/*.md paths added or modified by commit c
// relative to its first parent. For a root (parentless) commit, every kb/*.md
// file in its tree is treated as a creation.
func okfChangedFactPaths(s *Service, c *object.Commit) ([]okfChange, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}

	// Root commit: no parent to diff against. Enumerate the tree directly
	// rather than diffing against a storer-less empty Tree literal, which is
	// unreliable in go-git.
	if c.NumParents() == 0 {
		var out []okfChange
		err = tree.Files().ForEach(func(f *object.File) error {
			if ch, ok := okfChangeFromFile(f.Name, true, func() (string, error) { return f.Contents() }); ok {
				out = append(out, ch)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return out, nil
	}

	parent, err := c.Parent(0)
	if err != nil {
		return nil, err
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return nil, err
	}
	changes, err := parentTree.Diff(tree)
	if err != nil {
		return nil, err
	}

	var out []okfChange
	for _, ch := range changes {
		from, to, err := ch.Files()
		if err != nil {
			return nil, err
		}
		if to == nil {
			continue // deletion — not a creation/update event
		}
		// ch.To.Name is the full tree path (e.g. "kb/decisions/.../x.md");
		// to.Name from Files() is only the basename, so it cannot be used for
		// the ontology-prefix filter. from == nil means the path was absent
		// from the parent tree — an Insert, i.e. a Creation.
		if change, ok := okfChangeFromFile(ch.To.Name, from == nil, func() (string, error) { return to.Contents() }); ok {
			out = append(out, change)
		}
	}
	return out, nil
}

// okfChangeFromFile builds an okfChange for a kb/*.md path, reading the fact's
// title and its per-revision snapshot (confidence, body digest, ref count)
// best-effort. created reports whether the path was newly added by the commit.
// It returns ok=false for paths outside the ontology or non-.md files.
//
// The body is reduced to a short digest rather than retained: the History
// deltas only ever ask whether the body CHANGED, so holding revision bodies
// for a whole corpus would be pure waste.
func okfChangeFromFile(name string, created bool, contents func() (string, error)) (okfChange, bool) {
	if !strings.HasPrefix(name, okfOntologyRoot+"/") || !strings.HasSuffix(name, ".md") {
		return okfChange{}, false
	}
	ch := okfChange{path: name, created: created}
	if content, err := contents(); err == nil {
		if f, err := fact.ParseFact(name, content); err == nil {
			ch.title = f.Title
			ch.confidence = f.Confidence
			ch.refCount = len(f.Refs)
			sum := sha256.Sum256([]byte(f.Body))
			ch.bodyDigest = hex.EncodeToString(sum[:8])
		}
	}
	return ch, true
}

// okfAgentEmailDomain is the address suffix knomit's own agents commit under.
const okfAgentEmailDomain = "@agents.knomit.io"

// okfOperationLabel names what a commit did, for the History line.
//
// knomit encodes the operation in the author address as
// "<agent>+<op>@agents.knomit.io". When there is no "+op" suffix, a non-agent
// address means a person committed directly.
//
// This label is DISPLAY ONLY. It must not feed generated.by or OKF's "human:"
// actor convention: it is evidence of who committed, not evidence that anyone
// reviewed the claim, and consumers derive trust tiers from the latter.
func okfOperationLabel(email string) string {
	if op := parseOperation(email); op != "" {
		return op
	}
	if email == "" || strings.HasSuffix(email, okfAgentEmailDomain) {
		return "edit"
	}
	return "human"
}

// okfIdentity is the FIXED author/committer identity for every OKF commit.
// Never derived from the machine, the branch, or the clock.
var okfIdentity = object.Signature{Name: "knomit okf-mapper", Email: "okf-mapper@knomit.io"}

// EnsureOKF regenerates the okf/<branch> ref if and only if the marker
// (sourceSHA, MapperVersion) misses, then returns the OKF commit hash.
func (s *Service) EnsureOKF(ctx context.Context, branch string) (plumbing.Hash, error) {
	tipRef, err := s.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("okf: resolve %s: %w", branch, err)
	}
	sourceSHA := tipRef.Hash()

	if cur, err := s.rh.gits.OKFMarkerGet(branch); err == nil && cur != "" {
		if parts := strings.SplitN(cur, "\n", 3); len(parts) == 3 &&
			parts[0] == sourceSHA.String() && parts[1] == fmt.Sprint(okf.MapperVersion) {
			return plumbing.NewHash(parts[2]), nil // marker hit, zero work
		}
	}

	rootSHA, err := s.RootCommit(ctx, branch)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("okf: repo id: %w", err)
	}
	repoID := rootSHA
	if len(repoID) > 12 {
		repoID = repoID[:12]
	}

	facts, err := s.okfReadFacts(ctx, sourceSHA)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	hist, err := s.okfHistory(ctx, sourceSHA)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	bundle, skips := okf.Build(okf.RepoIdentity{ID: repoID}, facts, hist.Events, okf.RenderOpts{
		Ontology: s.okfOntologyDoc(sourceSHA),
	})
	if skips.Skipped > 0 {
		// Conformance is an output invariant; log but proceed with the rest.
		for _, r := range skips.Reasons {
			s.rh.logOKFSkip(branch, r)
		}
	}
	if err := okf.Validate(bundle); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("okf: generated bundle not conformant: %w", err)
	}

	treeHash, err := s.okfWriteTree(bundle)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	// Chain onto the previous OKF commit if one exists (snapshot-chain history).
	// If that commit's tree is identical to the freshly computed one, the
	// snapshot is unchanged: return it as-is (refreshing the marker) instead of
	// chaining an identical-content commit. This keeps EnsureOKF idempotent and
	// makes regeneration at the same source reproduce the same OKF commit SHA,
	// which a bare parent chain would otherwise break.
	var parents []plumbing.Hash
	if prev, err := s.rh.gits.Reference(plumbing.NewBranchReferenceName("okf/" + branch)); err == nil {
		prevCommit, err := object.GetCommit(s.rh.gits, prev.Hash())
		if err != nil {
			// The ref exists but its commit object won't load (corrupted or
			// dangling ref): fail loudly instead of silently minting an
			// orphan commit, which would break the snapshot-chain invariant.
			return plumbing.ZeroHash, fmt.Errorf("okf: load previous okf commit for %s: %w", branch, err)
		}
		if prevCommit.TreeHash == treeHash {
			if err := s.rh.gits.OKFMarkerSet(branch,
				fmt.Sprintf("%s\n%d\n%s", sourceSHA.String(), okf.MapperVersion, prev.Hash().String())); err != nil {
				return plumbing.ZeroHash, err
			}
			return prev.Hash(), nil
		}
		parents = []plumbing.Hash{prev.Hash()}
	}

	// Timestamp the OKF commit from the SOURCE commit — never the clock.
	srcCommit, err := object.GetCommit(s.rh.gits, sourceSHA)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	sig := okfIdentity
	sig.When = srcCommit.Committer.When.UTC()

	commit := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      fmt.Sprintf("okf: snapshot of %s at %s\n\nmapper-version: %d", branch, sourceSHA.String()[:12], okf.MapperVersion),
		TreeHash:     treeHash,
		ParentHashes: parents,
	}
	obj := s.rh.gits.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	commitHash, err := s.rh.gits.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	if err := s.rh.gits.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName("okf/"+branch), commitHash),
	); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := s.rh.gits.OKFMarkerSet(branch,
		fmt.Sprintf("%s\n%d\n%s", sourceSHA.String(), okf.MapperVersion, commitHash.String())); err != nil {
		return plumbing.ZeroHash, err
	}
	return commitHash, nil
}

// okfWriteTree writes every bundle file as a blob and builds the nested tree
// objects bottom-up, returning the root tree hash. Deterministic: entries are
// sorted git-canonically, so identical bundles yield an identical root tree
// SHA and go-git's reader can locate every path.
func (s *Service) okfWriteTree(b okf.Bundle) (plumbing.Hash, error) {
	// dir -> tree entries; built by writing blobs first, then folding up.
	type node struct {
		files   map[string]plumbing.Hash // basename -> blob
		subdirs map[string]bool          // basename -> present
	}
	nodes := map[string]*node{"": {files: map[string]plumbing.Hash{}, subdirs: map[string]bool{}}}
	ensure := func(dir string) *node {
		if nodes[dir] == nil {
			nodes[dir] = &node{files: map[string]plumbing.Hash{}, subdirs: map[string]bool{}}
		}
		return nodes[dir]
	}

	for _, f := range b.Files {
		blob := s.rh.gits.NewEncodedObject()
		blob.SetType(plumbing.BlobObject)
		w, err := blob.Writer()
		if err != nil {
			return plumbing.ZeroHash, err
		}
		if _, err := w.Write(f.Content); err != nil {
			return plumbing.ZeroHash, err
		}
		_ = w.Close()
		bh, err := s.rh.gits.SetEncodedObject(blob)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		dir, base := splitPath(f.Path)
		ensure(dir).files[base] = bh
		// register dir chain
		for dir != "" {
			parent, self := splitPath(dir)
			ensure(parent).subdirs[self] = true
			dir = parent
		}
	}

	// Fold deepest dirs first so every child subtree hash is known before its
	// parent references it. Depth is the segment count: root "" is 0, a
	// top-level dir like "decisions" is 1. Using strings.Count("/") alone would
	// tie root and top-level dirs (both zero slashes) and, resolved by unstable
	// map order, could fold the root before its children — leaving a zero
	// subtree hash and making the output non-deterministic.
	dirs := make([]string, 0, len(nodes))
	for d := range nodes {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return okfDirDepth(dirs[i]) > okfDirDepth(dirs[j])
	})
	treeHash := map[string]plumbing.Hash{}
	for _, d := range dirs {
		n := nodes[d]
		var entries []object.TreeEntry
		for base, bh := range n.files {
			entries = append(entries, object.TreeEntry{Name: base, Mode: filemode.Regular, Hash: bh})
		}
		for base := range n.subdirs {
			child := base
			if d != "" {
				child = d + "/" + base
			}
			entries = append(entries, object.TreeEntry{Name: base, Mode: filemode.Dir, Hash: treeHash[child]})
		}
		// Git's canonical tree order compares entry names as if directory
		// names had a trailing "/". go-git's tree reader depends on this
		// ordering to locate entries, so sort accordingly (not plain byte
		// order on the bare name).
		sort.Slice(entries, func(i, j int) bool {
			return okfTreeEntryKey(entries[i]) < okfTreeEntryKey(entries[j])
		})
		tree := &object.Tree{Entries: entries}
		obj := s.rh.gits.NewEncodedObject()
		if err := tree.Encode(obj); err != nil {
			return plumbing.ZeroHash, err
		}
		th, err := s.rh.gits.SetEncodedObject(obj)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		treeHash[d] = th
	}
	return treeHash[""], nil
}

// okfTreeEntryKey returns the sort key git uses for a tree entry: the entry
// name, with a trailing "/" appended for directories. This makes "foo" (file)
// and "foo" (dir) order as git canonicalizes them.
func okfTreeEntryKey(e object.TreeEntry) string {
	if e.Mode == filemode.Dir {
		return e.Name + "/"
	}
	return e.Name
}

// okfDirDepth is the number of path segments in a bundle directory: root ""
// is 0, "decisions" is 1, "decisions/okf" is 2.
func okfDirDepth(d string) int {
	if d == "" {
		return 0
	}
	return strings.Count(d, "/") + 1
}

func splitPath(p string) (dir, base string) {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
}

// OKFTarball ensures the okf/<branch> bundle exists, then streams it as a
// gzipped tarball to w. The tar entries use fixed mode/mtime for determinism.
// Both the tar and gzip writers are closed explicitly (tar first, to flush
// its footer into the gzip stream, then gzip) rather than via defer, so a
// finalize failure on either is surfaced instead of silently returning a
// truncated/corrupt stream as success.
func (s *Service) OKFTarball(ctx context.Context, branch string, w io.Writer) (err error) {
	if _, err := s.EnsureOKF(ctx, branch); err != nil {
		return err
	}
	ref, err := s.rh.gits.Reference(plumbing.NewBranchReferenceName("okf/" + branch))
	if err != nil {
		return ErrBranchNotFound
	}
	commit, err := object.GetCommit(s.rh.gits, ref.Hash())
	if err != nil {
		return err
	}
	tree, err := commit.Tree()
	if err != nil {
		return err
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	if walkErr := tree.Files().ForEach(func(f *object.File) error {
		content, err := f.Contents()
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:    f.Name,
			Mode:    0o644,
			Size:    int64(len(content)),
			ModTime: commit.Committer.When.UTC(), // from source, not clock
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = tw.Write([]byte(content))
		return err
	}); walkErr != nil {
		_ = tw.Close()
		_ = gz.Close()
		return walkErr
	}

	if cerr := tw.Close(); cerr != nil { // flush tar footer
		_ = gz.Close()
		return cerr
	}
	return gz.Close() // flush gzip footer
}

// logOKFSkip records a fact that could not be mapped into the OKF bundle.
// Skips are non-fatal: conformance is an output invariant, so the fact is
// dropped and the rest of the bundle proceeds.
func (rh *repoHandler) logOKFSkip(branch, reason string) {
	log.Warn().Str("branch", branch).Str("reason", reason).Msg("okf: skipped non-conformable fact")
}
