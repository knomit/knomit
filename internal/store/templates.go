package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/fact"
)

// F24 template reads. Like SkillIndex, a TemplateIndex reads ONE commit's own
// tree and the caller chooses the commit: the repos layer passes the tip of
// the source repo's consensus branch (UpstreamTip(UpstreamBranch())), never
// its agent branch or an experiment.

// Template refusals found while reading a template's folder. Each is a
// property of the template's content, not of the request.
var (
	// ErrTemplateNotFound: no folder fact.TemplatesDir/<name>/ at the commit.
	ErrTemplateNotFound = errors.New("template not found")
	// ErrTemplateNotRegular: the folder holds a symlink, a submodule or an
	// executable file. Only plain files (mode 100644) are copied: anything
	// else would not land in the new repo as it is in the template.
	ErrTemplateNotRegular = errors.New("template holds a file that is not a plain file (symlink, submodule or executable)")
	// ErrTemplateTooLarge: more than fact.TemplateMaxFiles files or
	// fact.TemplateMaxBytes bytes. Refused, never truncated.
	ErrTemplateTooLarge = errors.New("template is too large")
	// ErrTemplateLayout: a path the template may not carry
	// (fact.TemplatePathAllowed), or two paths differing only by case.
	ErrTemplateLayout = errors.New("template holds a path outside README.md and .knomit/")
)

// TemplateIndex reads templates out of one commit's tree.
type TemplateIndex interface {
	// TemplateFilesAt reads every file of template name at commit, keyed by
	// its path relative to the template folder (the path it lands at),
	// refusing (ErrTemplate*) a missing folder, a file that is not plain, a
	// path the layout forbids, and a template over the limits. The walk
	// stops at the first refusal; no partial result is returned.
	TemplateFilesAt(ctx context.Context, commit plumbing.Hash, name string) ([]TemplateFile, error)
	// TemplateExistsAt reports whether template name's folder exists at
	// commit.
	TemplateExistsAt(ctx context.Context, commit plumbing.Hash, name string) (bool, error)
	// TemplateFactsAt reads every direct child <root>/templates/<folder>/*.md
	// at commit (the candidate describing facts), sorted by path. Absent is
	// empty.
	TemplateFactsAt(ctx context.Context, commit plumbing.Hash, ontologyRoot string) ([]TemplateFactFile, error)
}

// TemplateFile is one file of a template.
type TemplateFile struct {
	Path string // relative to the template folder, slash-separated
	Blob string
	Data []byte
}

// TemplateFactFile is one candidate describing fact.
type TemplateFactFile struct {
	Folder string // the template folder name it sits under
	Path   string // the fact's repo path
	Data   []byte
}

// Templates returns the template reader of this store.
func (s *Service) Templates() TemplateIndex { return s.rh }

func (rh *repoHandler) treeAt(commit plumbing.Hash, dir string) (*object.Tree, error) {
	c, err := rh.repo.CommitObject(commit)
	if err != nil {
		return nil, fmt.Errorf("templates: commit %s: %w", commit, err)
	}
	root, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("templates: tree of %s: %w", commit, err)
	}
	t, err := root.Tree(dir)
	if errors.Is(err, object.ErrDirectoryNotFound) || errors.Is(err, object.ErrEntryNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("templates: %s at %s: %w", dir, commit, err)
	}
	return t, nil
}

// TemplateExistsAt implements TemplateIndex.
func (rh *repoHandler) TemplateExistsAt(ctx context.Context, commit plumbing.Hash, name string) (bool, error) {
	if !fact.ValidTemplateName(name) {
		return false, nil
	}
	t, err := rh.treeAt(commit, fact.TemplateFolder(name))
	return t != nil, err
}

// TemplateFilesAt implements TemplateIndex.
func (rh *repoHandler) TemplateFilesAt(ctx context.Context, commit plumbing.Hash, name string) ([]TemplateFile, error) {
	if !fact.ValidTemplateName(name) {
		return nil, fmt.Errorf("%w: invalid template name %q", ErrTemplateNotFound, name)
	}
	t, err := rh.treeAt(commit, fact.TemplateFolder(name))
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("%w: %s at %s", ErrTemplateNotFound, fact.TemplateFolder(name), commit)
	}
	w := &templateWalk{rh: rh, seen: map[string]string{}}
	if err := w.walk(ctx, t, ""); err != nil {
		return nil, err
	}
	sort.Slice(w.out, func(i, j int) bool { return w.out[i].Path < w.out[j].Path })
	return w.out, nil
}

type templateWalk struct {
	rh    *repoHandler
	out   []TemplateFile
	total int64
	seen  map[string]string // lowercased path -> path, for case duplicates
}

func (w *templateWalk) walk(ctx context.Context, t *object.Tree, prefix string) error {
	for _, e := range t.Entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := path.Join(prefix, e.Name)
		switch e.Mode {
		case filemode.Dir:
			sub, err := w.rh.repo.TreeObject(e.Hash)
			if err != nil {
				return fmt.Errorf("templates: folder %s: %w", rel, err)
			}
			if err := w.walk(ctx, sub, rel); err != nil {
				return err
			}
			continue
		case filemode.Regular, filemode.Deprecated:
		default:
			return fmt.Errorf("%w: %s (mode %s)", ErrTemplateNotRegular, rel, e.Mode)
		}
		if !fact.TemplatePathAllowed(rel) {
			return fmt.Errorf("%w: %s", ErrTemplateLayout, rel)
		}
		if other, dup := w.seen[strings.ToLower(rel)]; dup {
			return fmt.Errorf("%w: %s and %s differ only by case", ErrTemplateLayout, other, rel)
		}
		w.seen[strings.ToLower(rel)] = rel
		if len(w.out)+1 > fact.TemplateMaxFiles {
			return fmt.Errorf("%w: more than %d files", ErrTemplateTooLarge, fact.TemplateMaxFiles)
		}
		blob, err := w.rh.repo.BlobObject(e.Hash)
		if err != nil {
			return fmt.Errorf("templates: blob %s: %w", rel, err)
		}
		if w.total+blob.Size > fact.TemplateMaxBytes {
			return fmt.Errorf("%w: more than %d bytes", ErrTemplateTooLarge, fact.TemplateMaxBytes)
		}
		w.total += blob.Size
		r, err := blob.Reader()
		if err != nil {
			return fmt.Errorf("templates: read %s: %w", rel, err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			return fmt.Errorf("templates: read %s: %w", rel, err)
		}
		w.out = append(w.out, TemplateFile{Path: rel, Blob: e.Hash.String(), Data: data})
	}
	return nil
}

// TemplateFactsAt implements TemplateIndex.
func (rh *repoHandler) TemplateFactsAt(ctx context.Context, commit plumbing.Hash, ontologyRoot string) ([]TemplateFactFile, error) {
	dir := fact.TemplateFactsDir
	if r := strings.Trim(ontologyRoot, "/"); r != "" {
		dir = r + "/" + dir
	}
	t, err := rh.treeAt(commit, dir)
	if err != nil || t == nil {
		return nil, err
	}
	var out []TemplateFactFile
	for _, e := range t.Entries {
		if e.Mode != filemode.Dir || !fact.ValidTemplateName(e.Name) {
			continue
		}
		sub, err := rh.repo.TreeObject(e.Hash)
		if err != nil {
			return nil, fmt.Errorf("templates: folder %s/%s: %w", dir, e.Name, err)
		}
		for _, fe := range sub.Entries {
			if !fe.Mode.IsFile() || !strings.HasSuffix(fe.Name, ".md") {
				continue
			}
			blob, err := rh.repo.BlobObject(fe.Hash)
			if err != nil {
				return nil, fmt.Errorf("templates: blob %s/%s/%s: %w", dir, e.Name, fe.Name, err)
			}
			r, err := blob.Reader()
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(r)
			r.Close()
			if err != nil {
				return nil, err
			}
			out = append(out, TemplateFactFile{Folder: e.Name, Path: dir + "/" + e.Name + "/" + fe.Name, Data: data})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// ErrSystemTreePath refuses a WriteSystemTree path outside the template
// layout (fact.TemplatePathAllowed).
var ErrSystemTreePath = errors.New("system tree: only README.md and paths under .knomit/ may be written")

// WriteSystemTree writes files as ONE authored commit on branch, KEEPING THE
// CASE of every path. It is the door F24's create-from-template writes a
// template through: every fact door lowercases a nested path, which would
// turn .knomit/skills/x/SKILL.md into skill.md and README.md into readme.md.
//
// Because it goes around that normalization, it accepts the narrowest set
// that works and nothing else: README.md and paths under .knomit/
// (fact.TemplatePathAllowed), no two paths that differ only by case. It is
// never on the MCP surface. Otherwise it is batchWrite: read-only refused,
// validatePath, the signer taken before any object is written, the ctx
// trailers stamped, notifyCommit under the branch lock.
func (s *Service) WriteSystemTree(ctx context.Context, branch string, files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error) {
	fi := s.fi
	if fi.rh.readOnly {
		return "", nil, ErrRepoReadOnly
	}
	if len(files) == 0 {
		return "", nil, fmt.Errorf("%w: nothing to write", ErrSystemTreePath)
	}
	seen := make(map[string]string, len(files))
	for p := range files {
		if !fact.TemplatePathAllowed(p) {
			return "", nil, fmt.Errorf("%w: %q", ErrSystemTreePath, p)
		}
		if other, dup := seen[strings.ToLower(p)]; dup {
			return "", nil, fmt.Errorf("%w: %q and %q differ only by case", ErrSystemTreePath, other, p)
		}
		seen[strings.ToLower(p)] = p
		if err := validatePath(p); err != nil {
			return "", nil, fmt.Errorf("store: WriteSystemTree: %w", err)
		}
	}
	unlock := fi.rh.lockBranch(branch)
	defer unlock()
	cHash, blobHashes, err := fi.batchWriteLocked(ctx, branch, files, nil, nil, message, operation)
	if err != nil {
		return "", nil, err
	}
	if err := fi.rh.notifyCommit(ctx, branch, cHash); err != nil {
		return "", nil, err
	}
	return cHash.String(), blobHashes, nil
}
