package repos

import (
	"context"
	"errors"
	"strings"

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
func (m *Manager) ProbeOrigin(ctx context.Context, o OriginSpec) (ProbeResult, error) {
	// Every clone/fetch path gates filesystem origins, with no trusted
	// exemption. A probe reaches the same filesystem, so it gates too.
	if err := m.ValidateLocalOrigin(o.URL); err != nil {
		return ProbeResult{}, err
	}
	auth, err := m.ResolveAuth(authConfigFromSpec(&o), o.URL)
	if err != nil {
		return ProbeResult{Branches: []string{}, Detail: err.Error()}, nil
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

	refs, err := rem.ListContext(ctx, &gogit.ListOptions{Auth: auth})
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
			return ProbeResult{Branches: []string{}, Detail: err.Error()}, nil
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
