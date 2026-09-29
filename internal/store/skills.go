package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/fact"
)

// SkillIndex reads skills (fact.SkillsDir) out of one commit's own tree. The
// caller chooses the commit: the MCP layer passes the tip of the consensus
// branch (UpstreamTip(UpstreamBranch()), ruling D2), never the agent branch.
// The rules are privateFileAt's: absent is empty, never an error; a path that
// exists but cannot be read IS an error.
type SkillIndex interface {
	// SkillsAt lists every folder under fact.SkillsDir that holds a
	// SKILL.md file, sorted by folder name, with that file's blob hash and
	// content. A folder without one is not a skill and is not listed. No
	// skills folder at all is an empty list.
	SkillsAt(ctx context.Context, commit plumbing.Hash) ([]SkillEntry, error)
	// SkillFilesAt lists skill name's bundled files — every file under its
	// folder, recursively, except the top-level SKILL.md — sorted by path.
	// Contents are read only when asked for.
	SkillFilesAt(ctx context.Context, commit plumbing.Hash, name string) ([]SkillBundledFile, error)
}

// SkillEntry is one skill folder's SKILL.md at a commit.
type SkillEntry struct {
	// Dir is the folder name under fact.SkillsDir.
	Dir  string
	Blob string
	Data []byte
}

// SkillBundledFile is one bundled file of a skill.
type SkillBundledFile struct {
	// Path is relative to the skill's folder, slash-separated.
	Path string
	Blob string
	Size int64
	file *object.File
}

// Contents reads the file's bytes.
func (f SkillBundledFile) Contents() ([]byte, error) {
	if f.file == nil {
		return nil, fmt.Errorf("skills: %s: no blob", f.Path)
	}
	r, err := f.file.Reader()
	if err != nil {
		return nil, fmt.Errorf("skills: %s: %w", f.Path, err)
	}
	defer r.Close()
	return io.ReadAll(r)
}

// Skills returns the skill reader of this store.
func (s *Service) Skills() SkillIndex { return s.rh }

// skillsTree is fact.SkillsDir at commit, or nil when the commit has none.
func (rh *repoHandler) skillsTree(commit plumbing.Hash) (*object.Tree, error) {
	c, err := rh.repo.CommitObject(commit)
	if err != nil {
		return nil, fmt.Errorf("skills: commit %s: %w", commit, err)
	}
	root, err := c.Tree()
	if err != nil {
		return nil, fmt.Errorf("skills: tree of %s: %w", commit, err)
	}
	t, err := root.Tree(fact.SkillsDir)
	if errors.Is(err, object.ErrDirectoryNotFound) || errors.Is(err, object.ErrEntryNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("skills: %s at %s: %w", fact.SkillsDir, commit, err)
	}
	return t, nil
}

// skillFileEntry is the folder's SKILL.md entry, or nil. The name is matched
// without case, preferring the exact spelling: every knomit write door
// lowercases a nested path (fact.NormalizePath), so a skill authored through
// knomit itself lands as `skill.md`, while one committed with git keeps
// `SKILL.md`. Both are the same skill.
func skillFileEntry(sub *object.Tree) *object.TreeEntry {
	var folded *object.TreeEntry
	for i := range sub.Entries {
		e := &sub.Entries[i]
		if !e.Mode.IsFile() || !strings.EqualFold(e.Name, fact.SkillFileName) {
			continue
		}
		if e.Name == fact.SkillFileName {
			return e
		}
		if folded == nil {
			folded = e
		}
	}
	return folded
}

// SkillsAt implements SkillIndex.
func (rh *repoHandler) SkillsAt(ctx context.Context, commit plumbing.Hash) ([]SkillEntry, error) {
	t, err := rh.skillsTree(commit)
	if err != nil || t == nil {
		return nil, err
	}
	var out []SkillEntry
	for _, e := range t.Entries {
		if e.Mode.IsFile() {
			continue // a loose file directly under skills/ is not a skill
		}
		sub, err := t.Tree(e.Name)
		if err != nil {
			return nil, fmt.Errorf("skills: folder %q at %s: %w", e.Name, commit, err)
		}
		se := skillFileEntry(sub)
		if se == nil {
			continue
		}
		f, err := sub.TreeEntryFile(se)
		if err != nil {
			return nil, fmt.Errorf("skills: %s/%s blob at %s: %w", e.Name, fact.SkillFileName, commit, err)
		}
		body, err := f.Contents()
		if err != nil {
			return nil, fmt.Errorf("skills: %s/%s contents at %s: %w", e.Name, fact.SkillFileName, commit, err)
		}
		out = append(out, SkillEntry{Dir: e.Name, Blob: se.Hash.String(), Data: []byte(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out, nil
}

// SkillFilesAt implements SkillIndex.
func (rh *repoHandler) SkillFilesAt(ctx context.Context, commit plumbing.Hash, name string) ([]SkillBundledFile, error) {
	if !fact.ValidSkillName(name) {
		return nil, fmt.Errorf("skills: invalid skill name %q", name)
	}
	t, err := rh.skillsTree(commit)
	if err != nil || t == nil {
		return nil, err
	}
	sub, err := t.Tree(name)
	if errors.Is(err, object.ErrDirectoryNotFound) || errors.Is(err, object.ErrEntryNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("skills: folder %q at %s: %w", name, commit, err)
	}
	skip := ""
	if se := skillFileEntry(sub); se != nil {
		skip = se.Name
	}
	var out []SkillBundledFile
	err = sub.Files().ForEach(func(f *object.File) error {
		if f.Name == skip || !f.Mode.IsFile() {
			return nil
		}
		out = append(out, SkillBundledFile{Path: strings.TrimPrefix(f.Name, "/"), Blob: f.Hash.String(), Size: f.Size, file: f})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("skills: files of %q at %s: %w", name, commit, err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
