package store

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
)

// ErrNotFleet: the repository's ontology is not the fleet preset.
var ErrNotFleet = errors.New("not a fleet repository (its ontology is not the fleet preset)")

// FleetMember is one member record as read from a fleet repository's tree.
type FleetMember struct {
	fact.Member
	Path string // the record's path in the fleet repository
	Blob string // the record's blob hash at the read commit
}

// openGitDir opens a plain git repository: the directory itself (a bare
// repository or a work tree's root), else the enclosing work tree's .git.
func openGitDir(dir string) (*gogit.Repository, error) {
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		repo, err = gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{DetectDotGit: true})
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	return repo, nil
}

func resolveCommit(repo *gogit.Repository, rev string) (*object.Commit, error) {
	h, err := repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", rev, err)
	}
	return repo.CommitObject(*h)
}

// treeOntology reads the ontology file from a commit's tree, trying every
// ontology path newest first. (nil, nil) when the tree has none.
func treeOntology(c *object.Commit) ([]byte, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	for _, p := range fact.OntologyPathsNewestFirst() {
		f, err := tree.File(p)
		if errors.Is(err, object.ErrFileNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		s, err := f.Contents()
		return []byte(s), err
	}
	return nil, nil
}

// malformedLogged is the ONE-error-per-(fleet, path, blob) latch for member
// records the gate skips: a malformed record is logged at ERROR the first time
// this process meets that exact blob, never once per run.
var malformedLogged sync.Map

// loadMembers reads every member record under <root>/members/ in c's tree.
// A record that does not parse is skipped and logged (once per blob); it never
// fails the load.
func loadMembers(fleetID string, c *object.Commit, root string) ([]FleetMember, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	prefix := path.Join(root, fact.MembersTopic) + "/"
	var out []FleetMember
	err = tree.Files().ForEach(func(f *object.File) error {
		if !strings.HasPrefix(f.Name, prefix) || !strings.HasSuffix(f.Name, ".md") {
			return nil
		}
		content, err := f.Contents()
		if err != nil {
			return err
		}
		var m fact.Member
		ff, perr := fact.ParseFact(f.Name, content)
		if perr == nil {
			m, perr = fact.ParseMember(ff.Body)
		}
		if perr != nil {
			if _, seen := malformedLogged.LoadOrStore(fleetID+"\x00"+f.Name+"\x00"+f.Hash.String(), true); !seen {
				log.Error().Err(perr).Str("fleet", fleetID).Str("path", f.Name).Str("blob", f.Hash.String()).
					Msg("fleet: malformed member record skipped")
			}
			return nil
		}
		out = append(out, FleetMember{Member: m, Path: f.Name, Blob: f.Hash.String()})
		return nil
	})
	return out, err
}

// LoadFleet opens the fleet repository at dir, checks that its ontology (at
// rev) is the fleet preset, and returns the member records at rev. root is the
// repository's fact root ("" = "kb").
func LoadFleet(dir, rev, root string) ([]FleetMember, error) {
	if root == "" {
		root = "kb"
	}
	repo, err := openGitDir(dir)
	if err != nil {
		return nil, fmt.Errorf("fleet: %w", err)
	}
	c, err := resolveCommit(repo, rev)
	if err != nil {
		return nil, fmt.Errorf("fleet: %w", err)
	}
	raw, err := treeOntology(c)
	if err != nil {
		return nil, fmt.Errorf("fleet: ontology: %w", err)
	}
	ont, err := fact.ParseOntology(raw)
	if raw == nil || err != nil || !fact.IsFleetOntology(ont) {
		return nil, ErrNotFleet
	}
	return loadMembers(dir, c, root)
}

// FleetMembersAt returns the member records at branch in THIS store (a
// mounted fleet repository), after checking that its ontology there is the
// fleet preset. root is the store's fact root.
func (s *Service) FleetMembersAt(branch string) ([]FleetMember, error) {
	ref, err := s.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return nil, fmt.Errorf("fleet: %s: %w", branch, err)
	}
	c, err := object.GetCommit(s.rh.gits, ref.Hash())
	if err != nil {
		return nil, fmt.Errorf("fleet: %w", err)
	}
	raw, err := treeOntology(c)
	if err != nil {
		return nil, fmt.Errorf("fleet: ontology: %w", err)
	}
	ont, perr := fact.ParseOntology(raw)
	if raw == nil || perr != nil || !fact.IsFleetOntology(ont) {
		return nil, ErrNotFleet
	}
	root := s.rh.factRoot
	if root == "" {
		root = "kb"
	}
	return loadMembers("mounted", c, root)
}
