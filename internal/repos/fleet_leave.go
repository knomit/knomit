package repos

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"

	"knomit/internal/store/fleetrec"

	"github.com/rs/zerolog/log"

	"knomit/internal/domain/fact"
	"knomit/internal/pki"
	"knomit/internal/store"
)

// Leave means revoke (user ruling 2026-10-16: "the cert is valid per fleet,
// not in general"). A member cannot revoke its own leaf (only the root key
// signs a CRL), so the revocation is the MASTER's duty: once a member's `left`
// record is on the fleet's main, the master revokes the leaf that record
// names (leaf_serial, the certificate the member held when it left) exactly
// as Revoke does (revokeLeaf: the CRL and the revocation statement on its
// agent branch, its local copies, the CRL installed here), and its own round
// publishes it. A rejoin then needs no manual step: the master enrolls the
// same key again (the old leaf is on the CRL, so enroll admits it) and sends a
// new bundle.
//
// The duty runs in every round of an instance that acts as the fleet's master
// (actsAsFleetMaster, checked inside fleetLeaveRound for both hostings),
// BEFORE the round moves main, so the revocation goes out in that same round:
// on the hub in fleetHostRound (before the local reconcile's advance), on a
// forge after the fetch and the fetch judge (fleetPeers.duty, before the push
// and publish). It reads main only, and it is idempotent: a leaf main's CRL
// already lists is left alone, so a left record on main is revoked once.

// leaveRevokeReason is the reason a revocation the leave duty makes carries.
const leaveRevokeReason = "left the fleet"

// leftLeafDue is the leaf the leave duty revokes for mem, a record on main
// read from f, or nil. All of these must hold: main holds the fleet's PKI and
// a CRL; mem's state is `left` as written; mem names the leaf it left with
// (leaf_serial); main's current leaf for mem's agent has exactly that serial;
// main's CRL does not list it. So a stale left record merged after a manual
// revoke and a re-enroll names the OLD serial and never revokes the fresh
// leaf, and a left record written before leaf_serial existed revokes nothing
// (its operator revokes by hand). GET /fleet/members' revoke_due reads this
// same rule.
func leftLeafDue(f fleetrec.Fleet, mem fleetrec.Member) *x509.Certificate {
	if f.PKI == nil || f.PKI.CRL == nil || mem.State != fact.MemberLeft || mem.LeafSerial == "" {
		return nil
	}
	leaf, err := f.PKI.Cert(mem.Agent)
	if err != nil || leaf == nil || leaf.SerialNumber == nil {
		return nil
	}
	if leaf.SerialNumber.Text(16) != mem.LeafSerial || pki.IsRevoked(f.PKI.CRL, leaf.SerialNumber) {
		return nil
	}
	return leaf
}

// revokeLeftMembers revokes, for every member record on the fleet's main, the
// leaf leftLeafDue answers. It skips the pass, writing nothing, while the
// master's agent branch holds a CRL that main does not (an unpublished
// revocation): a CRL built from main's would drop it. The caller holds
// fleetOpMu and has checked actsAsFleetMaster; nothing is published here.
// Answers the agent ids it revoked.
func (m *Manager) revokeLeftMembers(ctx context.Context, ri *RepoInstance) ([]string, error) {
	f, err := m.fleetAtMain(ri)
	if err != nil {
		return nil, err
	}
	own := m.AgentID()
	type candidate struct {
		agent string
		leaf  *x509.Certificate
	}
	var cands []candidate
	for _, mem := range f.Members {
		if mem.Agent == own {
			continue
		}
		if mem.State == fact.MemberLeft && mem.LeafSerial == "" {
			log.Debug().Str("agent", mem.Agent).Msg("fleet: a left record names no certificate (written before leaf_serial); it is not revoked automatically")
		}
		if leaf := leftLeafDue(f, mem); leaf != nil {
			cands = append(cands, candidate{agent: mem.Agent, leaf: leaf})
		}
	}
	if len(cands) == 0 {
		return nil, nil
	}
	var unpublished bool
	var rerr error
	if err := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			rerr = errors.New("fleet repository is not open")
			return
		}
		mainCRL, _, err := svc.FileAt(svc.Remote().UpstreamBranch(), f.PKI.Layout.CRL, maxFleetCRLRead)
		if err != nil {
			rerr = err
			return
		}
		ownCRL, _, err := svc.FileAt(ri.AgentBranch(), f.PKI.Layout.CRL, maxFleetCRLRead)
		if err != nil {
			rerr = err
			return
		}
		unpublished = !bytes.Equal(mainCRL, ownCRL)
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

// fleetLeaveRound is the leave duty in a round of either hosting. It is
// master work and checks so itself, for both hostings: nothing runs unless
// this instance acts as the fleet's master (actsAsFleetMaster: it holds the
// root AND main's marker names it AND it is not paused), so a root holder
// main does not name, or a paused master, revokes nobody wherever the call
// sits. The caller holds fleetOpMu. A failure is logged; the next round tries
// again.
func (ri *RepoInstance) fleetLeaveRound(ctx context.Context, svc *store.Service) {
	if !ri.actsAsFleetMaster(svc) {
		return
	}
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
// the next round runs it. fleetLeaveRound checks the master predicate.
func (ri *RepoInstance) fleetMasterDuty(ctx context.Context, svc *store.Service) {
	if ri == nil || !ri.fleetDoor || !ri.IsFleet() || svc == nil {
		return
	}
	m := ri.env.m
	if !m.fleetOpMu.TryLock() {
		return
	}
	defer m.fleetOpMu.Unlock()
	ri.fleetLeaveRound(ctx, svc)
	ri.fleetRenewRound(ctx, svc) // fleet_renew.go
}
