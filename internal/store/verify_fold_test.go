package store

import (
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// foldFixture builds signed histories in memory for the fold tests. Every
// commit is signed by the key given (nil = unsigned) with an author email
// claiming that key's fingerprint, like knomit's own commits.
type foldFixture struct {
	t         *testing.T
	r         *memRepo
	op, a, b  ssh.Signer // operator, agent a, agent b
	stranger  ssh.Signer
	root      StaticRoot
	ontPath   string
	baseFiles map[string]string
}

func newFoldFixture(t *testing.T) *foldFixture {
	f := &foldFixture{
		t:        t,
		r:        newMemRepo(t),
		op:       namedSigner(t, "operator"),
		a:        namedSigner(t, "agent-a"),
		b:        namedSigner(t, "agent-b"),
		stranger: namedSigner(t, "stranger"),
		ontPath:  ".knomit/ontology.yaml",
	}
	fp, err := keyFingerprint(f.op.PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	f.root = StaticRoot{Fingerprint: fp}
	f.baseFiles = map[string]string{f.ontPath: f.ont("")}
	return f
}

func keyLine(s ssh.Signer) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey())))
}

// ont renders an ontology with the given mode ("" = no root attributes) and
// signer list.
func (f *foldFixture) ont(mode string, signers ...ssh.Signer) string {
	var b strings.Builder
	b.WriteString("id: x\nname: X\n")
	if mode != "" || len(signers) > 0 {
		b.WriteString("attributes:\n")
		if mode != "" {
			b.WriteString("  verify_signatures: " + mode + "\n")
		}
		if len(signers) > 0 {
			b.WriteString("  verify_signers:\n")
			for _, s := range signers {
				b.WriteString("    - " + keyLine(s) + "\n")
			}
		}
	}
	b.WriteString("topics:\n  notes:\n    description: d\n")
	return b.String()
}

// commit makes a commit with files, parents and signer. authorFP overrides the
// claimed fingerprint ("" = the signer's own; "exp" = an experiment author).
func (f *foldFixture) commit(files map[string]string, signer ssh.Signer, parents ...*object.Commit) *object.Commit {
	return f.commitAs(files, signer, "", parents...)
}

func (f *foldFixture) commitAs(files map[string]string, signer ssh.Signer, authorFP string, parents ...*object.Commit) *object.Commit {
	f.t.Helper()
	r := f.r
	r.n++
	when := time.Unix(1790000000+int64(r.n), 0).UTC()
	claim := authorFP
	if claim == "" && signer != nil {
		fp, _ := keyFingerprint(signer.PublicKey())
		claim = fp[:8]
	}
	email := "unsigned@example.com"
	switch {
	case claim == "exp":
		email = "exp/feat+update@agents.knomit.io"
	case claim != "":
		email = "host-" + claim + "+learn@agents.knomit.io"
	}
	c := &object.Commit{
		Author:    object.Signature{Name: "a", Email: email, When: when},
		Committer: object.Signature{Name: "a", Email: email, When: when},
		Message:   "c",
		TreeHash:  r.tree(files),
	}
	for _, p := range parents {
		c.ParentHashes = append(c.ParentHashes, p.Hash)
	}
	if signer != nil {
		payload, err := commitPayload(c)
		if err != nil {
			f.t.Fatal(err)
		}
		sig, err := signCommit(signer, payload)
		if err != nil {
			f.t.Fatal(err)
		}
		c.PGPSignature = sig
	}
	o := r.st.NewEncodedObject()
	if err := c.Encode(o); err != nil {
		f.t.Fatal(err)
	}
	h, err := r.st.SetEncodedObject(o)
	if err != nil {
		f.t.Fatal(err)
	}
	got, err := object.GetCommit(r.st, h)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func (f *foldFixture) verifier() *verifier { return &verifier{st: f.r.st, root: f.root} }

func (f *foldFixture) rootFold(tip *object.Commit) foldResult {
	f.t.Helper()
	res, err := f.verifier().fold(plumbing.ZeroHash, verifyContext{Mode: VerifyOff}, tip.Hash)
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}

func refusedSet(res foldResult) map[plumbing.Hash]string {
	out := map[plumbing.Hash]string{}
	for _, r := range res.Refused {
		out[plumbing.NewHash(r.Commit)] = r.Rule
	}
	return out
}

// enabled returns a history root → operator enable (mode, signers a and b).
func (f *foldFixture) enabled(mode string) (*object.Commit, map[string]string) {
	root := f.commit(f.baseFiles, nil)
	files := with(f.baseFiles, f.ontPath, f.ont(mode, f.a, f.b))
	return f.commit(files, f.op, root), files
}

// TestFold_OffRepoChecksNothing: default off (proposal test 15). No signature
// or M3 call, whatever the history holds.
func TestFold_OffRepoChecksNothing(t *testing.T) {
	f := newFoldFixture(t)
	c1 := f.commit(f.baseFiles, nil)
	c2 := f.commit(with(f.baseFiles, "kb/x.md", "x"), f.stranger, c1)
	res := f.rootFold(c2)
	if res.Final.Mode != VerifyOff || res.SigChecks != 0 || len(res.Refused) != 0 {
		t.Fatalf("off repo: %+v", res)
	}
}

// TestFold_EnforceRefusesUnsignedAndStranger: tests 1 and the core of R1.
func TestFold_EnforceRefusesUnsignedAndStranger(t *testing.T) {
	f := newFoldFixture(t)
	e, files := f.enabled(VerifyEnforce)
	good := f.commit(with(files, "kb/g.md", "g"), f.a, e)
	unsigned := f.commit(with(files, "kb/g.md", "g", "kb/u.md", "u"), nil, good)
	stranger := f.commit(with(files, "kb/g.md", "g", "kb/u.md", "u", "kb/s.md", "s"), f.stranger, unsigned)
	res := f.rootFold(stranger)
	got := refusedSet(res)
	if got[unsigned.Hash] != RuleSignature || got[stranger.Hash] != RuleSignature {
		t.Fatalf("unsigned and stranger must be refused: %+v", res.Refused)
	}
	if res.NewAnchor != good.Hash {
		t.Fatalf("anchor = %s, want the last good commit %s", res.NewAnchor, good.Hash)
	}
	if !res.Closed() || res.SigChecks == 0 {
		t.Fatalf("enforce must close and check: %+v", res)
	}
}

// TestFold_EnableNeedsTheOperator: a stranger (or an admitted agent) cannot
// turn verification on; the change is rejected and, the repo not enforcing,
// reported and ignored.
func TestFold_EnableNeedsTheOperator(t *testing.T) {
	f := newFoldFixture(t)
	root := f.commit(f.baseFiles, nil)
	e := f.commit(with(f.baseFiles, f.ontPath, f.ont(VerifyEnforce, f.stranger)), f.stranger, root)
	after := f.commit(with(f.baseFiles, f.ontPath, f.ont(VerifyEnforce, f.stranger), "kb/x.md", "x"), f.stranger, e)
	res := f.rootFold(after)
	if res.Final.Mode != VerifyOff {
		t.Fatalf("a stranger's enable must not take effect: %+v", res.Final)
	}
	if len(res.Reported) != 1 || res.Reported[0].Rule != RulePolicyChange {
		t.Fatalf("the rejected enable must be reported: %+v", res.Reported)
	}
	if res.SigChecks != 0 {
		t.Fatalf("still off: nothing is checked, got %d", res.SigChecks)
	}
}

// TestFold_UnrootedIsClosed: V4-2, proposal test 28. With no operator key, an
// enable in history closes the advance before it, in every mode.
func TestFold_UnrootedIsClosed(t *testing.T) {
	f := newFoldFixture(t)
	f.root = StaticRoot{}
	root := f.commit(f.baseFiles, nil)
	pre := f.commit(with(f.baseFiles, "kb/p.md", "p"), nil, root)
	e := f.commit(with(f.baseFiles, "kb/p.md", "p", f.ontPath, f.ont(VerifyLog, f.a)), f.op, pre)
	res := f.rootFold(e)
	if !res.Unrooted || !res.Closed() {
		t.Fatalf("unrooted must close: %+v", res)
	}
	if refusedSet(res)[e.Hash] != RuleUnrooted {
		t.Fatalf("the enable must be refused as unrooted: %+v", res.Refused)
	}
	if res.NewAnchor != pre.Hash {
		t.Fatalf("anchor = %s, want the first-parent predecessor of the enable %s", res.NewAnchor, pre.Hash)
	}
}

// TestFold_EnableOnASecondParent: proposal test 18. [garbage1, E, garbage2]
// on a side branch merged into an off main: garbage2 is checked (not an
// ancestor of E), garbage1 is baseline (an ancestor of E).
func TestFold_EnableOnASecondParent(t *testing.T) {
	f := newFoldFixture(t)
	root := f.commit(f.baseFiles, nil)
	g1 := f.commit(with(f.baseFiles, "kb/g1.md", "1"), nil, root)
	eFiles := with(f.baseFiles, "kb/g1.md", "1", f.ontPath, f.ont(VerifyEnforce, f.a))
	e := f.commit(eFiles, f.op, g1)
	g2 := f.commit(with(eFiles, "kb/g2.md", "2"), nil, e)
	mainC := f.commit(with(f.baseFiles, "kb/m.md", "m"), f.a, root)
	merged := with(eFiles, "kb/g2.md", "2", "kb/m.md", "m")
	m := f.commit(merged, nil, mainC, g2)
	res := f.rootFold(m)
	got := refusedSet(res)
	if _, ok := got[g1.Hash]; ok {
		t.Fatal("garbage1 is an ancestor of the enable: baseline, not checked")
	}
	if got[g2.Hash] != RuleSignature {
		t.Fatalf("garbage2 must be checked and refused: %+v", res.Refused)
	}
}

// TestFold_ForkFromBeforeTheEnable: proposal test 25. A branch cut before the
// enable and merged after it is checked.
func TestFold_ForkFromBeforeTheEnable(t *testing.T) {
	f := newFoldFixture(t)
	root := f.commit(f.baseFiles, nil)
	fork := f.commit(with(f.baseFiles, "kb/evil.md", "x"), nil, root)
	e, files := func() (*object.Commit, map[string]string) {
		files := with(f.baseFiles, f.ontPath, f.ont(VerifyEnforce, f.a))
		return f.commit(files, f.op, root), files
	}()
	m := f.commit(with(files, "kb/evil.md", "x"), nil, e, fork)
	res := f.rootFold(m)
	got := refusedSet(res)
	if got[fork.Hash] != RuleSignature {
		t.Fatalf("a fork cut before the enable must be checked and refused: %+v", res.Refused)
	}
	if got[m.Hash] != RuleMerge {
		t.Fatalf("the merge of a refused parent must be refused: %+v", res.Refused)
	}
	if res.NewAnchor != e.Hash {
		t.Fatalf("anchor = %s, want the enable %s", res.NewAnchor, e.Hash)
	}
}

// TestFold_Onboarding: proposal test 19. D (signed by a new key K) made BEFORE
// the operator adds K at C passes once C is in the history, and so does the
// new machine's own merge after C.
func TestFold_Onboarding(t *testing.T) {
	f := newFoldFixture(t)
	k := namedSigner(t, "new-machine")
	e, files := f.enabled(VerifyEnforce)
	d := f.commit(with(files, "kb/d.md", "d"), k, e)
	cFiles := with(files, f.ontPath, f.ont(VerifyEnforce, f.a, f.b, k))
	c := f.commit(cFiles, f.op, e)
	merge := f.commit(with(cFiles, "kb/d.md", "d"), k, c, d)
	res := f.rootFold(merge)
	if len(res.Refused) != 0 {
		t.Fatalf("onboarding must pass in the realistic order: %+v", res.Refused)
	}
	if res.NewAnchor != merge.Hash {
		t.Fatalf("anchor must pass everything: %s", res.NewAnchor)
	}

	// An unsigned addition of the stranger, then the stranger's commit.
	sFiles := with(cFiles, f.ontPath, f.ont(VerifyEnforce, f.a, f.b, k, f.stranger))
	add := f.commit(sFiles, nil, merge)
	bad := f.commit(with(sFiles, "kb/s.md", "s"), f.stranger, add)
	res = f.rootFold(bad)
	got := refusedSet(res)
	if got[add.Hash] == "" || got[bad.Hash] == "" {
		t.Fatalf("an unauthorised addition and its key's commit must be refused: %+v", res.Refused)
	}
}

// TestFold_RevocationByFork: proposal test 26. K removed at R: a K commit on
// a branch cut before R and merged after R is refused; K's commits that are
// ancestors of R stay accepted.
func TestFold_RevocationByFork(t *testing.T) {
	f := newFoldFixture(t)
	e, files := f.enabled(VerifyEnforce)
	early := f.commit(with(files, "kb/early.md", "e"), f.b, e)
	forked := f.commit(with(files, "kb/late.md", "l"), f.b, e) // cut before R
	rFiles := with(files, "kb/early.md", "e", f.ontPath, f.ont(VerifyEnforce, f.a))
	r := f.commit(rFiles, f.op, early)
	m := f.commit(with(rFiles, "kb/late.md", "l"), f.a, r, forked)
	res := f.rootFold(m)
	got := refusedSet(res)
	if _, ok := got[early.Hash]; ok {
		t.Fatal("K's commit before the removal must stay accepted")
	}
	if got[forked.Hash] != RuleSignature {
		t.Fatalf("K's commit on a fork cut before the removal must be refused: %+v", res.Refused)
	}
	if res.NewAnchor != r.Hash {
		t.Fatalf("anchor = %s, want the removal %s", res.NewAnchor, r.Hash)
	}
}

// TestFold_RejectedChangeInEnforceRefusesTheCommit: proposal test 27 (V4-4).
func TestFold_RejectedChangeInEnforceRefusesTheCommit(t *testing.T) {
	f := newFoldFixture(t)
	e, files := f.enabled(VerifyEnforce)
	relax := f.commit(with(files, f.ontPath, f.ont(VerifyOff, f.a, f.b)), f.a, e) // admitted, not the operator
	res := f.rootFold(relax)
	if refusedSet(res)[relax.Hash] != RulePolicyChange {
		t.Fatalf("enforce: an unauthorised relaxation refuses its commit: %+v", res.Refused)
	}
	if res.Final.Mode != VerifyEnforce {
		t.Fatalf("the relaxation must not take effect: %s", res.Final.Mode)
	}

	// The same in log: reported, ignored, the commit otherwise judged (and fine).
	f2 := newFoldFixture(t)
	e2, files2 := f2.enabled(VerifyLog)
	relax2 := f2.commit(with(files2, f2.ontPath, f2.ont(VerifyOff, f2.a, f2.b)), f2.a, e2)
	res2 := f2.rootFold(relax2)
	if len(res2.Refused) != 0 || len(res2.Reported) != 1 || res2.Final.Mode != VerifyLog {
		t.Fatalf("log: reported and ignored: %+v", res2)
	}
}

// TestFold_TightenToEnforceByAnAdmittedSigner: log → enforce needs only an
// admitted signer; a stranger's tightening is ignored (V3-6).
func TestFold_TightenToEnforceByAnAdmittedSigner(t *testing.T) {
	f := newFoldFixture(t)
	e, files := f.enabled(VerifyLog)
	up := f.commit(with(files, f.ontPath, f.ont(VerifyEnforce, f.a, f.b)), f.a, e)
	if res := f.rootFold(up); res.Final.Mode != VerifyEnforce {
		t.Fatalf("an admitted signer may tighten to enforce: %+v", res.Final)
	}
	f2 := newFoldFixture(t)
	e2, files2 := f2.enabled(VerifyLog)
	up2 := f2.commit(with(files2, f2.ontPath, f2.ont(VerifyEnforce, f2.a, f2.b)), f2.stranger, e2)
	if res := f2.rootFold(up2); res.Final.Mode != VerifyLog {
		t.Fatalf("a stranger's tightening must be ignored: %+v", res.Final)
	}
}

// TestFold_LogFailureIsRefusedUnderEnforce: proposal test 29 (V5-1). A commit
// that failed in log, then an accepted log→enforce after it: the failure is an
// ancestor of the tightening and is still refused.
func TestFold_LogFailureIsRefusedUnderEnforce(t *testing.T) {
	f := newFoldFixture(t)
	e, files := f.enabled(VerifyLog)
	g := f.commit(with(files, "kb/g.md", "g"), nil, e)
	s := f.commit(with(files, "kb/g.md", "g", f.ontPath, f.ont(VerifyEnforce, f.a, f.b)), f.op, g)
	res := f.rootFold(s)
	if res.Final.Mode != VerifyEnforce || refusedSet(res)[g.Hash] != RuleSignature {
		t.Fatalf("the log-mode failure must be refused once enforce arrives: %+v", res)
	}
	if res.NewAnchor != e.Hash {
		t.Fatalf("anchor = %s, want the enable %s", res.NewAnchor, e.Hash)
	}
}

// TestFold_AuthorClaim: proposal test 2.
func TestFold_AuthorClaim(t *testing.T) {
	f := newFoldFixture(t)
	e, files := f.enabled(VerifyEnforce)
	bFP, _ := keyFingerprint(f.b.PublicKey())
	lie := f.commitAs(with(files, "kb/l.md", "l"), f.a, bFP[:8], e)
	res := f.rootFold(lie)
	if refusedSet(res)[lie.Hash] != RuleAuthorClaim {
		t.Fatalf("an author claiming b but signed by a must be refused: %+v", res.Refused)
	}
	exp := f.commitAs(with(files, "kb/x.md", "x"), f.a, "exp", e)
	if res := f.rootFold(exp); len(res.Refused) != 0 {
		t.Fatalf("an experiment author claims nothing and must pass: %+v", res.Refused)
	}
}

// TestFold_UnreadableAndShadow: proposal test 20 and #312 item (b).
func TestFold_UnreadableAndShadow(t *testing.T) {
	f := newFoldFixture(t)
	e, files := f.enabled(VerifyEnforce)
	bad := f.commit(with(files, f.ontPath, strings.Replace(f.ont(VerifyEnforce, f.a, f.b), "enforce", "yes", 1)), f.op, e)
	res := f.rootFold(bad)
	if refusedSet(res)[bad.Hash] != RuleUnreadable || res.Final.Mode != VerifyEnforce {
		t.Fatalf("an invalid value is UNKNOWN, refused in enforce, never off: %+v", res)
	}

	// A repo living on the legacy path; a commit adds the canonical file without the keys.
	g := newFoldFixture(t)
	g.ontPath = ".domains/ontology.yaml"
	g.baseFiles = map[string]string{g.ontPath: g.ont("")}
	le, lfiles := g.enabled(VerifyEnforce)
	shadow := g.commit(with(lfiles, ".knomit/ontology.yaml", g.ont("")), g.a, le)
	res = g.rootFold(shadow)
	if refusedSet(res)[shadow.Hash] != RulePolicyChange || res.Final.Mode != VerifyEnforce {
		t.Fatalf("a shadowing canonical file is an unauthorised relaxation: %+v", res)
	}
}

// TestFold_Rewind: an anchor that is not an ancestor of the tip.
func TestFold_Rewind(t *testing.T) {
	f := newFoldFixture(t)
	e, files := f.enabled(VerifyEnforce)
	a1 := f.commit(with(files, "kb/1.md", "1"), f.a, e)
	other := f.commit(with(files, "kb/2.md", "2"), f.a, e)
	res, err := f.verifier().fold(a1.Hash, verifyContext{Mode: VerifyEnforce, Signers: []string{keyLine(f.a), keyLine(f.b)}}, other.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Rewind || res.NewAnchor != a1.Hash {
		t.Fatalf("rewind must keep the anchor: %+v", res)
	}
}

// TestFold_AcceptWaivesASignatureOnly: V5-2, proposal test 30.
func TestFold_AcceptWaivesASignatureOnly(t *testing.T) {
	f := newFoldFixture(t)
	e, files := f.enabled(VerifyEnforce)
	unsigned := f.commit(with(files, "kb/u.md", "u"), nil, e)
	relax := f.commit(with(files, "kb/u.md", "u", f.ontPath, f.ont(VerifyOff, f.a, f.b)), nil, unsigned)
	v := f.verifier()
	v.accepted = func(h plumbing.Hash) bool { return h == unsigned.Hash || h == relax.Hash }
	res, err := v.fold(plumbing.ZeroHash, verifyContext{Mode: VerifyOff}, relax.Hash)
	if err != nil {
		t.Fatal(err)
	}
	got := refusedSet(res)
	if _, ok := got[unsigned.Hash]; ok {
		t.Fatal("--accept must waive the unsigned commit's signature")
	}
	if got[relax.Hash] != RulePolicyChange || res.Final.Mode != VerifyEnforce {
		t.Fatalf("--accept must never waive a policy change: %+v", res)
	}
}

// TestFold_GitHubMergeInContext: proposal test 7 in the fold. An unsigned M3
// merge of accepted parents is accepted; the anchor passes it.
func TestFold_GitHubMergeInContext(t *testing.T) {
	f := newFoldFixture(t)
	e, files := f.enabled(VerifyEnforce)
	left := f.commit(with(files, "kb/l.md", "l"), f.a, e)
	right := f.commit(with(files, "kb/r.md", "r"), f.b, e)
	m := f.commit(with(files, "kb/l.md", "l", "kb/r.md", "r"), nil, left, right)
	res := f.rootFold(m)
	if len(res.Refused) != 0 || res.NewAnchor != m.Hash {
		t.Fatalf("a clean unsigned merge of accepted parents must pass: %+v", res)
	}
}

// TestFold_AnchorContextComesFromTheFold: V5-2. In log, a commit whose file
// carries an ignored relaxation can be anchored; the context AT it is still
// log, never the file's off.
func TestFold_AnchorContextComesFromTheFold(t *testing.T) {
	f := newFoldFixture(t)
	e, files := f.enabled(VerifyLog)
	relax := f.commit(with(files, f.ontPath, f.ont(VerifyOff, f.a, f.b)), f.a, e)
	res := f.rootFold(relax)
	if res.NewAnchor != relax.Hash || res.NewAnchorCtx.Mode != VerifyLog {
		t.Fatalf("anchor context must be the fold's (log), not the file's (off): %+v", res)
	}
}

// TestFold_ConcurrentChangesAreOrderIndependent: R3-1. The operator adds key K
// on branch A while admitted agent a tightens log → enforce on sibling B; the
// operator merges both. The tightening must be accepted in BOTH parent orders:
// each commit is judged against its own ancestry, never a running context in
// which A's addition makes B look like it removes K.
func TestFold_ConcurrentChangesAreOrderIndependent(t *testing.T) {
	for _, aFirst := range []bool{true, false} {
		f := newFoldFixture(t)
		k := namedSigner(t, "k")
		root := f.commit(f.baseFiles, nil)
		e := f.commit(with(f.baseFiles, f.ontPath, f.ont(VerifyLog, f.a)), f.op, root)
		addK := f.commit(with(f.baseFiles, f.ontPath, f.ont(VerifyLog, f.a, k)), f.op, e)
		tighten := f.commit(with(f.baseFiles, f.ontPath, f.ont(VerifyEnforce, f.a)), f.a, e)
		both := with(f.baseFiles, f.ontPath, f.ont(VerifyEnforce, f.a, k))
		var m *object.Commit
		if aFirst {
			m = f.commit(both, f.op, addK, tighten)
		} else {
			m = f.commit(both, f.op, tighten, addK)
		}
		res := f.rootFold(m)
		require.Empty(t, res.Refused, "aFirst=%v: concurrent legitimate changes must both pass", aFirst)
		require.Empty(t, res.Reported, "aFirst=%v", aFirst)
		require.Equal(t, VerifyEnforce, res.Final.Mode, "aFirst=%v", aFirst)
		require.Len(t, res.Final.Signers, 2, "aFirst=%v: K was added", aFirst)
		require.Equal(t, m.Hash, res.NewAnchor, "aFirst=%v", aFirst)
	}
}

// TestFold_MergeAdoptingAnOlderPolicyIsJudged: R3-1, second symptom. After an
// operator enable, an admitted agent signs a merge of main and a branch forked
// before the enable, taking the fork's (off) ontology. The merge equals one
// parent's file, but not its ancestry's context: it is a relaxation, refused
// in enforce and reported in log.
func TestFold_MergeAdoptingAnOlderPolicyIsJudged(t *testing.T) {
	for _, mode := range []string{VerifyEnforce, VerifyLog} {
		for _, mainFirst := range []bool{true, false} {
			f := newFoldFixture(t)
			root := f.commit(f.baseFiles, nil)
			fork := f.commit(with(f.baseFiles, "kb/f.md", "f"), f.a, root)
			enFiles := with(f.baseFiles, f.ontPath, f.ont(mode, f.a))
			e := f.commit(enFiles, f.op, root)
			old := with(f.baseFiles, "kb/f.md", "f") // the fork's ontology: no attributes
			var m *object.Commit
			if mainFirst {
				m = f.commit(old, f.a, e, fork)
			} else {
				m = f.commit(old, f.a, fork, e)
			}
			res := f.rootFold(m)
			var rule string
			if mode == VerifyEnforce {
				rule = refusedSet(res)[m.Hash]
			} else {
				for _, r := range res.Reported {
					if r.Commit == m.Hash.String() {
						rule = r.Rule
					}
				}
			}
			require.Equal(t, RulePolicyChange, rule, "mode=%s mainFirst=%v: the merge adopting an older policy must be judged", mode, mainFirst)
			require.Equal(t, mode, res.Final.Mode, "the relaxation never takes effect")
		}
	}
}

// TestFold_AuthorClaimIsCaseInsensitive: R3-2. An uppercase or mixed-case
// claim of another agent's fingerprint is refused like a lowercase one.
func TestFold_AuthorClaimIsCaseInsensitive(t *testing.T) {
	for _, variant := range []func(string) string{strings.ToUpper, func(s string) string { return strings.ToUpper(s[:4]) + s[4:] }} {
		f := newFoldFixture(t)
		e, files := f.enabled(VerifyEnforce)
		bFP, _ := keyFingerprint(f.b.PublicKey())
		lie := f.commitAs(with(files, "kb/l.md", "l"), f.a, variant(bFP[:8]), e)
		res := f.rootFold(lie)
		require.Equal(t, RuleAuthorClaim, refusedSet(res)[lie.Hash], "claim %q signed by a must be refused", variant(bFP[:8]))
	}
}

// TestFold_AcceptWaivesAnUnsignedMergeFailingM3: D3-1 (a). The criss-cross
// auto-merge shape (aa7e5e12 on cyberai-kb) is refused by M3, and accepted once
// its hash is on the accept list. The waiver never covers an unaccepted parent.
func TestFold_AcceptWaivesAnUnsignedMergeFailingM3(t *testing.T) {
	f := newFoldFixture(t)
	e, files := f.enabled(VerifyEnforce)
	x := f.commit(with(files, "kb/x.md", "x"), f.a, e)
	y := f.commit(with(files, "kb/y.md", "y"), f.b, e)
	both := with(files, "kb/x.md", "x", "kb/y.md", "y")
	xy := f.commit(both, f.a, x, y)
	yx := f.commit(both, f.b, y, x)
	cc := f.commit(both, nil, xy, yx) // unsigned criss-cross
	res := f.rootFold(cc)
	require.Equal(t, RuleMerge, refusedSet(res)[cc.Hash], "precondition: M3 refuses the criss-cross")

	v := f.verifier()
	v.accepted = func(h plumbing.Hash) bool { return h == cc.Hash }
	res, err := v.fold(plumbing.ZeroHash, verifyContext{Mode: VerifyOff}, cc.Hash)
	require.NoError(t, err)
	require.Empty(t, res.Refused, "an accepted criss-cross merge passes")
	require.Equal(t, cc.Hash, res.NewAnchor)

	// An unaccepted (refused) parent is not waived by accepting the merge.
	bad := f.commit(with(files, "kb/z.md", "z"), nil, e)
	m := f.commit(with(files, "kb/x.md", "x", "kb/z.md", "z"), nil, x, bad)
	v.accepted = func(h plumbing.Hash) bool { return h == m.Hash }
	res, err = v.fold(plumbing.ZeroHash, verifyContext{Mode: VerifyOff}, m.Hash)
	require.NoError(t, err)
	require.Equal(t, RuleMerge, refusedSet(res)[m.Hash], "accepting a merge never waives its refused parent")
}
