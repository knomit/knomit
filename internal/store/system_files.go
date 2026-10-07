package store

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/fact"
)

// SystemFileIndex reads one file under fact.PrivateRoot (.knomit/) by its
// exact path, out of one commit's own tree. It is the READ side of the system
// root that the fact tools open (user ruling 2026-10-06): knomit_explain, the
// REST fact GET and the refs gate use it. Nothing here writes, lists or
// indexes: a system file is reachable by exact path only.
type SystemFileIndex interface {
	// SystemFileAt reads path at commit, or at the tip of branch when commit
	// is "". found is false when the path is absent there OR names a
	// directory — only a file is a system file. Content is read only when the
	// blob is at most maxContent bytes (pass 0 for none); a larger file comes
	// back with Truncated set and no Content.
	//
	// path must satisfy fact.IsSystemFilePath; anything else is an error, so a
	// caller cannot turn this into a reader for kb/.drafts/ or .github/.
	SystemFileAt(ctx context.Context, branch, commit, path string, maxContent int64) (sf SystemFile, found bool, err error)
}

// SystemFile is one file under fact.PrivateRoot at a commit.
type SystemFile struct {
	// Commit is the commit the file was read at (the branch tip when the
	// caller passed none).
	Commit    string
	Blob      string
	Size      int64
	Content   []byte
	Truncated bool
}

// SystemFiles returns the system-file reader of this store.
func (s *Service) SystemFiles() SystemFileIndex { return s.rh }

// SystemFileAt implements SystemFileIndex.
func (rh *repoHandler) SystemFileAt(ctx context.Context, branch, commit, path string, maxContent int64) (SystemFile, bool, error) {
	if !fact.IsSystemFilePath(path) {
		return SystemFile{}, false, fmt.Errorf("%w: system file: %q is not a file path under %s/", ErrInvalidPath, path, fact.PrivateRoot)
	}
	if commit == "" {
		head, err := rh.HeadCommit(ctx, branch)
		if err != nil {
			return SystemFile{}, false, fmt.Errorf("system file: %w", err)
		}
		commit = head
	}
	if !plumbing.IsHash(commit) {
		return SystemFile{}, false, fmt.Errorf("%w: system file: %q is not a full 40-hex commit hash", ErrInvalidPath, commit)
	}
	c, err := rh.repo.CommitObject(plumbing.NewHash(commit))
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		// No such commit here: nothing to read, which is "not found", not a
		// failure of the store.
		return SystemFile{Commit: commit}, false, nil
	}
	if err != nil {
		return SystemFile{}, false, fmt.Errorf("system file: commit %s: %w", commit, err)
	}
	tree, err := c.Tree()
	if err != nil {
		return SystemFile{}, false, fmt.Errorf("system file: tree of %s: %w", commit, err)
	}
	entry, err := tree.FindEntry(path)
	// A path that descends THROUGH a file (".knomit/x.md/y") fails in go-git
	// as ErrObjectNotFound: it looks for a tree where a blob is. That is
	// "no such file" too.
	if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) ||
		errors.Is(err, plumbing.ErrObjectNotFound) {
		return SystemFile{Commit: commit}, false, nil
	}
	if err != nil {
		return SystemFile{}, false, fmt.Errorf("system file: %q at %s: %w", path, commit, err)
	}
	if !entry.Mode.IsFile() {
		return SystemFile{Commit: commit}, false, nil
	}
	f, err := tree.TreeEntryFile(entry)
	if err != nil {
		return SystemFile{}, false, fmt.Errorf("system file: blob %q at %s: %w", path, commit, err)
	}
	sf := SystemFile{Commit: commit, Blob: entry.Hash.String(), Size: f.Size}
	if maxContent <= 0 {
		return sf, true, nil
	}
	if f.Size > maxContent {
		sf.Truncated = true
		return sf, true, nil
	}
	r, err := f.Reader()
	if err != nil {
		return SystemFile{}, false, fmt.Errorf("system file: read %q at %s: %w", path, commit, err)
	}
	defer r.Close()
	if sf.Content, err = io.ReadAll(r); err != nil {
		return SystemFile{}, false, fmt.Errorf("system file: read %q at %s: %w", path, commit, err)
	}
	return sf, true, nil
}
