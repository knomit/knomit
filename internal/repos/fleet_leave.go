package repos

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/store"
)

// Leave means revoke (user ruling 2026-10-16: "the cert is valid per fleet,
// not in general"). A member cannot revoke its own leaf (only the root key
// signs a CRL), so the revocation is the MASTER's duty: once a member's `left`
// record is on the fleet's main, the master revokes that member's leaf exactly
// as Revoke does (revokeLeaf: the CRL and the revocation statement on its
// agent branch, its local copies, the CRL installed here), and its own round
// publishes it. A rejoin then needs no manual step: the master enrolls the
// same key again (the old leaf is on the CRL, so enroll admits it) and sends a
// new bundle.
//
// The duty runs in every master round, on both hostings, BEFORE the round
// moves main, so the revocation goes out in that same round: on the hub in
// fleetHostRound (before the local reconcile's advance), on a forge after the
// fetch and the fetch judge (fleetPeers.duty, before the push and publish).
// It reads main only, and it is idempotent: a leaf main's CRL already lists is
// left alone, so a left record on main is revoked once.

// leaveRevokeReason is the reason a revocation the leave duty makes carries.
const leaveRevokeReason = "left the fleet"

// revokeLeftMembers revokes the leaf of every member whose record on the
// fleet's main is `left` (as written) while main holds its leaf and main's CRL
// does not list it. One exception keeps a rejoin possible: a leaf that landed
// on main AFTER the left record is a re-enroll that followed an earlier
// revocation (enroll refuses an id whose leaf on main is unrevoked), so it is
// the rejoin's fresh leaf and is not revoked; the rejoin's active record then
// replaces the left one. It skips the pass, writing nothing, while the
// master's agent branch holds a CRL that main does not (an unpublished
// revocation): a CRL built from main's would drop it. The caller holds
// fleetOpMu and has checked that this instance is the fleet's master; nothing
// is published here. Answers the agent ids it revoked.
func (m *Manager) revokeLeftMembers(ctx context.Context, ri *RepoInstance) ([]string, error) {
	f, err := m.fleetAtMain(ri)
	if err != nil {
		return nil, err
	}
	if f.PKI == nil || f.PKI.CRL == nil {
		return nil, nil
	}
	own := m.AgentID()
	type candidate struct {
		agent, record, leafPath string
		leaf                    *x509.Certificate
	}
	var cands []candidate
	for _, mem := range f.Members {
		if mem.State != fact.MemberLeft || mem.Agent == own {
			continue
		}
		leaf, lerr := f.PKI.Cert(mem.Agent)
		if lerr != nil {
			log.Warn().Err(lerr).Str("agent", mem.Agent).Msg("fleet: a left member's certificate on main cannot be read; it is not revoked")
			continue
		}
		if leaf == nil || pki.IsRevoked(f.PKI.CRL, leaf.SerialNumber) {
			continue
		}
		cands = append(cands, candidate{agent: mem.Agent, record: mem.Path, leafPath: f.PKI.Layout.LeafPath(mem.Agent), leaf: leaf})
	}
	if len(cands) == 0 {
		return nil, nil
	}
	paths := make([]string, 0, 2*len(cands))
	for _, c := range cands {
		paths = append(paths, c.record, c.leafPath)
	}
	var depth map[string]int
	var unpublished bool
	var rerr error
	if err := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			rerr = errors.New("fleet repository is not open")
			return
		}
		upstream := svc.UpstreamBranch()
		mainCRL, _, err := svc.FileAt(upstream, f.PKI.Layout.CRL, maxFleetCRLRead)
		if err != nil {
			rerr = err
			return
		}
		ownCRL, _, err := svc.FileAt(ri.AgentBranch(), f.PKI.Layout.CRL, maxFleetCRLRead)
		if err != nil {
			rerr = err
			return
		}
		if unpublished = !bytes.Equal(mainCRL, ownCRL); unpublished {
			return
		}
		depth, rerr = svc.FleetLandedDepth(upstream, paths)
	}); err != nil {
		return nil, err
	}
	if rerr != nil {
		return nil, rerr
	}
	if unpublished {
		log.Info().Str("fleet", ri.Name()).Msg("fleet: a left member is not revoked yet: this master's CRL is not on main; the next round, after the publish, revokes it")
		return nil, nil
	}
	root, err := pki.LoadRoot(m.fleetRootDir())
	if err != nil {
		return nil, err
	}
	base := f.PKI.CRL
	var revoked []string
	for _, c := range cands {
		rd, rok := depth[c.record]
		ld, lok := depth[c.leafPath]
		if rok && lok && ld < rd {
			log.Info().Str("agent", c.agent).Msg("fleet: a left member's certificate on main was issued after it left (a rejoin); it is not revoked")
			continue
		}
		out, next, err := m.revokeLeaf(ctx, ri, root, base, c.agent, c.leaf, leaveRevokeReason)
		if err != nil {
			return revoked, fmt.Errorf("revoke %s, who left: %w", c.agent, err)
		}
		if out.InstallError != "" {
			log.Warn().Str("agent", c.agent).Str("install_error", out.InstallError).Msg("fleet: the CRL revoking a left member was not installed here")
		}
		base = next
		revoked = append(revoked, c.agent)
	}
	return revoked, nil
}

// maxFleetCRLRead bounds the read of a CRL file for the leave duty's
// comparison of main's CRL with the agent branch's: the bound the fleet
// reader itself puts on a CRL with no declared max_size.
const maxFleetCRLRead = 64 << 20

// fleetLeaveRound is the leave duty in a master round (both hostings): the
// caller holds fleetOpMu and has checked actsAsFleetMaster (root held AND
// main's marker names this instance AND not paused). A failure is logged;
// the next round tries again.
func (ri *RepoInstance) fleetLeaveRound(ctx context.Context) {
	m := ri.env.m
	revoked, err := m.revokeLeftMembers(ctx, ri)
	if err != nil && ctx.Err() == nil {
		log.Warn().Err(err).Str("fleet", ri.Name()).Msg("fleet: revoking a member that left failed; the next round retries")
	}
	if len(revoked) > 0 {
		log.Info().Strs("agents", revoked).Str("fleet", ri.Name()).Msg("fleet: members that left are revoked; this round publishes it")
	}
}

// fleetMasterDuty is fleetPeers.duty for ri: the forge master's duties in its
// origin round, after the fetch and the fetch judge and before the push and
// the publish of main. Like fleetHostRound it never waits for fleetOpMu (a
// fleet operation in flight may run this very round inline): it skips, and
// the next round runs it.
func (ri *RepoInstance) fleetMasterDuty(ctx context.Context, svc *store.Service) {
	if ri == nil || !ri.fleetDoor || !ri.IsFleet() || svc == nil {
		return
	}
	m := ri.env.m
	if !m.fleetOpMu.TryLock() {
		return
	}
	defer m.fleetOpMu.Unlock()
	if !ri.actsAsFleetMaster(svc) {
		return
	}
	ri.fleetLeaveRound(ctx)
}
