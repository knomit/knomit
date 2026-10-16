package repos

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// The host record (artifacts/fleet/host, fact.FleetHostPath) is where the
// fleet is hosted, on the fleet repository's main: the fleet repository
// URL(s) members use. The fleet's master writes it when it founds the fleet
// (CreateFleet) and when it becomes the host (HostFleetHere, POST /fleet/host),
// and the hub re-asserts it in every round of its local reconcile
// (fleetHostRound), so a failed write is repaired and a change of
// external_addresses followed; nothing else writes it, it never carries a
// URL's credentials, and the commit judge refuses a member commit that
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
	return m.hubFleetURLsFor(ri.Name())
}

// hubFleetURLsFor is hubFleetURLs for a fleet repository named repo, which
// need not exist yet: a standalone instance names where members would join
// a fleet created here (GET /fleet host.would_serve).
func (m *Manager) hubFleetURLsFor(repo string) []string {
	urls := []string{}
	for _, a := range m.deps.Cfg.ExternalAddresses {
		if strings.HasPrefix(strings.ToLower(a), "https://") {
			urls = append(urls, strings.TrimSuffix(a, "/")+"/git/"+repo)
		}
	}
	return urls
}

// writeFleetHost commits the host record holding urls on ri's agent branch,
// unless the branch already holds exactly that record. The caller is the
// master, under fleetOpMu, and publishes main. Every URL is written without
// its userinfo (credentialFree): the record is on main, replicated to every
// member, forever.
func (m *Manager) writeFleetHost(ctx context.Context, ri *RepoInstance, urls []string) error {
	if h := currentFleetHooks().hostWrite; h != nil {
		if err := h(); err != nil {
			return err
		}
	}
	clean := credentialFreeAll(urls)
	want := fact.RenderFleetHost(clean)
	cur, ok, err := m.fleetHostAt(ri, ri.AgentBranch())
	if err != nil {
		return err
	}
	if ok && cur == want {
		return nil
	}
	return m.writeFleetArtifacts(ctx, ri, map[string]string{fact.FleetHostPath: want},
		"fleet: hosted at "+strings.Join(clean, ", "))
}

// fleetHostRound is the hub's duty in each round of its local reconcile,
// before main follows the agent branch (runLocalReconcileLoop: only a copy
// with NO origin runs it, so only the hub): re-assert the host record with
// the hub's URLs as the current config gives them (hubFleetURLs). It writes
// nothing when the record is current, so a round costs one read; it repairs
// a detach whose record write failed (HostFleetHere answers the detach as
// done), and it follows a change of external_addresses. Only the fleet's
// master writes it, as everywhere. A fleet operation in flight holds
// fleetOpMu: the round skips (TryLock, never waits: a create or a detach runs
// this very loop inline under Synchronous) and the next one writes.
func (ri *RepoInstance) fleetHostRound(ctx context.Context, svc *store.Service) {
	if ri == nil || !ri.fleetDoor || !ri.IsFleet() || svc == nil {
		return
	}
	m := ri.env.m
	if !m.fleetOpMu.TryLock() {
		return
	}
	defer m.fleetOpMu.Unlock()
	if !ri.isFleetMaster(svc) {
		return
	}
	if err := m.writeFleetHost(ctx, ri, m.hubFleetURLs(ri)); err != nil && ctx.Err() == nil {
		log.Warn().Err(err).Str("fleet", ri.Name()).Msg("fleet: the host record could not be re-asserted; the next round retries")
	}
}

// credentialFreeAll is credentialFree of each of urls, in order.
func credentialFreeAll(urls []string) []string {
	out := make([]string, len(urls))
	for i, u := range urls {
		out[i] = credentialFree(u)
	}
	return out
}

// sameFleetURL reports whether a and b name the same fleet repository as a
// member reaches it: compared without credentials, with the scheme and host
// in any case, a default port (443 for https, 80 for http) dropped, and a
// trailing slash ignored; the path is otherwise exact. A string that is not
// a URL with a scheme is compared as it is.
//
// The browser mirrors this in web/src/fleetText.ts (sameFleetURL, the Move
// flow's "already on this forge"); both are held to the one table
// internal/repos/testdata/same_fleet_url.json, so change them together.
func sameFleetURL(a, b string) bool {
	norm := func(u string) string {
		u = credentialFree(u)
		p, err := url.Parse(u)
		if err != nil || !strings.Contains(u, "://") {
			return strings.TrimSuffix(u, "/")
		}
		host := strings.ToLower(p.Hostname())
		if port := p.Port(); port != "" && !(p.Scheme == "https" && port == "443" || p.Scheme == "http" && port == "80") {
			host += ":" + port
		}
		return strings.ToLower(p.Scheme) + "://" + host + strings.TrimSuffix(p.EscapedPath(), "/")
	}
	return norm(a) == norm(b)
}

// boolPtr is &b.
func boolPtr(b bool) *bool { return &b }

// credentialFree is u without its secrets: what is written to the host
// record, shown by GET /fleet, put in a bundle and quoted in a refusal.
//
//   - An http(s) URL (or any scheme but ssh) loses its whole userinfo: a
//     token is often the user part (https://ghp_…@github.com).
//   - An ssh:// URL keeps its user and loses only a password: the user is
//     the account the transport logs in as (git@…), not a secret, and the
//     bundle's URL stays the origin's.
//   - A URL that does not parse still loses everything before the last @ of
//     its authority (the `scheme://user:pass@` shape), so a typo in the host
//     never puts the token into a refusal's text.
//   - A string that is not a URL with a scheme (an scp-style git@host:path
//     names a user, not a secret) is returned unchanged.
func credentialFree(u string) string {
	if !strings.Contains(u, "://") {
		return u
	}
	p, err := url.Parse(u)
	if err != nil {
		return userinfoShape.ReplaceAllString(u, "$1")
	}
	if p.User == nil {
		return u
	}
	if strings.EqualFold(p.Scheme, "ssh") {
		if _, hasPassword := p.User.Password(); !hasPassword {
			return u
		}
		p.User = url.User(p.User.Username())
		return p.String()
	}
	p.User = nil
	return p.String()
}

// userinfoShape is `scheme://` followed by an authority that holds an @: the
// part up to its last @ is the userinfo, whatever it contains.
var userinfoShape = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*://)[^/?#]*@`)

// urlInText is a URL quoted inside free text (an error, a log line): a
// scheme, then everything up to whitespace or a quote.
var urlInText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>` + "`" + `]+`)

// credentialFreeText is text with every URL in it credentialFree, the S6
// fallback for one that does not parse included. It is for the texts that
// quote a URL someone else built: Go's HTTP client quotes the request URL
// and redacts only a password, never a token in the user slot
// (https://<token>@github.com/…), and go-git's transport errors quote it
// whole. Every refusal and log line of POST /fleet/follow goes through it.
func credentialFreeText(s string) string {
	if !strings.Contains(s, "://") {
		return s
	}
	return urlInText.ReplaceAllStringFunc(s, credentialFree)
}

// credentialFreeErr is err with its text credentialFreeText, its causes kept
// for errors.Is and errors.As. A *FleetError anywhere in the chain is
// served by errors.As as a copy whose Msg is credential-free too, since the
// REST layer writes that Msg, not Error().
func credentialFreeErr(err error) error {
	if err == nil {
		return nil
	}
	return &credentialFreeError{err: err}
}

type credentialFreeError struct{ err error }

func (e *credentialFreeError) Error() string { return credentialFreeText(e.err.Error()) }
func (e *credentialFreeError) Unwrap() error { return e.err }
func (e *credentialFreeError) As(target any) bool {
	t, ok := target.(**FleetError)
	if !ok {
		return false
	}
	var fe *FleetError
	if !errors.As(e.err, &fe) {
		return false
	}
	cp := *fe
	cp.Msg = credentialFreeText(fe.Msg)
	*t = &cp
	return true
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
