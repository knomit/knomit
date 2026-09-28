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
	// certificate principal's ID). Empty when Refusal is set.
	Pusher string
	// Refusal, when non-empty, refuses the whole request with this text: a
	// principal that is not an instance certificate, one without push:own,
	// a subscription. Decided at the edge, where the principal is.
	Refusal string
	// OwnBranch is this host's own agent branch, which is never written
	// through receive-pack.
	OwnBranch string
	// Members returns the fleet repository's member records. Called only when
	// verify_signatures is log or enforce; nil or an error means this instance
	// has no fleet to judge against.
	Members func(context.Context) ([]FleetMember, error)
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
// updating EXACTLY its own agent branch (F11). It is knomit's own code over
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
//  3. the new commits (a walk that stops at the host's commits) judged when
//     verify_signatures at the host's upstream says so;
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
	commits, _, err := q.newCommits(cmd.New)
	if err != nil {
		refuse(fmt.Sprintf("knomit: the pushed history could not be read: %v", err))
		return
	}
	objs, err := q.reachableNew(commits)
	if err != nil {
		refuse(fmt.Sprintf("knomit: the push is incomplete: %v", err))
		return
	}
	if msg := rp.verify(r.Context(), pol, commits, lg); msg != "" {
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

	branch := cmd.Name.Short()
	ev := lg.Info().Str("old", shortRefHash(cmd.Old)).Str("new", shortRefHash(cmd.New)).
		Int("commits", len(commits)).Int("objects", len(objs))
	switch {
	case seeded:
		ev.Msg("receive-pack: registered")
	case res.Mode == ModeRewound:
		// A real non-fast-forward of an existing peer branch: allowed (the
		// peer's own reconcile force-pushes), and worth one line above INFO.
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
	case len(pol.Pusher) < 8:
		return nil, errNoPushPolicy
	}
	if _, err := rp.rh.resolveRef(ctx, rp.upstream()); err != nil {
		return nil, fmt.Sprintf("%s: %q", errUpstreamMissing.Error(), rp.upstream())
	}
	if len(req.Commands) != 1 {
		return nil, fmt.Sprintf("knomit: a push may update exactly one ref, refs/heads/agent/<your branch>; this push names %d", len(req.Commands))
	}
	cmd := req.Commands[0]
	name := cmd.Name.String()
	fp8 := pki.Short(pol.Pusher)
	switch {
	case cmd.Action() == packp.Delete:
		return nil, fmt.Sprintf("knomit: branch deletion is not allowed: %s", name)
	case !strings.HasPrefix(name, "refs/heads/agent/"):
		return nil, fmt.Sprintf("knomit: only your own agent branch may be pushed; %s is not an agent branch", name)
	case !strings.HasSuffix(name, "-"+fp8):
		return nil, fmt.Sprintf("knomit: %s is not your agent branch (your certificate's fingerprint is %s)", name, fp8)
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
func (rp *receivePack) verify(ctx context.Context, pol PushPolicy, commits []*object.Commit, lg zerolog.Logger) string {
	upstream := rp.upstream()
	mode, err := rp.rh.verifyModeAt(ctx, upstream)
	if err != nil {
		return fmt.Sprintf("knomit: verify_signatures at %s could not be read: %v", upstream, err)
	}
	if mode == VerifyOff || len(commits) == 0 {
		return ""
	}
	var members []FleetMember
	if pol.Members == nil {
		err = ErrNoFleet
	} else {
		members, err = pol.Members(ctx)
	}
	if err != nil {
		if mode == VerifyEnforce {
			return fmt.Sprintf("knomit: verify_signatures is enforce but this instance has no fleet repository to judge against: %v", err)
		}
		lg.Warn().Err(err).Msg("receive-pack: verify_signatures is log but there is no fleet repository; commits not judged")
		return ""
	}
	idx := indexMembers(members)
	for _, c := range commits {
		ref, ok := idx.judgeCommit(c)
		if ok {
			continue
		}
		if mode == VerifyEnforce {
			return fmt.Sprintf("knomit: commit %s refused by verify_signatures=enforce: %s: %s", c.Hash, ref.Rule, ref.Reason)
		}
		lg.Warn().Str("commit", c.Hash.String()).Str("rule", ref.Rule).Str("reason", ref.Reason).
			Msg("receive-pack: commit would be refused (verify_signatures=log)")
	}
	return ""
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

// pushRegisterHook, when set (tests only), runs inside register after the
// old-hash check, on the uncancellable context — the point a disconnecting
// client must no longer be able to abort.
var pushRegisterHook func()

// verifyModeAt reads verify_signatures from the ontology at branch's tip.
func (rh *repoHandler) verifyModeAt(ctx context.Context, branch string) (string, error) {
	h, err := rh.resolveRef(ctx, branch)
	if err != nil {
		return "", err
	}
	c, err := object.GetCommit(rh.gits, h)
	if err != nil {
		return "", err
	}
	raw, err := treeOntology(c)
	if err != nil {
		return "", err
	}
	if raw == nil {
		return VerifyOff, nil
	}
	vs, err := fact.ReadVerifySettings(raw)
	if err != nil {
		return "", err
	}
	if !vs.Valid {
		return "", errors.New("unknown value")
	}
	return vs.Mode, nil
}
