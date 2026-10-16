package store

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/platform/version"
)

// PushPolicy is what the HTTP edge knows about ONE receive-pack request and
// the store does not: who is pushing and what this repo's instance allows.
// internal/web builds it per request from the principal and the repo and
// attaches it with WithPushPolicy; the store handler is built once and cached
// (Service.Handler), so the policy can never be a construction argument.
//
// A request without a policy is refused: receive-pack is reachable only
// through an edge that decided who is asking.
type PushPolicy struct {
	// Pusher is the full 64-hex key fingerprint of the pushing instance (the
	// certificate principal's ID). Empty when Refusal is set. It decides no
	// ref: it is compared, in full, with each new commit's SSHSIG signer
	// (signer = pusher), never with any part of a branch name.
	Pusher string
	// Refusal, when non-empty, refuses the whole request with this text: a
	// principal that is not an instance certificate, one without push:own,
	// a subscription. Decided at the edge, where the principal is.
	Refusal string
	// OwnBranch is this host's own agent branch, which is never written
	// through receive-pack.
	OwnBranch string
	// Fleet returns the fleet repository's member records and certificate
	// artifacts, both read from its main at ONE revision. Called only when
	// verify_signatures is log or enforce; nil or an error means this instance
	// has no fleet to judge against.
	Fleet func(context.Context) (Fleet, error)
	// Judged, when set, is told the commit judge's verdict on the pushed
	// branch: refusal is the text the pusher was refused with by the
	// private-path closure or verify, or "" when the push was admitted and
	// registered. Whole-request refusals (the policy's, the one-ref rule's)
	// and broken packs are not verdicts and are not told. tip is the pushed
	// tip, agent its author's agent id ("" when not an agent address). The
	// fleet hub keeps it as the master's refusals (repos.FleetPushJudged).
	Judged func(branch, tip, agent, refusal string)
}

type pushPolicyKey struct{}

// WithPushPolicy attaches p to ctx for the receive-pack handler.
func WithPushPolicy(ctx context.Context, p PushPolicy) context.Context {
	return context.WithValue(ctx, pushPolicyKey{}, p)
}

func pushPolicyFrom(ctx context.Context) (PushPolicy, bool) {
	p, ok := ctx.Value(pushPolicyKey{}).(PushPolicy)
	return p, ok
}

// errNoPushPolicy refuses a receive-pack request that did not come through an
// edge that decided who is pushing.
const errNoPushPolicy = "knomit: pushing is not available here: no verified pusher"

// receivePack serves git's receive-pack for one store: an enrolled peer
// updating EXACTLY one agent/* branch that is not this host's own (F11). It is knomit's own code over
// go-git's codec, as upload-pack is: go-git's server writes the pack into the
// store before any ref is judged, advertises every ref and delete-refs, and
// ignores the old hash — each of which F11 forbids.
//
// The order of a POST is load-bearing:
//
//  1. the whole-request refusals and the ONE-ref rule, from the commands
//     alone — all or nothing, so a push naming the own branch and anything
//     else promotes nothing;
//  2. the pack into a quarantine (memory; host reads for thin-pack bases);
//  3. the new commits (a walk that stops at the host's commits): refused in
//     every mode when one's private paths differ from its merge base with
//     the upstream (privatePathClosure), then
//     judged when verify_signatures at the host's upstream says so;
//  4. under the branch lock, on a context that cannot be cancelled: the
//     old-hash check, ONE transaction promoting the new objects, then the
//     branch seeded from the upstream (first push) and moved through
//     advanceBranchTo, which ends in notifyCommit.
//
// Nothing before 4 writes to the host; nothing in 4 may be abandoned by a
// client that disconnects, because the ref moves before the SQL that
// describes it.
type receivePack struct {
	rh       *repoHandler
	upstream func() string
}

func (rp *receivePack) advertise(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/x-git-receive-pack-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	refuse := func(msg string) {
		// After the service header, as upload-pack's refusal: that is the
		// position both git and go-git render as the remote's message.
		e := pktline.NewEncoder(w)
		_ = e.Encodef("# service=git-receive-pack\n")
		_ = e.Flush()
		_ = e.Encodef("ERR %s\n", msg)
	}
	pol, ok := pushPolicyFrom(r.Context())
	if !ok {
		refuse(errNoPushPolicy)
		return
	}
	if pol.Refusal != "" {
		refuse(pol.Refusal)
		return
	}
	curated, _, err := buildAdvRefs(rp.rh, rp.upstream(), nil)
	if err != nil {
		refuse(err.Error())
		return
	}
	// The SAME curated view upload-pack serves (it is the client's haves, so
	// it keeps the pack small), minus HEAD: receive-pack never advertises one.
	ar := packp.NewAdvRefs()
	for name, h := range curated.References {
		ar.References[name] = h
	}
	caps := ar.Capabilities
	_ = caps.Set(capability.Agent, "knomit/"+version.Version)
	_ = caps.Set(capability.ReportStatus)
	_ = caps.Set(capability.Sideband64k)
	_ = caps.Set(capability.OFSDelta)
	// delete-refs is deliberately absent: a push never deletes a branch here.
	ar.Prefix = [][]byte{[]byte("# service=git-receive-pack"), pktline.Flush}
	_ = ar.Encode(w)
}

// pushReply is where a POST's answer goes: band 1 of a sideband muxer when
// the client asked for one, else the raw response.
type pushReply struct {
	w   http.ResponseWriter
	out io.Writer
	mux *sideband.Muxer
}

func (p *pushReply) progress(format string, args ...any) {
	if p.mux != nil {
		_, _ = p.mux.WriteChannel(sideband.ProgressMessage, []byte(fmt.Sprintf(format, args...)))
	}
}

func (p *pushReply) end() {
	if p.mux != nil {
		// The flush-pkt ends a sideband stream (as upload-pack's does).
		_ = pktline.NewEncoder(p.w).Flush()
	}
}

// refuse sends the ERR pkt that names the rule (F11 R5). git renders it as
// "fatal: remote error: <msg>", go-git returns <msg> as its error.
func (p *pushReply) refuse(msg string) {
	_ = pktline.NewEncoder(p.out).Encodef("ERR %s\n", msg)
	p.end()
}

func (rp *receivePack) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := gitRequestBody(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer body.Close()
	req := packp.NewReferenceUpdateRequest()
	if err := req.Decode(body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	reply := &pushReply{w: w, out: w}
	if sb, ok := requestedSideband(req.Capabilities); ok {
		reply.mux = sideband.NewMuxer(sb, w)
		reply.out = reply.mux
	}
	pol, _ := pushPolicyFrom(r.Context())
	lg := log.With().Str("repo", rp.rh.name).Str("pusher", pol.Pusher).Logger()

	// Refusals from the commands alone read nothing more: the client is
	// still sending its pack, and an answer written before the body is
	// consumed can reach it as a broken pipe instead of the message.
	refuseUnread := func(msg string) {
		if req.Packfile != nil {
			_, _ = io.Copy(io.Discard, req.Packfile)
		}
		lg.Warn().Str("rule", msg).Msg("receive-pack: refused")
		reply.refuse(msg)
	}

	cmd, msg := rp.judgeRequest(r.Context(), req)
	if msg != "" {
		refuseUnread(msg)
		return
	}
	lg = lg.With().Str("ref", cmd.Name.String()).Logger()
	refuse := func(msg string) {
		lg.Warn().Str("rule", msg).Msg("receive-pack: refused")
		reply.refuse(msg)
	}

	q := newQuarantine(rp.rh.gits)
	var pack *bufio.Reader
	if req.Packfile != nil {
		// Decode leaves the rest of the body here even when nothing follows
		// the commands (an update to a commit the host already holds).
		pack = bufio.NewReader(req.Packfile)
		if _, err := pack.Peek(1); err == io.EOF {
			pack = nil
		}
	}
	if pack != nil {
		parser, err := packfile.NewParserWithStorage(packfile.NewScanner(pack), q)
		if err == nil {
			_, err = parser.Parse()
		}
		if err != nil {
			refuse(fmt.Sprintf("knomit: the pack could not be read: %v", err))
			return
		}
	}
	if _, err := object.GetCommit(q, cmd.New); err != nil {
		refuse(fmt.Sprintf("knomit: %s would point at %s, which is not a commit this push sent or this host holds", cmd.Name, cmd.New))
		return
	}
	commits, _, err := q.newCommits(cmd.New, rp.rh.heldAdmitted(r.Context(), rp.upstream()))
	if err != nil {
		refuse(fmt.Sprintf("knomit: the pushed history could not be read: %v", err))
		return
	}
	objs, err := q.reachableNew(commits)
	if err != nil {
		refuse(fmt.Sprintf("knomit: the push is incomplete: %v", err))
		return
	}
	branch := cmd.Name.Short()
	judged := func(refusal string) {
		if pol.Judged == nil {
			return
		}
		agent := ""
		if c, err := object.GetCommit(q, cmd.New); err == nil {
			agent = agentIDOfAuthor(c.Author.Email)
		}
		pol.Judged(branch, cmd.New.String(), agent, refusal)
	}
	if msg := rp.privatePathClosure(r.Context(), q, commits); msg != "" {
		judged(msg)
		refuse(msg)
		return
	}
	if msg := rp.verify(r.Context(), pol, commits, lg); msg != "" {
		judged(msg)
		refuse(msg)
		return
	}

	res, seeded, msg, err := rp.register(r.Context(), cmd, objs)
	if msg != "" {
		refuse(msg)
		return
	}
	if err != nil {
		lg.Error().Err(err).Msg("receive-pack: registration failed")
		reply.refuse(fmt.Sprintf("knomit: %s could not be registered: %v", cmd.Name, err))
		return
	}
	judged("")

	ev := lg.Info().Str("old", shortRefHash(cmd.Old)).Str("new", shortRefHash(cmd.New)).
		Int("commits", len(commits)).Int("objects", len(objs))
	switch {
	case seeded:
		ev.Msg("receive-pack: registered")
	case res.Mode == ModeRewound:
		// A real non-fast-forward of an existing agent/ branch: allowed, by
		// design. No branch is owned (D18/D19), so ANY authenticated pusher
		// may update or rewind ANY peer's agent/ branch here, not only the
		// peer whose commits are on it; commits the host had not merged yet
		// disappear from this branch until someone pushes them again. Worth
		// one line above INFO, naming the pusher (on lg).
		lg.Warn().Str("old", shortRefHash(cmd.Old)).Str("new", shortRefHash(cmd.New)).
			Int("commits", len(commits)).Int("objects", len(objs)).
			Msg("receive-pack: force-updated (not a descendant of the previous tip)")
	default:
		ev.Msg("receive-pack: updated")
	}
	reply.progress("knomit: received %d objects\n", len(objs))
	reply.progress("knomit: %s registered at %s\n", branch, cmd.New)
	if req.Capabilities.Supports(capability.ReportStatus) {
		rs := packp.NewReportStatus()
		rs.UnpackStatus = "ok"
		rs.CommandStatuses = []*packp.CommandStatus{{ReferenceName: cmd.Name, Status: "ok"}}
		_ = rs.Encode(reply.out)
	}
	reply.end()
}

// judgeRequest applies every rule that the commands and the policy decide,
// to the WHOLE request: the answer is one command to apply, or the refusal
// text. Never a subset — a push naming the own branch and anything else is
// refused whole (F11 review M3).
func (rp *receivePack) judgeRequest(ctx context.Context, req *packp.ReferenceUpdateRequest) (*packp.Command, string) {
	pol, ok := pushPolicyFrom(ctx)
	switch {
	case !ok:
		return nil, errNoPushPolicy
	case pol.Refusal != "":
		return nil, pol.Refusal
	case pol.Pusher == "":
		return nil, errNoPushPolicy
	}
	if _, err := rp.rh.resolveRef(ctx, rp.upstream()); err != nil {
		return nil, fmt.Sprintf("%s: %q", errUpstreamMissing.Error(), rp.upstream())
	}
	if len(req.Commands) != 1 {
		return nil, fmt.Sprintf("knomit: a push may update exactly one ref, refs/heads/agent/<your branch>; this push names %d", len(req.Commands))
	}
	// No rule here asks whose branch the ref is. Branch ownership is not an
	// identity property (D18/D19): a forge cannot enforce it, so a knomit host
	// does not depend on it either, and nothing parses a fingerprint out of a
	// ref name. Any enrolled pusher may update any agent/* ref other than this
	// host's own; what keeps a branch's CONTENT honest is the commit judge plus
	// signer = pusher (verify) and the merge-time dot-path guard.
	cmd := req.Commands[0]
	name := cmd.Name.String()
	switch {
	case cmd.Action() == packp.Delete:
		return nil, fmt.Sprintf("knomit: branch deletion is not allowed: %s", name)
	case name == "refs/heads/"+rp.upstream():
		// Checked on its own, before the agent/ prefix: the upstream (the
		// consensus/default branch) is never pushed, even when its recorded
		// name happens to sit under agent/.
		return nil, fmt.Sprintf("knomit: %s is this repo's upstream branch; it is never pushed", name)
	case !strings.HasPrefix(name, "refs/heads/agent/"):
		return nil, fmt.Sprintf("knomit: only agent branches may be pushed; %s is not an agent branch", name)
	case pol.OwnBranch != "" && name == "refs/heads/"+pol.OwnBranch:
		return nil, fmt.Sprintf("knomit: %s is this host's own branch; it is written only by this host", name)
	}
	return cmd, ""
}

// verify judges the pushed commits when verify_signatures, read at the HOST's
// upstream tip, says so (F11 D3): off → nothing (the fleet is never touched);
// log → each refusal logged, the push accepted; enforce → the first refusal
// refuses the push. The mode is read at the host's upstream, as CheckRange
// reads it at Main: a pushed ontology cannot choose its own check. Returns
// the refusal text, or "".
//
// Beyond judgeCommit (the rules CheckRange shares), a signed new commit must
// be signed by the PUSHER (signerIsPusher): a member pushes only what its own
// key signed, never another active member's commits. The rule rides the mode
// like every other judgement here: enforce refuses, log logs, off checks
// nothing — under off and log an unsigned commit is accepted anyway, so a
// signer rule there would stop nobody.
func (rp *receivePack) verify(ctx context.Context, pol PushPolicy, commits []*object.Commit, lg zerolog.Logger) string {
	return rp.rh.judgeIncoming(ctx, rp.upstream(), pol.Fleet, func(c *object.Commit) (Refusal, bool) {
		return signerIsPusher(c, pol.Pusher)
	}, commits, lg)
}

// CommitRefusedByJudge is the part of judgeIncoming's refusal text that says
// the judge refused a commit ("knomit: commit <hash> refused by
// verify_signatures=enforce: <rule>: <reason>"). A pusher's recorded push
// error carries it, which is how a member's status tells the host refusing
// a commit of its branch from a failure a retry can cure.
const CommitRefusedByJudge = "refused by verify_signatures=enforce"

// judgeIncoming is the judge where a branch ENTERS this repository, shared by
// the two entries: receive-pack (a push to a knomit host, verify) and the
// fleet master's fetch (AdmitPeerBranch, a forge). It reads verify_signatures
// at upstream's tip, loads the fleet from fleet, and judges every commit with
// judgeCommit; signer, when not nil, is one more rule for a commit judgeCommit
// accepted (receive-pack's signer = pusher; a fetch has no pusher). Returns
// the refusal text, or "".
func (rh *repoHandler) judgeIncoming(ctx context.Context, upstream string, fleetOf func(context.Context) (Fleet, error), signer func(*object.Commit) (Refusal, bool), commits []*object.Commit, lg zerolog.Logger) string {
	mode, ont, err := rh.verifyModeAt(ctx, upstream)
	if err != nil {
		return fmt.Sprintf("knomit: verify_signatures at %s could not be read: %v", upstream, err)
	}
	if mode == VerifyOff || len(commits) == 0 {
		return ""
	}
	var fleet Fleet
	if fleetOf == nil {
		err = ErrNoFleet
	} else {
		fleet, err = fleetOf(ctx)
	}
	if err != nil {
		if mode == VerifyEnforce {
			return fmt.Sprintf("knomit: verify_signatures is enforce but this instance has no fleet repository to judge against: %v", err)
		}
		lg.Warn().Err(err).Msg("receive-pack: verify_signatures is log but there is no fleet repository; commits not judged")
		return ""
	}
	// Rule A (self-registration) exists only where member records live: this
	// repository must itself be the fleet repository (its ontology at the
	// upstream is the fleet ontology).
	selfRoot := ""
	if fact.IsFleetOntology(ont) {
		if selfRoot = rh.factRoot; selfRoot == "" {
			selfRoot = "kb"
		}
	}
	gate := newJudge(fleet, selfRoot, ont, rh.factRoot)
	// A member's branch, never this instance's own: the host-record rule
	// applies (hostRecord).
	gate.entry = true
	for _, c := range commits {
		ref, ok := gate.judgeCommit(c)
		if ok && signer != nil {
			ref, ok = signer(c)
		}
		if ok {
			continue
		}
		if mode == VerifyEnforce {
			return fmt.Sprintf("knomit: commit %s %s: %s: %s", c.Hash, CommitRefusedByJudge, ref.Rule, ref.Reason)
		}
		lg.Warn().Str("commit", c.Hash.String()).Str("rule", ref.Rule).Str("reason", ref.Reason).
			Msg("receive-pack: commit would be refused (verify_signatures=log)")
	}
	return ""
}

// signerIsPusher refuses a signed commit whose SSHSIG key is not the pusher's
// certificate key. Called only for a commit judgeCommit accepted, so a commit
// without a verifiable signature here is a two-parent merge that passed M3
// (it adds nothing beyond its parents; unsigned, or a signature that does not
// verify) and has no signer to compare. A merge that passed M3 with a
// signature that DOES verify still needs it to be the pusher's.
//
// On such a clean merge this check protects nothing: a pusher can strip or
// corrupt the signature and the same merge is admitted (the S2 ruling in
// judgeSigner). That is harmless, because a clean merge carries no content
// beyond its parents, and its parents are either new in this push (judged
// on their own) or held commits reachable from main or an admitted agent
// branch (on the fleet repository newCommits judges any other held commit as
// if new); the check matters for every other signed commit.
//
// Only the commits newCommits returns reach this: those the push sent, and on
// the fleet repository also every held commit that is NOT reachable from main
// or from a local agent/* branch (heldAdmitted). A member merging the host's
// main into its branch pushes other members' commits, but those are on main
// or on admitted branches, so only the member's own merge commit is judged.
//
// INVARIANT this relies on: every commit the walk skips is reachable from
// main or from a ref that was itself admitted. On the fleet repository the
// walk enforces it (PR #524 B1): its copy can hold commits nobody judged (a
// master with an origin fetches refused member tips; a member clone made
// before PR #524 copied every agent/* branch), and a clean merge of such a
// commit and main would otherwise be the only commit judged, admitting the
// unjudged one through it. A fleet copy with an origin also accepts no push at
// all (repos.Manager.FleetPushRefusal, at the web edge in pushPolicy): defence
// in depth, and "nobody pushes to a non-master". Every other repository keeps
// "held = judged or written here", which holds because it never fetches
// another agent's branch after its clone.
func signerIsPusher(c *object.Commit, pusher string) (Refusal, bool) {
	s, err := verifyCommitSignature(c)
	if err != nil {
		return Refusal{}, true
	}
	if !strings.EqualFold(s.Fingerprint, pusher) {
		return Refusal{Commit: c.Hash.String(), Rule: RuleNotPusher, SignerFP: s.Fingerprint,
			Reason: fmt.Sprintf("signed by %s, not by the pusher %s", pki.Short(s.Fingerprint), pki.Short(pusher))}, false
	}
	return Refusal{}, true
}

// privatePathClosure refuses a push whose new commits change a private path
// (fact.IsPrivatePath: any segment beginning with "."; .knomit/ is the
// system — ontology, triggers, recipes, skills). It runs in every
// verify_signatures mode.
//
// It is EARLY FEEDBACK, not the authority. It judges only the commits a push
// sends: a tip the host already holds brings none and passes untouched. The
// authority is the merge-time guard in MergePushed and MergeConsensus (the
// only routes from a pushed branch into the host's agent branch, which the
// upstream follows), which judges the merge result.
//
// The rule: every new commit carries the private state of its merge base
// with the host's upstream — "a pushed branch carries the .knomit/ of where
// it forked off the upstream". It accepts a branch that never merged the
// upstream (its base is its own fork point) and one that merged it (the base
// is the upstream commit it merged), and refuses an evil merge and a merge
// that takes an OLDER .knomit/ from a parent the host already holds. That
// last case is why "differs from every parent" is NOT enough: the old version
// equals one parent, yet a three-way merge into the agent branch applies it
// as a revert of the newer state.
//
// No merge base (unrelated history) compares against the empty tree: such a
// commit may carry no private path at all. A missing upstream never gets
// here: judgeRequest refuses the push first.
func (rp *receivePack) privatePathClosure(ctx context.Context, q *quarantine, commits []*object.Commit) string {
	return rp.rh.privatePathClosure(ctx, q, rp.upstream(), commits)
}

// privatePathClosure is receivePack.privatePathClosure for any entry: st
// holds the commits (a push's quarantine; the store itself for a fetch).
func (rh *repoHandler) privatePathClosure(ctx context.Context, st storer.EncodedObjectStorer, upstream string, commits []*object.Commit) string {
	if len(commits) == 0 {
		return ""
	}
	uh, err := rh.resolveRef(ctx, upstream)
	if err != nil {
		return fmt.Sprintf("knomit: %s could not be read: %v", upstream, err)
	}
	up, err := object.GetCommit(st, uh)
	if err != nil {
		return fmt.Sprintf("knomit: %s could not be read: %v", upstream, err)
	}
	shapeOK := map[plumbing.Hash]bool{}
	for _, c := range commits {
		// The closure's DiffTree ends silently at a directory entry that is
		// not a tree (as does every go-git walker; checkTreeShape), so a
		// bogus entry would hide the private paths after it. reachableNew
		// does not catch one that names an object the host already holds.
		// Refused here, in every verify_signatures mode, with the judge's
		// rule name.
		if err := commitTreeShape(c, shapeOK); err != nil {
			return fmt.Sprintf("knomit: commit %s refused: %s: %v", c.Hash, RuleMalformedTree, err)
		}
		p, err := privateChangeFromBase(c, up)
		if err != nil {
			return fmt.Sprintf("knomit: commit %s could not be compared with %s: %v", c.Hash, upstream, err)
		}
		if p != "" {
			return fmt.Sprintf("knomit: commit %s changes %s relative to %s; paths with a segment beginning with \".\" (such as %s/) cannot be pushed", c.Hash, p, upstream, fact.PrivateRoot)
		}
	}
	return ""
}

// privateChangeFromBase returns the first private path (sorted) whose content
// in c differs from that at the merge base of c and up, or "". With several
// best bases (a criss-cross) matching ANY of them is enough. On its own that
// would be unsafe — the merge machinery picks one base, and a commit matching
// only the other could revert through it — but push time is only a fast
// fail: the authority is the merge-time guard (guardPrivate in MergePushed
// and MergeConsensus), which refuses any merge whose result changes the host
// agent branch's private paths. Requiring EVERY base instead wedged a member
// branch for good on a benign criss-cross (its merge stays new on every later
// push).
//
// c's tree must be well formed (privatePathClosure checks it first): DiffTree
// ends silently at a directory entry that is not a tree. A base tree is one
// the host holds; one truncated there only makes c's paths look added, which
// refuses rather than hides.
func privateChangeFromBase(c, up *object.Commit) (string, error) {
	tree, err := c.Tree()
	if err != nil {
		return "", err
	}
	bases, err := c.MergeBase(up)
	if err != nil {
		return "", err
	}
	baseTrees := []*object.Tree{nil} // no merge base: the empty tree
	if len(bases) > 0 {
		baseTrees = baseTrees[:0]
		for _, b := range bases {
			bt, err := b.Tree()
			if err != nil {
				return "", err
			}
			baseTrees = append(baseTrees, bt)
		}
	}
	first := ""
	for i, bt := range baseTrees {
		changes, err := object.DiffTree(bt, tree)
		if err != nil {
			return "", err
		}
		out := ""
		for _, ch := range changes {
			for _, name := range []string{ch.From.Name, ch.To.Name} {
				if name != "" && fact.IsPrivatePath(name) && (out == "" || name < out) {
					out = name
				}
			}
		}
		if out == "" {
			return "", nil // matches this base
		}
		if i == 0 {
			first = out
		}
	}
	return first, nil
}

// register makes the pushed tip the branch's tip on this host, through the
// commit chokepoint. Returns a refusal text for the one rule only decidable
// under the lock (the old hash), else the move's result.
//
// Everything from the promote on runs on a context that cannot be cancelled
// (F11 review M1): CreateBranch and advanceBranchTo both move the ref BEFORE
// the context-bound SQL that describes it, and a request context is
// cancelled whenever the client goes away.
func (rp *receivePack) register(ctx context.Context, cmd *packp.Command, objs []plumbing.EncodedObject) (res MainReconcileResult, seeded bool, refusal string, err error) {
	branch := cmd.Name.Short()
	defer rp.rh.lockBranch(branch)()

	cur := plumbing.ZeroHash
	if ref, rerr := rp.rh.gits.Reference(cmd.Name); rerr == nil {
		cur = ref.Hash()
	} else if !errors.Is(rerr, plumbing.ErrReferenceNotFound) {
		return res, false, "", fmt.Errorf("read %s: %w", cmd.Name, rerr)
	}
	if cur != cmd.Old {
		return res, false, fmt.Sprintf("knomit: %s moved since it was advertised (have %s, you expected %s); fetch and push again", cmd.Name, cur, cmd.Old), nil
	}

	ctx = context.WithoutCancel(ctx)
	if pushRegisterHook != nil {
		pushRegisterHook()
	}
	if err := rp.rh.gits.PutObjects(ctx, objs); err != nil {
		return res, false, "", fmt.Errorf("promote: %w", err)
	}
	if cur == plumbing.ZeroHash {
		// Seed from the upstream: the new branch inherits its rows and, above
		// all, its index watermark, so the first sync is a diff and not a full
		// re-index of every fact (measured 2.3 s vs 6.5 ms for a one-commit
		// push on a 100-fact repo).
		if err := rp.rh.CreateBranch(ctx, branch, rp.upstream()); err != nil {
			return res, false, "", fmt.Errorf("seed: %w", err)
		}
		seeded = true
	}
	res, _, err = rp.rh.advanceBranchTo(ctx, branch, cmd.New)
	if err != nil {
		return res, seeded, "", err
	}
	if seeded && res.Mode == ModeNoop {
		// The pushed tip IS the upstream tip: CreateBranch set the ref and its
		// rows, but no move ran notifyCommit. Run it, so a registration always
		// passes the chokepoint.
		if err := rp.rh.notifyCommit(ctx, branch, cmd.New); err != nil {
			return res, seeded, "", err
		}
	}
	return res, seeded, "", nil
}

// heldAdmitted is newCommits' admitted predicate for a push to this
// repository: nil (every held commit ends the walk) unless this repository is
// the fleet repository, where a held commit ends it only when it is reachable
// from the consensus branch or from a local refs/heads/agent/* branch. Those
// refs are admitted by construction: receive-pack's register and the fleet
// master's promotePeer create them only after the judge, and this host's own
// agent branch is written only by this host. Remote-tracking refs are not
// admitted: a fetch stores whatever the origin had.
//
// The fleet is recognised by the ontology at the upstream's tip; when that
// cannot be read the strict rule applies (judging more is the safe side, and
// verify refuses the push anyway).
//
// Cost: a frontier commit that IS a head tip answers at once; any other one
// builds, once per push and only then, the set of commits reachable from the
// heads. The fleet repository is small; every other repository keeps the walk
// proportional to the push.
func (rh *repoHandler) heldAdmitted(ctx context.Context, upstream string) func(plumbing.Hash) (bool, error) {
	if _, ont, err := rh.verifyModeAt(ctx, upstream); err == nil && !fact.IsFleetOntology(ont) {
		return nil
	}
	var heads []plumbing.Hash
	var reach map[plumbing.Hash]bool
	loaded := false
	load := func() error {
		loaded = true
		if up, err := rh.gits.Reference(plumbing.NewBranchReferenceName(upstream)); err == nil {
			heads = append(heads, up.Hash())
		} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return err
		}
		iter, err := rh.gits.IterReferences()
		if err != nil {
			return err
		}
		defer iter.Close()
		return iter.ForEach(func(ref *plumbing.Reference) error {
			if ref.Type() == plumbing.HashReference && strings.HasPrefix(ref.Name().String(), "refs/heads/agent/") {
				heads = append(heads, ref.Hash())
			}
			return nil
		})
	}
	return func(h plumbing.Hash) (bool, error) {
		if !loaded {
			if err := load(); err != nil {
				return false, err
			}
		}
		for _, head := range heads {
			if head == h {
				return true, nil
			}
		}
		if reach == nil {
			reach = map[plumbing.Hash]bool{}
			for _, head := range heads {
				if reach[head] {
					continue
				}
				if err := walkHistory(rh.gits, head, reach, func(c *object.Commit) { reach[c.Hash] = true }); err != nil {
					return false, err
				}
			}
		}
		return reach[h], nil
	}
}

// pushRegisterHook, when set (tests only), runs inside register after the
// old-hash check, on the uncancellable context — the point a disconnecting
// client must no longer be able to abort.
var pushRegisterHook func()

// verifyModeAt reads verify_signatures from the ontology at branch's tip, and
// that ontology: the judge reads from it whether this repository is the fleet
// (rule A) and its artifact rules (R7). With verification on, an ontology
// that does not parse is an error: its rules cannot be known.
func (rh *repoHandler) verifyModeAt(ctx context.Context, branch string) (mode string, ont *fact.Ontology, err error) {
	h, err := rh.resolveRef(ctx, branch)
	if err != nil {
		return "", nil, err
	}
	c, err := object.GetCommit(rh.gits, h)
	if err != nil {
		return "", nil, err
	}
	raw, err := treeOntology(c)
	if err != nil {
		return "", nil, err
	}
	if raw == nil {
		return VerifyOff, nil, nil
	}
	vs, err := fact.ReadVerifySettings(raw)
	if err != nil {
		return "", nil, err
	}
	if !vs.Valid {
		return "", nil, errors.New("unknown value")
	}
	if vs.Mode == VerifyOff {
		return VerifyOff, nil, nil
	}
	if ont, err = fact.ParseOntology(raw); err != nil {
		return "", nil, err
	}
	return vs.Mode, ont, nil
}
