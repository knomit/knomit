package repos

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/platform/version"
	"knomit/internal/store"
)

// gitPathSuffix is what a peer appends to one of this instance's addresses to
// reach its git endpoint (internal/web mounts the git handler there).
const gitPathSuffix = "/git"

// hostPlatform is where the os and arch capabilities come from: the Go
// runtime of this binary. A variable only so a test can prove the record
// carries whatever it returns rather than a constant.
var hostPlatform = func() (goos, goarch string) { return runtime.GOOS, runtime.GOARCH }

// NoAddressesNotice is shown (REST, Fleet tab, CLI) while external_addresses
// is empty. Registration still succeeds: peers just cannot reach this
// instance from its record.
const NoAddressesNotice = "no external addresses configured; set `external_addresses` in knomit.toml"

// advertised is this instance's member record as the CURRENT config and
// binary describe it: the advertised fields only (agent is set, state is
// not). Nothing here is derived from the listen address or probed: the
// addresses are the operator's list verbatim, the rest is detected on this
// host.
func (m *Manager) advertised() fact.Member {
	cfg := m.deps.Cfg
	id := store.AgentIDOf(m.deps.AgentBranch)
	host, _ := os.Hostname()
	goos, goarch := hostPlatform()
	mem := fact.Member{
		Agent:     id,
		Host:      host,
		Branch:    m.deps.AgentBranch,
		Addresses: append([]string{}, cfg.ExternalAddresses...),
		Capabilities: map[string]string{
			"os":        goos,
			"arch":      goarch,
			"version":   version.String(),
			"read_only": strconv.FormatBool(cfg.ReadOnly),
		},
	}
	if m.deps.Signer != nil {
		mem.Key = m.deps.Signer.PublicKey()
	}
	// The same condition that mounts /git (app: git.serve builds the handler;
	// web: it is mounted only when not read-only). A suffix that 404s would
	// be a guess.
	if cfg.Git.Serve && !cfg.ReadOnly {
		mem.Git = gitPathSuffix
	}
	return mem
}

// ownRecordAt reads this instance's member record on branch of the fleet
// repository, with its blob. found is false when the branch has none.
func (m *Manager) ownRecordAt(ri *RepoInstance, branch string) (rec store.FleetMember, found bool, err error) {
	id := store.AgentIDOf(m.deps.AgentBranch)
	werr := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			err = errors.New("fleet repository is not open")
			return
		}
		var ms []store.FleetMember
		ms, err = svc.FleetMembersAt(branch)
		for _, mem := range ms {
			if strings.EqualFold(mem.Agent, id) {
				rec, found = mem, true
				return
			}
		}
	})
	if werr != nil && err == nil {
		err = werr
	}
	return rec, found, err
}

// writeRecord commits rec as this instance's member record on the fleet
// repository's agent branch. With expectBlob it is a compare-and-swap
// (knomit_update's if_commit semantics): nothing is written if the record
// changed since it was read.
func (m *Manager) writeRecord(ctx context.Context, ri *RepoInstance, rec fact.Member, message, expectBlob string) error {
	if m.deps.Signer == nil {
		return store.ErrNoSigner
	}
	f := fact.Fact{Title: "member " + rec.Agent, Body: fact.RenderMember(rec), Kind: fact.Pragmatic, Type: fact.Policy, Confidence: 1, Sources: 1, Entities: []string{rec.Agent}}
	content, err := fact.SerializeFact(f)
	if err != nil {
		return err
	}
	p := memberRecordPath(ri.OntologyRoot(), rec.Agent)
	var werr error
	if err := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			werr = errors.New("fleet repository is not open")
			return
		}
		if expectBlob != "" {
			_, werr = svc.Facts().WriteFactIfUnchanged(ctx, ri.AgentBranch(), p, content, message, "update", expectBlob)
			return
		}
		_, werr = svc.Facts().WriteFact(ctx, ri.AgentBranch(), p, content, message, "update")
	}); err != nil {
		return err
	}
	return werr
}

// refreshOwnRecord compares the advertised fields the current config and
// binary give with the own record on the fleet repository's agent branch head
// and, when they differ, writes ONE new version of the record. state is kept
// as the record has it; notes are kept verbatim. It returns the names of the
// fields it changed (nil: the record was current, nothing written).
//
// It never touches another member's record, and it is never called from a
// knowledge base's write path: only at boot (fleetBootReconcile) and on a
// re-registration with the same fleet (RegisterFleet).
func (m *Manager) refreshOwnRecord(ctx context.Context, ri *RepoInstance) ([]string, error) {
	cur, found, err := m.ownRecordAt(ri, ri.AgentBranch())
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("fleet: this instance's member record is missing on %s; re-register to write it", ri.AgentBranch())
	}
	want := m.advertised()
	changed := cur.Member.AdvertisedDiff(want)
	if len(changed) == 0 {
		return nil, nil
	}
	want.Agent, want.State, want.Notes = cur.Agent, cur.State, cur.Notes
	msg := "fleet: " + want.Agent + " record updated: " + strings.Join(changed, ", ")
	if err := m.writeRecord(ctx, ri, want, msg, cur.Blob); err != nil {
		return nil, err
	}
	return changed, nil
}

// fleetBootReconcile runs once per boot, after Start opened the repositories:
// when this instance is registered, bring its own member record up to date
// with the current config and binary (one commit, or none), and let the
// ordinary sync loop push it. Standalone, registering, unregistering and
// read-only instances write nothing. A failure is a WARN and is retried at
// the next boot or re-registration; it never affects startup.
func (m *Manager) fleetBootReconcile(ctx context.Context) {
	row, err := m.fleetDB()
	if err != nil {
		log.Warn().Err(err).Msg("fleet: boot reconcile skipped: cannot read the fleet state")
		return
	}
	if row.State != FleetRegistered {
		return
	}
	if m.deps.Cfg.ReadOnly {
		log.Info().Msg("fleet record not reconciled: this instance is read-only and never pushes")
		return
	}
	ri := m.fleetRepo()
	if ri == nil {
		log.Warn().Msg("fleet: boot reconcile skipped: registered, but the fleet repository is not open; retried at the next boot")
		return
	}
	changed, err := m.refreshOwnRecord(ctx, ri)
	if err != nil {
		log.Warn().Err(err).Str("fleet", ri.Name()).Msg("fleet: boot reconcile failed; retried at the next boot")
		return
	}
	if len(changed) == 0 {
		log.Info().Str("fleet", ri.Name()).Msg("fleet record current")
		return
	}
	log.Info().Str("fleet", ri.Name()).Msg("fleet record updated: " + strings.Join(changed, ", "))
}

// startFleetBootReconcile launches fleetBootReconcile off the startup path.
// Close waits for it before releasing the handles it uses.
func (m *Manager) startFleetBootReconcile() {
	m.fleetBootWg.Add(1)
	go func() {
		defer m.fleetBootWg.Done()
		m.fleetBootReconcile(m.ctx)
	}()
}
