package store

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/pki"
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
// ontology path newest first. (nil, nil) when the tree has none. A symlink at
// the first rung present is fact.ErrSymlinkNotFollowed naming it: never its
// link text, never "none".
func treeOntology(c *object.Commit) ([]byte, error) {
	_, data, err := treeOntologyFile(c)
	return data, err
}

// treeOntologyFile is treeOntology also returning the rung it read ("" when
// none).
func treeOntologyFile(c *object.Commit) (string, []byte, error) {
	tree, err := c.Tree()
	if err != nil {
		return "", nil, err
	}
	for _, p := range fact.OntologyPathsNewestFirst() {
		f, err := tree.File(p)
		if errors.Is(err, object.ErrFileNotFound) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		if f.Mode == filemode.Symlink {
			return "", nil, fact.SymlinkNotFollowed(p)
		}
		s, err := f.Contents()
		return p, []byte(s), err
	}
	return "", nil, nil
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
	f, err := LoadFleetAt(dir, rev, root)
	return f.Members, err
}

// LoadFleetAt is LoadFleet also returning the certificate artifacts at rev,
// read from the same commit.
func LoadFleetAt(dir, rev, root string) (Fleet, error) {
	repo, err := openGitDir(dir)
	if err != nil {
		return Fleet{}, fmt.Errorf("fleet: %w", err)
	}
	c, err := resolveCommit(repo, rev)
	if err != nil {
		return Fleet{}, fmt.Errorf("fleet: %w", err)
	}
	return loadFleetAt(dir, c, root)
}

// FleetMembersAt returns the member records at branch in THIS store (a
// mounted fleet repository), after checking that its ontology there is the
// fleet preset. root is the store's fact root.
func (s *Service) FleetMembersAt(branch string) ([]FleetMember, error) {
	f, err := s.FleetAt(branch)
	return f.Members, err
}

// FleetPKIAt returns the fleet's certificate artifacts at branch in THIS store
// (a mounted fleet repository): nil, nil when that revision holds no
// artifacts/fleet/root.crt.
func (s *Service) FleetPKIAt(branch string) (*FleetPKI, error) {
	f, err := s.FleetAt(branch)
	return f.PKI, err
}

// FleetAt returns everything the judge reads from THIS store (a mounted fleet
// repository) at branch, all from the ONE commit branch names: the member
// records and the certificate artifacts. Reading them separately could pair
// the records of one revision with the CRL of another.
func (s *Service) FleetAt(branch string) (Fleet, error) {
	ref, err := s.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return Fleet{}, fmt.Errorf("fleet: %s: %w", branch, err)
	}
	c, err := object.GetCommit(s.rh.gits, ref.Hash())
	if err != nil {
		return Fleet{}, fmt.Errorf("fleet: %w", err)
	}
	return loadFleetAt("mounted", c, s.rh.factRoot)
}

// Fleet is the judge's input from a fleet repository at ONE revision.
type Fleet struct {
	Members []FleetMember
	PKI     *FleetPKI // nil: the revision holds no artifacts/fleet/root.crt
}

// loadFleetAt checks that c's ontology is the fleet preset and reads its
// member records (under <root>/members/) and certificate artifacts.
func loadFleetAt(fleetID string, c *object.Commit, root string) (Fleet, error) {
	if root == "" {
		root = "kb"
	}
	raw, err := treeOntology(c)
	if err != nil {
		return Fleet{}, fmt.Errorf("fleet: ontology: %w", err)
	}
	ont, perr := fact.ParseOntology(raw)
	if raw == nil || perr != nil || !fact.IsFleetOntology(ont) {
		return Fleet{}, ErrNotFleet
	}
	members, err := loadMembers(fleetID, c, root)
	if err != nil {
		return Fleet{}, err
	}
	p, err := loadFleetPKI(c)
	if err != nil {
		return Fleet{}, fmt.Errorf("fleet: %w", err)
	}
	return Fleet{Members: members, PKI: p}, nil
}

// The fleet's certificate artifacts (D7): plain files on the fleet's main,
// written by the fleet root's holder. Repo-root paths, not under the fact root.
const (
	fleetRootCertPath = fact.ArtifactsRoot + "/fleet/root.crt"
	fleetCRLPath      = fact.ArtifactsRoot + "/fleet/crl.pem"
	fleetCertsDir     = fact.ArtifactsRoot + "/fleet/certs"
)

// FleetPKI is the fleet's certificate artifacts at one revision: the root
// certificate, the CRL, and the leaf for each agent id at
// artifacts/fleet/certs/<id>.pem.
//
// A file that is present but does not parse never fails the load: a broken
// artifact on main must not refuse every push, including the one that fixes
// it. It is kept as an error instead, and every rule that needs it fails
// CLOSED: a self-registration is never admitted on a root, CRL or leaf
// nobody can read, and a revocation is believed only from a CRL the root
// signed, about a leaf the root signed.
type FleetPKI struct {
	Root    *x509.Certificate    // nil when RootErr is set
	RootErr error                // root.crt present but unusable
	CRL     *x509.RevocationList // nil when CRLErr is set
	CRLErr  error                // crl.pem absent or unparseable
	certs   map[string][]byte    // lowercased <id> -> raw PEM (nil: two spellings of one id)
}

// Cert returns the leaf artifact for agent id, looked up by the id as a file
// name and never parsed: nil, nil when there is none.
func (p *FleetPKI) Cert(id string) (*x509.Certificate, error) {
	raw, ok := p.certs[strings.ToLower(id)]
	if !ok {
		return nil, nil
	}
	name := fleetCertsDir + "/" + id + ".pem"
	if raw == nil {
		return nil, fmt.Errorf("%s: more than one file for this id (case differs)", name)
	}
	blk, _ := pem.Decode(raw)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("%s: no CERTIFICATE PEM block", name)
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return c, nil
}

// loadFleetPKI reads the certificate artifacts from c's tree: nil, nil when
// there is no root.crt (no PKI: the judge's rules are what they were before
// the artifacts existed). The leaves are read as raw bytes here and parsed
// per author id on demand. They are read eagerly because a caller may judge
// after its read handle on the store is released (receive-pack's policy
// callback), so nothing may touch the store lazily; the cost is the same
// order as the member records, which are all read and parsed already.
func loadFleetPKI(c *object.Commit) (*FleetPKI, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	rootRaw, ok, err := treeBytes(tree, fleetRootCertPath)
	if err != nil || !ok {
		return nil, err
	}
	p := &FleetPKI{certs: map[string][]byte{}}
	if blk, _ := pem.Decode(rootRaw); blk == nil || blk.Type != "CERTIFICATE" {
		p.RootErr = fmt.Errorf("%s: no CERTIFICATE PEM block", fleetRootCertPath)
	} else if rc, perr := x509.ParseCertificate(blk.Bytes); perr != nil {
		p.RootErr = fmt.Errorf("%s: %w", fleetRootCertPath, perr)
	} else if !rc.IsCA {
		p.RootErr = fmt.Errorf("%s is not a CA certificate", fleetRootCertPath)
	} else {
		p.Root = rc
	}
	crlRaw, ok, err := treeBytes(tree, fleetCRLPath)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		p.CRLErr = fmt.Errorf("%w: no %s", pki.ErrCRLMissing, fleetCRLPath)
	default:
		if blk, _ := pem.Decode(crlRaw); blk == nil || blk.Type != "X509 CRL" {
			p.CRLErr = fmt.Errorf("%w: %s: no X509 CRL PEM block", pki.ErrCRLInvalid, fleetCRLPath)
		} else if crl, perr := x509.ParseRevocationList(blk.Bytes); perr != nil {
			p.CRLErr = fmt.Errorf("%w: %s: %v", pki.ErrCRLInvalid, fleetCRLPath, perr)
		} else {
			p.CRL = crl
		}
	}
	certs, err := tree.Tree(fleetCertsDir)
	if errors.Is(err, object.ErrDirectoryNotFound) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range certs.Entries {
		id, isPEM := strings.CutSuffix(e.Name, ".pem")
		if !isPEM || id == "" || !e.Mode.IsFile() || e.Mode == filemode.Symlink {
			continue
		}
		raw, _, err := treeBytes(certs, e.Name)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(id)
		if _, dup := p.certs[key]; dup {
			p.certs[key] = nil // two spellings of one id: neither is believed
			continue
		}
		p.certs[key] = raw
	}
	return p, nil
}

// treeBytes reads a regular file from tree: ok=false when absent. A symlink
// counts as absent: its link text is not the file.
func treeBytes(tree *object.Tree, p string) ([]byte, bool, error) {
	f, err := tree.File(p)
	if errors.Is(err, object.ErrFileNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if f.Mode == filemode.Symlink {
		return nil, false, nil
	}
	s, err := f.Contents()
	if err != nil {
		return nil, false, err
	}
	return []byte(s), true, nil
}

// FleetKeysOf returns every key agentID's member record has held in this
// store's (a mounted fleet repository's) history of branch: the record's
// VERSIONS, which is where an agent's key history lives. Malformed versions
// are skipped.
func (s *Service) FleetKeysOf(branch, agentID string) ([]ssh.PublicKey, error) {
	ref, err := s.rh.gits.Reference(plumbing.NewBranchReferenceName(branch))
	if err != nil {
		return nil, fmt.Errorf("fleet: %s: %w", branch, err)
	}
	root := s.rh.factRoot
	if root == "" {
		root = "kb"
	}
	var keys []ssh.PublicKey
	seen := map[string]bool{}
	err = walkHistory(s.rh.gits, ref.Hash(), nil, func(c *object.Commit) {
		ms, lerr := loadMembers("mounted", c, root)
		if lerr != nil {
			return
		}
		for _, m := range ms {
			k := string(m.Key.Marshal())
			if strings.EqualFold(m.Agent, agentID) && !seen[k] {
				seen[k] = true
				keys = append(keys, m.Key)
			}
		}
	})
	return keys, err
}
