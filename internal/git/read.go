// Read-only operations on the git store: file reads, directory listings, log,
// grep, and diffing. None of these methods modify the repository.
package git

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// ReadFileWithHash returns both the file content and the blob hash for the given path.
func (s *Store) ReadFileWithHash(path string) (string, string, error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return "", "", fmt.Errorf("ReadFileWithHash: head: %w", err)
	}
	commit, err := s.repo.CommitObject(headRef.Hash())
	if err != nil {
		return "", "", fmt.Errorf("ReadFileWithHash: commit: %w", err)
	}
	tree, err := commit.Tree()
	if err != nil {
		return "", "", fmt.Errorf("ReadFileWithHash: tree: %w", err)
	}
	entry, err := tree.FindEntry(path)
	if err != nil {
		return "", "", fmt.Errorf("ReadFileWithHash: entry %s: %w", path, err)
	}
	blob, err := s.repo.BlobObject(entry.Hash)
	if err != nil {
		return "", "", fmt.Errorf("ReadFileWithHash: blob: %w", err)
	}
	r, err := blob.Reader()
	if err != nil {
		return "", "", fmt.Errorf("ReadFileWithHash: reader: %w", err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		return "", "", fmt.Errorf("ReadFileWithHash: read: %w", err)
	}
	return string(b), entry.Hash.String(), nil
}

// ReadFile reads the content of path from the HEAD commit.
func (s *Store) ReadFile(path string) (string, error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return "", fmt.Errorf("ReadFile: head: %w", err)
	}

	commit, err := s.repo.CommitObject(headRef.Hash())
	if err != nil {
		return "", fmt.Errorf("ReadFile: commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return "", fmt.Errorf("ReadFile: tree: %w", err)
	}

	f, err := tree.File(path)
	if err != nil {
		return "", fmt.Errorf("ReadFile: file %q: %w", path, err)
	}

	return f.Contents()
}

// FileExists returns true if path exists in HEAD, false+nil if not found.
func (s *Store) FileExists(path string) (bool, error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return false, fmt.Errorf("FileExists: head: %w", err)
	}

	commit, err := s.repo.CommitObject(headRef.Hash())
	if err != nil {
		return false, fmt.Errorf("FileExists: commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return false, fmt.Errorf("FileExists: tree: %w", err)
	}

	_, err = tree.FindEntry(path)
	if err == object.ErrEntryNotFound || err == object.ErrDirectoryNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("FileExists: find entry: %w", err)
	}
	return true, nil
}

// ListDir returns entries under path in HEAD's tree.
// Subdirectories have IsDir=true, .md files have IsDir=false.
func (s *Store) ListDir(path string) ([]DirEntry, error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("ListDir: head: %w", err)
	}

	commit, err := s.repo.CommitObject(headRef.Hash())
	if err != nil {
		return nil, fmt.Errorf("ListDir: commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, fmt.Errorf("ListDir: tree: %w", err)
	}

	// Navigate to the subtree at path (use root tree directly when path is empty).
	var subtree *object.Tree
	if path == "" {
		subtree = tree
	} else {
		subtree, err = tree.Tree(path)
		if err != nil {
			return nil, fmt.Errorf("ListDir: subtree %q: %w", path, err)
		}
	}

	var entries []DirEntry
	for _, e := range subtree.Entries {
		if e.Mode == filemode.Dir {
			entries = append(entries, DirEntry{Name: e.Name, IsDir: true})
		} else if strings.HasSuffix(e.Name, ".md") {
			entries = append(entries, DirEntry{Name: e.Name, IsDir: false})
		}
		// Omit non-.md files
	}
	return entries, nil
}

// ListAll returns paths of all .md files under HEAD.
func (s *Store) ListAll() ([]string, error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("ListAll: head: %w", err)
	}

	commit, err := s.repo.CommitObject(headRef.Hash())
	if err != nil {
		return nil, fmt.Errorf("ListAll: commit: %w", err)
	}

	fileIter, err := commit.Files()
	if err != nil {
		return nil, fmt.Errorf("ListAll: files: %w", err)
	}
	defer fileIter.Close()

	var paths []string
	err = fileIter.ForEach(func(f *object.File) error {
		if strings.HasSuffix(f.Name, ".md") {
			paths = append(paths, f.Name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ListAll: iterate: %w", err)
	}

	sort.Strings(paths)
	return paths, nil
}

// Log returns log entries for commits that modified path (up to 50).
func (s *Store) Log(path string) ([]LogEntry, error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("Log: head: %w", err)
	}

	logIter, err := s.repo.Log(&gogit.LogOptions{
		From:     headRef.Hash(),
		FileName: &path,
		Order:    gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return nil, fmt.Errorf("Log: %w", err)
	}
	defer logIter.Close()

	var entries []LogEntry
	err = logIter.ForEach(func(c *object.Commit) error {
		if len(entries) >= 50 {
			return io.EOF
		}
		hash := c.Hash.String()
		if len(hash) > 8 {
			hash = hash[:8]
		}
		firstLine := c.Message
		if idx := strings.IndexByte(firstLine, '\n'); idx >= 0 {
			firstLine = firstLine[:idx]
		}
		entries = append(entries, LogEntry{
			Commit:  hash,
			Date:    c.Committer.When.UTC().Format(time.RFC3339),
			Message: firstLine,
		})
		return nil
	})
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("Log: iterate: %w", err)
	}

	return entries, nil
}

// Grep searches all .md files in HEAD for pattern, returns matching paths.
func (s *Store) Grep(pattern string) ([]string, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("Grep: compile pattern: %w", err)
	}

	headRef, err := s.repo.Head()
	if err != nil {
		return nil, fmt.Errorf("Grep: head: %w", err)
	}

	commit, err := s.repo.CommitObject(headRef.Hash())
	if err != nil {
		return nil, fmt.Errorf("Grep: commit: %w", err)
	}

	fileIter, err := commit.Files()
	if err != nil {
		return nil, fmt.Errorf("Grep: files: %w", err)
	}
	defer fileIter.Close()

	var matches []string
	err = fileIter.ForEach(func(f *object.File) error {
		if !strings.HasSuffix(f.Name, ".md") {
			return nil
		}
		content, err := f.Contents()
		if err != nil {
			return fmt.Errorf("Grep: read %q: %w", f.Name, err)
		}
		if re.MatchString(content) {
			matches = append(matches, f.Name)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("Grep: iterate: %w", err)
	}

	sort.Strings(matches)
	return matches, nil
}

// DiffFiles returns paths added/modified/deleted between fromCommit and HEAD.
// Only .md files are returned. If fromCommit is empty, diffs from empty tree.
func (s *Store) DiffFiles(fromCommit string) (added, modified, deleted []string, err error) {
	headRef, err := s.repo.Head()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: head: %w", err)
	}

	toCommit, err := s.repo.CommitObject(headRef.Hash())
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: to commit: %w", err)
	}
	toTree, err := toCommit.Tree()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: to tree: %w", err)
	}

	var fromTree *object.Tree
	if fromCommit != "" {
		fromHash := plumbing.NewHash(fromCommit)
		fc, err := s.repo.CommitObject(fromHash)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("DiffFiles: from commit: %w", err)
		}
		fromTree, err = fc.Tree()
		if err != nil {
			return nil, nil, nil, fmt.Errorf("DiffFiles: from tree: %w", err)
		}
	}

	changes, err := object.DiffTree(fromTree, toTree)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("DiffFiles: diff tree: %w", err)
	}

	for _, ch := range changes {
		from := ch.From.Name
		to := ch.To.Name

		switch {
		case from == "" && to != "":
			// Added
			if strings.HasSuffix(to, ".md") {
				added = append(added, to)
			}
		case from != "" && to == "":
			// Deleted
			if strings.HasSuffix(from, ".md") {
				deleted = append(deleted, from)
			}
		default:
			// Modified (or renamed — treat as modified for now)
			if strings.HasSuffix(to, ".md") {
				modified = append(modified, to)
			}
		}
	}

	sort.Strings(added)
	sort.Strings(modified)
	sort.Strings(deleted)
	return added, modified, deleted, nil
}
