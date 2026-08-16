package repos

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	gogitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"
)

// ProbeResult is what a pre-create look at a remote can establish.
//
// A probe is `remote.ListContext` — it sees REFS ONLY. It deliberately carries
// no fact count, commit count or ontology id: establishing those needs a real
// fetch. Nothing in the pre-create UI may claim them.
//
// Branches is always a non-nil slice on every path where ProbeOrigin returns
// a nil error, even when empty — the OpenAPI schema declares it a plain
// array (no nullable:true) and the web client types it string[], so a nil
// slice serializing as JSON null would be a runtime error in the wizard.
type ProbeResult struct {
	Reachable      bool     `json:"reachable"`
	Empty          bool     `json:"empty"`
	AuthRequired   bool     `json:"auth_required"`
	UpstreamBranch string   `json:"upstream_branch"`
	Branches       []string `json:"branches"`
	Detail         string   `json:"detail,omitempty"`
}

// ProbeOrigin inspects a remote without cloning it, so the create wizard can
// tell "join an existing knowledge base" from "seed an empty one".
//
// Unreachable and auth-required are RESULTS, not errors — both are ordinary
// states the wizard renders and recovers from. A non-nil error means the
// request itself was refused, which today means only the local-origin gate.
//
// The result is advisory. A remote can gain refs between the probe and the
// create, so Create re-asserts emptiness itself; never treat this as authority.
//
// The ref listing is bounded by Cfg.Git.NetworkTimeout (see probeCtx). A
// remote that never answers therefore comes back as an ordinary unreachable
// result after that budget, not as a call that hangs for as long as the
// transport allows.
func (m *Manager) ProbeOrigin(ctx context.Context, o OriginSpec) (ProbeResult, error) {
	// Every clone/fetch path gates filesystem origins, with no trusted
	// exemption. A probe reaches the same filesystem, so it gates too.
	if err := m.ValidateLocalOrigin(o.URL); err != nil {
		return ProbeResult{}, err
	}
	// A credential that cannot even be ASSEMBLED is an auth problem, not a
	// reachability one: nothing here touched the network, so "unreachable" is a
	// claim about the remote we have no evidence for. The concrete case is an
	// SSH URL (git@host:repo) with no usable key on this machine
	// (TestManager_ResolveAuth_SSHNoKeyFails) — a perfectly reachable remote.
	//
	// Reporting it as {Reachable:false} was a DEAD END, not just a mislabel:
	// stepsFor (web/src/wizardState.ts) collapses an unreachable probe to
	// ['source'], and the access step — the only place a credential can be
	// entered — is exactly what that removes. So the user was told the wrong
	// cause AND denied the one control that fixes it.
	//
	// Reachable:true here therefore means "no evidence of a reachability
	// failure", which is the reading every consumer already gives it for the
	// auth-required case ProbeOrigin reports below (also Reachable:true with
	// Empty unknown/false). seedProbeErr (lifecycle.go) orders !Reachable →
	// AuthRequired → !Empty, so this lands in its authentication arm.
	auth, err := m.ResolveAuth(authConfigFromSpec(&o), o.URL)
	if err != nil {
		return ProbeResult{
			Reachable:    true,
			AuthRequired: true,
			Branches:     []string{},
			Detail:       err.Error(),
		}, nil
	}

	repo, err := gogit.Init(memory.NewStorage(), nil)
	if err != nil {
		return ProbeResult{}, err
	}
	rem, err := repo.CreateRemote(&gogitconfig.RemoteConfig{
		Name: "origin",
		URLs: []string{o.URL},
	})
	if err != nil {
		return ProbeResult{Branches: []string{}, Detail: err.Error()}, nil
	}

	// Bound the ref listing by the configured network timeout, exactly as every
	// other remote git call in this package does (see the netCtx wrappers in
	// internal/store: remote_sync.go's push, repo.go's fetch). Without it a
	// black-holed host leaves the wizard's first step spinning for as long as
	// the TCP stack takes to give up — which is the one thing design §6 said
	// must not happen. A zero timeout keeps the parent unchanged, matching
	// netCtxWith's "0 means no bound" convention.
	netCtx, cancel := probeCtx(ctx, m.deps.Cfg.Git.NetworkTimeout)
	defer cancel()

	refs, err := rem.ListContext(netCtx, &gogit.ListOptions{Auth: auth})
	if err != nil {
		empty, authRequired := classifyProbeError(err)
		switch {
		case empty:
			return ProbeResult{
				Reachable:      true,
				Empty:          true,
				Branches:       []string{},
				UpstreamBranch: resolveUpstream(o.Branch, "", nil),
			}, nil
		case authRequired:
			return ProbeResult{
				Reachable:    true,
				AuthRequired: true,
				Branches:     []string{},
				Detail:       err.Error(),
			}, nil
		default:
			return ProbeResult{Branches: []string{}, Detail: probeFailureDetail(ctx, netCtx, err, m.deps.Cfg.Git.NetworkTimeout)}, nil
		}
	}

	res := ProbeResult{Reachable: true, Branches: []string{}}
	var head string
	for _, ref := range refs {
		if ref.Name() == plumbing.HEAD && ref.Type() == plumbing.SymbolicReference {
			head = ref.Target().Short()
			continue
		}
		if ref.Name().IsBranch() {
			res.Branches = append(res.Branches, ref.Name().Short())
		}
	}
	res.Empty = len(res.Branches) == 0
	res.UpstreamBranch = resolveUpstream(o.Branch, head, res.Branches)
	return res, nil
}

// probeCtx bounds a single probe's network call by timeout, mirroring
// internal/store's netCtxWith (branch.go): timeout <= 0 returns the parent
// unchanged with a no-op cancel, so a config that disables the bound keeps the
// legacy wait-forever behaviour rather than failing instantly.
func probeCtx(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

// probeFailureDetail renders the detail string for a probe that failed for a
// reason classifyProbeError could not name.
//
// It distinguishes OUR deadline from the caller's cancellation: when the
// derived context expired but the request context is still live, the remote
// simply did not answer inside the configured budget, and "context deadline
// exceeded" is a useless thing to put in front of a user. A cancel that came
// from the caller (the wizard's Cancel button, a dropped HTTP request) keeps
// go-git's own error, since the caller already knows why it stopped.
func probeFailureDetail(parent, derived context.Context, err error, timeout time.Duration) string {
	if parent.Err() == nil && errors.Is(derived.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("timed out after %s waiting for the remote to answer", timeout)
	}
	return err.Error()
}

// classifyProbeError inspects an error returned by remote.ListContext and
// reports whether it signals an empty remote or a missing/rejected
// credential, as opposed to a genuine connectivity failure (bad host,
// refused connection, timeout, ...). Pure and side-effect free so it can be
// (and is, in probe_test.go) unit-tested against synthetic errors without a
// network or a live server.
//
// Coverage is transport-specific and asymmetric — this is NOT "the same
// check for every transport":
//
//   - HTTP(S): go-git's http transport wraps a 401/403 response in the
//     exported sentinels transport.ErrAuthenticationRequired /
//     ErrAuthorizationFailed (plumbing/transport/http/common.go:584,586),
//     so those are matched with errors.Is — a reliable, typed check.
//
//   - SSH: go-git's ssh transport (plumbing/transport/ssh/common.go) does
//     NOT wrap authentication failures in any sentinel or typed error at
//     all — connect() just returns whatever golang.org/x/crypto/ssh's
//     NewClientConn produced. Verified against the vendored source
//     (golang.org/x/crypto@v0.53.0/ssh/client.go:85 wraps with
//     fmt.Errorf("ssh: handshake failed: %w", err); when every offered auth
//     method is rejected, the wrapped error is the bare
//     fmt.Errorf("ssh: unable to authenticate, attempted methods %v, no
//     supported methods remain", tried) from client_auth.go:154). There is
//     no sentinel or type to errors.Is/errors.As against, so this is
//     matched on that message's stable substring instead. The substring is
//     specific to actual credential rejection — other SSH failures (DNS,
//     connection refused, timeout, host-key mismatch) do not contain it and
//     fall through to the generic "unreachable" case.
func classifyProbeError(err error) (empty bool, authRequired bool) {
	if err == nil {
		return false, false
	}
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return true, false
	}
	if errors.Is(err, transport.ErrAuthenticationRequired) || errors.Is(err, transport.ErrAuthorizationFailed) {
		return false, true
	}
	if strings.Contains(err.Error(), "ssh: unable to authenticate") {
		return false, true
	}
	return false, false
}

// resolveUpstream mirrors InitFromRemote's preference order so the wizard shows
// the branch the clone would actually adopt: an explicit request wins, then
// "main", then the remote's symbolic HEAD, then "main" as the last resort.
//
// Also covers the empty-remote case (head="", branches=nil): with no refs to
// consider it collapses to "the requested branch, else main", which is why
// ProbeOrigin no longer needs a separate helper for that case.
func resolveUpstream(requested, head string, branches []string) string {
	if requested != "" {
		return requested
	}
	for _, b := range branches {
		if b == "main" {
			return "main"
		}
	}
	if head != "" {
		return head
	}
	return "main"
}
