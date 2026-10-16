package repos

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/pki"
	"knomit/internal/store"
)

// The kinds of a FleetEvent. Each has a source of record; nothing here
// keeps a log of its own:
//   - created, joined, admitted, left, host-changed: main's first-parent
//     history (store.FleetMainLog): the commit that adds the fleet root
//     certificate; a member record that becomes active, or turns left; a
//     merge of a member's tip into main; a change of the host record;
//   - enrolled: the master's issuance log (<home>/fleet/issued.jsonl), which
//     moves with the root to a new master; a member has none;
//   - revoked: the CRL on main, with the revoked leaf's agent from
//     issued/<serial>.pem and the reason from revocations/<serial>.txt;
//   - refused: the master's judge, the last refusalRing refusals it recorded
//     (fleetPeers.ring, in memory: a restart starts it empty): the hub's
//     receive-pack (FleetPushJudged), the forge fetch judge and the merger's
//     re-judge; the agent is the refused tip's author.
//
// The local acts that leave no record (a root key imported, a stand-down, a
// decommission) are not events: there is nothing to read them from.
const (
	FleetEventCreated     = "created"
	FleetEventEnrolled    = "enrolled"
	FleetEventJoined      = "joined"
	FleetEventAdmitted    = "admitted"
	FleetEventRefused     = "refused"
	FleetEventRevoked     = "revoked"
	FleetEventLeft        = "left"
	FleetEventHostChanged = "host-changed"
)

// MaxFleetActivity is the most events GET /fleet/activity returns, and its
// default.
const MaxFleetActivity = 200

// FleetEvent is one row of GET /fleet/activity.
type FleetEvent struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Agent  string    `json:"agent"`
	Serial string    `json:"serial,omitempty"`
	Branch string    `json:"branch,omitempty"`
	Tip    string    `json:"tip,omitempty"`
	Detail string    `json:"detail"`
}

// FleetActivity is the newest limit events (1..MaxFleetActivity) of the
// fleet, newest first, from the sources the kinds above name. The walk of
// main is bounded by maxFleetWalk first-parent commits and by limit entries.
func (m *Manager) FleetActivity(ctx context.Context, limit int) ([]FleetEvent, error) {
	if limit <= 0 || limit > MaxFleetActivity {
		limit = MaxFleetActivity
	}
	ri := m.fleetRepo()
	if ri == nil {
		return nil, fleetErr(http.StatusConflict, "not_registered", "this instance is standalone: no fleet repository is mounted")
	}
	f, err := m.fleetAtMain(ri)
	if err != nil {
		return nil, err
	}
	branchOf := map[string]string{}
	agentOf := map[string]string{}
	for _, mem := range f.Members {
		branchOf[mem.Agent] = mem.Branch
		if mem.Branch != "" {
			agentOf[mem.Branch] = mem.Agent
		}
	}
	ont := ri.Ontology()
	layout, _ := ont.PKILayout()

	var events []FleetEvent
	var commits []store.FleetMainCommit
	var lerr error
	if err := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			lerr = errors.New("fleet repository is not open")
			return
		}
		commits, lerr = svc.FleetMainLog(svc.UpstreamBranch(), layout.Root, maxFleetWalk, limit)
	}); err != nil {
		return nil, err
	}
	if lerr != nil {
		return nil, lerr
	}
	for _, c := range commits {
		events = append(events, mainEvents(c, branchOf)...)
	}
	events = append(events, m.enrolledEvents()...)
	events = append(events, m.revokedEvents(ri, f)...)
	for _, r := range ri.peers.RefusalLog() {
		// The refused tip's author; a tip whose author is not an agent
		// address falls back to the record that names the branch.
		agent := r.Agent
		if agent == "" {
			agent = agentOf[r.Branch]
		}
		events = append(events, FleetEvent{At: r.At, Kind: FleetEventRefused, Agent: agent,
			Branch: r.Branch, Tip: r.Tip, Detail: r.Reason})
	}
	sortFleetEvents(events)
	if len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

// fleetKindOrder ranks the kinds for events at the same instant, newest
// first: the later step of a member's lifecycle first (a revocation in the
// second of the join it ends reads above it), the creation last.
var fleetKindOrder = map[string]int{
	FleetEventLeft: 0, FleetEventRevoked: 1, FleetEventRefused: 2, FleetEventHostChanged: 3,
	FleetEventAdmitted: 4, FleetEventJoined: 5, FleetEventEnrolled: 6, FleetEventCreated: 7,
}

// sortFleetEvents orders events newest first by their full timestamp (a
// commit's is whole seconds; the issuance log, the CRL and the ring keep what
// they have, sub-second included), then, at the same instant, by a stable
// key: the kind (fleetKindOrder), the agent, the serial, the branch, the tip,
// the detail. Never by the order the sources were read in (PR #527 N4).
func sortFleetEvents(events []FleetEvent) {
	sort.Slice(events, func(i, j int) bool {
		a, b := events[i], events[j]
		if !a.At.Equal(b.At) {
			return a.At.After(b.At)
		}
		if ka, kb := fleetKindOrder[a.Kind], fleetKindOrder[b.Kind]; ka != kb {
			return ka < kb
		}
		for _, c := range [][2]string{{a.Agent, b.Agent}, {a.Serial, b.Serial}, {a.Branch, b.Branch}, {a.Tip, b.Tip}, {a.Detail, b.Detail}} {
			if c[0] != c[1] {
				return c[0] < c[1]
			}
		}
		return false
	})
}

// mainEvents is what one first-parent commit of main says, in the order a
// reader reads it newest first: the host change, the merge, then each
// record's turn (left before joined), then the creation.
func mainEvents(c store.FleetMainCommit, branchOf map[string]string) []FleetEvent {
	var out []FleetEvent
	if c.HostChanged {
		d := "the host record is removed"
		if len(c.Host) > 0 {
			d = "hosted at " + strings.Join(c.Host, ", ")
		}
		out = append(out, FleetEvent{At: c.At, Kind: FleetEventHostChanged, Agent: c.Author, Detail: d})
	}
	if c.MergedTip != "" {
		branch := branchOf[c.MergedBy]
		what := branch
		if what == "" {
			what = "a branch"
		}
		out = append(out, FleetEvent{At: c.At, Kind: FleetEventAdmitted, Agent: c.MergedBy, Branch: branch, Tip: c.MergedTip,
			Detail: what + " @ " + shortHash(c.MergedTip) + " merged into main"})
	}
	for _, rc := range c.Records {
		wasActive := rc.Before != nil && rc.Before.State == fact.MemberActive
		switch {
		case rc.After != nil && rc.After.State == fact.MemberLeft && wasActive:
			out = append(out, FleetEvent{At: c.At, Kind: FleetEventLeft, Agent: rc.Agent, Branch: rc.After.Branch,
				Detail: "its record on main says left"})
		case rc.After != nil && rc.After.State == fact.MemberActive && !wasActive:
			out = append(out, FleetEvent{At: c.At, Kind: FleetEventJoined, Agent: rc.Agent, Branch: rc.After.Branch,
				Detail: "its active record is on main"})
		}
	}
	if c.RootAdded {
		out = append(out, FleetEvent{At: c.At, Kind: FleetEventCreated, Agent: c.Author, Detail: "the fleet root certificate is on main"})
	}
	return out
}

// enrolledEvents is the master's issuance log, one event per leaf issued; an
// instance with no log (a member) has none. A log that cannot be read is
// logged and shows nothing.
func (m *Manager) enrolledEvents() []FleetEvent {
	issued, err := pki.LoadIssued(m.fleetRootDir())
	if err != nil {
		log.Warn().Err(err).Msg("fleet activity: the issuance log cannot be read")
		return nil
	}
	out := make([]FleetEvent, 0, len(issued))
	for _, is := range issued {
		out = append(out, FleetEvent{At: is.IssuedAt.UTC(), Kind: FleetEventEnrolled, Agent: pki.AgentIDOfEmail(is.Email),
			Serial: is.Serial, Detail: "certificate serial " + is.Serial + " issued"})
	}
	return out
}

// revokedEvents is one event per serial the CRL on main (f, read at main)
// lists: the agent from the issued leaf on main (issued/<serial>.pem, when
// the root's signature on it holds), the reason from the root-signed
// statement (revocations/<serial>.txt) when it verifies.
func (m *Manager) revokedEvents(ri *RepoInstance, f store.Fleet) []FleetEvent {
	if f.PKI == nil || f.PKI.CRL == nil {
		return nil
	}
	ont := ri.Ontology()
	issued, ierr := artifactPattern(ont, fact.PKIIssuance)
	statements, serr := artifactPattern(ont, fact.PKIRevocation)
	var out []FleetEvent
	_ = ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			return
		}
		upstream := svc.UpstreamBranch()
		for _, e := range f.PKI.CRL.RevokedCertificateEntries {
			serial := e.SerialNumber.Text(16)
			ev := FleetEvent{At: e.RevocationTime.UTC(), Kind: FleetEventRevoked, Serial: serial,
				Detail: "certificate serial " + serial + " is on the CRL"}
			if ierr == nil && f.PKI.Root != nil {
				if raw, ok, err := svc.FileAt(upstream, strings.Replace(issued, "*", serial, 1), 64<<10); err == nil && ok {
					if id, err := pki.CheckIssuance(raw, f.PKI.Root); err == nil {
						ev.Agent = id.AgentID
					}
				}
			}
			if serr == nil && f.PKI.Root != nil {
				if raw, ok, err := svc.FileAt(upstream, strings.Replace(statements, "*", serial, 1), 4<<10); err == nil && ok {
					if st, err := pki.VerifyRevocation(raw, f.PKI.Root); err == nil && st.Reason != "" {
						ev.Detail += ": " + st.Reason
					}
				}
			}
			out = append(out, ev)
		}
	})
	return out
}
