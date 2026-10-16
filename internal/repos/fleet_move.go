package repos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/rs/zerolog/log"

	"knomit/internal/store"
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
		upstream = svc.UpstreamBranch()
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
func (m *Manager) followForge(ctx context.Context, ri *RepoInstance, decided OriginSpec, prev *Origin) (string, error) {
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
			credentialFree(decided.URL)+" holds another repository: it has no "+upstream+", but other branches or tags. The fleet moves only to an EMPTY forge repository (no branch, no tag) or to the one this fleet lives on. Nothing changed")
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
		return fmt.Errorf("%w: the remote is unreachable: %s", ErrOriginUnreachable, res.Detail)
	case res.AuthRequired:
		return fmt.Errorf("%w: the remote refused the credential: %s", ErrOriginUnreachable, res.Detail)
	}
	return nil
}

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
func (m *Manager) moveFleetToForge(ctx context.Context, ri *RepoInstance, decided OriginSpec, prev *Origin) error {
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
func (m *Manager) restoreFleetOrigin(ctx context.Context, ri *RepoInstance, prev *Origin) error {
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
