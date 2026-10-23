package repos

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/domain/fact"
	"knomit/internal/foundation/pki"
)

// Fleet join (member): the one production caller of pki.InstallBundle in this
// file set besides the fleet create (archtest installCallers). See
// fleet_membership.go for the operations overview.

// FleetJoinSpec is what POST /fleet/join takes: the bundle (which names the
// fleet repository URL) and, for a forge-hosted fleet only, the joiner's own
// forge credential.
type FleetJoinSpec struct {
	AuthMethod, AuthToken string
	Bundle                []byte
}

// JoinFleet joins the fleet at the bundle's URL with the bundle the master's
// enroll returned (see the file comment). It answers state registered (this
// instance has the fleet repository) with record_state pending: the record is
// on this instance's own agent branch, judged by nothing yet on a forge, and
// is accepted only once the master merges it into main (recordStateOf). The
// answer's root_fingerprint is the root the bundle installed (§1.5 Join step
// 2: supplying the bundle is the operator's consent to that root).
func (m *Manager) JoinFleet(ctx context.Context, spec FleetJoinSpec) (FleetStatus, error) {
	defer m.lockFleetOp()()
	if err := m.recoverFleetCreate(ctx); err != nil {
		return FleetStatus{}, err
	}
	db := m.ControlDB()
	if db == nil {
		return FleetStatus{}, errors.New("fleet join: control.db is not open")
	}
	b, err := m.joinPreflight(db, spec.Bundle)
	if err != nil {
		return FleetStatus{}, err
	}
	if _, err := pki.InstallBundle(m.deps.Cfg.TLS.Dir, m.deps.KeyPath, spec.Bundle); err != nil {
		return FleetStatus{}, fmt.Errorf("fleet join: install the bundle: %w", err)
	}
	refuse := func(ri *RepoInstance, ferr *FleetError) (FleetStatus, error) {
		if uerr := m.undoJoin(context.WithoutCancel(ctx), ri); uerr != nil {
			ferr.Msg += "; undoing the join failed (" + uerr.Error() + ")"
		}
		_ = recordFleetAttempt(db, ferr, time.Now())
		return FleetStatus{}, ferr
	}
	// The clone decides the transport (ResolveOrigin) against the trust just
	// installed: the hub's listener chains to the bundle's root -> cert; a
	// forge is not a peer and takes the joiner's own credential.
	ri, err := m.Create(ctx, CreateSpec{Name: m.freeFleetName(b.FleetURL), Mode: "clone",
		Origin: &OriginSpec{URL: b.FleetURL, AuthMethod: spec.AuthMethod, AuthToken: spec.AuthToken}, fleetDoor: true}, func(Event) {})
	if errors.Is(err, ErrPeerCredential) {
		return refuse(nil, fleetErr(http.StatusUnprocessableEntity, "auth_invalid", err.Error()))
	}
	// On the clone path ErrOriginUnreachable comes only from ResolveOrigin's
	// classification, which join:check makes too: the same cause answers the
	// same code on both routes (joinReachErr).
	if errors.Is(err, ErrOriginUnreachable) {
		return refuse(nil, joinReachErr(originHost(b.FleetURL), err))
	}
	if err != nil {
		return refuse(nil, fleetErr(http.StatusBadGateway, "clone_failed", err.Error()))
	}
	if !fact.IsFleetOntology(ri.Ontology()) {
		return refuse(ri, fleetErr(http.StatusUnprocessableEntity, "not_a_fleet", "the repository's ontology is not the fleet ontology"))
	}
	if f, ferr := m.fleetAtMain(ri); ferr != nil || f.PKI == nil || f.PKI.Root == nil || !f.PKI.Root.Equal(b.Root) {
		return refuse(ri, fleetErr(http.StatusUnprocessableEntity, "root_differs", "the fleet repository's root (artifacts/fleet/root.crt on its main) is not the bundle's root: the bundle is for another fleet"))
	}
	if err := recordFleetRepo(db, ri.UID()); err != nil {
		return refuse(ri, fleetErr(http.StatusInternalServerError, "record_failed", err.Error()))
	}
	if err := m.writeOwnRecord(ctx, ri, fact.MemberActive); err != nil {
		return refuse(ri, fleetErr(http.StatusInternalServerError, "record_failed", err.Error()))
	}
	if err := m.pushAgentBranch(ctx, ri); err != nil {
		return refuse(ri, fleetErr(http.StatusConflict, "join_refused", "the fleet did not take this instance's record: "+err.Error()+". Nothing is kept; join again (a leaf the master issued just now reaches the fleet's main on its next round)"))
	}
	if err := recordFleetAttempt(db, nil, time.Now()); err != nil {
		return FleetStatus{}, err
	}
	if err := setFleetState(db, FleetRegistered, time.Now()); err != nil {
		return FleetStatus{}, err
	}
	log.Info().Str("fleet", ri.Name()).Msg("fleet: joined, record pending; it is accepted once the master merges it into main")
	// The join installed this instance's certificate (fleet_create.go's
	// create does the same).
	m.startTLSAfterInstall("fleet join")
	return m.FleetStatus(ctx)
}

// joinPreflight is every refusal a join (and its check) makes before
// anything is installed: this instance is in no fleet (fleet_present) and
// holds no trust material (trust_installed); the bundle is a bundle document
// naming the fleet URL (bundle_invalid), for this instance's key
// (key_mismatch) and agent id (agent_mismatch).
func (m *Manager) joinPreflight(db *sql.DB, raw []byte) (pki.Bundle, error) {
	if err := m.refuseFleetPresent(db); err != nil {
		return pki.Bundle{}, err
	}
	if err := m.refuseInstalledTrust(); err != nil {
		return pki.Bundle{}, err
	}
	b, err := pki.SplitBundle(raw)
	if err != nil {
		return pki.Bundle{}, fleetErr(http.StatusUnprocessableEntity, "bundle_invalid", err.Error())
	}
	if b.FleetURL == "" {
		return pki.Bundle{}, fleetErr(http.StatusUnprocessableEntity, "bundle_invalid", "the bundle names no fleet repository URL: enroll this instance again on the master")
	}
	if why := bundleURLProblem(b.FleetURL); why != "" {
		return pki.Bundle{}, fleetErr(http.StatusUnprocessableEntity, "bundle_invalid", "the bundle's fleet repository URL "+credentialFree(b.FleetURL)+" "+why+": enroll this instance again on the master")
	}
	if _, err := pki.PreviewBundle(m.deps.KeyPath, raw); err != nil {
		if errors.Is(err, pki.ErrKeyMismatch) {
			return pki.Bundle{}, fleetErr(http.StatusUnprocessableEntity, "key_mismatch", err.Error())
		}
		return pki.Bundle{}, fleetErr(http.StatusUnprocessableEntity, "bundle_invalid", err.Error())
	}
	var leafID string
	if len(b.Leaf.EmailAddresses) == 1 {
		leafID = pki.AgentIDOfEmail(b.Leaf.EmailAddresses[0])
	}
	if leafID != m.AgentID() {
		return pki.Bundle{}, fleetErr(http.StatusUnprocessableEntity, "agent_mismatch", fmt.Sprintf("the bundle's certificate is for agent %q, not this instance's %q", leafID, m.AgentID()))
	}
	return b, nil
}

// bundleURLProblem is why a bundle's url: line is not a fleet repository URL
// a join can clone, or "": the line is unauthenticated (SplitBundle checks
// only the agent, the root and the serial), so the join checks its shape
// before anything dials it, and join and join:check refuse a bad one with
// the same code (bundle_invalid; PR #527 N1). A master's enroll writes one of
// two shapes: a hub's plain https://<address>/git/<repo> (an https URL with a
// host, never the internal fleet scheme pki.GitURL derives), or a forge-hosted fleet's
// origin URL, which the origin routes accept as https, ssh:// or scp-style
// git@host:path, or file:// under a local origin root. Anything else (another
// scheme, plain http, a URL with no host) is refused.
func bundleURLProblem(u string) string {
	if !strings.Contains(u, "://") {
		if at, colon := strings.Index(u, "@"), strings.Index(u, ":"); at > 0 && colon > at+1 && colon < len(u)-1 {
			return "" // scp-style git@host:path, a forge's ssh origin
		}
		return "is not a URL"
	}
	p, err := url.Parse(u)
	if err != nil {
		return "is not a URL"
	}
	switch p.Scheme {
	case "https", "ssh":
		if p.Hostname() == "" {
			return "names no host"
		}
		return ""
	case "file":
		if p.Path == "" {
			return "names no path"
		}
		return ""
	}
	return "is not https:// (a hub's) or a forge origin's (https://, ssh://, git@host:path, file://)"
}

// refuseFleetOrigin refuses an origin for the fleet repository that no
// member's join could clone (#527 F1): the bundle carries the master's origin
// URL, and a join refuses every shape bundleURLProblem refuses (plain http,
// another scheme, no host). Asked where the choice is made, a forge create
// and an attach to the fleet repository, so the operator is not sent to
// re-enroll joiners for a scheme the master's own origin has.
func refuseFleetOrigin(u string) error {
	if why := bundleURLProblem(u); why != "" {
		return fleetErr(http.StatusUnprocessableEntity, "origin_invalid",
			"the fleet repository's origin "+credentialFree(u)+" "+why+
				": members join only an https://, ssh://, git@host:path or file:// origin, so a fleet cannot live there")
	}
	return nil
}

// The two FleetJoinCheck.Hosting values.
const (
	HostingMaster = "master" // a role, not a branch (consensus_branch_literals_test allows this name)
	HostingForge  = "forge"
)

// FleetJoinCheck is what POST /fleet/join:check answers: where the bundle's
// fleet lives and the root the operator confirms BEFORE the join installs it.
type FleetJoinCheck struct {
	URL string `json:"url"`
	// Hosting is "master" (the URL is an instance of the bundle's fleet,
	// reached over the fleet transport with the bundle's certificate) or
	// "forge" (anything else: the joiner's own credential is used).
	Hosting         string `json:"hosting"`
	Host            string `json:"host"`
	RootFingerprint string `json:"root_fingerprint"`
	AgentID         string `json:"agent_id"`
	// AuthRequired: a forge refused the credential given (or none).
	AuthRequired bool   `json:"auth_required"`
	Detail       string `json:"detail,omitempty"`
}

// CheckFleetJoin is the join's check (POST /fleet/join:check): the join's
// pre-flights, then the bundle's URL is reached presenting the BUNDLE's
// certificate and judged against the BUNDLE's root, without installing
// anything (the material is written to a private temporary directory that is
// removed before returning). An instance of that fleet is hosting "master"
// (a credential for it is auth_invalid); anything else is a forge, probed
// with the joiner's credential (cert as input is auth_invalid). A host that
// refuses the bundle, or whose certificate this bundle refuses, is
// bundle_refused (422); any other failure to classify is unreachable (502),
// as the join says for the same cause (joinReachErr). It writes nothing.
func (m *Manager) CheckFleetJoin(ctx context.Context, spec FleetJoinSpec) (FleetJoinCheck, error) {
	defer m.lockFleetOp()()
	db := m.ControlDB()
	if db == nil {
		return FleetJoinCheck{}, errors.New("fleet join check: control.db is not open")
	}
	b, err := m.joinPreflight(db, spec.Bundle)
	if err != nil {
		return FleetJoinCheck{}, err
	}
	if spec.AuthMethod == "cert" {
		return FleetJoinCheck{}, fleetErr(http.StatusUnprocessableEntity, "auth_invalid", ErrCertNotAnInput.Error())
	}
	agent := pki.AgentIDOfEmail(b.Leaf.EmailAddresses[0]) // joinPreflight checked it is this instance's
	rootID, _ := pki.RootID(b.Root)
	out := FleetJoinCheck{URL: b.FleetURL, Host: originHost(b.FleetURL), RootFingerprint: rootID, AgentID: agent}

	tmp, err := os.MkdirTemp("", "knomit-join-check-")
	if err != nil {
		return FleetJoinCheck{}, err
	}
	defer os.RemoveAll(tmp)
	for name, body := range map[string][]byte{pki.InstanceCertFile: certPEMOf(b.Leaf), pki.RootCertFile: b.RootPEM, pki.CRLFile: b.CRLPEM} {
		if err := os.WriteFile(filepath.Join(tmp, name), body, 0o600); err != nil {
			return FleetJoinCheck{}, err
		}
	}
	cctx := ctx
	if d := m.deps.Cfg.Git.NetworkTimeout; d > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	peer, err := pki.ClassifyOrigin(cctx, tmp, m.deps.KeyPath, b.FleetURL)
	if err != nil {
		return FleetJoinCheck{}, joinReachErr(out.Host, err)
	}
	if peer {
		if spec.AuthMethod != "" && spec.AuthMethod != "none" || spec.AuthToken != "" {
			return FleetJoinCheck{}, fleetErr(http.StatusUnprocessableEntity, "auth_invalid", ErrPeerCredential.Error())
		}
		out.Hosting = HostingMaster
		return out, nil
	}
	out.Hosting = HostingForge
	// Not marked decided: ResolveOrigin judges the joiner's own auth like any
	// probe's (trust_installed guarantees this instance holds no fleet root,
	// so it dials nothing here).
	res, err := m.ProbeOriginRefs(ctx, OriginSpec{URL: b.FleetURL, AuthMethod: spec.AuthMethod, AuthToken: spec.AuthToken})
	if err != nil {
		return FleetJoinCheck{}, err
	}
	switch {
	case res.AuthRequired:
		out.AuthRequired, out.Detail = true, res.Detail
	case !res.Reachable:
		return FleetJoinCheck{}, fleetErr(http.StatusBadGateway, "unreachable", "couldn't reach "+out.Host+": "+res.Detail)
	}
	return out, nil
}

// joinReachErr names a failed classification of the bundle's fleet URL, for
// join and join:check alike. Only a genuine refusal is bundle_refused (422):
// the host refused this bundle's certificate (pki.ErrRefusedByPeer: typically
// a bundle revoked since it was issued), or our verifier refused the
// certificate the host presents as one of this fleet's
// (pki.ErrOriginCertRefused). Everything else is unreachable (502): no TCP
// connection, a timeout or a reset after the handshake, this instance's own
// trust material unusable. Those are no verdict on the bundle, and "ask for a
// new bundle" would send the operator the wrong way.
//
// The text is the classification's own: the join's ResolveOrigin wrap
// (ErrOriginUnreachable, "origin was not attached") is dropped (reachCause),
// so both routes say the same words for the same cause.
func joinReachErr(host string, err error) *FleetError {
	cause := reachCause(err).Error()
	if errors.Is(err, pki.ErrRefusedByPeer) || errors.Is(err, pki.ErrOriginCertRefused) {
		return fleetErr(http.StatusUnprocessableEntity, "bundle_refused", "the fleet at "+host+" refused this bundle ("+cause+"): ask the fleet's master for a new bundle")
	}
	return fleetErr(http.StatusBadGateway, "unreachable", "couldn't reach "+host+": "+cause)
}

// reachCause is err without ResolveOrigin's wrap (fmt.Errorf("%w: %w",
// ErrOriginUnreachable, cause)): the cause. Any other error is itself.
func reachCause(err error) error {
	m, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err
	}
	var rest []error
	wrapped := false
	for _, e := range m.Unwrap() {
		if e == ErrOriginUnreachable {
			wrapped = true
			continue
		}
		rest = append(rest, e)
	}
	if !wrapped || len(rest) != 1 {
		return err
	}
	return rest[0]
}

// originHost is the host an origin URL names: host[:port] of a URL, the
// host of an scp-style git@host:path, "" for a local path.
func originHost(u string) string {
	if strings.Contains(u, "://") {
		if p, err := url.Parse(u); err == nil {
			return p.Host
		}
		return ""
	}
	if at, colon := strings.Index(u, "@"), strings.Index(u, ":"); at >= 0 && colon > at {
		return u[at+1 : colon]
	}
	return ""
}

// undoJoin takes a refused join back: the agent branch the mount may have
// pushed to a forge (with the lease, as the create's rollback does), the
// repository, the installed material. Every step runs; the errors of the
// last two are joined. The retract is best effort: a knomit host refuses
// every delete, and what the mount pushed there is main's tip, no record.
func (m *Manager) undoJoin(ctx context.Context, ri *RepoInstance) error {
	var errs []error
	if ri != nil {
		if err := m.retractFleetBranch(ctx, ri); err != nil {
			log.Info().Err(err).Str("fleet", ri.Name()).Msg("fleet join refused: the agent branch on the origin is left (it holds no record)")
		}
		if err := m.deleteRepo(ri.Name(), "fleet join refused"); err != nil {
			errs = append(errs, err)
		}
	}
	if err := pki.Uninstall(m.deps.Cfg.TLS.Dir); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// installedLeafSerial is the serial (lowercase hex) of the instance
// certificate installed in [tls].dir, "" when none is installed or it does
// not parse.
func (m *Manager) installedLeafSerial() string {
	st, err := pki.Status(m.deps.Cfg.TLS.Dir, m.deps.KeyPath)
	if err != nil || st.Cert == nil || st.Cert.Serial == nil {
		return ""
	}
	return st.Cert.Serial.Text(16)
}
