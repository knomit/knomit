package repos

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
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
}

func (e *FleetError) Error() string { return e.Code + ": " + e.Msg }

func fleetErr(status int, code, msg string) *FleetError {
	return &FleetError{Status: status, Code: code, Msg: msg}
}

// FleetStatus is what GET /api/v1/fleet reports.
type FleetStatus struct {
	State        string     `json:"state"` // standalone | registering | registered | unregistering
	FleetRepo    string     `json:"fleet_repo,omitempty"`
	FleetURL     string     `json:"fleet_url,omitempty"`
	AgentID      string     `json:"agent_id"`
	RecordState  string     `json:"record_state,omitempty"` // pending | active | left | revoked, read at the fleet's main
	Since        *time.Time `json:"since,omitempty"`
	RegisteredAt *time.Time `json:"registered_at,omitempty"`
	LastAttempt  *time.Time `json:"last_attempt,omitempty"`
	LastError    string     `json:"last_error,omitempty"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// fleetRepo is the ONE mounted repository whose ontology is the fleet preset
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
// and its record's state at the fleet's main ("pending" until a human merges
// the registration).
func (m *Manager) FleetStatus(ctx context.Context) (FleetStatus, error) {
	row, err := m.fleetDB()
	if err != nil {
		return FleetStatus{}, err
	}
	st := FleetStatus{
		State: row.State, AgentID: row.AgentID, Since: timePtr(row.Since),
		RegisteredAt: timePtr(row.RegisteredAt), LastAttempt: timePtr(row.LastAttempt), LastError: row.LastError,
	}
	ri := m.fleetRepo()
	if ri == nil {
		return st, nil
	}
	st.FleetRepo = ri.Name()
	if o, oerr := m.originOf(ri); oerr == nil && o != nil {
		st.FleetURL = o.URL
	}
	st.RecordState = "pending"
	members, lerr := m.fleetMembersAtMain(ri)
	if lerr == nil {
		for _, mem := range members {
			if strings.EqualFold(mem.Agent, row.AgentID) {
				st.RecordState = mem.State
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

func (m *Manager) fleetMembersAtMain(ri *RepoInstance) ([]store.FleetMember, error) {
	upstream := "main"
	if o, err := m.originOf(ri); err == nil && o != nil && o.Branch != "" {
		upstream = o.Branch
	}
	var out []store.FleetMember
	var lerr error
	if err := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			lerr = errors.New("fleet repository is not open")
			return
		}
		out, lerr = svc.FleetMembersAt(upstream)
	}); err != nil {
		return nil, err
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

// RegisterFleet joins the fleet at url: mount it (clone, with an agent
// branch), refuse unless its ontology is the fleet preset, write this
// instance's member record on the agent branch, push. A human accepts by
// merging the agent branch into the fleet's main. The same URL again refreshes
// the record (for example after a key rotation).
func (m *Manager) RegisterFleet(ctx context.Context, url, authMethod, authToken string) (FleetStatus, error) {
	row, err := m.fleetDB()
	if err != nil {
		return FleetStatus{}, err
	}
	switch row.State {
	case FleetRegistering:
		return FleetStatus{}, fleetErr(http.StatusConflict, "registration_pending", "a registration is still being pushed; wait for it or check last_error")
	case FleetUnregistering:
		return FleetStatus{}, fleetErr(http.StatusConflict, "unregistration_pending", "an unregistration is still being pushed; it must finish (or the fleet repository be archived) before registering again")
	}
	db := m.ControlDB()
	ri := m.fleetRepo()
	if ri != nil {
		o, oerr := m.originOf(ri)
		if oerr != nil || o == nil || !sameFleetURL(o.URL, url) {
			return FleetStatus{}, fleetErr(http.StatusConflict, "already_registered", "this instance is registered with another fleet; unregister first")
		}
	} else {
		name := m.freeFleetName(url)
		created, cerr := m.Create(ctx, CreateSpec{Name: name, Mode: "clone", Origin: &OriginSpec{URL: url, AuthMethod: authMethod, AuthToken: authToken}}, func(Event) {})
		if cerr != nil {
			_ = recordFleetAttempt(db, cerr, time.Now())
			return FleetStatus{}, fleetErr(http.StatusBadGateway, "clone_failed", cerr.Error())
		}
		if !fact.IsFleetOntology(created.Ontology()) {
			msg := "the repository's ontology is not the fleet preset"
			if derr := m.deleteRepo(created.Name()); derr != nil {
				log.Warn().Err(derr).Str("repo", created.Name()).Msg("fleet: removing a non-fleet mount failed")
				msg += fmt.Sprintf("; removing the mount %q failed (%v): archive it by hand", created.Name(), derr)
				_ = recordFleetAttempt(db, errors.New(msg), time.Now())
			}
			return FleetStatus{}, fleetErr(http.StatusUnprocessableEntity, "not_a_fleet", msg)
		}
		ri = created
	}
	if err := m.writeOwnRecord(ctx, ri, fact.MemberActive); err != nil {
		return FleetStatus{}, err
	}
	if err := setFleetState(db, FleetRegistering, time.Now()); err != nil {
		return FleetStatus{}, err
	}
	m.pushFleet(ctx, ri)
	return m.FleetStatus(ctx)
}

// UnregisterFleet leaves the fleet: commit this instance's record with state
// left on the agent branch, enter unregistering, push. Only a SUCCESSFUL push
// unmounts the fleet repository and returns to standalone; a failed push is
// retried on the sync loop until it succeeds or the user archives the fleet
// repository by hand. A departure is never silent.
func (m *Manager) UnregisterFleet(ctx context.Context) (FleetStatus, error) {
	row, err := m.fleetDB()
	if err != nil {
		return FleetStatus{}, err
	}
	ri := m.fleetRepo()
	switch {
	case row.State == FleetUnregistering:
		return FleetStatus{}, fleetErr(http.StatusConflict, "unregistration_pending", "an unregistration is already being pushed")
	case ri == nil || row.State == FleetStandalone:
		return FleetStatus{}, fleetErr(http.StatusConflict, "not_registered", "this instance is standalone")
	}
	if err := m.writeOwnRecord(ctx, ri, fact.MemberLeft); err != nil {
		return FleetStatus{}, err
	}
	if err := setFleetState(m.ControlDB(), FleetUnregistering, time.Now()); err != nil {
		return FleetStatus{}, err
	}
	m.pushFleet(ctx, ri)
	return m.FleetStatus(ctx)
}

// pushFleet pushes the fleet repository's agent branch now and applies the
// outcome. The sync loop calls fleetPushed with its own pushes, which is the
// retry.
func (m *Manager) pushFleet(ctx context.Context, ri *RepoInstance) {
	var perr error
	werr := ri.WithRead(func(svc *store.Service) {
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
	})
	if werr != nil && perr == nil {
		perr = werr
	}
	m.fleetPushed(ri.Name(), perr)
}

// fleetPushed applies one push outcome of the fleet repository to the state
// machine: a failure records last_error and changes nothing else; a success
// clears it and completes registering (-> registered) or unregistering
// (-> unmount, standalone). Pushes of any other repository are ignored.
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
	switch row.State {
	case FleetRegistering:
		if err := setFleetState(db, FleetRegistered, now); err != nil {
			log.Warn().Err(err).Msg("fleet: entering registered failed")
		}
	case FleetUnregistering:
		if err := m.deleteRepo(repo); err != nil {
			_ = recordFleetAttempt(db, fmt.Errorf("unmount after the departure was pushed: %w", err), now)
			return
		}
		if err := setFleetState(db, FleetStandalone, now); err != nil {
			log.Warn().Err(err).Msg("fleet: entering standalone failed")
		}
	}
}

// FleetRetry pushes the fleet repository once, as a sync tick would. Used by
// the sync loop's absence (tests, a disabled background sync) and by callers
// that want an immediate retry.
func (m *Manager) FleetRetry(ctx context.Context) {
	if ri := m.fleetRepo(); ri != nil {
		m.pushFleet(ctx, ri)
	}
}

// guardFleetRemoval refuses a bare archive or delete of the fleet repository
// while registered (the departure must be pushed first, through unregister).
// While unregistering, archiving by hand is the user's escape hatch: it ends
// the retries and returns to standalone.
func (m *Manager) guardFleetRemoval(name string) error {
	if !m.IsFleetRepo(name) {
		return nil
	}
	db := m.ControlDB()
	if db == nil {
		return nil
	}
	row, err := readFleetRow(db)
	if err != nil {
		return nil
	}
	switch row.State {
	case FleetUnregistering, FleetStandalone:
		return nil
	}
	return fleetErr(http.StatusConflict, "use_unregister", "this is the fleet repository: unregister (DELETE /api/v1/fleet, knomit fleet unregister) so the departure is pushed")
}

// fleetRemoved is called after the fleet repository was archived or deleted:
// whatever the state was, the instance is standalone now.
func (m *Manager) fleetRemoved(wasFleet bool) {
	if !wasFleet {
		return
	}
	if db := m.ControlDB(); db != nil {
		if err := setFleetState(db, FleetStandalone, time.Now()); err != nil {
			log.Warn().Err(err).Msg("fleet: entering standalone after removal failed")
		}
	}
}

// writeOwnRecord writes this instance's member record on the fleet
// repository's agent branch: the persisted agent id, the CURRENT key, state,
// host and branch. The path is stable per agent, so the record is one fact
// whose versions are the agent's history.
func (m *Manager) writeOwnRecord(ctx context.Context, ri *RepoInstance, state string) error {
	if m.deps.Signer == nil {
		return store.ErrNoSigner
	}
	branch := ri.AgentBranch()
	id := store.AgentIDOf(m.deps.AgentBranch)
	host, _ := os.Hostname()
	body := fact.RenderMember(fact.Member{Agent: id, State: state, Key: m.deps.Signer.PublicKey(), Host: host, Branch: m.deps.AgentBranch})
	f := fact.Fact{Title: "member " + id, Body: body, Kind: fact.Pragmatic, Type: fact.Policy, Confidence: 1, Sources: 1, Entities: []string{id}}
	content, err := fact.SerializeFact(f)
	if err != nil {
		return err
	}
	p := memberRecordPath(ri.OntologyRoot(), id)
	var werr error
	if err := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			werr = errors.New("fleet repository is not open")
			return
		}
		_, werr = svc.Facts().WriteFact(ctx, branch, p, content, "fleet: "+id+" is "+state, "update")
	}); err != nil {
		return err
	}
	return werr
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

func sameFleetURL(a, b string) bool {
	norm := func(s string) string { return strings.TrimSuffix(strings.TrimRight(strings.TrimSpace(s), "/"), ".git") }
	return norm(a) == norm(b)
}

// memberRecordPath is the stable path of an agent's member record: one fact
// per agent, so its versions are the agent's history. The file name keeps the
// uuid8 shape, derived from the id instead of random.
func memberRecordPath(root, agentID string) string {
	sum := sha256.Sum256([]byte(agentID))
	return path.Join(root, fact.MembersTopic, strings.ToLower(agentID), hex.EncodeToString(sum[:4])+".md")
}

// ownFleetKeys is every key this instance's member record has held in its
// fleet (the record's versions), for E4. Nil when standalone.
func (m *Manager) ownFleetKeys() []ssh.PublicKey {
	ri := m.fleetRepo()
	if ri == nil {
		return nil
	}
	upstream := "main"
	if o, err := m.originOf(ri); err == nil && o != nil && o.Branch != "" {
		upstream = o.Branch
	}
	var keys []ssh.PublicKey
	_ = ri.WithRead(func(svc *store.Service) {
		if svc != nil {
			keys, _ = svc.FleetKeysOf(upstream, store.AgentIDOf(m.deps.AgentBranch))
		}
	})
	return keys
}
