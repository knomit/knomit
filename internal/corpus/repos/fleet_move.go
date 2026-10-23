package repos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"knomit/internal/corpus/repos/control"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/rs/zerolog/log"

	"knomit/internal/engine/store"
	"knomit/internal/engine/store/remotesync"
)

// POST /fleet/follow with a FORGE URL (user rulings 2026-10-16: every
// configuration a regular repo has that the fleet repository needs is a
// /fleet operation; the hub may move the fleet onto a forge). One route,
// three cases, all under fleetOpMu and after F1 (refuseFleetOrigin):
//
//   - a forge copy rotates its credential or moves to another forge URL;
//   - a member whose copy follows a knomit host points at the forge the
//     fleet moved to, with its OWN credential;
//   - the fleet's master moves the fleet onto a forge (from the hub, or from
//     another forge): moveFleetToForge.
//
// The same-fleet check is the remote's main: it must carry THIS fleet's
// root (artifacts/fleet/root.crt equal to the one on this copy's main), or,
// for the master only, be empty (a new forge repository the master seeds).
// Anything else is not_this_fleet.

// remoteRoot is what a forge's main says about which fleet it holds.
type remoteRoot int

const (
	remoteRootUnknown remoteRoot = iota // could not look; detail says why
	remoteEmpty                         // the remote advertises no ref at all: no branch, no tag
	remoteRootMatch                     // main carries this fleet's root
	remoteRootDiffers                   // main carries another root, or none
	remoteNoMain                        // no main, but other branches or tags: someone else's repository
)

// advEmpty is an advertisement with no ref at all: no branch and no tag
// (a HEAD alone, pointing nowhere, is what a new repository advertises). A
// remote whose main is missing but which holds master, trunk or a tag is
// NOT empty (#529 B1): it is someone else's repository.
func advEmpty(adv *packp.AdvRefs) bool {
	if adv == nil {
		return true
	}
	for name := range adv.References {
		if name != plumbing.HEAD.String() {
			return false
		}
	}
	return len(adv.Peeled) == 0
}

// remoteFleetRoot reads rootPath at the tip of branch on the remote o names
// (a shallow single-branch clone into the probe's budgeted memory store,
// as ProbeInitialized does) and compares it with want.
func (m *Manager) remoteFleetRoot(ctx context.Context, o OriginSpec, branch, rootPath string, want []byte) (remoteRoot, string) {
	rp, err := m.beginRemoteProbe(ctx, o)
	if err != nil {
		return remoteRootUnknown, err.Error()
	}
	defer rp.close()
	switch {
	case rp.authErr != nil:
		return remoteRootUnknown, rp.authErr.Error()
	case rp.advErr != nil:
		return remoteRootUnknown, rp.advErr.Error()
	case advEmpty(rp.adv):
		return remoteEmpty, ""
	case !advHasBranch(rp.adv, branch):
		return remoteNoMain, ""
	}
	opts := &gogit.CloneOptions{
		URL: rp.gitURL, Auth: rp.auth, SingleBranch: true, Tags: gogit.NoTags,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
	}
	if advertisesShallow(rp.adv) {
		opts.Depth = 1
	}
	repo, cerr := gogit.CloneContext(rp.netCtx, newBudgetedStorage(m.deps.Cfg.Git.MaxProbeBytes), nil, opts)
	if cerr != nil {
		return remoteRootUnknown, probeFailureDetail(ctx, rp.netCtx, cerr, m.deps.Cfg.Git.NetworkTimeout)
	}
	tree, terr := tipTree(repo)
	if terr != nil {
		return remoteRootUnknown, terr.Error()
	}
	f, ferr := tree.File(rootPath)
	if errors.Is(ferr, object.ErrFileNotFound) || errors.Is(ferr, object.ErrDirectoryNotFound) || errors.Is(ferr, object.ErrEntryNotFound) {
		return remoteRootDiffers, ""
	}
	if ferr != nil {
		return remoteRootUnknown, ferr.Error()
	}
	r, rerr := f.Reader()
	if rerr != nil {
		return remoteRootUnknown, rerr.Error()
	}
	defer r.Close()
	got, rerr := io.ReadAll(io.LimitReader(r, maxFleetHostSize*4))
	if rerr != nil {
		return remoteRootUnknown, rerr.Error()
	}
	if bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		return remoteRootMatch, ""
	}
	return remoteRootDiffers, ""
}

// tipTree is the tree of repo's HEAD commit.
func tipTree(repo *gogit.Repository) (*object.Tree, error) {
	head, err := repo.Head()
	if err != nil {
		return nil, err
	}
	c, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, err
	}
	return c.Tree()
}

// localFleetRoot is the root certificate on this copy's main and its path.
func (m *Manager) localFleetRoot(ri *RepoInstance) (path string, pem []byte, upstream string, err error) {
	ont := ri.Ontology()
	if ont == nil {
		return "", nil, "", errors.New("fleet repository has no ontology")
	}
	layout, ok := ont.PKILayout()
	if !ok {
		return "", nil, "", errors.New("the fleet ontology declares no pki root")
	}
	var found bool
	if werr := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errors.New("fleet repository is not open")
			return
		}
		upstream = svc.Remote().UpstreamBranch()
		pem, found, err = svc.FileAt(upstream, layout.Root, maxFleetHostSize*4)
	}); werr != nil {
		return "", nil, "", werr
	}
	if err == nil && !found {
		err = fmt.Errorf("%s is not on this copy's %s", layout.Root, upstream)
	}
	return layout.Root, pem, upstream, err
}

// followForge is followFleetHost for a forge URL other than the one this
// copy follows (decided.AuthMethod is not cert; the stored forge is a
// credential change, changeFleetCredential). The caller holds fleetOpMu and
// ran F1. On the master it is a MOVE (moveFleetToForge); on a member it
// follows the forge the fleet moved to.
func (m *Manager) followForge(ctx context.Context, ri *RepoInstance, decided OriginSpec, prev *control.Origin) (string, error) {
	master := false
	if err := ri.WithRead(func(svc *store.Service) { master = ri.isFleetMaster(svc) }); err != nil {
		return "", err
	}
	rootPath, rootPEM, upstream, err := m.localFleetRoot(ri)
	if err != nil {
		return "", err
	}
	decided.Branch = upstream
	if err := m.probeFleetForge(ctx, decided); err != nil {
		return "", err
	}
	state, detail := m.remoteFleetRoot(ctx, decided, upstream, rootPath, rootPEM)
	switch state {
	case remoteRootUnknown:
		return "", fmt.Errorf("%w: could not read %s on %s: %s", ErrOriginUnreachable, upstream, credentialFree(decided.URL), detail)
	case remoteRootDiffers:
		return "", fleetErr(http.StatusUnprocessableEntity, "not_this_fleet",
			credentialFree(decided.URL)+" holds another fleet or another repository: its "+upstream+" does not carry this fleet's root ("+rootPath+"). Use the URL of the forge repository this fleet lives on")
	case remoteNoMain:
		return "", fleetErr(http.StatusUnprocessableEntity, "not_this_fleet",
			credentialFree(decided.URL)+" holds another repository: it has no "+upstream+", but other branches or tags. The fleet moves only to an EMPTY forge repository (no branch, no tag) or to the one this fleet lives on")
	case remoteEmpty:
		if !master {
			return "", fleetErr(http.StatusUnprocessableEntity, "not_this_fleet",
				credentialFree(decided.URL)+" is empty: only the fleet's master moves the fleet to a new forge repository. Point this copy at it once the master has moved the fleet there")
		}
	}
	if master {
		if err := m.moveFleetToForge(ctx, ri, decided, prev); err != nil {
			return "", err
		}
		return FleetFollowMoved, nil
	}
	if _, err := m.Send(ctx, ri, AttachOrigin(decided)); err != nil {
		return "", err
	}
	return FleetFollowFollowed, nil
}

// probeFleetForge is the reachability check a forge follow makes with the
// credential given, as the attach's own guard asks: unreachable, or a
// credential the forge refused, is ErrOriginUnreachable (502 unreachable).
// The detail quotes go-git and the HTTP client, which quote the URL:
// FollowFleetHost's credentialFreeErr strips its userinfo.
func (m *Manager) probeFleetForge(ctx context.Context, decided OriginSpec) error {
	res, err := m.ProbeOriginRefs(ctx, decided)
	if err != nil {
		return err
	}
	switch {
	case !res.Reachable:
		return &forgeProbeError{"the forge could not be reached: " + res.Detail}
	case res.AuthRequired:
		return &forgeProbeError{"the forge refused the credential: " + res.Detail}
	}
	return nil
}

// forgeProbeError is probeFleetForge's refusal. It is ErrOriginUnreachable to
// errors.Is (502 unreachable) but does not carry its text, "origin was not
// attached": a follow's probe runs before anything is attached, and on the
// credential change the origin stays attached all along (#529 N-3).
type forgeProbeError struct{ msg string }

func (e *forgeProbeError) Error() string        { return e.msg }
func (e *forgeProbeError) Is(target error) bool { return target == ErrOriginUnreachable }

// moveFleetToForge is the master's move of the fleet onto the forge decided
// names (from the hub, which has no origin, or from another forge). In order:
//
//  1. seed the forge: this copy's agent branch tip as main (without force)
//     and as the agent branch (store PublishTo), BEFORE the forge is the
//     origin, so the origin round the attach starts fetches a main equal to
//     the tip and never adopts an older forge main as fetched (the forge's
//     main is trusted as fetched, rewinds included: trust-the-forge-main);
//  2. attach the forge as the origin (AttachOrigin: its probe and ontology
//     guard; the Sync stage restarts on the origin loop, which on the master
//     is the fetching master: SyncWithPeers, the judge, the merger, publish);
//  3. write the host record: the forge URL, credential-free;
//  4. push the agent branch and publish main (pushFleet, as every master write
//     is published; a failure is the origin round's to retry).
//
// A failure in 1 or 2 changes nothing here (what step 1 pushed stays on the
// forge, which nothing reads until an origin points there). A failure in 3
// restores the previous origin (none for the hub), so this copy is exactly
// as it was, and is returned.
func (m *Manager) moveFleetToForge(ctx context.Context, ri *RepoInstance, decided OriginSpec, prev *control.Origin) error {
	gitURL, auth, err := m.ResolveAuth(authConfigFromSpec(&decided), decided.URL)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrOriginUnreachable, err)
	}
	var perr error
	if werr := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			perr = errors.New("fleet repository is not open")
			return
		}
		if h := currentFleetHooks().seedPublish; h != nil {
			if perr = h(); perr != nil {
				return
			}
		}
		perr = svc.Remote().PublishTo(ctx, gitURL, ri.AgentBranch(), decided.Branch, auth)
	}); werr != nil {
		return werr
	}
	if perr != nil {
		m.noteOwnJudge(perr)
		return fleetErr(http.StatusConflict, "publish_refused",
			credentialFree(decided.URL)+" refused this fleet's "+decided.Branch+" (it must be empty or behind this copy's): "+perr.Error()+". Nothing changed here")
	}
	if _, err := m.Send(ctx, ri, AttachOrigin(decided)); err != nil {
		return err
	}
	if err := m.writeFleetHost(ctx, ri, []string{decided.URL}); err != nil {
		if rerr := m.restoreFleetOrigin(ctx, ri, prev); rerr != nil {
			return fmt.Errorf("fleet: the host record was not written (%v), and restoring the previous origin failed: %w", err, rerr)
		}
		return fmt.Errorf("fleet: the host record was not written, so this copy is back on its previous origin: %w", err)
	}
	if err := m.pushFleet(ctx, ri); err != nil {
		// The push error quotes the forge URL as go-git built it: credential-free.
		log.Warn().Str("error", credentialFreeText(err.Error())).Str("fleet", ri.Name()).Msg("fleet: main is not published on the forge yet; the origin round retries")
	}
	log.Info().Str("fleet", ri.Name()).Str("forge", credentialFree(decided.URL)).Bool("from_hub", prev == nil).
		Msg("fleet: the master moved the fleet onto a forge")
	return nil
}

// restoreFleetOrigin puts back the origin a failed move replaced: none (the
// hub) is a detach; a stored one is attached again as it was (a cert origin
// is classified again, since cert is never an input).
func (m *Manager) restoreFleetOrigin(ctx context.Context, ri *RepoInstance, prev *control.Origin) error {
	ctx = context.WithoutCancel(ctx)
	if prev == nil {
		_, err := m.Send(ctx, ri, DetachOrigin())
		return err
	}
	method, token := prev.AuthMethod, prev.AuthToken
	if method == "cert" {
		method, token = "", ""
	}
	_, err := m.Send(ctx, ri, AttachOrigin(OriginSpec{URL: prev.URL, Branch: prev.Branch, AuthMethod: method, AuthToken: token}))
	return err
}

// ErrCodeUnpublishedChanges is the refusal of a root export or a stand-down
// while the acting master's fleet branch holds commits its main does not.
const ErrCodeUnpublishedChanges = "unpublished_changes"

// flushBeforeSwitch is the master switch's first step (user ruling: "flush"
// all changes, "block any new incoming changes, THEN give up the master
// role"): on the acting master, publish what its fleet branch holds
// (publishMasterWrite: a forge's agent branch and main, the hub's local
// main), then refuse (409 unpublished_changes) while the branch still holds
// a commit main does not. Those commits are master writes main has not seen
// (a revoke whose publish failed, a renewal, a re-signed CRL, a merged
// member record): a new master works from ITS main and would never see them,
// so a revocation would be lost and the new master could renew the revoked
// leaf or re-sign a CRL that collides with the unpublished one (PR #537
// re-review R1). Root export, stand-down and a forge decommission call it.
// The caller holds fleetOpMu, which keeps every master duty out (they
// TryLock it), and StandDown also holds the merger's run lock.
// Not the acting master: nothing to flush.
func (m *Manager) flushBeforeSwitch(ctx context.Context, ri *RepoInstance) error {
	acts := false
	if err := ri.WithRead(func(svc *store.Service) { acts = ri.actsAsFleetMaster(svc) }); err != nil {
		return err
	}
	if !acts {
		return nil
	}
	perr := m.publishMasterWrite(ctx, ri)
	published, err := m.fleetBranchPublished(ctx, ri)
	if err != nil {
		return err
	}
	if published {
		return nil
	}
	why := ""
	if perr != nil {
		why = " The publish just tried failed: " + credentialFreeErr(perr).Error() + "."
	}
	origin, oerr := m.hasOrigin(ri)
	if oerr != nil {
		return oerr
	}
	return fleetErr(http.StatusConflict, ErrCodeUnpublishedChanges, "this instance's fleet branch holds changes the fleet's main does not have yet (a revoke, a renewal, a re-signed CRL or a member's record): the master role cannot be handed on before they are published, or the next master would never see them."+why+" "+unpublishedWayOut(perr, origin))
}

// unpublishedWayOut is the last sentence of an unpublished_changes refusal:
// what lets the publish succeed. A main held by the judge of this instance's
// own commits never moves by waiting (every round refuses the same commit),
// so it points at the drop-commit card; otherwise a forge needs its origin
// reachable, and the hub (no origin) needs its next sync round.
func unpublishedWayOut(perr error, origin bool) string {
	switch {
	case errors.Is(perr, remotesync.ErrOwnCommitRefused):
		c := mainHeldCommit(perr.Error())
		if c == "" {
			return "The fleet's rules refuse a commit on this instance's own branch, so main does not move while it is there and waiting does not help: drop it first (the fleet page's \"Drop commit\" card), then try again"
		}
		return "The fleet's rules refuse commit " + shortHash(c) + " on this instance's own branch, so main does not move while it is there and waiting does not help: drop it first (the fleet page's \"Drop commit " + shortHash(c) + "\" card), then try again"
	case origin:
		return "Let the publish succeed first (fix the fleet's origin, or wait for the next sync round), then try again"
	}
	return "Let the publish succeed first (wait for the next sync round, or see the fleet's last error), then try again"
}

// fleetBranchPublished reports whether the fleet repository's main holds
// every commit of this instance's fleet branch (its tip is main's tip or an
// ancestor of it).
func (m *Manager) fleetBranchPublished(ctx context.Context, ri *RepoInstance) (bool, error) {
	var ok bool
	var rerr error
	if err := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			rerr = errors.New("fleet repository is not open")
			return
		}
		agent, err := svc.HeadCommit(ctx, ri.AgentBranch())
		if err != nil {
			rerr = err
			return
		}
		main, err := svc.HeadCommit(ctx, svc.Remote().UpstreamBranch())
		if err != nil {
			rerr = err
			return
		}
		ok, rerr = svc.FactsRepo().IsAncestor(ctx, plumbing.NewHash(agent), plumbing.NewHash(main))
	}); err != nil {
		return false, err
	}
	return ok, rerr
}

// refuseOldMasterActive is Host here's one check (old_master_active): the
// master marker on this copy's main, as its ordinary sync left it, must name
// no master (the old master stood down) or this instance. A marker naming
// another instance means that master did not stand down, or this copy has not
// synced since it did.
func (m *Manager) refuseOldMasterActive(ri *RepoInstance, o *control.Origin) error {
	named, err := m.fleetMasterAtMain(ri)
	if err != nil {
		return err
	}
	if named == "" || named == m.AgentID() {
		return nil
	}
	host := originHost(o.URL)
	return fleetErr(http.StatusConflict, "old_master_active",
		"this copy's main still names "+named+" as the fleet's acting master: it has not stood down (stand down there first), or this copy has not synced since (last sync "+m.lastFleetSync(ri)+"). Hosting the fleet here now leaves out every change made on "+host+" after this copy's last sync. Confirm to host it here anyway")
}

// lastFleetSync is when this copy last fetched from its origin ("unknown"
// when never, or when it cannot be read).
func (m *Manager) lastFleetSync(ri *RepoInstance) string {
	out := "unknown"
	_ = ri.WithRead(func(svc *store.Service) {
		if r, err := svc.Remote().GetRemote("origin"); err == nil && r != nil && r.LastSyncAt != nil && *r.LastSyncAt != "" {
			out = *r.LastSyncAt
		}
	})
	return out
}

// HostFleetHere is POST /fleet/host, "Host the fleet here": this instance's
// copy of the fleet repository drops its origin and becomes the hub. The
// check (guardFleetDetach) and the detach run under fleetOpMu as one step, so
// a stand-down (DELETE /fleet/root, which passes while the copy has an
// origin) cannot remove the key between the master check and the detach and
// leave a hub with no master.
//
// A detach that succeeded makes this master the host, so it records that on
// main: the host record (fleet_host.go) with the hub's URLs, committed on the
// agent branch and published (main fast-forwards here: there is no origin
// any more; a fast-forward that fails now is the local reconcile's to retry).
// The detach stands when the write fails, and it is answered as done: the
// hub's local reconcile re-asserts the record every round
// (RepoInstance.fleetHostRound), so the next round writes it.
//
// It hosts only when the master marker on its own main, from its ordinary
// sync, names no master (the old master stood down) or this instance
// (refuseOldMasterActive), or with spec.ConfirmLoss: the old master is gone,
// or this copy has not synced since it stood down, and what it changed after
// this copy's last sync is left out (the dead-master fallback). Nothing asks
// the old host anything. Hosting writes the marker naming this instance.
func (m *Manager) HostFleetHere(ctx context.Context, spec FleetHostSpec) (FleetStatus, error) {
	if err := m.hostFleetHere(ctx, spec); err != nil {
		return FleetStatus{}, err
	}
	// The hub serves members over the mTLS listener: a configured one that
	// never started (the certificate came after the boot) starts now.
	m.startTLSAfterInstall("fleet host")
	return m.FleetStatus(ctx)
}

// FleetHostSpec is what POST /fleet/host takes. ConfirmLoss is the user's
// confirm that changes made on the old host after this copy's last sync are
// not included (read only when main's master marker names another instance).
type FleetHostSpec struct {
	ConfirmLoss bool
}

func (m *Manager) hostFleetHere(ctx context.Context, spec FleetHostSpec) error {
	defer m.lockFleetOp()()
	ri := m.fleetRepo()
	if ri == nil {
		return fleetErr(http.StatusConflict, "not_registered", "this instance is in no fleet")
	}
	if origin, err := m.hasOrigin(ri); err != nil {
		return err
	} else if !origin {
		return fleetErr(http.StatusConflict, "hub", "this instance already hosts the fleet: its copy of "+ri.Name()+" has no origin")
	}
	if err := m.guardFleetDetach(ri); err != nil {
		return err
	}
	if o, err := m.originOf(ri); err != nil {
		return err
	} else if o != nil && !spec.ConfirmLoss {
		if err := m.refuseOldMasterActive(ri, o); err != nil {
			return err
		}
	}
	if _, err := m.Send(ctx, ri, DetachOrigin()); err != nil {
		return err
	}
	// This instance is the acting master now (fact.FleetMasterPath), the
	// marker a confirmed takeover overwrites; published with the host record
	// below.
	if err := m.writeFleetMaster(ctx, ri); err != nil {
		log.Warn().Err(err).Str("fleet", ri.Name()).Msg("fleet: this instance hosts the fleet now, but the master marker was not written")
	}
	// A push this copy made to the old host (refused once its master stood
	// down) is no sync error of the host: it pushes nowhere now.
	if db := m.ControlDB(); db != nil {
		if err := recordFleetAttempt(db, nil, time.Now()); err != nil {
			log.Warn().Err(err).Msg("fleet: clearing the last sync error after hosting failed")
		}
	}
	if err := m.writeFleetHost(ctx, ri, m.hubFleetURLs(ri)); err != nil {
		log.Warn().Err(err).Str("fleet", ri.Name()).
			Msg("fleet: this instance hosts the fleet now, but the host record was not written; the local reconcile's next round writes it")
		return nil
	}
	if err := m.publishFleetMainNow(ctx, ri); err != nil {
		log.Warn().Err(err).Str("fleet", ri.Name()).Msg("fleet: main did not follow the host record yet; the local reconcile retries")
	}
	return nil
}

// FollowFleetHost is POST /fleet/follow: this instance's copy of the fleet
// repository follows the fleet from url, or changes its credential for the
// forge it follows. It is the one way an origin is attached to the fleet
// repository, under fleetOpMu like every fleet operation, and it runs the
// attach's own checks (guardAttach): an origin no member's join could clone
// is origin_invalid (#527 F1), an unreachable one is refused, and the
// remote's ontology must be the fleet's.
//
// What it does is decided by the URL against the stored origin (PR #529
// S3), never by which page asked:
//
//   - the stored forge, or no url, on a forge copy: a CREDENTIAL change. The
//     new credential is probed and stored, and that is all: no push, no host
//     record, nothing a member sees. The stored credential unchanged is
//     answered FleetFollowUnchanged and nothing runs;
//   - an instance of THIS fleet (its certificate chains to this fleet's
//     root): this copy follows it (a master switch). A credential is refused
//     (ErrPeerCredential): the certificate is the credential;
//   - another forge URL: followForge. The master MOVES the fleet there; a
//     member follows the forge the fleet moved to.
//
// Every error text it answers is credential-free (credentialFreeErr): a
// token in a URL's user slot is quoted by the HTTP client and by go-git.
func (m *Manager) FollowFleetHost(ctx context.Context, o OriginSpec) (FleetFollowed, error) {
	change, err := m.followFleetHost(ctx, o)
	if err != nil {
		return FleetFollowed{}, credentialFreeErr(err)
	}
	st, err := m.FleetStatus(ctx)
	return FleetFollowed{FleetStatus: st, Change: change}, err
}

// FleetFollowed is what POST /fleet/follow answers: the new state and what
// the follow changed.
type FleetFollowed struct {
	FleetStatus
	Change string `json:"change"`
}

// The FleetFollowed.Change values.
const (
	FleetFollowFollowed   = "followed"   // this copy follows the fleet from the URL now
	FleetFollowMoved      = "moved"      // the master moved the fleet onto the forge
	FleetFollowCredential = "credential" // the same forge with a new credential
	FleetFollowUnchanged  = "unchanged"  // the stored forge and credential: nothing ran
)

func (m *Manager) followFleetHost(ctx context.Context, o OriginSpec) (string, error) {
	defer m.lockFleetOp()()
	ri := m.fleetRepo()
	if ri == nil {
		return "", fleetErr(http.StatusConflict, "not_registered", "this instance is in no fleet")
	}
	prev, err := m.originOf(ri)
	if err != nil {
		return "", err
	}
	forgeCopy := prev != nil && prev.AuthMethod != "cert"
	o.URL = strings.TrimSpace(o.URL)
	if o.URL == "" {
		// A credential change on a forge copy: the stored URL stays.
		if !forgeCopy {
			return "", fleetErr(http.StatusBadRequest, "url_required", "the body must carry the URL this copy follows the fleet from")
		}
		o.URL = prev.URL
	}
	if err := refuseFleetOrigin(o.URL); err != nil {
		return "", err
	}
	if forgeCopy && sameFleetURL(prev.URL, o.URL) {
		return m.changeFleetCredential(ctx, ri, o, prev)
	}
	decided, err := m.ResolveOrigin(ctx, o)
	if err != nil {
		return "", err
	}
	if decided.AuthMethod != "cert" {
		return m.followForge(ctx, ri, decided, prev)
	}
	if err := ri.WithRead(func(svc *store.Service) { decided.Branch = svc.Remote().UpstreamBranch() }); err != nil {
		return "", err
	}
	if _, err = m.Send(ctx, ri, AttachOrigin(decided)); err != nil {
		return "", err
	}
	return FleetFollowFollowed, nil
}

// changeFleetCredential is a follow of the forge this copy already follows
// ("Update access", on the master and on a member alike). The URL stays the
// stored one. A credential not given keeps the stored one: no method, or
// the stored method with no token (the page prefills the method and never
// holds the secret, #529 N-b); auth_method none is the explicit reset to
// anonymous. The new credential must reach the forge (the probe the attach's
// guard asks for) before it is stored. Then it is stored, and that is all: on
// the master no seed, no host record, no publish (#529 S3, N-a).
func (m *Manager) changeFleetCredential(ctx context.Context, ri *RepoInstance, o OriginSpec, prev *control.Origin) (string, error) {
	o.URL = prev.URL
	if o.AuthToken == "" && (o.AuthMethod == "" || o.AuthMethod == prev.AuthMethod) {
		o.AuthMethod, o.AuthToken = prev.AuthMethod, prev.AuthToken
	}
	if err := refuseMethodForURL(o.URL, o.AuthMethod); err != nil {
		return "", err
	}
	if o.AuthMethod == prev.AuthMethod && o.AuthToken == prev.AuthToken {
		return FleetFollowUnchanged, nil
	}
	decided, err := m.ResolveOrigin(ctx, o)
	if err != nil {
		return "", err
	}
	if decided.AuthMethod == "cert" {
		return "", fleetErr(http.StatusConflict, "not_this_fleet",
			credentialFree(o.URL)+" now answers as an instance of this fleet, not as a forge, and takes no credential. Point this copy at it (Fleet → Settings → Point at the new host)")
	}
	if err := ri.WithRead(func(svc *store.Service) { decided.Branch = svc.Remote().UpstreamBranch() }); err != nil {
		return "", err
	}
	if err := m.probeFleetForge(ctx, decided); err != nil {
		return "", err
	}
	if _, err := m.Send(ctx, ri, AttachOrigin(decided)); err != nil {
		return "", err
	}
	log.Info().Str("fleet", ri.Name()).Str("forge", credentialFree(o.URL)).Str("auth_method", decided.AuthMethod).
		Msg("fleet: this copy's credential for the forge changed")
	return FleetFollowCredential, nil
}

// guardFleetDetach is the fleet repository's detach check (R3.3): a member's
// copy keeps its origin (without one it has no main and nothing to push to);
// the master's detach makes its copy the hub, which needs the mTLS listener
// and an address to advertise.
func (m *Manager) guardFleetDetach(ri *RepoInstance) error {
	holds := false
	if err := ri.WithRead(func(svc *store.Service) { holds = ri.fleetDoor && ri.IsFleet() && m.holdsFleetRoot(svc) }); err != nil {
		return err
	}
	if !holds {
		return fleetErr(http.StatusConflict, "member_keeps_origin", "a member's fleet copy keeps its origin; only the holder of the fleet's root key hosts the fleet (it becomes the master)")
	}
	return m.refuseNoHubListener()
}
