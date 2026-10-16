package repos

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// The host record (artifacts/fleet/host, fact.FleetHostPath) is where the
// fleet is hosted, on the fleet repository's main: the fleet repository
// URL(s) members use. The fleet's master writes it when it founds the fleet
// (CreateFleet) and when it becomes the host (DetachRepoOrigin on the master);
// nothing else writes it, and the commit judge refuses a member commit that
// changes it (the host-record rule). A member reads it at main to tell the
// URL the fleet is hosted at now from the one its copy syncs from (GET
// /fleet: fleet_urls and host_record). The document is fact.RenderFleetHost's.

// maxFleetHostSize is what a reader reads of the record: the fleet
// ontology's max_size for it.
const maxFleetHostSize = 4 << 10

// hubFleetURLs is the fleet repository URL members use at each of this
// instance's https external addresses, when this instance is the hub:
// https://<address>/git/<repo>. An address that is not https:// is left
// out: members reach the hub's mTLS listener over https only (the enroll's
// address_not_https).
func (m *Manager) hubFleetURLs(ri *RepoInstance) []string {
	urls := []string{}
	for _, a := range m.deps.Cfg.ExternalAddresses {
		if strings.HasPrefix(strings.ToLower(a), "https://") {
			urls = append(urls, strings.TrimSuffix(a, "/")+"/git/"+ri.Name())
		}
	}
	return urls
}

// writeFleetHost commits the host record holding urls on ri's agent branch,
// unless the branch already holds exactly that record. The caller is the
// master, under fleetOpMu, and publishes main.
func (m *Manager) writeFleetHost(ctx context.Context, ri *RepoInstance, urls []string) error {
	want := fact.RenderFleetHost(urls)
	cur, ok, err := m.fleetHostAt(ri, ri.AgentBranch())
	if err != nil {
		return err
	}
	if ok && cur == want {
		return nil
	}
	return m.writeFleetArtifacts(ctx, ri, map[string]string{fact.FleetHostPath: want},
		"fleet: hosted at "+strings.Join(urls, ", "))
}

// fleetHostAt is the host record at branch of ri, as written; ok is false
// when there is none.
func (m *Manager) fleetHostAt(ri *RepoInstance, branch string) (string, bool, error) {
	var raw []byte
	var ok bool
	var rerr error
	if err := ri.WithRead(func(svc *store.Service) {
		if svc == nil {
			rerr = errors.New("fleet repository is not open")
			return
		}
		raw, ok, rerr = svc.FileAt(branch, fact.FleetHostPath, maxFleetHostSize)
	}); err != nil {
		return "", false, err
	}
	return string(raw), ok, rerr
}

// fleetHostAtMain is the URLs the host record at the fleet repository's main
// holds: nil when main holds none, or one that cannot be read (logged by the
// caller's error).
func (m *Manager) fleetHostAtMain(ri *RepoInstance) ([]string, error) {
	var upstream string
	if err := ri.WithRead(func(svc *store.Service) {
		if svc != nil {
			upstream = svc.UpstreamBranch()
		}
	}); err != nil {
		return nil, err
	}
	if upstream == "" {
		return nil, fmt.Errorf("fleet repository: %w", store.ErrNoConsensusBranch)
	}
	raw, ok, err := m.fleetHostAt(ri, upstream)
	if err != nil || !ok {
		return nil, err
	}
	return fact.ParseFleetHost([]byte(raw))
}
