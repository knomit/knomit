package repos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"golang.org/x/crypto/ssh"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// FleetError is a refusal of a fleet operation, with the HTTP status and the
// machine-readable code the REST layer returns (F09 design, state machine).
type FleetError struct {
	Status int
	Code   string
	Msg    string
	// Choices, when set, are the values the caller may pick from to retry
	// (address_required: this instance's external_addresses).
	Choices []string
}

func (e *FleetError) Error() string { return e.Code + ": " + e.Msg }

func fleetErr(status int, code, msg string) *FleetError {
	return &FleetError{Status: status, Code: code, Msg: msg}
}

// FleetStatus is what GET /api/v1/fleet reports.
type FleetStatus struct {
	State        string     `json:"state"` // standalone | registering | registered
	FleetRepo    string     `json:"fleet_repo,omitempty"`
	FleetURL     string     `json:"fleet_url,omitempty"`
	AgentID      string     `json:"agent_id"`
	RecordState  string     `json:"record_state,omitempty"` // pending | active | left | revoked, read at the fleet's main
	Since        *time.Time `json:"since,omitempty"`
	RegisteredAt *time.Time `json:"registered_at,omitempty"`
	LastAttempt  *time.Time `json:"last_attempt,omitempty"`
	LastError    string     `json:"last_error,omitempty"`

	// F10. ExternalAddresses is the configured list ([] when unset). Record is
	// this instance's own record on its agent branch head (nil when there is
	// none); RecordCurrent says whether it matches what the current config
	// and binary advertise; RecordPendingUpdate says the fleet's main still
	// holds an older version of it. Notice is set while no external address
	// is configured.
	ExternalAddresses   []string      `json:"external_addresses"`
	Record              *MemberFields `json:"record,omitempty"`
	RecordCurrent       *bool         `json:"record_current,omitempty"`
	RecordPendingUpdate bool          `json:"record_pending_update"`
	Notice              string        `json:"notice,omitempty"`

	// Master says this instance is the fleet's master (isFleetMaster: the
	// root key file is here and its root is the one on the fleet's main).
	// Hosting is where the fleet repository's copy here syncs: "none" (no
	// origin: this instance is the hub) or the origin's URL; empty when no
	// fleet repository is mounted. RootDir is <home>/fleet, where the root
	// key lives on the master (and where a master switch copies it to).
	Master  bool   `json:"master"`
	Hosting string `json:"hosting,omitempty"`
	RootDir string `json:"root_dir"`
}

// MemberFields are a member record's advertised, informational fields as the
// REST layer shows them (the key is not repeated here).
type MemberFields struct {
	Addresses    []string          `json:"addresses"`
	Git          string            `json:"git,omitempty"`
	Capabilities map[string]string `json:"capabilities"`
	Host         string            `json:"host,omitempty"`
}

// MemberFieldsOf is m's advertised fields for display, as written (someone
// else's odd values are shown, never used).
func MemberFieldsOf(m fact.Member) MemberFields {
	f := MemberFields{Addresses: m.Addresses, Git: m.Git, Capabilities: m.Capabilities, Host: m.Host}
	if f.Addresses == nil {
		f.Addresses = []string{}
	}
	if f.Capabilities == nil {
		f.Capabilities = map[string]string{}
	}
	return f
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// fleetRepo is the ONE mounted repository whose ontology is the fleet ontology
// (nil when there is none). That is how an instance knows its fleet: no name,
// no URL in control.db.
func (m *Manager) fleetRepo() *RepoInstance {
	for _, name := range m.Names() {
		if ri := m.Get(name); ri != nil && fact.IsFleetOntology(ri.Ontology()) {
			return ri
		}
	}
	return nil
}

// IsFleetRepo reports whether name is this instance's fleet repository.
func (m *Manager) IsFleetRepo(name string) bool {
	ri := m.fleetRepo()
	return ri != nil && ri.Name() == name
}

func (m *Manager) fleetDB() (*FleetRow, error) {
	db := m.ControlDB()
	if db == nil {
		return nil, errors.New("fleet: control.db is not open")
	}
	r, err := readFleetRow(db)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// FleetStatus reports the state machine, the fleet repository, this agent's id
// and its record's state at the fleet's main ("pending" until the fleet's
// master merges the registration into main).
func (m *Manager) FleetStatus(ctx context.Context) (FleetStatus, error) {
	row, err := m.fleetDB()
	if err != nil {
		return FleetStatus{}, err
	}
	st := FleetStatus{
		State: row.State, AgentID: row.AgentID, Since: timePtr(row.Since),
		RegisteredAt: timePtr(row.RegisteredAt), LastAttempt: timePtr(row.LastAttempt), LastError: row.LastError,
		ExternalAddresses: append([]string{}, m.deps.Cfg.ExternalAddresses...),
		RootDir:           m.fleetRootDir(),
	}
	if len(st.ExternalAddresses) == 0 {
		st.Notice = NoAddressesNotice
	}
	ri := m.fleetRepo()
	if ri == nil {
		return st, nil
	}
	head, onHead, _ := m.ownRecordAt(ri, ri.AgentBranch())
	if onHead {
		f := MemberFieldsOf(head.Member)
		st.Record = &f
		current := head.Member.SameAdvertised(m.advertised())
		st.RecordCurrent = &current
	}
	st.FleetRepo = ri.Name()
	if o, oerr := m.originOf(ri); oerr == nil {
		st.Hosting = "none"
		if o != nil {
			st.FleetURL, st.Hosting = o.URL, o.URL
		}
	}
	_ = ri.WithRead(func(svc *store.Service) { st.Master = ri.isFleetMaster(svc) })
	st.RecordState = "pending"
	members, lerr := m.fleetMembersAtMain(ri)
	if lerr == nil {
		for _, mem := range members {
			if mem.Agent == row.AgentID {
				st.RecordState = mem.State
				// The RECORD differs, not "the branch is ahead": the sync
				// loop merges main into the agent branch, so the branch is
				// routinely ahead by merge commits with nothing to accept.
				st.RecordPendingUpdate = onHead && mem.Blob != head.Blob
				break
			}
		}
	}
	return st, nil
}

// FleetMembers returns the member records at the fleet repository's main.
func (m *Manager) FleetMembers(ctx context.Context) ([]store.FleetMember, error) {
	ri := m.fleetRepo()
	if ri == nil {
		return nil, fleetErr(http.StatusConflict, "not_registered", "this instance is standalone: no fleet repository is mounted")
	}
	return m.fleetMembersAtMain(ri)
}

// FleetAtMain returns what the commit judge reads from the fleet repository's
// main: the member records and the certificate artifacts, from one revision.
func (m *Manager) FleetAtMain(ctx context.Context) (store.Fleet, error) {
	ri := m.fleetRepo()
	if ri == nil {
		return store.Fleet{}, fleetErr(http.StatusConflict, "not_registered", "this instance is standalone: no fleet repository is mounted")
	}
	return m.fleetAtMain(ri)
}

// fleetMembersAtMain reads the member records at the fleet repository's
// consensus branch (fleetAtMain), as readers show them: a member whose leaf
// the CRL at that SAME revision lists is "revoked", whatever its record says
// (M6: revoked is derived, never written). Only readers get this view
// (FleetStatus, GET /fleet/members); the judge reads the records as written
// (FleetAtMain) and refuses a revoked leaf by rule B.
func (m *Manager) fleetMembersAtMain(ri *RepoInstance) ([]store.FleetMember, error) {
	f, err := m.fleetAtMain(ri)
	for i := range f.Members {
		if f.PKI.Revoked(f.Members[i].Agent) {
			f.Members[i].State = fact.MemberRevoked
		}
	}
	return f.Members, err
}

// fleetAtMain reads the fleet at the fleet repository's consensus branch: the
// origin's branch, else the one recorded with the repo
// (store.UpstreamBranch). Never a name assumed for it.
func (m *Manager) fleetAtMain(ri *RepoInstance) (store.Fleet, error) {
	var out store.Fleet
	var lerr error
	if err := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			lerr = errors.New("fleet repository is not open")
			return
		}
		upstream := svc.UpstreamBranch()
		if upstream == "" {
			lerr = fmt.Errorf("fleet repository: %w", store.ErrNoConsensusBranch)
			return
		}
		out, lerr = svc.FleetAt(upstream)
	}); err != nil {
		return store.Fleet{}, err
	}
	return out, lerr
}

func (m *Manager) originOf(ri *RepoInstance) (*Origin, error) {
	o := m.Origins()
	if o == nil {
		return nil, errors.New("origins unavailable")
	}
	return o.Get(ri.UID())
}

// pushFleet pushes the fleet repository's agent branch now and applies the
// outcome. The sync loop calls fleetPushed with its own pushes, which is the
// retry. This push is one-shot: it neither consults nor changes the loop's
// circuit breakers. The retry DOES ride them: while the loop's push breaker is
// open the loop does not push, so a pending publish (a forge create whose
// main is not pushed yet, state registering) completes only at the breaker's next probe — up to 30 minutes later (breakerMaxOpen).
func (m *Manager) pushFleet(ctx context.Context, ri *RepoInstance) error {
	perr := m.pushAgentBranch(ctx, ri)
	if perr == nil {
		publishes := false
		if werr := ri.WithRead(func(svc *store.Service) { publishes = ri.publishesFleetMain(svc) }); werr != nil {
			perr = werr
		} else if publishes {
			// As the sync loop's round does: a push of the fleet repository
			// by its root holder includes publishing main.
			perr = m.publishFleetMainNow(ctx, ri)
		}
	}
	m.fleetPushed(ri.Name(), perr)
	return perr
}

// pushAgentBranch pushes the fleet repository's agent branch to its origin
// once (the one push of a join or a leave, and the first half of pushFleet).
func (m *Manager) pushAgentBranch(ctx context.Context, ri *RepoInstance) error {
	var perr error
	if werr := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			perr = errors.New("fleet repository is not open")
			return
		}
		remote, err := svc.Remote().GetRemote("origin")
		if err != nil || remote == nil {
			perr = fmt.Errorf("fleet repository has no origin: %v", err)
			return
		}
		auth, err := makeRemoteAuthFn(m.deps.Cfg.Remote, m.deps.KeyPath)(remote)
		if err != nil {
			perr = err
			return
		}
		_, perr = svc.Remote().Push(ctx, ri.AgentBranch(), auth)
	}); werr != nil && perr == nil {
		perr = werr
	}
	return perr
}

// fleetPushed applies one push outcome of the fleet repository to the state
// machine: a failure records last_error and changes nothing else; a success
// clears it and completes registering (-> registered). Pushes of any other
// repository are ignored. Leaving is not a state: POST /fleet/leave pushes
// once and removes the fleet whatever the push did (LeaveFleet).
//
// Registering on the instance that holds the fleet root (a forge create
// whose publish of main failed) completes only when main is published. The
// publish is part of every push of the fleet repository by its root holder
// (RepoInstance.publishesFleetMain: the sync loop's round and pushFleet), so
// pushErr already carries a failed publish and the state stays registering,
// with last_error, while it fails. A member has nothing to publish, and its
// pushed agent branch completes its registration.
func (m *Manager) fleetPushed(repo string, pushErr error) {
	if !m.IsFleetRepo(repo) {
		return
	}
	db := m.ControlDB()
	if db == nil {
		return
	}
	now := time.Now()
	if err := recordFleetAttempt(db, pushErr, now); err != nil {
		log.Warn().Err(err).Msg("fleet: recording the push attempt failed")
	}
	if pushErr != nil {
		return
	}
	row, err := readFleetRow(db)
	if err != nil {
		return
	}
	if row.State == FleetRegistering {
		if err := setFleetState(db, FleetRegistered, now); err != nil {
			log.Warn().Err(err).Msg("fleet: entering registered failed")
		}
	}
}

// FleetRetry pushes the fleet repository once, as a sync tick would. Used by
// the sync loop's absence (tests, a disabled background sync) and by callers
// that want an immediate retry.
func (m *Manager) FleetRetry(ctx context.Context) {
	if ri := m.fleetRepo(); ri != nil {
		_ = m.pushFleet(ctx, ri)
	}
}

// guardFleetRemoval refuses an archive or delete of the fleet repository
// through the generic doors, in every fleet state and for every role: the
// fleet repository is removed only by knomit's fleet code, which uses the
// unguarded deleteRepo.
func (m *Manager) guardFleetRemoval(name string) error {
	if !m.IsFleetRepo(name) {
		return nil
	}
	return fleetErr(http.StatusConflict, "fleet_repository", "this is the fleet repository: it is never archived or deleted directly; leave with `POST /api/v1/fleet/leave` (Fleet page); the master decommissions with `POST /api/v1/fleet/decommission`")
}

// writeOwnRecord writes this instance's member record on the fleet
// repository's agent branch: the persisted agent id, state, and the advertised
// fields the current config and binary give (key, host, branch, addresses,
// git, capabilities). The path is stable per agent, so the record is one fact
// whose versions are the agent's history.
func (m *Manager) writeOwnRecord(ctx context.Context, ri *RepoInstance, state string) error {
	rec := m.advertised()
	rec.State = state
	return m.writeRecord(ctx, ri, rec, "fleet: "+rec.Agent+" is "+state, "")
}

// freeFleetName picks a local name for the fleet repository mount: the URL's
// last segment (without .git), made unique. It is a label only.
func (m *Manager) freeFleetName(url string) string {
	base := strings.TrimSuffix(path.Base(strings.TrimRight(url, "/")), ".git")
	if base == "" || base == "." || base == "/" {
		base = "fleet"
	}
	name := base
	for i := 2; m.Get(name) != nil; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// memberRecordPath is the stable path of an agent's member record: one fact
// per agent, so its versions are the agent's history. The file name keeps the
// uuid8 shape, derived from the id instead of random.
func memberRecordPath(root, agentID string) string {
	sum := sha256.Sum256([]byte(agentID))
	return path.Join(root, fact.MembersTopic, agentID, hex.EncodeToString(sum[:4])+".md")
}

// ownFleetKeys is every key this instance's member record has held in its
// fleet (the record's versions), for E4. Nil when standalone.
func (m *Manager) ownFleetKeys() []ssh.PublicKey {
	ri := m.fleetRepo()
	if ri == nil {
		return nil
	}
	var keys []ssh.PublicKey
	_ = ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		if upstream := svc.UpstreamBranch(); upstream != "" {
			keys, _ = svc.FleetKeysOf(upstream, m.AgentID())
		}
	})
	return keys
}
