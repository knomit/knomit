package repos

import (
	"context"
	"errors"
	"fmt"
	"io"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"

	"knomit/internal/fact"
	"knomit/internal/store"
)

// A repository is a knomit knowledge base IF AND ONLY IF it has an ontology at
// one of fact.OntologyPathsNewestFirst's rungs — canonically
// .knomit/ontology.yaml. Nothing else makes one: not being non-empty, not
// having commits, not having a "main".
//
// That single question has three answers, and the third is not a formality.
// A check that FAILED established nothing, and both ways of guessing are
// unrecoverable, because a repo's ontology is fixed at create time and never
// user-editable afterwards (kb/invariants/fact/ontology/immutable-after-create):
// guessing "initialized" discards the ontology the user chose, and guessing
// "not initialized" writes one over a knowledge base that already had its own.
// So the unknown state is spelled, carried through the API, and blocks the
// wizard rather than collapsing into either answer.
const (
	// InitializedUnknown means the check did not complete. NEVER treat it as
	// either answer — offer a retry.
	InitializedUnknown = ""
	// InitializedYes means the branch carries an ontology: joining it is the
	// only correct mode, and its ontology governs.
	InitializedYes = "yes"
	// InitializedNo means the branch exists and carries no ontology: it can be
	// initialized, which writes one on knomit's own agent branch.
	InitializedNo = "no"
)

var (
	// ErrRemoteNotInitialized is returned when a mode that JOINS an existing
	// knowledge base ("clone") is pointed at a branch that has no ontology.
	// Before this existed the clone succeeded and the missing ontology was
	// silently replaced by fact.DefaultOntology() at the repo's next open —
	// the user picked "Code", got "General", and was never told.
	ErrRemoteNotInitialized = errors.New("remote branch has no knomit ontology; use mode \"initialize\" to write one")
	// ErrRemoteAlreadyInitialized is the mirror: mode "initialize" refuses a
	// branch that is already a knowledge base rather than writing a second
	// ontology over the one that governs it.
	ErrRemoteAlreadyInitialized = errors.New("remote branch already has a knomit ontology; use mode \"clone\" to join it")
	// ErrRemoteNoBranches is returned for a remote with no refs at all. knomit
	// never creates a branch on a remote other than its own agent branch, so
	// there is nothing to cut that branch FROM and the request is refused with
	// the one instruction that fixes it.
	ErrRemoteNoBranches = errors.New("remote has no branches; create a \"main\" branch first — one commit is enough")
)

// InitializedResult is what a per-branch initialization check establishes.
//
// It is deliberately NOT folded into ProbeResult: that probe answers questions
// about the REMOTE (reachable, has refs, may we push), and this one answers a
// question about a BRANCH — a repo can carry .knomit/ontology.yaml on main and
// not on develop. The branch is not known when the origin probe runs, which is
// why the wizard asks for one in between.
type InitializedResult struct {
	// Initialized is one of InitializedUnknown / InitializedYes /
	// InitializedNo. omitempty is deliberate: the unknown state is the absent
	// field, so a client that forgets to handle it reads undefined rather than
	// a plausible-looking answer.
	Initialized string `json:"initialized,omitempty"`
	// Branch is the branch actually inspected, which is the caller's when it
	// named one and the remote's default when it did not.
	Branch string `json:"branch,omitempty"`
	// Detail explains an InitializedUnknown in the words the transport used.
	Detail string `json:"detail,omitempty"`
	// OntologyID is the id of the ontology found, when one was. It answers
	// "WHICH knowledge base is this?" — the question that decides whether an
	// existing repo may attach to this remote, since two different taxonomies
	// cannot govern one repo and the local one can never be changed.
	//
	// Empty whenever Initialized is not "yes", and also when the ontology was
	// found but could not be parsed: an id that could not be read is not an id,
	// and guessing one would make a conflict check answer confidently about
	// nothing.
	OntologyID string `json:"ontology_id,omitempty"`
}

// ProbeInitialized reports whether the given branch of a remote already holds a
// knomit ontology, WITHOUT creating anything locally.
//
// The whole transfer is a `SingleBranch` + `NoTags` clone into a budgeted
// in-memory store: one branch, no tags, discarded when this returns. It is
// `Depth: 1` — one commit's tree, no history — ONLY when the server advertised
// the `shallow` capability. Measured 2026-08-17 against live GitHub and GitLab
// remotes, shallow is honoured by both — the worst case in that measurement
// was a 1,409-blob tip at 16.5 MB in 822 ms, and the case this endpoint
// actually serves (a repo with one commit) is a few hundred bytes and one
// round trip.
//
// A server that did not advertise shallow gets a FULL single-branch clone
// instead. Sending a depth to one is not a degraded request: go-git's client
// adds the `shallow` capability whenever a depth is set, and a server that did
// not advertise it rejects the request whole — knomit's own endpoint answered
// that with an HTTP 500, and real git refuses up front with "Server does not
// support shallow clients". budgetedStorage bounds the fallback.
//
// filter=blob:none would make even that near-free and is deliberately NOT used:
// go-git has packp.FilterBlobNone() and UploadRequest.Filter, but neither is
// plumbed through FetchOptions, so reaching it means dropping to the
// lower-level session API. Revisit only if someone points knomit at something
// enormous.
//
// Like ProbeOrigin, a remote that could not be read is a RESULT, not an error:
// it comes back as InitializedUnknown with a Detail. A non-nil error means the
// request itself was refused, which today means only the local-origin gate.
func (m *Manager) ProbeInitialized(ctx context.Context, o OriginSpec) (InitializedResult, error) {
	// Every clone/fetch path gates filesystem origins, with no trusted
	// exemption — and this one genuinely clones.
	if err := m.ValidateLocalOrigin(o.URL); err != nil {
		return InitializedResult{}, err
	}
	auth, err := m.ResolveAuth(authConfigFromSpec(&o), o.URL)
	if err != nil {
		// A credential that cannot even be assembled never reached the remote,
		// so nothing about the branch was established. Same reading ProbeOrigin
		// gives this case, just landing in the third state instead of an
		// auth-required one.
		return InitializedResult{Branch: o.Branch, Detail: err.Error()}, nil
	}

	netCtx, cancel := probeCtx(ctx, m.deps.Cfg.Git.NetworkTimeout)
	defer cancel()

	// WHICH BRANCH is this question about? Not always the one the caller named.
	//
	// A create does not read the consensus branch — it reads whatever
	// InitFromRemote ADOPTS, and that rule is: adopt origin/agent/<host> when
	// it exists, otherwise bootstrap the agent branch from the consensus branch
	// (store/repo.go, "If origin/agent/<host> exists, adopt it"). Both
	// initClone and initInitialize then run their ontology check against the
	// adopted branch.
	//
	// Asking about the consensus branch instead made re-creating a repository
	// THIS MACHINE had already initialized a permanent dead end. knomit never
	// writes to the consensus branch, so after an initialize it still has no
	// ontology and never will; the probe answered "no", the wizard derived the
	// only mode that answer permits — "initialize" — and the create refused it
	// with ErrRemoteAlreadyInitialized because the adopted agent branch was
	// already a knowledge base. There was no control anywhere in the wizard
	// that could send "clone", so "Try again" reproduced it forever.
	//
	// Only THIS machine's agent branch counts. Another machine's is neither
	// adopted nor inspected: we would cut our own agent branch from the
	// consensus branch, so the consensus branch is what the question is about.
	// ONE advertisement answers two questions: which branch a create would
	// adopt, and whether this server can serve a shallow clone at all. Reading
	// it through the transport session rather than remote.ListContext is what
	// makes the second answer available — ListContext drops the capabilities.
	// It stays inside the SAME netCtx budget, so an unresponsive remote cannot
	// cost a caller more than the configured timeout.
	adv, aerr := advertise(netCtx, o.URL, auth)
	if aerr != nil && !errors.Is(aerr, transport.ErrEmptyRemoteRepository) {
		// The adoption target could not be established, so neither can the
		// answer. The third state, for the same reason as everywhere else here:
		// guessing either way is unrecoverable.
		return InitializedResult{Branch: o.Branch, Detail: aerr.Error()}, nil
	}
	return m.probeInitializedBranch(ctx, netCtx, o, auth, createReadsBranch(adv, m.deps.AgentBranch, o.Branch), advertisesShallow(adv))
}

// createReadsBranch names the branch a create against this remote will
// actually READ — which is not always the one the caller asked about.
//
// store.BranchACreateReads is the rule ITSELF, the same function InitFromRemote
// applies to decide what to adopt, so the prediction and the act cannot drift.
// A remote with no refs at all has no agent branch either, and that is the
// caller's own ErrRemoteNoBranches case, not a failure to look.
func createReadsBranch(adv *packp.AdvRefs, agentBranch, requested string) string {
	return store.BranchACreateReads(advHasBranch(adv, agentBranch), agentBranch, requested)
}

// ProbeInitializedOn is ProbeInitialized for a caller that already knows
// WHICH branch the question is about. Subscribe mode uses it with the
// consensus branch: a subscription never adopts an agent branch, so the
// create-reads rule ProbeInitialized applies would answer for the wrong ref —
// a remote whose only knowledge base is this machine's own agent branch is
// not subscribable.
func (m *Manager) ProbeInitializedOn(ctx context.Context, o OriginSpec, inspect string) (InitializedResult, error) {
	if err := m.ValidateLocalOrigin(o.URL); err != nil {
		return InitializedResult{}, err
	}
	auth, err := m.ResolveAuth(authConfigFromSpec(&o), o.URL)
	if err != nil {
		return InitializedResult{Branch: inspect, Detail: err.Error()}, nil
	}
	netCtx, cancel := probeCtx(ctx, m.deps.Cfg.Git.NetworkTimeout)
	defer cancel()
	// This entry point has no listing of its own to piggyback on, so it pays
	// for the advertisement itself — inside the same netCtx budget. A round
	// trip is the price of not sending a depth to a server that cannot answer
	// it; the alternative, trying a shallow clone and retrying on failure,
	// guesses at which failures mean "no shallow" and spends the same trip
	// anyway when it guesses wrong.
	adv, aerr := advertise(netCtx, o.URL, auth)
	if aerr != nil && !errors.Is(aerr, transport.ErrEmptyRemoteRepository) {
		return InitializedResult{Branch: inspect, Detail: aerr.Error()}, nil
	}
	return m.probeInitializedBranch(ctx, netCtx, o, auth, inspect, advertisesShallow(adv))
}

// remoteProbe is ONE ref advertisement, plus the network budget it was read
// under, held open so a caller can ask SEVERAL questions of a remote for one
// round trip instead of one each.
//
// It exists for CreatePreflight, which asks two: WHICH KNOWLEDGE BASE is this
// (the duplicate layers) and IS THE BRANCH THIS CREATE WILL READ A KNOWLEDGE
// BASE (the shape check). Both answers come out of the same advertisement, and
// asking them separately would pay for it twice — on the path where the first
// answer is a refusal, the second is not even wanted.
//
// The ORDER is the point. identity() reads the advertisement alone, so a
// duplicate is refused having sent nothing but /info/refs; initializedOn()
// clones a branch, which is the first thing on this path that transfers
// objects. A preflight that asks them the other way round would refuse just as
// correctly and would have done the transfer anyway.
type remoteProbe struct {
	m       *Manager
	o       OriginSpec
	auth    transport.AuthMethod
	authErr error
	adv     *packp.AdvRefs
	advErr  error
	netCtx  context.Context
	cancel  context.CancelFunc
}

// beginRemoteProbe resolves credentials and makes the one advertisement. The
// caller MUST close the result.
//
// An unresolvable credential and an unreadable remote are both RESULTS carried
// on the struct, not errors: every question asked of the probe answers them in
// its own third state, exactly as the standalone entry points do. A non-nil
// error means the request itself was refused — today only the local-origin
// gate.
func (m *Manager) beginRemoteProbe(ctx context.Context, o OriginSpec) (*remoteProbe, error) {
	if err := m.ValidateLocalOrigin(o.URL); err != nil {
		return nil, err
	}
	p := &remoteProbe{m: m, o: o}
	p.netCtx, p.cancel = probeCtx(ctx, m.deps.Cfg.Git.NetworkTimeout)
	p.auth, p.authErr = m.ResolveAuth(authConfigFromSpec(&o), o.URL)
	if p.authErr != nil {
		return p, nil
	}
	p.adv, p.advErr = advertise(p.netCtx, o.URL, p.auth)
	if p.advErr != nil && errors.Is(p.advErr, transport.ErrEmptyRemoteRepository) {
		// A remote with no refs is not a failure to look: it is a remote with
		// no refs, which every caller here already has an answer for.
		p.advErr = nil
	}
	return p, nil
}

func (p *remoteProbe) close() {
	if p != nil && p.cancel != nil {
		p.cancel()
	}
}

// identity reports what the advertisement says about which knowledge base this
// remote holds. An advertisement we never got yields the zero value, which
// refuses nothing.
func (p *remoteProbe) identity() RemoteIdentity {
	if p == nil || p.authErr != nil || p.advErr != nil {
		return RemoteIdentity{}
	}
	return identityFromAdv(p.adv)
}

// initializedOn answers the ontology question for a named branch, reusing this
// probe's advertisement for the shallow-capability decision.
func (p *remoteProbe) initializedOn(parent context.Context, inspect string) (InitializedResult, error) {
	if p.authErr != nil {
		return InitializedResult{Branch: inspect, Detail: p.authErr.Error()}, nil
	}
	if p.advErr != nil {
		return InitializedResult{Branch: inspect, Detail: p.advErr.Error()}, nil
	}
	return p.m.probeInitializedBranch(parent, p.netCtx, p.o, p.auth, inspect, advertisesShallow(p.adv))
}

// initialized answers it for the branch a create would ADOPT, derived from
// this probe's own advertisement.
func (p *remoteProbe) initialized(parent context.Context) (InitializedResult, error) {
	if p.authErr != nil || p.advErr != nil {
		return p.initializedOn(parent, p.o.Branch)
	}
	return p.initializedOn(parent, createReadsBranch(p.adv, p.m.deps.AgentBranch, p.o.Branch))
}

// probeInitializedBranch is the shared body: shallow-clone `inspect` and look
// for an ontology in its tip tree. The two callers differ only in how they
// decide WHICH branch that is.
//
// It takes the network budget rather than deriving one, so ONE probeCtx spans
// every network call a single probe makes. ProbeInitialized needs that: it has
// already spent part of the budget on remoteHasBranch, and deriving a second
// one here would let an unresponsive remote cost a caller twice the configured
// timeout. parent is the CALLER's context, kept distinct from netCtx so
// probeFailureDetail can still tell "the caller gave up" from "our deadline
// expired".
// shallowOK says whether the server advertised the `shallow` capability. Only
// then is a depth requested: go-git's client adds the capability to the
// upload-pack request whenever a depth is set, and a server that did not
// advertise it rejects the whole request — which is how this probe used to
// come back as an HTTP 500 against knomit's own endpoint, and as "Server does
// not support shallow clients" from real git. Without shallow the clone is a
// full single-branch one, still bounded by budgetedStorage.
func (m *Manager) probeInitializedBranch(parent, netCtx context.Context, o OriginSpec, auth transport.AuthMethod, inspect string, shallowOK bool) (InitializedResult, error) {
	opts := &gogit.CloneOptions{
		URL:          o.URL,
		Auth:         auth,
		SingleBranch: true,
		Tags:         gogit.NoTags,
	}
	if shallowOK {
		opts.Depth = 1
	}
	// An empty Branch means "whatever the remote's HEAD points at" — go-git
	// resolves that itself when ReferenceName is unset. Naming a branch the
	// remote does not have fails the clone, which is an unknown, not a "no":
	// we did not look at anything.
	if inspect != "" {
		opts.ReferenceName = plumbing.NewBranchReferenceName(inspect)
	}

	// nil worktree: a bare clone. The tip TREE is the whole answer, and
	// checking a working copy out into a memfs would copy every blob at the tip
	// for nothing.
	// BOUNDED. The clone is Depth 1 and single-branch, which bounds the history
	// and not the content: the tip's whole tree is decoded onto the heap, from a
	// URL the caller chose, to answer a question that needs one tree entry.
	// budgetedStorage stops the transfer once it has cost more than the probe is
	// allowed to spend, and that failure is reported as the third state.
	store := newBudgetedStorage(m.deps.Cfg.Git.MaxProbeBytes)
	repo, cerr := gogit.CloneContext(netCtx, store, nil, opts)
	if errors.Is(cerr, errProbeTooLarge) {
		return InitializedResult{
			Branch: inspect,
			Detail: fmt.Sprintf("this remote is too large to inspect: reading %s exceeded the probe budget of %d bytes",
				inspect, m.deps.Cfg.Git.MaxProbeBytes),
		}, nil
	}
	if cerr != nil {
		if errors.Is(cerr, transport.ErrEmptyRemoteRepository) {
			// A remote with no refs has no branch to carry an ontology, so this
			// is not "no ontology on the branch" — there is no branch. The
			// wizard blocks on this state upstream (ProbeOrigin reports zero
			// branches); saying it here too keeps a direct caller from reading
			// it as an invitation to initialize.
			return InitializedResult{Branch: o.Branch, Detail: ErrRemoteNoBranches.Error()}, nil
		}
		return InitializedResult{
			Branch: o.Branch,
			Detail: probeFailureDetail(parent, netCtx, cerr, m.deps.Cfg.Git.NetworkTimeout),
		}, nil
	}

	branch := inspect
	head, herr := repo.Head()
	if herr != nil {
		return InitializedResult{Branch: branch, Detail: herr.Error()}, nil
	}
	if branch == "" {
		branch = head.Name().Short()
	}
	commit, cmerr := repo.CommitObject(head.Hash())
	if cmerr != nil {
		return InitializedResult{Branch: branch, Detail: cmerr.Error()}, nil
	}
	tree, terr := commit.Tree()
	if terr != nil {
		return InitializedResult{Branch: branch, Detail: terr.Error()}, nil
	}

	for _, p := range fact.OntologyPathsNewestFirst() {
		entry, ferr := tree.FindEntry(p)
		if ferr == nil {
			return InitializedResult{
				Initialized: InitializedYes,
				Branch:      branch,
				OntologyID:  ontologyIDFromTree(tree, entry),
			}, nil
		}
		if errors.Is(ferr, object.ErrEntryNotFound) || errors.Is(ferr, object.ErrDirectoryNotFound) {
			continue
		}
		// A tree we could not READ is not a tree without an ontology.
		return InitializedResult{Branch: branch, Detail: ferr.Error()}, nil
	}
	return InitializedResult{Initialized: InitializedNo, Branch: branch}, nil
}

// advHasBranch reports whether the advertisement carries refs/heads/<branch>.
//
// Refs only — no objects transfer — which is the same listing ProbeOrigin makes
// one step earlier in the wizard. It is repeated here rather than threaded
// through because this endpoint is reachable on its own, and because the answer
// must be about the remote as it is NOW: the branch it asks about is one knomit
// itself pushes, so a listing taken before an earlier create finished would be
// stale in exactly the case that matters.
//
// A nil advertisement is an empty remote: no refs, so no agent branch.
func advHasBranch(adv *packp.AdvRefs, branch string) bool {
	if adv == nil || branch == "" {
		return false
	}
	_, ok := adv.References[plumbing.NewBranchReferenceName(branch).String()]
	return ok
}

// ontologyIDFromTree reads the ontology blob already located in this tip tree
// and returns the id it declares, or "" if it cannot be read.
//
// Best-effort ON PURPOSE. The question this endpoint exists to answer is
// whether the branch HAS an ontology, and that has already been answered by
// finding the entry; the id is extra, and a document that will not parse must
// not turn a good "yes" into an unknown. Callers treat "" as "no id
// established" and must not read it as "no conflict".
func ontologyIDFromTree(tree *object.Tree, entry *object.TreeEntry) string {
	file, err := tree.TreeEntryFile(entry)
	if err != nil {
		return ""
	}
	rdr, err := file.Reader()
	if err != nil {
		return ""
	}
	defer func() { _ = rdr.Close() }()
	// Bounded: this is a remote-supplied file read into memory, and an ontology
	// that does not fit in the same cap the API accepts is not one this binary
	// would honour anyway.
	data, err := io.ReadAll(io.LimitReader(rdr, maxProbedOntologyBytes))
	if err != nil {
		return ""
	}
	ont, err := fact.ParseOntology(data)
	if err != nil {
		return ""
	}
	return ont.ID
}

// maxProbedOntologyBytes bounds the ontology read out of a probed remote.
const maxProbedOntologyBytes = 1 << 20

// ErrOriginOntologyConflict is returned when a remote being attached to an
// EXISTING repo is a knowledge base governed by a different ontology.
var ErrOriginOntologyConflict = errors.New("this remote is a different knowledge base")

// CheckOriginOntology gates attaching a remote to a repo that already exists.
//
// Create enforces "a repository is a knomit knowledge base if and only if it
// has an ontology" on both of its remote modes: clone refuses a remote without
// one, initialize refuses one that already has one. Attaching an origin is the
// OTHER door into the same situation, and it was not gated at all — so a repo
// created on the Code taxonomy could be pointed at a knowledge base on the
// General one, and from then on facts written locally and facts arriving from
// the remote were validated against different topic sets. The local ontology is
// fixed at create time and never user-editable, so there is no way back.
//
// What is allowed, and why it is not simply "refuse any remote with an
// ontology":
//
//   - the remote is NOT a knowledge base → fine. This is how an existing local
//     repo gets backed up: knomit writes its own agent branch there, carrying
//     the repo's ontology with it.
//   - the remote holds the SAME knowledge base (equal ontology id) → fine. This
//     is re-attaching a repo to its own remote, which is the ordinary case
//     after a machine is rebuilt.
//   - the remote holds a DIFFERENT one → refused here.
//
// A check that could not complete refuses NOTHING, exactly as elsewhere in this
// file: an unreachable remote or an ontology whose id could not be read
// establishes nothing, and refusing on it would block an attach that is very
// likely fine. The comparison is by ID rather than by content because content
// legitimately drifts — the boot-time preset refresh rewrites it — while the id
// is what names the taxonomy.
func (m *Manager) CheckOriginOntology(ctx context.Context, localOntologyID string, o OriginSpec) error {
	if localOntologyID == "" {
		return nil
	}
	res, err := m.ProbeInitialized(ctx, o)
	if err != nil || res.Initialized != InitializedYes || res.OntologyID == "" {
		return nil
	}
	if res.OntologyID == localOntologyID {
		return nil
	}
	return fmt.Errorf("%w: %s on %s is governed by %q, and this repository is governed by %q",
		ErrOriginOntologyConflict, res.Branch, o.URL, res.OntologyID, localOntologyID)
}

// errProbeTooLarge ends a probe whose transfer exceeded Cfg.Git.MaxProbeBytes.
var errProbeTooLarge = errors.New("probe budget exceeded")

// budgetedStorage is a memory store that refuses to hold more than a budget.
//
// It exists because the probe's cost is set by a REMOTE the caller names, not
// by anything knomit controls, and "clone the tip into RAM" has no natural
// ceiling. Refusing past the budget turns an unbounded allocation into the
// third state — nothing established, say so — which is the answer this file
// gives to every other kind of incomplete check.
type budgetedStorage struct {
	*memory.Storage
	budget int64
	spent  int64
}

func newBudgetedStorage(budget int64) *budgetedStorage {
	return &budgetedStorage{Storage: memory.NewStorage(), budget: budget}
}

func (b *budgetedStorage) SetEncodedObject(obj plumbing.EncodedObject) (plumbing.Hash, error) {
	if b.budget > 0 {
		b.spent += obj.Size()
		if b.spent > b.budget {
			return plumbing.ZeroHash, errProbeTooLarge
		}
	}
	return b.Storage.SetEncodedObject(obj)
}

// branchHasOntology reports whether branch in an already-open local store
// carries an ontology at any rung.
//
// This is the LOCAL counterpart of ProbeInitialized, and the authoritative one:
// initClone and initInitialize both run it against the branch they just built
// from the remote, so neither depends on a probe taken earlier in time against
// a remote that may have changed. It walks the same rungs
// fact.OntologyPathsNewestFirst gives repoBuilder.loadOntology, so "this repo
// has an ontology" means the same thing at create time and at open time.
//
// A read that FAILS is returned as an error, never as false: the caller refuses
// on false, and refusing on a failed read would turn a transient fault into a
// verdict about the user's data.
func branchHasOntology(ctx context.Context, svc *store.Service, branch string) (bool, error) {
	for _, p := range fact.OntologyPathsNewestFirst() {
		ok, err := svc.Facts().FactExists(ctx, branch, p)
		if err != nil {
			return false, fmt.Errorf("read %s on %s: %w", p, branch, err)
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}
