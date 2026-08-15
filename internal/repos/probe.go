package repos

import (
	"context"
	"errors"

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
		return ProbeResult{Detail: err.Error()}, nil
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
		return ProbeResult{Detail: err.Error()}, nil
	}

	refs, err := rem.ListContext(ctx, &gogit.ListOptions{Auth: auth})
	switch {
	case errors.Is(err, transport.ErrEmptyRemoteRepository):
		return ProbeResult{Reachable: true, Empty: true, UpstreamBranch: upstreamOrMain(o.Branch)}, nil
	case errors.Is(err, transport.ErrAuthenticationRequired),
		errors.Is(err, transport.ErrAuthorizationFailed):
		return ProbeResult{Reachable: true, AuthRequired: true, Detail: err.Error()}, nil
	case err != nil:
		return ProbeResult{Detail: err.Error()}, nil
	}

	res := ProbeResult{Reachable: true}
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

func upstreamOrMain(requested string) string {
	if requested != "" {
		return requested
	}
	return "main"
}

// resolveUpstream mirrors InitFromRemote's preference order so the wizard shows
// the branch the clone would actually adopt: an explicit request wins, then
// "main", then the remote's symbolic HEAD, then "main" as the last resort.
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
