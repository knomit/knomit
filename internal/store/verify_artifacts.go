package store

import (
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"path"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/store/fleetrec"
	"knomit/internal/store/internal/vcs"
)

// artifactRules is R7: the artifacts section of the JUDGED repository's
// ontology at main (fact.Ontology.ArtifactAttrs) applied to c. For every path
// under artifacts/ that c changes against EVERY parent (the empty tree for a
// root commit), the rules of that path must hold:
//
//   - a file with a change rule is never deleted;
//   - write-once: never modified (a first add is allowed);
//   - append-only: the new blob holds the old one as an exact byte prefix;
//   - replace: modified only when replace_when holds — crl-newer
//     (crlNewer) or leaf-renewal (leafRenewal);
//   - revoked_set: superset — a replacing CRL lists every serial the old one
//     lists (crlNewer);
//   - max_size: the new blob is no larger;
//   - a declared path holds a regular file: a tree, symlink or submodule AT a
//     declared path, or any entry UNDER a directory a pattern declares a file
//     (one the pattern matches while not matching the entry, such as
//     certs/<id>.pem/x; fleet/** matches both and declares no file), is
//     refused (RuleArtifactNotAFile);
//   - a pki file that is ADDED must hold as its role (pkiAdd): a root that is
//     a CA certificate, a CRL the root signed, a leaf that chains to the root,
//     is not revoked and is issued to the id its file name stands for, an
//     issuance record the root issued (pki.CheckIssuance), a revocation
//     statement the root signed (pki.VerifyRevocation).
//
// The PKI a primitive needs — the root, the CRL, the member records — is read
// from c's OWN tree. The root there is the root of every parent too (it is
// write-once, and a commit that changes it is refused by the same check), and
// a CRL c replaces is held to crl-newer in the same pass; so a commit is
// judged against exactly the trust state it leaves behind, the same in
// receive-pack and in `verify ci`.
//
// An undeclared path has no rule; a path whose rules are ambiguous, or whose
// blob cannot be read, is refused (RuleArtifactRules).
func (j judge) artifactRules(c *object.Commit) (Refusal, bool) {
	if !j.ont.HasArtifactRules() {
		return Refusal{}, true
	}
	h := c.Hash.String()
	refuse := func(rule, reason string) (Refusal, bool) {
		return Refusal{Commit: h, Rule: rule, Reason: reason}, false
	}
	tree, err := c.Tree()
	if err != nil {
		return refuse(RuleArtifactRules, fmt.Sprintf("the commit's tree cannot be read: %v", err))
	}
	parents, err := vcs.ParentTrees(c)
	if err != nil {
		return refuse(RuleArtifactRules, fmt.Sprintf("a parent's tree cannot be read: %v", err))
	}
	a := &artifactCheck{j: j, tree: tree, parents: parents}
	for _, pt := range parents {
		changes, err := object.DiffTree(pt, tree)
		if err != nil {
			return refuse(RuleArtifactRules, fmt.Sprintf("diff: %v", err))
		}
		for _, ch := range changes {
			if !underArtifacts(ch.From.Name) && !underArtifacts(ch.To.Name) {
				continue
			}
			if rule, reason := a.change(ch); rule != "" {
				return refuse(rule, reason)
			}
		}
	}
	return Refusal{}, true
}

func underArtifacts(p string) bool { return strings.HasPrefix(p, fact.ArtifactsRoot+"/") }

// artifactCheck judges the changes of one commit, reading its tree's PKI at
// most once.
type artifactCheck struct {
	j    judge
	tree *object.Tree
	// parents are the commit's parents' trees (nil entries: the empty tree).
	parents []*object.Tree

	pkiRead bool
	root    *x509.Certificate
	rootErr error
	crl     *x509.RevocationList
	crlErr  error
}

// inAnotherParent reports whether some parent tree holds blob at name: the
// same content (blob hash), not merely the same path.
//
// Why skipping such an add is safe (#533 review N2): every entry path walks
// EACH commit of the incoming range (receive-pack, the fetch judge, verify
// ci, the master's own range), so every parent of a merge is either already
// on main or itself judged in the same range; the blob it holds was judged
// as an add where it was added. A future path that judges a merge commit
// ALONE (only its result, not the commits under it) must not rely on this
// skip: there a parent could hold a blob nobody judged.
func (a *artifactCheck) inAnotherParent(name string, blob plumbing.Hash) bool {
	if len(a.parents) < 2 {
		return false
	}
	for _, pt := range a.parents {
		if pt == nil {
			continue
		}
		if e, err := pt.FindEntry(name); err == nil && e.Hash == blob {
			return true
		}
	}
	return false
}

// changeRule is the refusal rule for breaking a change rule.
func changeRule(change string) string {
	switch change {
	case fact.ChangeWriteOnce:
		return RuleArtifactWriteOnce
	case fact.ChangeAppendOnly:
		return RuleArtifactAppendOnly
	}
	return RuleArtifactReplace
}

// change judges one changed path; "" when it keeps its rules.
func (a *artifactCheck) change(ch *object.Change) (rule, reason string) {
	name := ch.To.Name
	if name == "" {
		name = ch.From.Name
	}
	if ch.To.Name != "" {
		if rule, reason := a.squat(name); rule != "" {
			return rule, reason
		}
	}
	attrs, err := a.j.ont.ArtifactAttrs(name)
	if err != nil {
		return RuleArtifactRules, err.Error()
	}
	if !attrs.Matched {
		return "", ""
	}
	// Deleted.
	if ch.To.Name == "" {
		if attrs.Change != "" {
			return changeRule(attrs.Change), fmt.Sprintf("%s is deleted, and a %s file is never deleted", name, attrs.Change)
		}
		return "", ""
	}
	if m := ch.To.TreeEntry.Mode; m != filemode.Regular && m != filemode.Executable {
		return RuleArtifactNotAFile, fmt.Sprintf("%s is a declared artifact and must be a regular file, not mode %s", name, m)
	}
	newF, err := ch.To.Tree.TreeEntryFile(&ch.To.TreeEntry)
	if err != nil {
		return RuleArtifactRules, fmt.Sprintf("%s cannot be read: %v", name, err)
	}
	if attrs.MaxSize > 0 && newF.Size > attrs.MaxSize {
		return RuleArtifactSize, fmt.Sprintf("%s is %d bytes, larger than its max_size of %d", name, newF.Size, attrs.MaxSize)
	}
	if bound := fleetrec.ReadBound(attrs); newF.Size > bound {
		return RuleArtifactRules, fmt.Sprintf("%s is %d bytes, more than the judge reads of a %s file (%d)", name, newF.Size, attrs.PKI, bound)
	}
	added := ch.From.Name == ""
	if added {
		if attrs.PKI == "" {
			return "", ""
		}
		// A merge that brings a pki file ANOTHER parent already holds, the
		// very same blob at the same path, adds nothing: that file was added,
		// and judged, in that parent's history. Judging it again against this
		// merge's CRL would refuse a merge for a revocation made after the
		// file was issued (the master merging a member's branch that forked
		// before a leaf it later revoked; a member merging such a main). A
		// blob no parent holds at that path is judged as before.
		if a.inAnotherParent(name, ch.To.TreeEntry.Hash) {
			return "", ""
		}
		raw, err := blobBytes(newF)
		if err != nil {
			return RuleArtifactRules, fmt.Sprintf("%s cannot be read: %v", name, err)
		}
		if err := a.pkiAdd(attrs.PKI, name, raw); err != nil {
			return RuleArtifactPKI, fmt.Sprintf("%s is added as the pki %s but does not hold: %v", name, attrs.PKI, err)
		}
		return "", ""
	}
	// Modified (DiffTree pairs only equal names: no rename detection).
	switch attrs.Change {
	case "":
		return "", ""
	case fact.ChangeWriteOnce:
		return RuleArtifactWriteOnce, fmt.Sprintf("%s is write-once and this commit modifies it", name)
	}
	oldF, err := ch.From.Tree.TreeEntryFile(&ch.From.TreeEntry)
	if err != nil {
		return RuleArtifactRules, fmt.Sprintf("%s (before) cannot be read: %v", name, err)
	}
	oldRaw, err := blobBytes(oldF)
	if err != nil {
		return RuleArtifactRules, fmt.Sprintf("%s (before) cannot be read: %v", name, err)
	}
	newRaw, err := blobBytes(newF)
	if err != nil {
		return RuleArtifactRules, fmt.Sprintf("%s cannot be read: %v", name, err)
	}
	switch attrs.Change {
	case fact.ChangeAppendOnly:
		if len(newRaw) <= len(oldRaw) || string(newRaw[:len(oldRaw)]) != string(oldRaw) {
			return RuleArtifactAppendOnly, fmt.Sprintf("%s is append-only and the new content does not start with the old (%d -> %d bytes)", name, len(oldRaw), len(newRaw))
		}
		return "", ""
	case fact.ChangeReplace:
		var err error
		switch attrs.ReplaceWhen {
		case fact.ReplaceCRLNewer:
			err = a.crlNewer(oldRaw, newRaw, attrs.RevokedSet == fact.RevokedSetSuperset)
		case fact.ReplaceLeafRenewal:
			err = a.leafRenewal(name, oldRaw, newRaw)
		default:
			err = fmt.Errorf("unknown replace_when %q", attrs.ReplaceWhen)
		}
		if err != nil {
			return RuleArtifactReplace, fmt.Sprintf("%s is replaced but replace_when %s does not hold: %v", name, attrs.ReplaceWhen, err)
		}
		return "", ""
	}
	return RuleArtifactRules, fmt.Sprintf("%s: unknown change rule %q", name, attrs.Change)
}

// squat refuses an entry UNDER a declared artifact file: when a declared
// pattern matches a proper ancestor directory of name (below artifacts/) but
// NOT name itself, that directory is declared a FILE, and this tree squats it
// — certs/<id>.pem/x would keep <id>'s leaf from ever being added, and a
// reader that skips non-files would see no leaf at all. A pattern that also
// matches name (fleet/**) declares a family of paths, not a file, and squats
// nothing (fact.Ontology.ArtifactFileAt).
func (a *artifactCheck) squat(name string) (rule, reason string) {
	rel := strings.TrimPrefix(name, fact.ArtifactsRoot+"/")
	for i := strings.IndexByte(rel, '/'); i >= 0; {
		dir := fact.ArtifactsRoot + "/" + rel[:i]
		if pattern, ok := a.j.ont.ArtifactFileAt(dir, name); ok {
			return RuleArtifactNotAFile, fmt.Sprintf("%s is under %s, which is a declared artifact file (%s), not a directory", name, dir, pattern)
		}
		next := strings.IndexByte(rel[i+1:], '/')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return "", ""
}

func blobBytes(f *object.File) ([]byte, error) {
	s, err := f.Contents()
	return []byte(s), err
}

// readPKI reads the declared root and CRL from the commit's own tree, once.
func (a *artifactCheck) readPKI() {
	if a.pkiRead {
		return
	}
	a.pkiRead = true
	layout, ok := a.j.ont.PKILayout()
	if !ok {
		a.rootErr, a.crlErr = errors.New("the ontology declares no pki: root"), errors.New("the ontology declares no pki: root")
		return
	}
	raw, ok, err := fleetrec.TreeBytesCapped(a.tree, layout.Root, fleetrec.PKIFileCap(a.j.ont, layout.Root))
	switch {
	case err != nil:
		a.rootErr = fmt.Errorf("%s: %w", layout.Root, err)
	case !ok:
		a.rootErr = fmt.Errorf("no %s", layout.Root)
	default:
		if a.root, err = fleetrec.ParseRootPEM(raw); err != nil {
			a.root, a.rootErr = nil, fmt.Errorf("%s: %w", layout.Root, err)
		}
	}
	if layout.CRL == "" {
		a.crlErr = errors.New("the ontology declares no pki: crl")
		return
	}
	raw, ok, err = fleetrec.TreeBytesCapped(a.tree, layout.CRL, fleetrec.PKIFileCap(a.j.ont, layout.CRL))
	switch {
	case err != nil:
		a.crlErr = fmt.Errorf("%s: %w", layout.CRL, err)
	case !ok:
		a.crlErr = fmt.Errorf("no %s", layout.CRL)
	default:
		if a.crl, err = fleetrec.ParseCRLPEM(raw); err != nil {
			a.crl, a.crlErr = nil, fmt.Errorf("%s: %w", layout.CRL, err)
		}
	}
}

// pkiAdd: an added pki file must hold as its role.
func (a *artifactCheck) pkiAdd(role, name string, raw []byte) error {
	switch role {
	case fact.PKIRoot:
		_, err := fleetrec.ParseRootPEM(raw)
		return err
	case fact.PKICRL:
		crl, err := fleetrec.ParseCRLPEM(raw)
		if err != nil {
			return err
		}
		a.readPKI()
		if a.root == nil {
			return fmt.Errorf("no root to check it against: %w", a.rootErr)
		}
		_, err = pki.CheckCRL(crl, a.root, nil)
		return err
	case fact.PKILeaf:
		_, _, err := a.validLeaf(name, raw)
		return err
	case fact.PKIIssuance, fact.PKIRevocation:
		a.readPKI()
		if a.root == nil {
			return fmt.Errorf("no root to check it against: %w", a.rootErr)
		}
		var err error
		if role == fact.PKIIssuance {
			_, err = pki.CheckIssuance(raw, a.root)
		} else {
			_, err = pki.VerifyRevocation(raw, a.root)
		}
		return err
	}
	return fmt.Errorf("unknown pki role %q", role)
}

// validLeaf checks a leaf file at name: it parses, chains to the tree's root,
// is not revoked by the tree's CRL, and its email SAN's agent id EQUALS the id
// its file name stands for, byte for byte (equality only, R2; ids are lower
// case and nothing is case folded, so certs/Vic.pem can hold no leaf). It
// returns the leaf and that id.
func (a *artifactCheck) validLeaf(name string, raw []byte) (*x509.Certificate, string, error) {
	layout, _ := a.j.ont.PKILayout()
	id, ok := layout.LeafID(path.Base(name))
	if !ok || path.Dir(name) != layout.LeafDir {
		return nil, "", fmt.Errorf("%s is not at a leaf path (%s)", name, layout.LeafPath("<id>"))
	}
	leaf, err := fleetrec.ParseCertPEM(raw)
	if err != nil {
		return nil, "", err
	}
	a.readPKI()
	if a.root == nil {
		return nil, "", fmt.Errorf("no root to check it against: %w", a.rootErr)
	}
	if a.crl == nil {
		return nil, "", fmt.Errorf("no CRL to check it against: %w", a.crlErr)
	}
	at := leaf.NotBefore
	if a.root.NotBefore.After(at) {
		at = a.root.NotBefore
	}
	ident, err := pki.VerifyInstanceChain(leaf, nil, a.root, a.crl, at, pki.UsageClient)
	if err != nil {
		return nil, "", err
	}
	if ident.AgentID == "" || ident.AgentID != id {
		return nil, "", fmt.Errorf("its email SAN %q names agent %q, but the file is for %q", ident.Email, ident.AgentID, id)
	}
	return leaf, id, nil
}

// crlNewer is replace_when: crl-newer: the new CRL is signed by the declared
// root, and its Number is strictly greater than the old CRL's. An old CRL that
// does not parse cannot be shown older, so nothing replaces it this way. With
// superset (revoked_set: superset) the new CRL must also list every serial
// the old one lists: a reissue never un-revokes.
func (a *artifactCheck) crlNewer(oldRaw, newRaw []byte, superset bool) error {
	crl, err := fleetrec.ParseCRLPEM(newRaw)
	if err != nil {
		return err
	}
	a.readPKI()
	if a.root == nil {
		return fmt.Errorf("no root to check it against: %w", a.rootErr)
	}
	if _, err := pki.CheckCRL(crl, a.root, nil); err != nil {
		return err
	}
	old, err := fleetrec.ParseCRLPEM(oldRaw)
	if err != nil || old.Number == nil {
		return fmt.Errorf("the CRL it replaces has no readable Number: %v", err)
	}
	if crl.Number.Cmp(old.Number) <= 0 {
		return fmt.Errorf("CRL Number %v is not greater than the %v it replaces (rollback)", crl.Number, old.Number)
	}
	if superset {
		for _, e := range old.RevokedCertificateEntries {
			if e.SerialNumber == nil || !pki.IsRevoked(crl, e.SerialNumber) {
				return fmt.Errorf("CRL Number %v drops serial %s that the CRL it replaces lists (revoked_set: superset)", crl.Number, serialText(e.SerialNumber))
			}
		}
	}
	return nil
}

func serialText(s *big.Int) string {
	if s == nil {
		return "<none>"
	}
	return s.Text(16)
}

// leafRenewal is replace_when: leaf-renewal: the new leaf holds (validLeaf:
// chains to the root, not revoked by the current CRL, issued to the file's
// id), its key is the current key of the ONE member record for that id in the
// commit's tree, and its NotBefore is strictly later than the old leaf's.
func (a *artifactCheck) leafRenewal(name string, oldRaw, newRaw []byte) error {
	leaf, id, err := a.validLeaf(name, newRaw)
	if err != nil {
		return err
	}
	old, err := fleetrec.ParseCertPEM(oldRaw)
	if err != nil {
		return fmt.Errorf("the leaf it replaces does not parse: %v", err)
	}
	if !leaf.NotBefore.After(old.NotBefore) {
		return fmt.Errorf("its NotBefore %s is not later than the replaced leaf's %s", leaf.NotBefore, old.NotBefore)
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return pki.ErrNotEd25519
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		return err
	}
	members, err := fleetrec.LoadMembersOfTree("judged", a.tree, a.j.kbRoot)
	if err != nil {
		return err
	}
	var recs []fact.Member
	for _, m := range members {
		if m.Agent == id {
			recs = append(recs, m.Member)
		}
	}
	if len(recs) != 1 {
		return fmt.Errorf("%d member records for agent %q, want exactly one", len(recs), id)
	}
	if !fact.SameKey(recs[0].Key, key) {
		return fmt.Errorf("its key is not the current key of agent %q's member record", id)
	}
	return nil
}
