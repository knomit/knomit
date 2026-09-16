package repos

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"

	"knomit/internal/store"
)

// ErrKnowledgeBaseAlreadyLocal refuses a create whose remote holds a knowledge
// base an ACTIVE repo on this machine already holds.
//
// The refusal itself is not new — Registry.RecordRepoID has always enforced
// one local copy per knowledge base
// (kb/invariants/repos/one-local-copy-per-knowledge-base) — but it used to
// arrive after the whole clone, after the store was opened, after the commit
// graph was backfilled and after registration, as a rollback. This error is
// the same verdict reached EARLY, and it is always wrapped with the name of
// the repo that already holds it, because "already registered" without a name
// is a dead end for the person reading it.
var ErrKnowledgeBaseAlreadyLocal = errors.New("this knowledge base is already registered locally")

// ErrRegistryUnavailable reports that a duplicate check could not be RUN.
//
// Distinct from a refusal on purpose, and never collapsed into "not a
// duplicate": a registry we could not read has established nothing, and
// treating that silence as a pass is how a check that exists stops protecting
// anything. It is also not the same as a refusal — the create may be perfectly
// fine — so it gets its own error and its own status.
var ErrRegistryUnavailable = errors.New("repo registry unavailable")

// alreadyLocal wraps ErrKnowledgeBaseAlreadyLocal with the holder's name, in
// the wording the origin-session flow already uses for the same verdict, so a
// user meets ONE description of this situation rather than two.
func alreadyLocal(holder string) error {
	return fmt.Errorf("%w: this remote holds the same knowledge base as the repo %q; "+
		"two local copies would clobber each other's agent branch on push",
		ErrKnowledgeBaseAlreadyLocal, holder)
}

// HeldByAnotherActiveRepo returns the NAME of an active repo other than
// selfUID whose registered identity is repoID, or "" if none.
//
// One local copy per knowledge base: two repos backed by the same root commit
// would both write agent/<host> and clobber each other on push to the shared
// origin. Registry.RecordRepoID enforces that structurally, but by the time it
// speaks — after a clone, or after a store swap — the expensive or
// irreversible work has already happened. Every caller that can reach a point
// of no return asks this question BEFORE it acts.
//
// selfUID is excluded: re-pointing a repo at the knowledge base it already
// holds is not a duplicate. A create passes its own fresh uid, which matches
// nothing.
//
// It lives in this package rather than in internal/web because BOTH now need
// it: the origin-session connect flow (its original home) and the create
// preflight.
func HeldByAnotherActiveRepo(m *Manager, selfUID, repoID string) (string, error) {
	if m == nil || repoID == "" {
		return "", nil
	}
	reg := m.Repos()
	if reg == nil {
		return "", nil
	}
	active, err := reg.List(StateActive)
	if err != nil {
		return "", fmt.Errorf("list registered repos: %w", err)
	}
	for _, rec := range active {
		if rec.UID != selfUID && rec.RepoID == repoID {
			return rec.Name, nil
		}
	}
	return "", nil
}

// RemoteIdentity is everything ONE ref advertisement says about which
// knowledge base a remote holds.
//
// Two independent answers, deliberately carried together because they come
// from the same round trip and are checked one after the other:
//
//   - RepoID is the remote's own CLAIM (the knomit-repo-id capability), empty
//     for any server that is not knomit. It is trusted only to REFUSE; nothing
//     records identity from it.
//   - Tips are the advertised branch heads. A commit is unique to a history,
//     so a tip we already hold is PROOF of the same knowledge base — regardless
//     of what the server does or does not claim about itself.
type RemoteIdentity struct {
	RepoID string
	Tips   []plumbing.Hash
}

// identityFromAdv reads a ref advertisement into a RemoteIdentity. A nil
// advertisement (an empty remote, or one that could not be read) yields the
// zero value, which refuses nothing — established nothing, refuses nothing.
func identityFromAdv(adv *packp.AdvRefs) RemoteIdentity {
	var id RemoteIdentity
	if adv == nil {
		return id
	}
	if adv.Capabilities != nil && adv.Capabilities.Supports(capKnomitRepoID) {
		if vals := adv.Capabilities.Get(capKnomitRepoID); len(vals) > 0 {
			id.RepoID = strings.ToLower(strings.TrimSpace(vals[0]))
		}
	}
	names := make([]string, 0, len(adv.References))
	for name := range adv.References {
		if strings.HasPrefix(name, "refs/heads/") {
			names = append(names, name)
		}
	}
	// Sorted so the holder a refusal names is the same on every run. Map order
	// would otherwise make the message nondeterministic when two local repos
	// could both claim a tip.
	sort.Strings(names)
	for _, name := range names {
		id.Tips = append(id.Tips, adv.References[name])
	}
	return id
}

// capKnomitRepoID is the capability internal/store advertises. Spelled here as
// a plain string because the capability package's type is what the store side
// needs; the two must not drift, and store's own advertisement test pins the
// wire spelling.
const capKnomitRepoID = "knomit-repo-id"

// tipHeldLocally returns the name of an ACTIVE repo whose store already holds
// one of these commits, or "" if none does.
//
// A HIT IS PROOF; A MISS PROVES NOTHING. Commits are unique to a history, so a
// local store holding a remote's advertised tip is holding the same knowledge
// base — that is a sufficient check, and it costs only local reads. But a miss
// means only that no local copy has fetched that particular commit, which is
// the ordinary state of a local copy that is simply BEHIND its remote. A miss
// must therefore never be read as "not a duplicate", and must never skip the
// authoritative root-commit check after the clone.
//
// Bounded by (advertised tips × active repos) local object lookups, no
// network.
func tipHeldLocally(m *Manager, tips []plumbing.Hash) string {
	if m == nil || len(tips) == 0 {
		return ""
	}
	var holder string
	m.ForEach(func(name string, ri *RepoInstance) {
		if holder != "" || ri == nil {
			return
		}
		_ = ri.WithRead(func(svc *store.Service) {
			if svc == nil {
				return
			}
			for _, tip := range tips {
				if svc.HasObject(tip.String()) {
					holder = name
					return
				}
			}
		})
	})
	return holder
}

// refuseIfKnowledgeBaseIsLocal runs the two CHEAP identity layers against one
// ref advertisement, cheapest first, and returns the refusal if either fires.
//
// Layer 1 — the remote's own claim (milliseconds, no I/O beyond the
// advertisement already in hand). A knomit origin advertises the root commit
// of its consensus branch; if an active local repo is registered under that
// identity, this remote IS that knowledge base. Only knomit origins answer
// this, and the claim is trusted only to refuse.
//
// Layer 2 — proof from the tips (milliseconds, local reads). Works against ANY
// git remote, knomit or not, and catches the case layer 1 cannot: a remote
// that says nothing about itself but whose tip we already hold.
//
// NEITHER IS AUTHORITATIVE ABOUT THE ABSENCE OF A DUPLICATE. Both are
// sufficient-only: layer 1 is silent for a non-knomit remote, and layer 2 is
// silent whenever the local copy is behind. Passing here means "no cheap proof
// of a duplicate was found", never "not a duplicate" — which is why layer 3
// (the root commit of the freshly cloned store, checked before registration)
// still runs unconditionally, and why RecordRepoID remains the backstop behind
// that.
//
// selfUID is the repo allowed to hold this identity already — a fresh uid for
// a create, which excludes nothing.
func refuseIfKnowledgeBaseIsLocal(m *Manager, selfUID string, id RemoteIdentity) error {
	if id.RepoID != "" {
		holder, err := HeldByAnotherActiveRepo(m, selfUID, id.RepoID)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrRegistryUnavailable, err)
		}
		if holder != "" {
			return alreadyLocal(holder)
		}
	}
	if holder := tipHeldLocally(m, id.Tips); holder != "" {
		return alreadyLocal(holder)
	}
	return nil
}
