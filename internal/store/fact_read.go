package store

import (
	"context"
	"fmt"
	"io"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// treeFileInsensitive walks a git tree matching each path component
// case-insensitively and returns the file contents.
func treeFileInsensitive(repo *gogit.Repository, tree *object.Tree, path string) (string, error) {
	parts := strings.Split(path, "/")
	cur := tree
	for i, part := range parts {
		lower := strings.ToLower(part)
		var matched *object.TreeEntry
		for j := range cur.Entries {
			if strings.ToLower(cur.Entries[j].Name) == lower {
				matched = &cur.Entries[j]
				break
			}
		}
		if matched == nil {
			return "", fmt.Errorf("component %q not found", part)
		}
		if i == len(parts)-1 {
			blob, err := repo.BlobObject(matched.Hash)
			if err != nil {
				return "", err
			}
			r, err := blob.Reader()
			if err != nil {
				return "", err
			}
			defer r.Close()
			b, err := io.ReadAll(r)
			return string(b), err
		}
		sub, err := repo.TreeObject(matched.Hash)
		if err != nil {
			return "", fmt.Errorf("subtree %q: %w", part, err)
		}
		cur = sub
	}
	return "", fmt.Errorf("empty path")
}

// ReadFileLastCommit finds the most recent ancestor of beforeCommitHash where
// path existed and returns its content and commit hash. Used to read facts
// that were deleted in beforeCommitHash (e.g. retract commits).
func (fi *factIndex) readFileLastCommit(ctx context.Context, branch, path, beforeCommitHash string) (content string, fromCommit string, err error) {
	path = strings.ToLower(path)
	startHash := plumbing.NewHash(beforeCommitHash)
	startCommit, err := fi.rh.repo.CommitObject(startHash)
	if err != nil {
		return "", "", fmt.Errorf("readFileLastCommit: commit: %w", err)
	}
	if len(startCommit.ParentHashes) == 0 {
		return "", "", fmt.Errorf("readFileLastCommit: %q: commit has no parents", path)
	}

	logIter, err := fi.rh.repo.Log(&gogit.LogOptions{
		From:     startCommit.ParentHashes[0],
		FileName: &path,
		Order:    gogit.LogOrderCommitterTime,
	})
	if err != nil {
		return "", "", fmt.Errorf("readFileLastCommit: log: %w", err)
	}
	defer logIter.Close()

	lastCommit, err := logIter.Next()
	if err != nil {
		return "", "", fmt.Errorf("readFileLastCommit: %q: no prior commit found", path)
	}

	content, err = fi.rh.readFileAtCommitHash(ctx, path, lastCommit.Hash.String())
	return content, lastCommit.Hash.String(), err
}

// FileExists returns true if path exists at the tip of branch, false+nil if not found.
func (fi *factIndex) fileExists(ctx context.Context, branch, path string) (bool, error) {
	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		return false, fmt.Errorf("fileExists: ref: %w", err)
	}

	commit, err := fi.rh.repo.CommitObject(headHash)
	if err != nil {
		return false, fmt.Errorf("fileExists: commit: %w", err)
	}

	tree, err := commit.Tree()
	if err != nil {
		return false, fmt.Errorf("fileExists: tree: %w", err)
	}

	_, err = tree.FindEntry(path)
	if err == object.ErrEntryNotFound || err == object.ErrDirectoryNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("fileExists: find entry: %w", err)
	}
	return true, nil
}

// ReadFact reads a fact from the store. With nil opts it reads from branch HEAD.
func (fi *factIndex) ReadFact(ctx context.Context, branch, path string, opts *ReadFactOpts) (ReadFactResult, error) {
	if opts == nil {
		opts = &ReadFactOpts{}
	}
	switch {
	case opts.BeforeCommit != "":
		content, fromCommit, err := fi.readFileLastCommit(ctx, branch, path, opts.BeforeCommit)
		if err != nil {
			return ReadFactResult{}, err
		}
		return ReadFactResult{Content: content, FromCommit: fromCommit}, nil
	case opts.AtCommit != "":
		content, err := fi.rh.readFileAtCommit(ctx, branch, path, opts.AtCommit)
		if err != nil {
			return ReadFactResult{}, err
		}
		return ReadFactResult{Content: content}, nil
	case opts.WithHash:
		content, blobHash, err := fi.rh.readFileWithHash(ctx, branch, path)
		if err != nil {
			return ReadFactResult{}, err
		}
		return ReadFactResult{Content: content, BlobHash: blobHash}, nil
	default:
		content, err := fi.rh.readFile(ctx, branch, path)
		if err != nil {
			return ReadFactResult{}, err
		}
		return ReadFactResult{Content: content}, nil
	}
}

// FactExists returns true if a fact exists at path on branch HEAD.
func (fi *factIndex) FactExists(ctx context.Context, branch, path string) (bool, error) {
	return fi.fileExists(ctx, branch, path)
}

// ListDir returns entries under path at the tip of branch.
// Subdirectories have IsDir=true, .md files have IsDir=false.
func (fi *factIndex) ListDir(ctx context.Context, branch, path string) ([]DirEntry, error) {
	path = strings.ToLower(path)
	headHash, err := fi.rh.resolveRef(ctx, branch)
	if err != nil {
		return nil, fmt.Errorf("ListDir: ref: %w", err)
	}

	commit, err := fi.rh.repo.CommitObject(headHash)
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

// ListAllWithHash returns all .md files at the tip of branch with their blob hashes.
// Single tree walk — no per-file I/O.
func (fi *factIndex) ListAllWithHash(ctx context.Context, branch string) ([]string, []string, error) {
	return fi.rh.ListAllWithHash(ctx, branch)
}

// ListAll returns paths of all .md files at the tip of branch.
func (fi *factIndex) ListAll(ctx context.Context, branch string) ([]string, error) {
	return fi.rh.ListAll(ctx, branch)
}

