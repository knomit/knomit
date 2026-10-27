package repos

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"knomit/internal/engine/store/fleetrec"

	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/domain/fact"
	"knomit/internal/engine/store"
	"knomit/internal/foundation/pki"
)

// The master's renewal duties (fleet design D17, P5), in each round of an
// instance that acts as the fleet's master (actsAsFleetMaster), after the
// leave duty and before the round moves main, so what they commit is
// published by that same round (the hub's local reconcile, the forge's push
// and publish). Every commit goes through the master's own-commit judge when
// main moves (#533), like enroll's and revoke's.
//
//   - CRL re-sign: when main's CRL is in the final third of its own lifetime
//     (pki.CRLNeedsResign) and the master's agent branch holds the same CRL
//     (nothing unpublished), the same entries are re-signed with Number + 1
//     for the CRL's own lifetime from now (pki.ReissueCRL), committed and
//     copied to <home>/fleet; this instance installs it from main in its
//     next tick, like every copy.
//   - Leaf renewal: every leaf on main in the final third of its own lifetime
//     (pki.NeedsRenewal) is re-issued for the same key and identity, for the
//     leaf's own lifetime, committed with its issuance record (the artifact
//     rules' leaf-renewal), when: main's CRL does not list it; exactly one
//     member record on main is filed for its id (what leaf-renewal checks);
//     that record is not `left` (the leave duty revokes those); the record's
//     key is the leaf's; and the agent branch holds the same leaf as main
//     (not renewed and unpublished). The master's own leaf included. Every
//     copy installs its renewed leaf from main in its own tick
//     (fleet_tick.go).
//
// THRESHOLDS are derived from each artifact's own validity (pki's final
// third), never a day count; a renewal is issued for the lifetime of what it
// replaces. Nothing here reads a constant validity.

// fleetRenewHooks are test seams; every field is nil in production.
type fleetRenewHooks struct {
	// agentRecords, when it returns an error, makes the read of agent's
	// records at branch fail (pushedLeaves): how a test gets an unreadable
	// folder without corrupting a store.
	agentRecords func(branch, agent string) error
}

var (
	fleetRenewHooksMu   sync.Mutex
	fleetRenewTestHooks fleetRenewHooks
)

func currentFleetRenewHooks() fleetRenewHooks {
	fleetRenewHooksMu.Lock()
	defer fleetRenewHooksMu.Unlock()
	return fleetRenewTestHooks
}

// fleetNowMu guards fleetNow.
var fleetNowMu sync.Mutex

// fleetNow is the clock the renewal duties DECIDE by (which artifact falls
// due); the certificates and CRLs they sign take their times from time.Now,
// so a test that moves this clock gets real, valid artifacts.
var fleetNow = time.Now

func fleetClock() time.Time {
	fleetNowMu.Lock()
	defer fleetNowMu.Unlock()
	return fleetNow()
}

// SetFleetClockForTest replaces the clock the master's renewal duties decide
// by (fleetNow) until the returned restore runs. For tests in other packages
// (the multi-instance e2e); production never calls it.
func SetFleetClockForTest(now func() time.Time) (restore func()) {
	fleetNowMu.Lock()
	prev := fleetNow
	fleetNow = now
	fleetNowMu.Unlock()
	return func() {
		fleetNowMu.Lock()
		fleetNow = prev
		fleetNowMu.Unlock()
	}
}

// fleetRenewRound is the renewal duties in a round of either hosting. Like
// fleetLeaveRound it is master work and checks so itself; the caller holds
// fleetOpMu. Failures are logged; the next round tries again.
func (ri *RepoInstance) fleetRenewRound(ctx context.Context, svc *store.Service) {
	if !ri.actsAsFleetMaster(svc) {
		return
	}
	m := ri.env.m
	if n, err := m.resignFleetCRL(ctx, ri); err != nil && ctx.Err() == nil {
		log.Warn().Err(err).Str("fleet", ri.Name()).Msg("fleet: re-signing the CRL failed; the next round retries")
	} else if n != "" {
		log.Info().Str("fleet", ri.Name()).Str("crl_number", n).Msg("fleet: the CRL is re-signed; this round publishes it")
	}
	renewed, err := m.renewFleetLeaves(ctx, ri)
	if err != nil && ctx.Err() == nil {
		log.Warn().Err(err).Str("fleet", ri.Name()).Msg("fleet: renewing a certificate failed; the next round retries")
	}
	if len(renewed) > 0 {
		log.Info().Strs("agents", renewed).Str("fleet", ri.Name()).Msg("fleet: certificates renewed; this round publishes them")
	}
}

// fleetPKIAt is the fleet's PKI at branch of ri, and its members.
func fleetAtBranch(ri *RepoInstance, branch string) (fleetrec.Fleet, error) {
	var out fleetrec.Fleet
	var ferr error
	if err := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			ferr = errors.New("fleet repository is not open")
			return
		}
		out, ferr = svc.FleetAt(branch)
	}); err != nil {
		return fleetrec.Fleet{}, err
	}
	return out, ferr
}

// resignFleetCRL is the CRL re-sign duty: the new CRL's Number, "" when
// nothing was due.
func (m *Manager) resignFleetCRL(ctx context.Context, ri *RepoInstance) (string, error) {
	f, err := m.fleetAtMain(ri)
	if err != nil {
		return "", err
	}
	if f.PKI == nil || f.PKI.CRL == nil || !pki.CRLNeedsResign(f.PKI.CRL, fleetClock()) {
		return "", nil
	}
	own, err := fleetAtBranch(ri, ri.AgentBranch())
	if err != nil {
		return "", err
	}
	if own.PKI == nil || own.PKI.CRL == nil || !bytes.Equal(own.PKI.CRL.Raw, f.PKI.CRL.Raw) {
		// A CRL this master has not published yet (a revocation, or a
		// re-sign of an earlier round): that one is newer; the next round
		// judges what main holds then.
		return "", nil
	}
	root, err := pki.LoadRoot(m.fleetRootDir())
	if err != nil {
		return "", err
	}
	now := time.Now()
	raw, number, err := pki.ReissueCRL(root, f.PKI.CRL, now, now.Add(pki.CRLLifetime(f.PKI.CRL)))
	if err != nil {
		return "", err
	}
	if err := m.writeFleetArtifacts(ctx, ri, map[string]string{f.PKI.Layout.CRL: string(raw)},
		"fleet: CRL re-signed (Number "+number.String()+")"); err != nil {
		return "", err
	}
	// The master's own copy (what GET /fleet/root hands a new master). The
	// installed CRL follows from main in this instance's own next tick, like
	// every copy's (it lists nothing new, so nothing waits on it).
	if err := pki.RecordCRL(m.fleetRootDir(), raw); err != nil {
		log.Warn().Err(err).Msg("fleet: the local copy of the re-signed CRL was not written")
	}
	return number.String(), nil
}

// leafRenewalDue is the leaf the renewal duty re-issues for id, read from
// main (f) and the master's agent branch (own), with the key it certifies;
// nil when none is due. The leaf is main's, due by its own lifetime, and the
// agent branch holds the same leaf (not renewed and unpublished). Every other
// condition must hold on BOTH main and the agent branch (renewableAt),
// because the round publishes the agent branch: a revocation, or a `left`
// record, that is on the agent branch and not on main yet (a revoke whose
// publish failed, a leave admitted in this very round) stops the renewal as
// surely as one on main (PR #537 review B1, B2). Otherwise the round would
// publish the revocation or the `left` record together with a fresh,
// unrevoked leaf, and nothing would ever revoke that one. A `left` record
// admitted on a member branch and not merged yet is on neither: the caller
// (renewFleetLeaves) also skips a leaf that id's own one names (pushedLeaves).
func leafRenewalDue(f, own fleetrec.Fleet, id string, now time.Time) (*x509.Certificate, ed25519.PublicKey) {
	leaf, err := f.PKI.Cert(id)
	if err != nil || leaf == nil || !pki.NeedsRenewal(leaf, now) {
		return nil, nil
	}
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, nil
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, nil
	}
	if own.PKI == nil {
		return nil, nil
	}
	if mine, err := own.PKI.Cert(id); err != nil || mine == nil || !bytes.Equal(mine.Raw, leaf.Raw) {
		return nil, nil
	}
	if !renewableAt(f, id, leaf, key) || !renewableAt(own, id, leaf, key) {
		return nil, nil
	}
	return leaf, pub
}

// pushedLeaves answers, for each id in ids, whether a `left` record FOR id
// that names serials[id] is on any pushed member branch of ri
// (isPushedBranch): a leave the master has admitted (the forge's fetch judge,
// the hub's receive-pack) and the consensus merger has not merged into the
// agent branch yet. The merger runs on its own goroutine, so leafRenewalDue's
// check of the agent branch alone loses this window whenever the duty reads
// the agent branch before the merge lands: the round then renews the
// leaver's leaf, the merge brings the `left` record (naming the OLD serial)
// after it, and the leave duty, which matches by serial (leftLeafDue), never
// revokes the renewed leaf (#651).
//
// Matched like leftLeafDue: the record must be id's own (in members/<id>/,
// agent: id, which the record-owner rule lets only id write) AND name id's
// current serial. A member's `left` record naming ANOTHER agent's serial is
// in the member's own folder, so it blocks no one else's renewal; a stale
// `left` record of an earlier membership names an earlier serial and blocks
// nothing. Every branch is read, not only agent/<id>: the judge admits id's
// commits on any agent/* branch, and a member's branch name is its own.
//
// A failure to read members/<id>/ on a branch is reported in errs[id] and id
// is NOT renewed this round (fail closed for that agent only: a missed
// renewal is retried next round; a leaf renewed for a leaver is never
// revoked). Only id's folder is read, so a defect elsewhere in a branch (its
// ontology, its PKI, another member's folder) blocks no one. err is a failure
// to list the branches at all: no one is renewed.
func pushedLeaves(ctx context.Context, ri *RepoInstance, serials map[string]string) (leaving map[string]bool, errs map[string]error, err error) {
	leaving, errs = map[string]bool{}, map[string]error{}
	rerr := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errors.New("fleet repository is not open")
			return
		}
		branches, lerr := svc.ListBranches(ctx)
		if lerr != nil {
			err = lerr
			return
		}
		upstream := svc.Remote().UpstreamBranch()
		for _, b := range branches {
			if !ri.isPushedBranch(b.Name, upstream) {
				continue
			}
			for id, serial := range serials {
				if leaving[id] || errs[id] != nil {
					continue
				}
				recs, aerr := svc.FleetAgentRecordsAt(b.Name, id)
				if h := currentFleetRenewHooks().agentRecords; h != nil && aerr == nil {
					aerr = h(b.Name, id)
				}
				if aerr != nil {
					errs[id] = fmt.Errorf("read %s's records at pushed branch %s: %w", id, b.Name, aerr)
					continue
				}
				for _, mem := range recs {
					if mem.Agent == id && mem.State == fact.MemberLeft && mem.LeafSerial == serial {
						leaving[id] = true
					}
				}
			}
		}
	})
	if rerr != nil {
		return nil, nil, rerr
	}
	return leaving, errs, err
}

// renewableAt reports whether leaf (id's, certifying key) may be renewed as
// fl, one branch's fleet, says: fl has a CRL and it does not list the leaf;
// fl holds exactly ONE member record for id (what the leaf-renewal artifact
// rule requires), and that record is not `left` (the leave duty revokes those)
// and names key.
func renewableAt(fl fleetrec.Fleet, id string, leaf *x509.Certificate, key ssh.PublicKey) bool {
	if fl.PKI == nil || fl.PKI.CRL == nil || pki.IsRevoked(fl.PKI.CRL, leaf.SerialNumber) {
		return false
	}
	var recs []fleetrec.Member
	for _, mem := range fl.Members {
		if mem.Agent == id {
			recs = append(recs, mem)
		}
	}
	return len(recs) == 1 && recs[0].State != fact.MemberLeft && fact.SameKey(recs[0].Key, key)
}

// renewFleetLeaves is the leaf renewal duty: the agent ids it re-issued a
// leaf for.
func (m *Manager) renewFleetLeaves(ctx context.Context, ri *RepoInstance) ([]string, error) {
	f, err := m.fleetAtMain(ri)
	if err != nil {
		return nil, err
	}
	if f.PKI == nil || f.PKI.Root == nil {
		return nil, nil
	}
	own, err := fleetAtBranch(ri, ri.AgentBranch())
	if err != nil {
		return nil, err
	}
	now := fleetClock()
	type due struct {
		id   string
		leaf *x509.Certificate
		pub  ed25519.PublicKey
	}
	var dues []due
	for _, id := range f.PKI.CertIDs() {
		if leaf, pub := leafRenewalDue(f, own, id, now); leaf != nil {
			dues = append(dues, due{id, leaf, pub})
		}
	}
	if len(dues) == 0 {
		return nil, nil
	}
	serials := make(map[string]string, len(dues))
	for _, d := range dues {
		serials[d.id] = d.leaf.SerialNumber.Text(16)
	}
	leaving, unread, err := pushedLeaves(ctx, ri, serials)
	if err != nil {
		return nil, err
	}
	kept := dues[:0]
	for _, d := range dues {
		if e := unread[d.id]; e != nil {
			log.Warn().Err(e).Str("fleet", ri.Name()).Str("agent", d.id).
				Msg("fleet: not renewing this certificate this round: a pushed branch's records for it cannot be read")
			continue
		}
		if !leaving[d.id] {
			kept = append(kept, d)
		}
	}
	if dues = kept; len(dues) == 0 {
		return nil, nil
	}
	root, err := pki.LoadRoot(m.fleetRootDir())
	if err != nil {
		return nil, err
	}
	issued, err := artifactPattern(ri.Ontology(), fact.PKIIssuance)
	if err != nil {
		return nil, err
	}
	var renewed []string
	var errs []error
	for _, d := range dues {
		email, err := pki.AgentEmail(d.id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		is, err := pki.IssueLeaf(root, d.pub, email, pki.RenewalValidity(d.leaf), m.renewalIssueTime(ri, d.id))
		if err != nil {
			errs = append(errs, fmt.Errorf("renew %s: %w", d.id, err))
			continue
		}
		if err := m.writeFleetArtifacts(ctx, ri, map[string]string{
			f.PKI.Layout.LeafPath(d.id):                       string(is.CertPEM),
			strings.Replace(issued, "*", is.Record.Serial, 1): string(is.CertPEM),
		}, "fleet: "+d.id+"'s certificate renewed (serial "+is.Record.Serial+")"); err != nil {
			errs = append(errs, fmt.Errorf("renew %s: %w", d.id, err))
			continue
		}
		if err := pki.RecordIssuance(m.fleetRootDir(), is); err != nil {
			log.Warn().Err(err).Str("agent", d.id).Msg("fleet: the local issuance log was not written for a renewal")
		}
		renewed = append(renewed, d.id)
	}
	return renewed, errors.Join(errs...)
}
