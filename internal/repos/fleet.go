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
	"knomit/internal/pki"
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
	State     string `json:"state"` // standalone | registering | registered
	FleetRepo string `json:"fleet_repo,omitempty"`
	AgentID   string `json:"agent_id"`
	// PublicKey is this instance's ssh-ed25519 key line, the key a master
	// enrolls (with the agent id) and every certificate of this instance
	// names.
	PublicKey    string     `json:"public_key"`
	RecordState  string     `json:"record_state,omitempty"` // pending | active | left | revoked, read at the fleet's main
	Since        *time.Time `json:"since,omitempty"`
	RegisteredAt *time.Time `json:"registered_at,omitempty"`
	LastAttempt  *time.Time `json:"last_attempt,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
	// LastErrorRefused: LastError is the fleet refusing this instance's
	// certificate at the TLS handshake (pki.ErrRefusedByPeer, as the recorded
	// error text carries it): revoked, or the fleet moved to another root.
	// A retry does not help, and the member cannot read the CRL to tell
	// which. Read from the recorded error only; nothing new is kept.
	LastErrorRefused bool `json:"last_error_refused,omitempty"`
	// LastErrorCommitRefused: LastError is the fleet's host refusing a
	// commit of this copy's push with its judge (store.CommitRefusedByJudge;
	// the text names the commit, the rule and the reason). Every sync sends
	// the same commit, so a retry does not help. Under the host-record and
	// artifact rules, which read the commit against its own parents, no later
	// main admits it either: the way out is to leave, have the master revoke
	// this instance's certificate (until Leave revokes it by itself) and
	// enroll it again, then join again.
	// It is the dead-master fallback's residue (an old master's master write
	// the new host's main lacks). It is also set for a commit that adds or
	// changes a fact failing the ontology's validations
	// (store.CommitRefusedByValidation). It is NOT set when the judge of the
	// master's OWN commits held its main back (store.OwnCommitRefusedText):
	// that is no host refusing this copy's push, and the leave/revoke advice
	// the UI shows for one does not apply; last_error says it in full. Read
	// from the recorded error only, as LastErrorRefused is.
	LastErrorCommitRefused bool `json:"last_error_commit_refused,omitempty"`
	// LastErrorMainHeld: LastError is the judge of THIS instance's own
	// commits holding the fleet's main back (store.OwnCommitRefusedText):
	// main stays where it is, every later move is refused too while the
	// commit is on the agent branch, and the way out is POST
	// /fleet/drop-commit with LastErrorMainHeldCommit. Read from the recorded
	// error only.
	LastErrorMainHeld bool `json:"last_error_main_held,omitempty"`
	// LastErrorMainHeldCommit is the refused commit's hash, from the same
	// text ("" when LastErrorMainHeld is false).
	LastErrorMainHeldCommit string `json:"last_error_main_held_commit,omitempty"`
	// LastErrorStoodDown: LastError is the fleet's old host refusing this
	// copy's push because its master stood down (FleetPushRefusalStoodDown):
	// the push goes to the new host once this copy points at it.
	LastErrorStoodDown bool `json:"last_error_stood_down,omitempty"`
	// SyncIntervalSeconds is how often this copy's origin round runs (the
	// origin record's interval, push_interval when shorter, 300 s when
	// unset): a member sees what the master's round merged into main only
	// after its own next round. Absent on a copy with no origin (the hub).
	SyncIntervalSeconds int `json:"sync_interval_seconds,omitempty"`

	// F10. Host is what this instance offers to host a fleet: its mTLS
	// listener and its external addresses. Record is this instance's own
	// record on its agent branch head (nil when there is none);
	// RecordCurrent says whether it matches what the current config and
	// binary advertise; RecordPendingUpdate says the fleet's main still holds
	// an older version of it. Notice is set while no external address is
	// configured.
	Host                FleetHostOffer `json:"host"`
	Record              *MemberFields  `json:"record,omitempty"`
	RecordCurrent       *bool          `json:"record_current,omitempty"`
	RecordPendingUpdate bool           `json:"record_pending_update"`
	Notice              string         `json:"notice,omitempty"`

	// Master says this instance is the fleet's master (isFleetMaster: it
	// holds the root, and the master marker on the fleet's main names it).
	// RootHeld says it holds the root (the key file is here and its root is
	// the one on main), master or not: a root holder that main does not name
	// is a member, a candidate while main names no master.
	// Hosting is where the fleet repository's copy here syncs: "none" (no
	// origin: this instance is the hub) or the origin's URL; empty when no
	// fleet repository is mounted. RootDir is <home>/fleet, where the root
	// key lives on the master (and where a master switch copies it to).
	Master   bool `json:"master"`
	RootHeld bool `json:"root_held,omitempty"`
	// MasterPaused, on the master, is why it does no master work now (no
	// merges, enroll, revoke, decommission, host-record write):
	// FleetPausedNotHosting; empty otherwise. ActingMaster is the agent id
	// the master marker on main names (fact.FleetMasterPath); empty when main
	// has none: a fleet with no acting master (its master stood down), or one
	// whose master has not written it yet.
	MasterPaused string `json:"master_paused,omitempty"`
	ActingMaster string `json:"acting_master,omitempty"`
	// RevokesLeft says THIS instance's rounds revoke the members that leave
	// (the leave duty): it acts as the fleet's master (Master, and not
	// MasterPaused) and the fleet's main holds its PKI with a CRL.
	RevokesLeft bool   `json:"revokes_left,omitempty"`
	Hosting     string `json:"hosting,omitempty"`
	RootDir     string `json:"root_dir"`

	// FleetURLs is the fleet repository URL(s) members use: on the hub, one
	// https://<address>/git/<repo> per https external address; on any other
	// copy, what the host record on the fleet's main says, else the origin
	// URL. Empty with no fleet repository. HostRecord is that host record as
	// main holds it (omitted when main holds none, or one that cannot be
	// read). FollowsHost is the server's comparison of the two: true on the
	// hub (it is the host), and on a copy with an origin whether that origin
	// is one of the host record's URLs (sameFleetURL: scheme and host in any
	// case, a default port, a trailing slash and credentials do not count);
	// nil when there is nothing to compare (no fleet repository, or no host
	// record on main). false is "this copy follows an old host". Every URL
	// here, Hosting's included, is shown without its userinfo.
	FleetURLs   []string `json:"fleet_urls"`
	HostRecord  []string `json:"host_record,omitempty"`
	FollowsHost *bool    `json:"follows_host,omitempty"`
	// InterruptedMove is set on the MASTER whose copy follows a forge that the
	// host record on main does not name (follows_host false): a move onto the
	// forge stopped after the attach and before the host record (#529 S4).
	// Members follow main, so the repair is the master's: put the fleet back
	// where the record says. The page offers exactly these, never "Point at
	// the new host" (that would follow this instance's own hub).
	InterruptedMove *FleetInterruptedMove `json:"interrupted_move,omitempty"`
	// LastErrorFleetMoved: this copy follows an old host (follows_host false)
	// and its last sync error is that host refusing its push because the
	// host's copy has an origin now (FleetPushRefusalHasOrigin): the fleet
	// moved. Retrying does not help until this copy points at the new host.
	// Read from the recorded error only, as LastErrorRefused is.
	LastErrorFleetMoved bool `json:"last_error_fleet_moved,omitempty"`
	// RootFingerprint is the fleet root installed in [tls].dir (omitted when
	// none is).
	RootFingerprint string `json:"root_fingerprint,omitempty"`
}

// FleetInterruptedMove is GET /fleet's interrupted_move: what the host
// record names, which is where the fleet goes back to. RecordIsHere: it
// names this instance (its hub URLs), so the move started at the hub and
// Host the fleet here is the repair. MoveBackTo: it names a forge (the one
// the move started from, credential-free), and Move the fleet there is the
// repair; empty when RecordIsHere.
type FleetInterruptedMove struct {
	RecordIsHere bool   `json:"record_is_here"`
	MoveBackTo   string `json:"move_back_to,omitempty"`
}

// FleetHostOffer is what this instance offers to host a fleet: its mTLS
// listener address ([tls].addr; "" when it has none, and a fleet hosted here
// is refused listener_required; GET /fleet shows it only to a local caller)
// and whether it has one (ListenerOn), its external_addresses ([] when
// unset), and WouldServe: on the fleet's master while its copy has an origin
// (so it is not the host), the fleet repository URLs members would use if it
// hosted the fleet here (hubFleetURLs), what the Host-here confirm names; on a
// standalone instance, the URLs of a fleet created here (the New fleet
// wizard's Hosting step).
type FleetHostOffer struct {
	Listener string `json:"listener"`
	// ListenerOn: the mTLS listener is CONFIGURED ([tls].addr is set). It
	// says nothing about whether it is up: see Listening.
	ListenerOn bool `json:"listener_on"`
	// Listening: the mTLS listener is bound and serving NOW, as the process
	// that bound the listeners reports it (SetTLSListener). false when it is
	// off, when it is configured but did not start (ListenerReason), and
	// before anyone reported. Only Listening means members can reach it.
	Listening bool `json:"listening"`
	// ListenerReason is why a configured listener is not listening:
	// no_certificate (nothing installed in [tls].dir when it last started:
	// a create or join installs one, and a restart or `knomit reload` starts
	// it), addr_in_use, or "" when it is listening, off, or unknown.
	ListenerReason    string   `json:"listener_reason,omitempty"`
	ExternalAddresses []string `json:"external_addresses"`
	WouldServe        []string `json:"would_serve,omitempty"`
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
		LastErrorRefused: strings.Contains(row.LastError, pki.ErrRefusedByPeer.Error()),
		LastErrorCommitRefused: (strings.Contains(row.LastError, store.CommitRefusedByJudge) || strings.Contains(row.LastError, store.CommitRefusedByValidation)) &&
			!strings.Contains(row.LastError, store.OwnCommitRefusedText),
		LastErrorStoodDown:      strings.Contains(row.LastError, FleetPushRefusalStoodDown),
		LastErrorMainHeldCommit: mainHeldCommit(row.LastError),
		Host: FleetHostOffer{Listener: m.deps.Cfg.TLS.Addr, ListenerOn: m.deps.Cfg.TLS.Addr != "",
			ExternalAddresses: append([]string{}, m.deps.Cfg.ExternalAddresses...)},
		RootDir:   m.fleetRootDir(),
		FleetURLs: []string{},
	}
	st.LastErrorMainHeld = st.LastErrorMainHeldCommit != ""
	if f := m.tlsListener.Load(); f != nil {
		listening, reason := (*f)()
		st.Host.Listening = listening != ""
		if st.Host.ListenerOn && !st.Host.Listening {
			st.Host.ListenerReason = reason
		}
	}
	if m.deps.Signer != nil {
		st.PublicKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(m.deps.Signer.PublicKey())))
	}
	if len(st.Host.ExternalAddresses) == 0 {
		st.Notice = NoAddressesNotice
	}
	if root, err := pki.InstalledRoot(m.deps.Cfg.TLS.Dir); err == nil {
		st.RootFingerprint = root.Fingerprint
	}
	ri := m.fleetRepo()
	if ri == nil {
		// Standalone: where members would join a fleet created here (the
		// create's default name), for the New fleet wizard's Hosting step.
		if st.State == FleetStandalone {
			st.Host.WouldServe = credentialFreeAll(m.hubFleetURLsFor(m.freeFleetName(defaultFleetRepoName)))
		}
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
	if hosted, herr := m.fleetHostAtMain(ri); herr != nil {
		log.Warn().Err(herr).Str("fleet", ri.Name()).Msg("fleet: the host record on main cannot be read")
	} else if len(hosted) > 0 {
		st.HostRecord = credentialFreeAll(hosted)
	}
	_ = ri.WithRead(func(svc *store.Service) {
		st.Master = ri.isFleetMaster(svc)
		st.RootHeld = ri.fleetDoor && ri.IsFleet() && m.holdsFleetRoot(svc)
	})
	if st.RootHeld {
		st.MasterPaused = m.fleetMasterPause(ri)
	}
	if st.Master && st.MasterPaused == "" {
		if f, ferr := m.fleetAtMain(ri); ferr == nil && f.PKI != nil && f.PKI.CRL != nil {
			st.RevokesLeft = true
		}
	}
	if named, merr := m.fleetMasterAtMain(ri); merr == nil {
		st.ActingMaster = named
	} else {
		log.Warn().Err(merr).Str("fleet", ri.Name()).Msg("fleet: the master marker on main cannot be read")
	}
	if o, oerr := m.originOf(ri); oerr == nil {
		st.Hosting = "none"
		switch {
		case o == nil:
			st.FleetURLs = credentialFreeAll(m.hubFleetURLs(ri))
			st.FollowsHost = boolPtr(true)
		case len(st.HostRecord) > 0:
			st.Hosting, st.FleetURLs = credentialFree(o.URL), st.HostRecord
			follows := false
			for _, u := range st.HostRecord {
				follows = follows || sameFleetURL(o.URL, u)
			}
			st.FollowsHost = &follows
		default:
			st.Hosting, st.FleetURLs = credentialFree(o.URL), []string{credentialFree(o.URL)}
		}
		if o != nil && st.RootHeld {
			st.Host.WouldServe = credentialFreeAll(m.hubFleetURLs(ri))
		}
		if o != nil && st.Master && o.AuthMethod != "cert" && st.FollowsHost != nil && !*st.FollowsHost {
			st.InterruptedMove = m.interruptedMove(ri, st.HostRecord)
		}
		if o != nil {
			_ = ri.WithRead(func(svc *store.Service) {
				if r, rerr := svc.Remote().GetRemote("origin"); rerr == nil && r != nil {
					st.SyncIntervalSeconds = int(loopInterval(r) / time.Second)
				}
			})
		}
	}
	st.LastErrorFleetMoved = st.FollowsHost != nil && !*st.FollowsHost && strings.Contains(row.LastError, FleetPushRefusalHasOrigin)
	if f, lerr := m.fleetAtMain(ri); lerr == nil {
		var onMain bool
		var written store.FleetMember
		for _, mem := range f.Members {
			if mem.Agent == row.AgentID {
				onMain, written = true, mem
				break
			}
		}
		st.RecordState = recordStateOf(head, onHead, written, onMain, f.PKI.Revoked(row.AgentID))
		// The RECORD differs, not "the branch is ahead": the sync loop
		// merges main into the agent branch, so the branch is routinely
		// ahead by merge commits with nothing to accept.
		st.RecordPendingUpdate = onHead && onMain && st.RecordState != RecordPending && written.Blob != head.Blob
	} else if onHead {
		// main cannot be read: as far as this copy can tell, the record is
		// on its branch only.
		st.RecordState = RecordPending
	}
	return st, nil
}

// RecordPending is FleetStatus.RecordState for a candidate record: on this
// instance's own agent branch, and not (in that state) on the fleet's main.
const RecordPending = "pending"

// recordStateOf is the ONE rule for FleetStatus.RecordState (user ruling
// 2026-10-16: "our candidate record in our branch means pending. our record
// in main means accepted"). It reads the record on this instance's agent
// branch (branch, onBranch) and the record on the fleet's main as written
// (main, onMain), and nothing else:
//
//   - on the branch, and main holds none or holds it in another state (a
//     rejoin's active record while main still says left): "pending", the
//     candidate is not accepted yet;
//   - on main in the branch's state (or on main only): accepted, and the
//     state is main's: active, left, or revoked when main's CRL lists the
//     leaf at main's certificate path (revokedAtMain; M6: derived, never
//     written). A newer version of an accepted record that differs only in
//     its advertised fields is record_pending_update, not pending;
//   - on neither: "" (no record; the field is omitted).
func recordStateOf(branch store.FleetMember, onBranch bool, main store.FleetMember, onMain, revokedAtMain bool) string {
	switch {
	case onBranch && (!onMain || main.State != branch.State):
		return RecordPending
	case !onMain:
		return ""
	case revokedAtMain:
		return fact.MemberRevoked
	}
	return main.State
}

// interruptedMove is FleetStatus.InterruptedMove for the master whose forge
// copy does not follow the host record hosted (credential-free): the record
// names this instance's hub URLs (the move started at the hub), or a forge.
func (m *Manager) interruptedMove(ri *RepoInstance, hosted []string) *FleetInterruptedMove {
	for _, u := range hosted {
		for _, h := range m.hubFleetURLs(ri) {
			if sameFleetURL(u, h) {
				return &FleetInterruptedMove{RecordIsHere: true}
			}
		}
	}
	out := &FleetInterruptedMove{}
	if len(hosted) > 0 {
		out.MoveBackTo = hosted[0]
	}
	return out
}

// MemberEnrolled is the derived state of an agent id that has a leaf on the
// fleet's main and no member record: the master enrolled it and it has not
// joined yet (or its join is not merged). Never written to a record.
const MemberEnrolled = "enrolled"

// maxFleetWalk bounds every walk of the fleet repository's main for the
// Fleet page (FleetMembers' since, FleetActivity): that many first-parent
// commits, newest first.
const maxFleetWalk = 2000

// FleetMemberRow is one row of GET /fleet/members: a member record as
// readers see it (fleetMembersAtMain), or an enrolled id (a leaf on main, no
// record: State MemberEnrolled, or revoked when the CRL lists the leaf; Path
// the leaf's). Since is the committer time of the commit that brought the
// row's current version (the record, else the leaf) onto main; nil when that
// is older than the maxFleetWalk commits walked. RevokeDue: the record is a
// `left` record whose leaf the master's leave duty revokes (leftLeafDue),
// which it does once an instance that acts as the master runs its round.
type FleetMemberRow struct {
	store.FleetMember
	Since     *time.Time
	RevokeDue bool
}

// FleetMembers returns the rows of GET /fleet/members at the fleet
// repository's main: every member record, then every enrolled id, each with
// its since.
func (m *Manager) FleetMembers(ctx context.Context) ([]FleetMemberRow, error) {
	ri := m.fleetRepo()
	if ri == nil {
		return nil, fleetErr(http.StatusConflict, "not_registered", "this instance is standalone: no fleet repository is mounted")
	}
	f, err := m.fleetAtMain(ri)
	if err != nil {
		return nil, err
	}
	due := map[string]bool{}
	for _, mem := range f.Members {
		due[mem.Path] = leftLeafDue(f, mem) != nil
	}
	deriveRevoked(f)
	rows := make([]FleetMemberRow, 0, len(f.Members))
	recorded := map[string]bool{}
	for _, mem := range f.Members {
		rows = append(rows, FleetMemberRow{FleetMember: mem, RevokeDue: due[mem.Path]})
		recorded[mem.Agent] = true
	}
	for _, id := range f.PKI.CertIDs() {
		if recorded[id] {
			continue
		}
		state := MemberEnrolled
		if f.PKI.Revoked(id) {
			state = fact.MemberRevoked
		}
		rows = append(rows, FleetMemberRow{FleetMember: store.FleetMember{
			Member: fact.Member{Agent: id, State: state, Addresses: []string{}}, Path: f.PKI.Layout.LeafPath(id)}})
	}
	paths := make([]string, len(rows))
	for i := range rows {
		paths[i] = rows[i].Path
	}
	var since map[string]time.Time
	var serr error
	if err := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			serr = errors.New("fleet repository is not open")
			return
		}
		since, serr = svc.FleetSince(svc.UpstreamBranch(), paths, maxFleetWalk)
	}); err != nil {
		return nil, err
	}
	if serr != nil {
		return nil, serr
	}
	for i := range rows {
		if t, ok := since[rows[i].Path]; ok {
			rows[i].Since = &t
		}
	}
	return rows, nil
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
	deriveRevoked(f)
	return f.Members, err
}

// deriveRevoked sets State revoked on every record of f whose leaf the CRL of
// the same revision lists (M6).
func deriveRevoked(f store.Fleet) {
	for i := range f.Members {
		if f.PKI.Revoked(f.Members[i].Agent) {
			f.Members[i].State = fact.MemberRevoked
		}
	}
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
	return fleetErr(http.StatusConflict, "fleet_repository", "this is the fleet repository: it is never archived or deleted directly; leave the fleet (Fleet → Settings → Leave), or, on the master, decommission it (Fleet → Settings → Decommission)")
}

// writeOwnRecord writes this instance's member record on the fleet
// repository's agent branch: the persisted agent id, state, and the advertised
// fields the current config and binary give (key, host, branch, addresses,
// git, capabilities). The path is stable per agent, so the record is one fact
// whose versions are the agent's history.
func (m *Manager) writeOwnRecord(ctx context.Context, ri *RepoInstance, state string) error {
	rec := m.advertised()
	rec.State = state
	if state == fact.MemberLeft {
		// The leaf this instance leaves with: the only one the master's leave
		// duty revokes for this record (a stale left record never names a
		// later re-enroll's leaf). None installed: the record names none and
		// the master's operator revokes by hand.
		rec.LeafSerial = m.installedLeafSerial()
	}
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
