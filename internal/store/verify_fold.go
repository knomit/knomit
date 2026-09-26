package store

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/pki"
)

// Verification modes, ordered: off < log < enforce.
const (
	VerifyOff     = "off"
	VerifyLog     = "log"
	VerifyEnforce = "enforce"
)

func modeRank(m string) int {
	switch m {
	case VerifyLog:
		return 1
	case VerifyEnforce:
		return 2
	}
	return 0
}

// RootOfTrust decides whether a commit signer is the OPERATOR: the only party
// who may enable verification, relax it, or change the signer list (F09,
// user decision (a)). One implementation today, a static key from
// [verify].operator_key; F19's operator certificate under the master root is
// the other, swapped in by configuration.
type RootOfTrust interface {
	Configured() bool
	IsOperator(CommitSigner) bool
}

// StaticRoot is the RootOfTrust of one operator public key. The zero value is
// unconfigured ("unrooted").
type StaticRoot struct {
	Fingerprint string // pki.Fingerprint of the operator key; "" = unrooted
}

// NewStaticRoot parses an authorized-key line (ssh-ed25519 only). An empty
// line is the unconfigured root; a malformed one is an error the caller must
// surface (boot refuses), never an unrooted instance with a misleading reason.
func NewStaticRoot(line string) (StaticRoot, error) {
	if strings.TrimSpace(line) == "" {
		return StaticRoot{}, nil
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return StaticRoot{}, fmt.Errorf("operator key: not an authorized-key line: %w", err)
	}
	fp, err := keyFingerprint(pk)
	if err != nil {
		return StaticRoot{}, fmt.Errorf("operator key: %w", err)
	}
	return StaticRoot{Fingerprint: fp}, nil
}

func (r StaticRoot) Configured() bool { return r.Fingerprint != "" }

func (r StaticRoot) IsOperator(s CommitSigner) bool {
	return r.Fingerprint != "" && s.Fingerprint == r.Fingerprint
}

// keyFingerprint is pki.Fingerprint of an ssh-ed25519 public key.
func keyFingerprint(pk ssh.PublicKey) (string, error) {
	cpk, ok := pk.(ssh.CryptoPublicKey)
	if !ok {
		return "", ErrNotEd25519Key
	}
	ed, ok := cpk.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNotEd25519Key, pk.Type())
	}
	return pki.Fingerprint(ed), nil
}

// verifyContext is the policy in force: the mode and the admitted signers as
// the FOLD computed them. It is never read from an anchor commit's file (which
// may carry a change the fold rejected).
type verifyContext struct {
	Mode    string   `json:"mode"`
	Signers []string `json:"signers"` // authorized-key lines, sorted
}

func (c verifyContext) fingerprints() map[string]bool {
	out := map[string]bool{}
	for _, l := range c.Signers {
		if pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(l)); err == nil {
			if fp, err := keyFingerprint(pk); err == nil {
				out[fp] = true
			}
		}
	}
	return out
}

// Refusal is one commit the fold did not accept.
type Refusal struct {
	Commit   string `json:"commit"`
	Rule     string `json:"rule"`
	SignerFP string `json:"signer_fp,omitempty"`
	Reason   string `json:"reason"`
}

// Refusal rules.
const (
	RuleSignature    = "signature"     // unsigned, bad, or a signer not admitted
	RuleAuthorClaim  = "author-claim"  // author claims another agent's fingerprint
	RuleMerge        = "merge"         // unsigned merge that fails M3
	RulePolicyChange = "policy-change" // an unauthorised change to verify_signatures / verify_signers
	RuleUnreadable   = "unreadable"    // the ontology's setting cannot be read
	RuleUnrooted     = "unrooted"      // a policy change this instance cannot judge: no operator key
)

// foldResult is the verifier's answer for one advance.
type foldResult struct {
	Final        verifyContext // the context with every accepted change applied
	NewAnchor    plumbing.Hash // furthest first-parent commit whose range fully passed
	NewAnchorCtx verifyContext // the context in force AT NewAnchor
	Refused      []Refusal     // commits NOT accepted: the anchor stops before the first on the first-parent chain
	Reported     []Refusal     // accepted commits with a note: a policy change rejected and ignored, or an unreadable setting, outside enforce
	SigChecks    int           // signature and M3 evaluations performed (0 for an off repo)
	Unrooted     bool          // a policy change could not be judged: the advance is closed at NewAnchor in every mode
	Rewind       bool          // the anchor is not an ancestor of the tip

	// Advanced lists the range commits now below the new anchor (NewAnchor and
	// its ancestors in the range), so a caller caching the anchor's history can
	// extend it instead of re-walking.
	Advanced []plumbing.Hash
}

// Closed reports whether the local upstream must stop at NewAnchor instead of
// moving to the tip: enforce, or any unjudgeable policy change (unrooted).
func (r foldResult) Closed() bool { return r.Final.Mode == VerifyEnforce || r.Unrooted }

// verifier runs the F09 fold over one object store.
type verifier struct {
	st       storer.EncodedObjectStorer
	root     RootOfTrust
	accepted func(plumbing.Hash) bool // --accept waivers: a failing SIGNATURE only

	// below, when set, is the set of commits reachable from the anchor passed
	// to fold (a cache the caller keeps across ticks). Nil: fold walks the
	// anchor's history itself.
	below map[plumbing.Hash]bool

	byTop map[string]settingsState // settingsAt memo, see there
}

// settingsState is what a commit's ontology says about verification.
type settingsState struct {
	Settings fact.VerifySettings
	Known    bool // false: unreadable or invalid → UNKNOWN, never off
}

// settingsAt reads the verify settings from c's own tree, choosing the ontology
// file exactly as the store does (the first of OntologyPathsNewestFirst that
// exists). A path present as a directory, or a blob that cannot be read, is
// UNKNOWN rather than "try the next path", so a shadowing directory cannot fall
// through. No ontology at all reads as off.
//
// Cost: the answer depends only on the root-tree entries of the three paths'
// top-level names, so it is memoised by those entries' hashes. Consecutive
// commits almost always share them, and a history walk then decodes one root
// tree per commit and parses each distinct ontology once.
func (v *verifier) settingsAt(c *object.Commit, cache map[plumbing.Hash]settingsState) (settingsState, error) {
	if s, ok := cache[c.Hash]; ok {
		return s, nil
	}
	tree, err := c.Tree()
	if err != nil {
		return settingsState{}, fmt.Errorf("verify: tree of %s: %w", c.Hash, err)
	}
	var key strings.Builder
	for _, p := range fact.OntologyPathsNewestFirst() {
		top, _, _ := strings.Cut(p, "/")
		key.WriteString(top)
		key.WriteByte('=')
		if e, err := tree.FindEntry(top); err == nil {
			key.WriteString(e.Hash.String())
			key.WriteString(e.Mode.String())
		}
		key.WriteByte(';')
	}
	if v.byTop == nil {
		v.byTop = map[string]settingsState{}
	}
	if st, ok := v.byTop[key.String()]; ok {
		cache[c.Hash] = st
		return st, nil
	}
	st := settingsState{Settings: fact.VerifySettings{Mode: VerifyOff, Valid: true}, Known: true}
	for _, p := range fact.OntologyPathsNewestFirst() {
		entry, err := tree.FindEntry(p)
		if errors.Is(err, object.ErrEntryNotFound) || errors.Is(err, object.ErrDirectoryNotFound) {
			continue
		}
		if err != nil || !entry.Mode.IsFile() {
			st = settingsState{}
			break
		}
		f, err := tree.TreeEntryFile(entry)
		if err != nil {
			st = settingsState{}
			break
		}
		body, err := f.Contents()
		if err != nil {
			st = settingsState{}
			break
		}
		s, err := fact.ReadVerifySettings([]byte(body))
		if err != nil || !s.Valid {
			st = settingsState{}
			break
		}
		st = settingsState{Settings: s, Known: true}
		break
	}
	v.byTop[key.String()] = st
	cache[c.Hash] = st
	return st, nil
}

// authorClaimRe matches the fingerprint an agent author email claims:
// <host>-<fp8>[+<op>]@agents.knomit.io. Experiment authors (exp/<name>+op@…)
// carry no fingerprint and so make no claim.
var authorClaimRe = regexp.MustCompile(`-([0-9a-f]{8})(?:\+[a-z0-9-]+)?@agents\.knomit\.io$`)

// change is one accepted component of a policy change.
type change struct {
	at       plumbing.Hash
	stricter bool
	mode     string // "" when this component is a signer change
	add      string // authorized-key line added
	remove   string // authorized-key line removed
}

// fold judges the advance from anchor (with context actx) to tip. anchor ==
// ZeroHash is the ROOT fold: the whole history of tip, starting from off.
//
// Rules (proposal §7.2, revision 5):
//   - A commit whose resolved settings differ from EVERY parent's is a policy
//     change. Its components are judged by the change rule: an enable, a
//     relaxation, and any signer-list change need the OPERATOR; log→enforce
//     needs an admitted signer. With no operator configured, a change needing
//     one is unjudgeable: the advance closes before it in every mode.
//   - A rejected change refuses its commit when the final mode is enforce and
//     is reported and ignored otherwise.
//   - A STRICTER accepted change reaches every range commit except its own
//     ancestors; a LOOSER one reaches every range commit. The mode is never
//     exempted: the final mode applies to every checked commit.
//   - With an off anchor, a commit is checked only when it is not an ancestor
//     of an accepted enable.
//   - Merges are judged in the final context: signed by an admitted signer, or
//     M3 over accepted parents.
func (v *verifier) fold(anchor plumbing.Hash, actx verifyContext, tip plumbing.Hash) (foldResult, error) {
	res := foldResult{NewAnchor: anchor, NewAnchorCtx: actx, Final: actx}
	tipC, err := object.GetCommit(v.st, tip)
	if err != nil {
		return res, fmt.Errorf("verify: tip %s: %w", tip, err)
	}

	// Commits reachable from the anchor are outside the range.
	below := v.below
	if anchor != plumbing.ZeroHash && anchor == tip {
		return res, nil
	}
	if below == nil {
		below = map[plumbing.Hash]bool{}
		v.below = below // hand the walk back to a caller that caches it
		if anchor != plumbing.ZeroHash {
			if err := v.walk(anchor, nil, func(c *object.Commit) { below[c.Hash] = true }); err != nil {
				return res, err
			}
		}
	}

	// The range T ^A in topological order, parents first.
	var order []*object.Commit
	if err := v.walk(tip, below, func(c *object.Commit) { order = append(order, c) }); err != nil {
		return res, err
	}
	// The anchor is an ancestor of the tip exactly when some range commit has
	// it as a parent: on any path tip → anchor, the commit just above the
	// anchor is a descendant of it, so it is in the range.
	if anchor != plumbing.ZeroHash {
		reached := false
		for _, c := range order {
			if slices.Contains(c.ParentHashes, anchor) {
				reached = true
				break
			}
		}
		if !reached {
			res.Rewind = true
			return res, nil
		}
	}
	inRange := map[plumbing.Hash]*object.Commit{}
	for _, c := range order {
		inRange[c.Hash] = c
	}

	// Pass 1: settings per commit, policy changes, and the running context.
	cache := map[plumbing.Hash]settingsState{}
	unreadable := map[plumbing.Hash]bool{}
	rejected := map[plumbing.Hash]string{} // commit → reason (rejected policy change)
	unrootedAt := map[plumbing.Hash]bool{}
	var changes []change
	g := verifyContext{Mode: actx.Mode, Signers: slices.Clone(actx.Signers)}
	sigs := map[plumbing.Hash]sigResult{}

	for _, c := range order {
		st, err := v.settingsAt(c, cache)
		if err != nil {
			return res, err
		}
		if !st.Known {
			unreadable[c.Hash] = true
			continue
		}
		var parents []settingsState
		for _, ph := range c.ParentHashes {
			pc, err := object.GetCommit(v.st, ph)
			if err != nil {
				return res, fmt.Errorf("verify: parent %s: %w", ph, err)
			}
			ps, err := v.settingsAt(pc, cache)
			if err != nil {
				return res, err
			}
			parents = append(parents, ps)
		}
		isChange := true
		for _, ps := range parents {
			if ps.Known && ps.Settings.Equal(st.Settings) {
				isChange = false
			}
		}
		if len(parents) == 0 && st.Settings.Mode == VerifyOff && len(st.Settings.Signers) == 0 {
			isChange = false // a root commit that says nothing
		}
		if !isChange {
			continue
		}
		comps := diffContext(g, st.Settings)
		if len(comps) == 0 {
			continue // the file changed to what the fold already has in force
		}
		sig := v.sig(c, sigs)
		var accepted []change
		for _, comp := range comps {
			comp.at = c.Hash
			needsOperator := !(comp.mode == VerifyEnforce && g.Mode == VerifyLog)
			ok := false
			switch {
			case needsOperator && !v.root.Configured():
				unrootedAt[c.Hash] = true
			case needsOperator:
				ok = sig.err == nil && v.root.IsOperator(sig.signer)
			default: // log → enforce: any admitted signer, or the operator
				ok = sig.err == nil && (g.fingerprints()[sig.signer.Fingerprint] || v.root.IsOperator(sig.signer))
			}
			if !ok {
				rejected[c.Hash] = describeChange(comp)
				continue
			}
			accepted = append(accepted, comp)
		}
		for _, a := range accepted {
			g = applyChange(g, a)
		}
		changes = append(changes, accepted...)
	}
	res.Final = g
	res.Unrooted = len(unrootedAt) > 0

	// Ancestor sets of every stricter change (and every enable), inside the range.
	ancOf := map[plumbing.Hash]map[plumbing.Hash]bool{}
	ancestors := func(h plumbing.Hash) map[plumbing.Hash]bool {
		if a, ok := ancOf[h]; ok {
			return a
		}
		a := map[plumbing.Hash]bool{}
		var visit func(x plumbing.Hash)
		visit = func(x plumbing.Hash) {
			c := inRange[x]
			if c == nil {
				return
			}
			for _, p := range c.ParentHashes {
				if !a[p] {
					a[p] = true
					visit(p)
				}
			}
		}
		visit(h)
		ancOf[h] = a
		return a
	}

	// Pass 2: judge each commit.
	accepted := map[plumbing.Hash]bool{}
	finalFPs := res.Final.fingerprints()
	for _, c := range order {
		h := c.Hash
		refuse := func(rule, fp, reason string) {
			res.Refused = append(res.Refused, Refusal{Commit: h.String(), Rule: rule, SignerFP: fp, Reason: reason})
		}
		if unrootedAt[h] {
			refuse(RuleUnrooted, "", "policy change needs the operator key: configure [verify].operator_key")
			continue
		}
		if !v.checked(h, actx, changes, ancestors) {
			// Not subject to verification (off, or baseline before an enable).
			// A rejected policy change is still worth telling the operator about.
			if reason, ok := rejected[h]; ok {
				res.Reported = append(res.Reported, Refusal{Commit: h.String(), Rule: RulePolicyChange,
					SignerFP: v.sig(c, sigs).signer.Fingerprint, Reason: "unauthorised policy change: " + reason})
			}
			accepted[h] = true
			continue
		}
		// A rejected policy change or an unreadable setting refuses the whole
		// commit in enforce (the file must never say something the fleet does
		// not enforce), and is a note outside it: no stranger can stall a repo
		// that does not enforce.
		enforcing := res.Final.Mode == VerifyEnforce
		note := func(rule, fp, reason string) bool {
			r := Refusal{Commit: h.String(), Rule: rule, SignerFP: fp, Reason: reason}
			if enforcing {
				res.Refused = append(res.Refused, r)
				return true
			}
			res.Reported = append(res.Reported, r)
			return false
		}
		if reason, ok := rejected[h]; ok {
			if note(RulePolicyChange, v.sig(c, sigs).signer.Fingerprint, "unauthorised policy change: "+reason) {
				continue
			}
		}
		if unreadable[h] {
			if note(RuleUnreadable, "", "the verify setting in this commit's ontology cannot be read") {
				continue
			}
		}
		// A failing commit is never accepted, in any mode: the modes differ only
		// in where the local upstream goes (log: the tip; enforce: the anchor).
		res.SigChecks++
		if ok, rule, fp, reason := v.judge(c, res.Final, finalFPs, v.signersAt(h, actx, changes, ancestors), accepted, below, sigs); ok {
			accepted[h] = true
		} else {
			refuse(rule, fp, reason)
		}
	}

	// New anchor: the furthest first-parent commit whose whole range passed.
	good := map[plumbing.Hash]bool{}
	for _, c := range order {
		ok := accepted[c.Hash]
		for _, p := range c.ParentHashes {
			if _, in := inRange[p]; in && !good[p] {
				ok = false
			}
		}
		good[c.Hash] = ok
	}
	var firstParent []plumbing.Hash
	for c := tipC; c != nil; {
		if _, in := inRange[c.Hash]; !in {
			break
		}
		firstParent = append(firstParent, c.Hash)
		if c.NumParents() == 0 {
			break
		}
		p, err := c.Parent(0)
		if err != nil {
			return res, fmt.Errorf("verify: first parent of %s: %w", c.Hash, err)
		}
		c = p
	}
	for _, h := range firstParent { // newest first
		if good[h] {
			res.NewAnchor = h
			res.NewAnchorCtx = contextAt(actx, changes, h, ancestors)
			res.Advanced = append(res.Advanced, h)
			for a := range ancestors(h) {
				if _, in := inRange[a]; in {
					res.Advanced = append(res.Advanced, a)
				}
			}
			break
		}
	}
	return res, nil
}

type sigResult struct {
	signer CommitSigner
	err    error
}

func (v *verifier) sig(c *object.Commit, cache map[plumbing.Hash]sigResult) sigResult {
	if r, ok := cache[c.Hash]; ok {
		return r
	}
	s, err := verifyCommitSignature(c)
	r := sigResult{signer: s, err: err}
	cache[c.Hash] = r
	return r
}

// checked reports whether commit h is subject to verification at all. With an
// anchor that is already verifying, every range commit is. With an off anchor,
// only the commits that are NOT ancestors of an accepted enable (the ancestors
// of the enable existed before the repo asked to be protected). If the final
// mode is off (a verified disable), nothing is.
func (v *verifier) checked(h plumbing.Hash, actx verifyContext, changes []change, ancestors func(plumbing.Hash) map[plumbing.Hash]bool) bool {
	mode := actx.Mode
	for _, c := range changes {
		if c.mode != "" {
			mode = c.mode
		}
	}
	if mode == VerifyOff {
		return false
	}
	if actx.Mode != VerifyOff {
		return true
	}
	for _, c := range changes {
		if c.mode != "" && c.stricter && modeRank(c.mode) > 0 && !ancestors(c.at)[h] {
			return true
		}
	}
	return false
}

// signersAt is the admitted set for a NON-merge commit h: the anchor's
// signers, plus every accepted addition, minus every accepted removal R for
// which h is not an ancestor of R.
func (v *verifier) signersAt(h plumbing.Hash, actx verifyContext, changes []change, ancestors func(plumbing.Hash) map[plumbing.Hash]bool) map[string]bool {
	set := map[string]bool{}
	for _, l := range actx.Signers {
		set[l] = true
	}
	for _, c := range changes {
		switch {
		case c.add != "":
			set[c.add] = true
		case c.remove != "" && !ancestors(c.at)[h]:
			delete(set, c.remove)
		}
	}
	lines := make([]string, 0, len(set))
	for l := range set {
		lines = append(lines, l)
	}
	return verifyContext{Signers: lines}.fingerprints()
}

// judge decides one checked commit.
func (v *verifier) judge(c *object.Commit, final verifyContext, finalFPs, signers map[string]bool, accepted, below map[plumbing.Hash]bool, sigs map[plumbing.Hash]sigResult) (ok bool, rule, fp, reason string) {
	s := v.sig(c, sigs)
	// The operator (the root of trust) is always an admitted signer: the key
	// that may change the policy may also sign content under it. It is the same
	// configured key on every instance of a fleet, so this does not make the
	// fold's answer depend on which instance runs it (an instance's OWN key
	// never counts unless it is listed).
	operator := s.err == nil && v.root.IsOperator(s.signer)
	if c.NumParents() >= 2 {
		if s.err == nil && (finalFPs[s.signer.Fingerprint] || operator) {
			return true, "", "", ""
		}
		for _, p := range c.ParentHashes {
			if !accepted[p] && !below[p] {
				return false, RuleMerge, s.signer.Fingerprint, "unsigned merge of a parent that was not accepted"
			}
		}
		verdict, err := checkM3(c)
		if err != nil {
			return false, RuleMerge, "", err.Error()
		}
		if !verdict.OK {
			return false, RuleMerge, s.signer.Fingerprint, verdict.Reason
		}
		return true, "", "", ""
	}
	waived := v.accepted != nil && v.accepted(c.Hash)
	if s.err != nil {
		if waived {
			return true, "", "", ""
		}
		return false, RuleSignature, "", s.err.Error()
	}
	if !signers[s.signer.Fingerprint] && !operator {
		if waived {
			return true, "", "", ""
		}
		return false, RuleSignature, s.signer.Fingerprint, "signer is not in verify_signers"
	}
	if m := authorClaimRe.FindStringSubmatch(c.Author.Email); m != nil && m[1] != pki.Short(s.signer.Fingerprint) {
		return false, RuleAuthorClaim, s.signer.Fingerprint,
			fmt.Sprintf("author claims %s, signed by %s", m[1], pki.Short(s.signer.Fingerprint))
	}
	return true, "", "", ""
}

// contextAt is the context in force at commit h: the anchor's, with every
// accepted change that is h itself or one of h's ancestors applied in order.
func contextAt(actx verifyContext, changes []change, h plumbing.Hash, ancestors func(plumbing.Hash) map[plumbing.Hash]bool) verifyContext {
	ctx := verifyContext{Mode: actx.Mode, Signers: slices.Clone(actx.Signers)}
	anc := ancestors(h)
	for _, c := range changes {
		if c.at == h || anc[c.at] {
			ctx = applyChange(ctx, c)
		}
	}
	return ctx
}

// diffContext splits the difference between the context in force and a
// commit's settings into components.
func diffContext(g verifyContext, s fact.VerifySettings) []change {
	var out []change
	if s.Mode != g.Mode {
		out = append(out, change{mode: s.Mode, stricter: modeRank(s.Mode) > modeRank(g.Mode)})
	}
	have := map[string]bool{}
	for _, l := range g.Signers {
		have[l] = true
	}
	want := map[string]bool{}
	for _, l := range s.Signers {
		want[l] = true
		if !have[l] {
			out = append(out, change{add: l})
		}
	}
	for _, l := range g.Signers {
		if !want[l] {
			out = append(out, change{remove: l, stricter: true})
		}
	}
	return out
}

func applyChange(g verifyContext, c change) verifyContext {
	out := verifyContext{Mode: g.Mode, Signers: slices.Clone(g.Signers)}
	switch {
	case c.mode != "":
		out.Mode = c.mode
	case c.add != "":
		if !slices.Contains(out.Signers, c.add) {
			out.Signers = append(out.Signers, c.add)
			sort.Strings(out.Signers)
		}
	case c.remove != "":
		out.Signers = slices.DeleteFunc(out.Signers, func(l string) bool { return l == c.remove })
	}
	return out
}

func describeChange(c change) string {
	switch {
	case c.mode != "":
		return "verify_signatures → " + c.mode
	case c.add != "":
		return "signer added"
	default:
		return "signer removed"
	}
}

// walk visits every commit reachable from start that is not in stop, parents
// before children (a post-order DFS), each exactly once.
func (v *verifier) walk(start plumbing.Hash, stop map[plumbing.Hash]bool, visit func(*object.Commit)) error {
	seen := map[plumbing.Hash]bool{}
	type frame struct {
		c    *object.Commit
		next int
	}
	root, err := object.GetCommit(v.st, start)
	if err != nil {
		return fmt.Errorf("verify: commit %s: %w", start, err)
	}
	stack := []*frame{{c: root}}
	seen[start] = true
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		if f.next < len(f.c.ParentHashes) {
			p := f.c.ParentHashes[f.next]
			f.next++
			if seen[p] || stop[p] {
				continue
			}
			seen[p] = true
			pc, err := object.GetCommit(v.st, p)
			if err != nil {
				return fmt.Errorf("verify: commit %s: %w", p, err)
			}
			stack = append(stack, &frame{c: pc})
			continue
		}
		stack = stack[:len(stack)-1]
		visit(f.c)
	}
	return nil
}
