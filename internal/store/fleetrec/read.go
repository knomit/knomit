package fleetrec

import (
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"sync"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/rs/zerolog/log"

	"knomit/internal/domain/fact"
	"knomit/internal/foundation/pki"
	"knomit/internal/store/internal/vcs"
)

// ErrNotFleet: the repository's ontology is not the fleet ontology.
var ErrNotFleet = errors.New("not a fleet repository (its ontology is not the fleet ontology)")

// Member is one member record as read from a fleet repository's tree.
type Member struct {
	fact.Member
	Path string // the record's path in the fleet repository
	Blob string // the record's blob hash at the read commit
}

// OpenGitDir opens a plain git repository: the directory itself (a bare
// repository or a work tree's root), else the enclosing work tree's .git.
func OpenGitDir(dir string) (*gogit.Repository, error) {
	repo, err := gogit.PlainOpen(dir)
	if err != nil {
		repo, err = gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{DetectDotGit: true})
	}
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dir, err)
	}
	return repo, nil
}

func ResolveCommit(repo *gogit.Repository, rev string) (*object.Commit, error) {
	h, err := repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", rev, err)
	}
	return repo.CommitObject(*h)
}

// malformedLogged is the ONE-error-per-(fleet, path, blob) latch for member
// records the gate skips: a malformed record is logged at ERROR the first time
// this process meets that exact blob, never once per run.
var malformedLogged sync.Map

// LoadMembers reads every member record under <root>/members/ in c's tree.
// A record that does not parse is skipped and logged (once per blob); it never
// fails the load.
func LoadMembers(fleetID string, c *object.Commit, root string) ([]Member, error) {
	tree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	return LoadMembersOfTree(fleetID, tree, root)
}

// LoadMembersOfTree is LoadMembers over a tree.
//
// A record counts only in its OWN folder: a record at members/<x>/... whose
// agent: is not <x> (or a file directly under members/, in no folder) is
// skipped and logged once per blob like a malformed one. Otherwise a record
// one member planted in its own folder for ANOTHER agent would make that
// agent ambiguous (two records) and lock it out, or pre-empt a joiner's
// rule A; the record-owner rule (recordOwner) refuses writing such a record,
// and this keeps one that is already on main harmless to every reader and
// to the judge.
func LoadMembersOfTree(fleetID string, tree *object.Tree, root string) ([]Member, error) {
	// tree.Files() ends SILENTLY at a directory entry that is not a tree, so
	// one bogus entry would hide every record after it (and a member could
	// plant one in its own folder to make the members sorting after it
	// vanish). A malformed tree fails the load instead.
	if err := vcs.CheckTreeShape(tree, nil); err != nil {
		return nil, fmt.Errorf("fleet %s: malformed tree: %w", fleetID, err)
	}
	prefix := path.Join(root, fact.MembersTopic) + "/"
	var out []Member
	err := tree.Files().ForEach(func(f *object.File) error {
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
		if folder, _, ok := strings.Cut(strings.TrimPrefix(f.Name, prefix), "/"); !ok || folder != m.Agent {
			if _, seen := malformedLogged.LoadOrStore(fleetID+"\x00"+f.Name+"\x00"+f.Hash.String(), true); !seen {
				log.Error().Str("fleet", fleetID).Str("path", f.Name).Str("blob", f.Hash.String()).Str("agent", m.Agent).
					Msg("fleet: member record outside its agent's folder skipped")
			}
			return nil
		}
		out = append(out, Member{Member: m, Path: f.Name, Blob: f.Hash.String()})
		return nil
	})
	return out, err
}

// LoadFleet opens the fleet repository at dir, checks that its ontology (at
// rev) is the fleet ontology, and returns the member records at rev. root is the
// repository's fact root ("" = "kb").
func LoadFleet(dir, rev, root string) ([]Member, error) {
	f, err := LoadFleetAt(dir, rev, root)
	return f.Members, err
}

// LoadFleetAt is LoadFleet also returning the certificate artifacts at rev,
// read from the same commit.
func LoadFleetAt(dir, rev, root string) (Fleet, error) {
	repo, err := OpenGitDir(dir)
	if err != nil {
		return Fleet{}, fmt.Errorf("fleet: %w", err)
	}
	c, err := ResolveCommit(repo, rev)
	if err != nil {
		return Fleet{}, fmt.Errorf("fleet: %w", err)
	}
	return LoadFleetAtCommit(dir, c, root)
}

// Fleet is the judge's input from a fleet repository at ONE revision.
type Fleet struct {
	Members []Member
	PKI     *PKI // nil: the revision declares no pki: root, or holds no root file yet
}

// LoadFleetAtCommit checks that c's ontology is the fleet ontology and reads its
// member records (under <root>/members/) and the certificate artifacts its
// ontology's artifacts section declares (pki: root | crl | leaf).
func LoadFleetAtCommit(fleetID string, c *object.Commit, root string) (Fleet, error) {
	if root == "" {
		root = "kb"
	}
	raw, err := vcs.TreeOntology(c)
	if err != nil {
		return Fleet{}, fmt.Errorf("fleet: ontology: %w", err)
	}
	ont, perr := fact.ParseOntology(raw)
	if raw == nil || perr != nil || !fact.IsFleetOntology(ont) {
		return Fleet{}, ErrNotFleet
	}
	members, err := LoadMembers(fleetID, c, root)
	if err != nil {
		return Fleet{}, err
	}
	tree, err := c.Tree()
	if err != nil {
		return Fleet{}, fmt.Errorf("fleet: %w", err)
	}
	p, err := loadPKI(tree, ont)
	if err != nil {
		return Fleet{}, fmt.Errorf("fleet: %w", err)
	}
	return Fleet{Members: members, PKI: p}, nil
}

// defaultPKIFileCap bounds a declared certificate artifact other than the CRL
// that has no max_size of its own: no reader holds an unbounded blob.
const defaultPKIFileCap = 64 << 10

// crlReadBound is how much of a CRL with no max_size a reader reads. It is a
// memory guard, not a policy cap: the CRL grows with every revocation and
// never shrinks (revoked_set: superset), so the fleet ontology declares no
// max_size on it, and this bound sits far above any real fleet's CRL (an
// entry is about 40 bytes: over a million revocations).
const crlReadBound = 64 << 20

// ReadBound is how much of a declared artifact with attributes a a reader
// reads: its max_size; else crlReadBound for a CRL, defaultPKIFileCap for any
// other pki file; else (a declared file with no pki role and no cap) no
// bound.
func ReadBound(a fact.ArtifactAttrs) int64 {
	switch {
	case a.MaxSize > 0:
		return a.MaxSize
	case a.PKI == fact.PKICRL:
		return crlReadBound
	case a.PKI != "":
		return defaultPKIFileCap
	}
	return math.MaxInt64
}

// PKI is the fleet's certificate artifacts at one revision: the root
// certificate, the CRL, and the leaf for each agent id, at the paths the
// fleet's ontology declares (Layout).
//
// A file that is present but does not parse, or is larger than its declared
// max_size, never fails the load: a broken artifact on main must not refuse
// every push. It is kept as an error instead, and every rule that needs it
// fails CLOSED: a self-registration is never admitted on a root, CRL or leaf
// nobody can read, and a revocation is believed only from a CRL the root
// signed, about a leaf the root signed.
type PKI struct {
	Layout  fact.PKILayout
	Root    *x509.Certificate    // nil when RootErr is set
	RootErr error                // the root file is present but unusable
	CRL     *x509.RevocationList // nil when CRLErr is set
	CRLErr  error                // the CRL is absent, unparseable or too large
	certs   map[string]leafBlob  // <id> (the file name's '*' part, exactly) -> the leaf file
}

// leafBlob is one leaf file as read: its bytes, or why it is unusable
// (larger than its cap).
type leafBlob struct {
	raw []byte
	err error
}

// Cert returns the leaf artifact for agent id, looked up by the id as a file
// name, byte for byte, and never parsed or case folded: nil, nil when there is
// none.
func (p *PKI) Cert(id string) (*x509.Certificate, error) {
	b, ok := p.certs[id]
	if !ok {
		return nil, nil
	}
	name := p.Layout.LeafPath(id)
	if b.err != nil {
		return nil, fmt.Errorf("%s: %w", name, b.err)
	}
	c, err := ParseCertPEM(b.raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return c, nil
}

// VerifyLeaf verifies leaf against this revision's root and CRL, at a moment
// inside the leaf's own validity (never the current time: the judge is
// historical, and TLS refuses an expired certificate on every connection).
// An unusable root or CRL is its load error; a listed serial is
// pki.ErrRevoked, and only from a CRL the root signed, about a leaf the root
// signed.
func (p *PKI) VerifyLeaf(leaf *x509.Certificate) (pki.Identity, error) {
	if p.Root == nil {
		return pki.Identity{}, p.RootErr
	}
	if p.CRL == nil {
		return pki.Identity{}, p.CRLErr
	}
	at := leaf.NotBefore
	if p.Root.NotBefore.After(at) {
		at = p.Root.NotBefore
	}
	return pki.VerifyInstanceChain(leaf, nil, p.Root, p.CRL, at, pki.UsageClient)
}

// Revoked reports whether agent id is revoked at this revision: the leaf at
// its leaf path chains to the root and its serial is on the CRL the root
// signed. It is how a reader derives a member's "revoked" state (M6): the
// state is never written to a record, it is the CRL's. A missing, unreadable
// or foreign leaf, or an unusable root or CRL, is not "revoked" (nothing the
// root signed says so); the judge refuses those on its own terms (rule B).
func (p *PKI) Revoked(id string) bool {
	if p == nil {
		return false
	}
	leaf, err := p.Cert(id)
	if leaf == nil || err != nil {
		return false
	}
	_, err = p.VerifyLeaf(leaf)
	return errors.Is(err, pki.ErrRevoked)
}

// CertIDs is every agent id a leaf file is filed under (the leaf pattern's
// '*' part, as Cert looks it up), sorted; nil-safe.
func (p *PKI) CertIDs() []string {
	if p == nil {
		return nil
	}
	ids := make([]string, 0, len(p.certs))
	for id := range p.certs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// ParseCertPEM parses one CERTIFICATE PEM block.
func ParseCertPEM(raw []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(raw)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("no CERTIFICATE PEM block")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// ParseRootPEM parses a root certificate: a CA certificate.
func ParseRootPEM(raw []byte) (*x509.Certificate, error) {
	c, err := ParseCertPEM(raw)
	if err != nil {
		return nil, err
	}
	if !c.IsCA {
		return nil, errors.New("not a CA certificate")
	}
	return c, nil
}

// ParseCRLPEM parses one X509 CRL PEM block.
func ParseCRLPEM(raw []byte) (*x509.RevocationList, error) {
	blk, _ := pem.Decode(raw)
	if blk == nil || blk.Type != "X509 CRL" {
		return nil, fmt.Errorf("%w: no X509 CRL PEM block", pki.ErrCRLInvalid)
	}
	crl, err := x509.ParseRevocationList(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", pki.ErrCRLInvalid, err)
	}
	return crl, nil
}

// PKIFileCap is the read bound for a declared certificate artifact
// (ReadBound); defaultPKIFileCap when its rules cannot be resolved.
func PKIFileCap(ont *fact.Ontology, p string) int64 {
	a, err := ont.ArtifactAttrs(p)
	if err != nil {
		return defaultPKIFileCap
	}
	return ReadBound(a)
}

// errTooLarge: a declared artifact is larger than its cap.
var errTooLarge = errors.New("larger than its max_size")

// loadPKI reads the certificate artifacts ont declares from tree: nil, nil
// when ont declares no pki: root or tree holds no root file (no PKI: the
// judge's rules are what they were before the artifacts existed). The leaves
// are read as raw bytes here and parsed per author id on demand. They are
// read eagerly because a caller may judge after its read handle on the store
// is released (receive-pack's policy callback), so nothing may touch the
// store lazily. Every read is bounded by the file's cap.
func loadPKI(tree *object.Tree, ont *fact.Ontology) (*PKI, error) {
	layout, ok := ont.PKILayout()
	if !ok {
		return nil, nil
	}
	rootRaw, ok, err := TreeBytesCapped(tree, layout.Root, PKIFileCap(ont, layout.Root))
	if !ok && err == nil {
		return nil, nil
	}
	p := &PKI{Layout: layout, certs: map[string]leafBlob{}}
	switch {
	case errors.Is(err, errTooLarge):
		p.RootErr = fmt.Errorf("%s: %w", layout.Root, err)
	case err != nil:
		return nil, err
	default:
		if p.Root, err = ParseRootPEM(rootRaw); err != nil {
			p.Root, p.RootErr = nil, fmt.Errorf("%s: %w", layout.Root, err)
		}
	}
	if layout.CRL == "" {
		p.CRLErr = fmt.Errorf("%w: the ontology declares no pki: crl", pki.ErrCRLMissing)
	} else {
		crlRaw, ok, err := TreeBytesCapped(tree, layout.CRL, PKIFileCap(ont, layout.CRL))
		switch {
		case errors.Is(err, errTooLarge):
			p.CRLErr = fmt.Errorf("%w: %s: %v", pki.ErrCRLInvalid, layout.CRL, err)
		case err != nil:
			return nil, err
		case !ok:
			p.CRLErr = fmt.Errorf("%w: no %s", pki.ErrCRLMissing, layout.CRL)
		default:
			if p.CRL, err = ParseCRLPEM(crlRaw); err != nil {
				p.CRL, p.CRLErr = nil, fmt.Errorf("%s: %w", layout.CRL, err)
			}
		}
	}
	if !layout.HasLeaves() {
		return p, nil
	}
	certs, err := tree.Tree(layout.LeafDir)
	if errors.Is(err, object.ErrDirectoryNotFound) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range certs.Entries {
		id, isLeaf := layout.LeafID(e.Name)
		if !isLeaf || !e.Mode.IsFile() || e.Mode == filemode.Symlink {
			continue
		}
		full := layout.LeafDir + "/" + e.Name
		raw, _, err := TreeBytesCapped(certs, e.Name, PKIFileCap(ont, full))
		if err != nil && !errors.Is(err, errTooLarge) {
			return nil, err
		}
		p.certs[id] = leafBlob{raw: raw, err: err}
	}
	return p, nil
}

// TreeBytesCapped reads a regular file from tree: ok=false when absent. A
// symlink counts as absent: its link text is not the file. A blob larger than
// limit is errTooLarge and is not read.
func TreeBytesCapped(tree *object.Tree, p string, limit int64) ([]byte, bool, error) {
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
	if f.Size > limit {
		return nil, true, fmt.Errorf("%w (%d > %d bytes)", errTooLarge, f.Size, limit)
	}
	s, err := f.Contents()
	if err != nil {
		return nil, false, err
	}
	return []byte(s), true, nil
}
