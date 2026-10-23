package repos

import (
	"context"
	"net/http"
	"path/filepath"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/pki"
)

// Fleet membership (POST /api/v1/fleet/enroll | join | leave | revoke |
// decommission, GET|PUT|DELETE /api/v1/fleet/root; every one served on the
// local listener only). Each runs under fleetOpMu, like the create, because
// each reads the fleet state and then changes it.
//
//   - ENROLL (master): issue a leaf for (agent id, ssh-ed25519 key), commit it
//     and its issuance record on the master's agent branch, publish main, and
//     hand back the bundle document (pki.FleetBundle): the fleet repository
//     URL members use (fleetURLForBundle), then leaf + root + CRL.
//   - JOIN (member): check the bundle is for THIS instance's key and id,
//     install it, clone the fleet repository at the bundle's URL through the
//     fleet door, check its root is the bundle's, record it, write the own
//     record and push it ONCE.
//     Any refusal, the push's included, undoes all of it: no repository, no
//     installed material, no record, state standalone.
//   - LEAVE (member): write `left`, push once, then remove the fleet here
//     whatever the push did (works offline). Leave means revoke: once the
//     left record is on main, the master's round revokes the leaf
//     (revokeLeftMembers, fleet_leave.go).
//   - REVOKE (master): a new CRL from the CURRENT one on main plus the leaf's
//     serial, its revocation statement, committed and published; the CRL is
//     installed here at once.
//   - DECOMMISSION (master): remove the fleet here and delete <home>/fleet.
//   - ROOT (master switch): GET returns the files of <home>/fleet, PUT
//     installs them where there are none, DELETE stands down (one last
//     commit on main says so, then the files go; the membership stays). It is
//     the switch's first step and needs nothing first; the new master's Host
//     here checks its own main for the marker's absence. Nothing contacts the other
//     side.
//
// Where each lives: fleet_enroll.go (ENROLL, renewal checks, bundle URL),
// fleet_join.go (JOIN, the join check), fleet_revoke.go (REVOKE, revokeLeaf),
// fleet_root.go (ROOT), fleet_leave.go (LEAVE, DECOMMISSION), fleet_move.go
// (Host here, Follow). This file keeps what they share: the master
// predicates and the rebuild.

// masterFleet is the fleet repository when this instance is its master and
// does master work now (no fleetMasterPause), else the refusal: enroll,
// revoke and decommission.
func (m *Manager) masterFleet() (*RepoInstance, error) {
	ri, err := m.masterFleetByRoot()
	if err != nil {
		return nil, err
	}
	if err := m.refuseMasterPaused(ri); err != nil {
		return nil, err
	}
	return ri, nil
}

// masterFleetByRoot is the fleet repository when this instance is its master
// (isFleetMaster: the root key is here and its root is on main), else the
// refusal.
func (m *Manager) masterFleetByRoot() (*RepoInstance, error) {
	ri := m.fleetRepo()
	if ri == nil {
		return nil, fleetErr(http.StatusConflict, "not_registered", "this instance is standalone: no fleet repository is mounted")
	}
	master := false
	if err := ri.WithRead(func(svc *store.Service) { master = ri.isFleetMaster(svc) }); err != nil {
		return nil, err
	}
	if !master {
		return nil, fleetErr(http.StatusConflict, "not_master", "this instance is not the fleet's master: "+filepath.Join(m.fleetRootDir(), pki.RootKeyFile)+" is not here, its root.crt is not the root on the fleet's main, or the fleet's main does not name it the acting master ("+fact.FleetMasterPath+")")
	}
	return ri, nil
}

// hasOrigin reports whether ri's copy here syncs with an origin.
func (m *Manager) hasOrigin(ri *RepoInstance) (bool, error) {
	o, err := m.originOf(ri)
	if err != nil {
		return false, err
	}
	return o != nil, nil
}

// publishMasterWrite publishes a commit the master just made on its agent
// branch: with an origin, push the agent branch and then main (pushFleet);
// without one (the hub), fast-forward main here.
func (m *Manager) publishMasterWrite(ctx context.Context, ri *RepoInstance) error {
	origin, err := m.hasOrigin(ri)
	if err != nil {
		return err
	}
	if origin {
		return m.pushFleet(ctx, ri)
	}
	return m.publishFleetMainNow(ctx, ri)
}

// refuseNoHubListener is what a copy of the fleet repository needs to be the
// hub (no origin): members push to it over the mTLS listener at the addresses
// its record advertises. Create without an origin and the master's detach of
// its origin both check it.
//
// The listener is judged as CONFIGURED (tlsAddr: the running listeners'
// [tls].addr, which a reload can set), never as listening: an instance that
// is not yet in a fleet cannot listen, for its listener waits for the
// certificate the create installs, and the create starts it
// (startTLSAfterInstall).
func (m *Manager) refuseNoHubListener() error {
	if m.tlsAddr() == "" {
		return fleetErr(http.StatusUnprocessableEntity, "listener_required", "a fleet hosted here needs the mTLS listener: set [tls].addr in knomit.toml (members push to it), then run `knomit reload`")
	}
	if len(m.deps.Cfg.ExternalAddresses) == 0 {
		return fleetErr(http.StatusUnprocessableEntity, "addresses_required", "a fleet hosted here needs external_addresses in knomit.toml: they are how members reach this instance")
	}
	return nil
}

// FleetIndexRebuild is what POST /fleet/index-rebuild answers: the index job
// rebuilding the fleet repository's agent branch (Absorbed: an identical
// rebuild already ran, and this is it).
type FleetIndexRebuild struct {
	JobID    string `json:"id"`
	Repo     string `json:"repo"`
	Branch   string `json:"branch"`
	Absorbed bool   `json:"absorbed"`
}

// RebuildFleetIndex is POST /fleet/index-rebuild (user ruling 2026-10-16):
// the repository Settings' Rebuild for the fleet repository, whose generic
// route (POST /repos/{repo}/branches/{b}/index-rebuilds) is refused like
// every generic mutation of it. The same lifecycle event (Rebuild) on the
// agent branch, the one the generic Rebuild names; its progress reads through
// the generic job routes. Under fleetOpMu like every fleet operation.
func (m *Manager) RebuildFleetIndex(ctx context.Context) (FleetIndexRebuild, error) {
	defer m.lockFleetOp()()
	ri := m.fleetRepo()
	if ri == nil {
		return FleetIndexRebuild{}, fleetErr(http.StatusConflict, "not_registered", "this instance is in no fleet")
	}
	branch := ri.AgentBranch()
	rep, err := m.Send(ctx, ri, Rebuild(branch))
	if err != nil {
		return FleetIndexRebuild{}, err
	}
	return FleetIndexRebuild{JobID: rep.JobID, Repo: ri.Name(), Branch: branch, Absorbed: rep.Absorbed}, nil
}
