package gitserve

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
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/domain/fact"
	"knomit/internal/platform/version"
	"knomit/internal/store/fleetrec"
	"knomit/internal/store/internal/vcs"
	"knomit/internal/store/merge"
	"knomit/internal/store/remotesync"
	"knomit/internal/store/verify"
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
	Fleet func(context.Context) (fleetrec.Fleet, error)
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
//     AdvanceBranchTo, which ends in NotifyCommit.
//
// Nothing before 4 writes to the host; nothing in 4 may be abandoned by a
// client that disconnects, because the ref moves before the SQL that
// describes it.
type receivePack struct {
	rh       *host
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
	lg := log.With().Str("repo", rp.rh.Name()).Str("pusher", pol.Pusher).Logger()

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

	q := newQuarantine(rp.rh.Storer())
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
			agent = vcs.AgentIDOfAuthor(c.Author.Email)
		}
		pol.Judged(branch, cmd.New.String(), agent, refusal)
	}
	if msg := rp.privatePathClosure(r.Context(), q, commits); msg != "" {
		judged(msg)
		refuse(msg)
		return
	}
	if msg := rp.verify(r.Context(), pol, cmd.New, commits, lg); msg != "" {
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

	ev := lg.Info().Str("old", vcs.ShortRefHash(cmd.Old)).Str("new", vcs.ShortRefHash(cmd.New)).
		Int("commits", len(commits)).Int("objects", len(objs))
	switch {
	case seeded:
		ev.Msg("receive-pack: registered")
	case res.Mode == merge.ModeRewound:
		// A real non-fast-forward of an existing agent/ branch: allowed, by
		// design. No branch is owned (D18/D19), so ANY authenticated pusher
		// may update or rewind ANY peer's agent/ branch here, not only the
		// peer whose commits are on it; commits the host had not merged yet
		// disappear from this branch until someone pushes them again. Worth
		// one line above INFO, naming the pusher (on lg).
		lg.Warn().Str("old", vcs.ShortRefHash(cmd.Old)).Str("new", vcs.ShortRefHash(cmd.New)).
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
	if _, err := rp.rh.ResolveRef(ctx, rp.upstream()); err != nil {
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
// be signed by the PUSHER (verify.SignerIsPusher): a member pushes only what its own
// key signed, never another active member's commits. The rule rides the mode
// like every other judgement here: enforce refuses, log logs, off checks
// nothing — under off and log an unsigned commit is accepted anyway, so a
// signer rule there would stop nobody.
func (rp *receivePack) verify(ctx context.Context, pol PushPolicy, tip plumbing.Hash, commits []*object.Commit, lg zerolog.Logger) string {
	return verify.JudgeIncoming(ctx, rp.rh.Repo, rp.upstream(), pol.Fleet, func(c *object.Commit) (verify.Refusal, bool) {
		return verify.SignerIsPusher(c, pol.Pusher)
	}, tip, commits, lg)
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
	return verify.PrivatePathClosure(ctx, rp.rh.Repo, q, rp.upstream(), commits)
}

// register makes the pushed tip the branch's tip on this host, through the
// commit chokepoint. Returns a refusal text for the one rule only decidable
// under the lock (the old hash), else the move's result.
//
// Everything from the promote on runs on a context that cannot be cancelled
// (F11 review M1): CreateBranch and AdvanceBranchTo both move the ref BEFORE
// the context-bound SQL that describes it, and a request context is
// cancelled whenever the client goes away.
func (rp *receivePack) register(ctx context.Context, cmd *packp.Command, objs []plumbing.EncodedObject) (res remotesync.MainReconcileResult, seeded bool, refusal string, err error) {
	branch := cmd.Name.Short()
	defer rp.rh.LockBranch(branch)()

	cur := plumbing.ZeroHash
	if ref, rerr := rp.rh.Storer().Reference(cmd.Name); rerr == nil {
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
	if err := rp.rh.Storer().PutObjects(ctx, objs); err != nil {
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
	res, _, err = rp.rh.sy.AdvanceBranchTo(ctx, branch, cmd.New)
	if err != nil {
		return res, seeded, "", err
	}
	if seeded && res.Mode == merge.ModeNoop {
		// The pushed tip IS the upstream tip: CreateBranch set the ref and its
		// rows, but no move ran NotifyCommit. Run it, so a registration always
		// passes the chokepoint.
		if err := rp.rh.NotifyCommit(ctx, branch, cmd.New); err != nil {
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
// master's AdmitPeerBranch (remotesync.Index) create them only after the judge, and this host's own
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
func (rh *host) heldAdmitted(ctx context.Context, upstream string) func(plumbing.Hash) (bool, error) {
	if _, ont, err := verify.VerifyModeAt(ctx, rh.Repo, upstream); err == nil && !fact.IsFleetOntology(ont) {
		return nil
	}
	var heads []plumbing.Hash
	var reach map[plumbing.Hash]bool
	loaded := false
	load := func() error {
		loaded = true
		if up, err := rh.Storer().Reference(plumbing.NewBranchReferenceName(upstream)); err == nil {
			heads = append(heads, up.Hash())
		} else if !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return err
		}
		iter, err := rh.Storer().IterReferences()
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
				if err := vcs.WalkHistory(rh.Storer(), head, reach, func(c *object.Commit) { reach[c.Hash] = true }); err != nil {
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
