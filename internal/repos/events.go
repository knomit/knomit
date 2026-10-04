package repos

// Events: every change to a mounted repo is one of these, sent to its Machine.
//
// An event names a TARGET stage. A foundation target (Populate, Open,
// Identify) REWINDS: the driver exits every entered stage down to the target,
// applies the change, and walks forward again. A worker target (Index, Serve,
// Sync) RESTARTS that one stage. Adding an event is one constructor here —
// target, guard, apply — and nothing anywhere else.
//
// A guard runs before anything is exited and may read the network (bounded by
// cfg.Git.NetworkTimeout): a refusal leaves the repo exactly as it was. An
// apply runs after the exits and before the walk; its error is the event's
// reply, never a reason to leave stages exited (rule 11).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/rs/zerolog/log"

	"knomit/internal/store"
)

var (
	// ErrNotOpen refuses an event while the repo's store is not attached: a
	// repo still being created (Populate), or one whose store is being
	// reopened. Transient; the HTTP layer answers 503.
	ErrNotOpen = errors.New("repo is not open yet")
	// ErrIndexing refuses Attach, Detach and Swap while the index job runs
	// (rule 3). The caller waits or cancels indexing (CancelIndex); the HTTP
	// layer answers 409 with a cancel-index link.
	ErrIndexing = errors.New("repo is indexing")
	// ErrIndexCancelled is the index result after CancelIndex: the status
	// reads index_state "error" with reason "indexing cancelled".
	ErrIndexCancelled = errors.New("indexing cancelled")
	// ErrUnavailable is the reply of a machine whose foundation could not be
	// built (a StageError carries which stage and why).
	ErrUnavailable = errors.New("repo is unavailable")
	// ErrClosed is the reply of a machine that has been unmounted.
	ErrClosed = errors.New("repo lifecycle is closed")
	// ErrOriginUnreachable is the Attach guard's verdict on a remote it could
	// not read: unreachable, timed out, or refusing the credential. Nothing
	// was persisted and the running sync loop was never stopped.
	ErrOriginUnreachable = errors.New("origin was not attached")
	// ErrSubscriptionOrigin refuses detaching the origin of a subscription,
	// which has no content of its own.
	ErrSubscriptionOrigin = errors.New("a subscription requires its origin")
)

// errNotOpenPopulating is ErrNotOpen for a repo still being created.
var errNotOpenPopulating = fmt.Errorf("%w: repo is populating", ErrNotOpen)

// StageError is the reply (and the Unavailable status) of a walk whose
// foundation stage failed. It unwraps to BOTH ErrUnavailable and the cause, so
// a caller can test for either (errors.Is(err, ErrRepoAlreadyRegistered)).
type StageError struct {
	Stage StageID
	Err   error
}

func (e *StageError) Error() string {
	return fmt.Sprintf("%s: %v", stages[e.Stage].Name(), e.Err)
}

// Unwrap exposes the sentinel and the cause.
func (e *StageError) Unwrap() []error { return []error{ErrUnavailable, e.Err} }

type eventKind int

const (
	evMount eventKind = iota
	evAttachOrigin
	evDetachOrigin
	evSwapStore
	evRebuild
	evCancelIndex
	evUnmount
)

// MountMode is how Populate brings a repo's database into existence.
type MountMode string

const (
	// MountExisting opens what is on disk (boot, restore, after a swap).
	MountExisting MountMode = "none"
	// MountCreate runs a create's init path (clone / initialize / subscribe /
	// preset / custom) under the Populate life.
	MountCreate MountMode = "create"
)

// MountSpec is what Populate does. Emit receives a create's progress lines.
type MountSpec struct {
	Mode   MountMode
	Create CreateSpec
	Emit   func(Event)
}

// SwapSpec is a disjoint-history connect: install TempDB over the repo's
// database and persist Origin, as one apply. IdentityBranch is the branch the
// one-local-copy check reads the incoming root commit from.
type SwapSpec struct {
	TempDB         string
	Origin         OriginSpec
	IdentityBranch string
}

// MachineEvent is one event for a Machine. Build it with a constructor; the
// zero value is not an event.
type MachineEvent struct {
	kind   eventKind
	target StageID

	mount  MountSpec
	origin OriginSpec
	swap   SwapSpec
	branch string
	reason string

	reply chan reply
}

// Reply is what Send returns besides an error.
type Reply struct {
	// JobID names a manual rebuild (Rebuild); Absorbed reports that an
	// identical rebuild was already running and this one joined it.
	JobID    string
	Absorbed bool
	// Status is the machine's status after the event was handled.
	Status Status
}

type reply struct {
	Reply
	err error
}

// Mount rewinds to Populate and walks the whole repo up: boot, create,
// restore.
func Mount(spec MountSpec) MachineEvent {
	if spec.Mode == "" {
		spec.Mode = MountExisting
	}
	return MachineEvent{kind: evMount, target: StagePopulate, mount: spec}
}

// AttachOrigin points the repo at spec (PUT /origin, a shared-history
// connect) and restarts Sync so the origin loop runs. spec is fully resolved
// by the caller except Branch, which may be empty (the stored branch, else
// the repo's consensus branch).
func AttachOrigin(spec OriginSpec) MachineEvent {
	return MachineEvent{kind: evAttachOrigin, target: StageSync, origin: spec}
}

// DetachOrigin removes the origin and restarts Sync, which then runs the
// origin-less local loop.
func DetachOrigin() MachineEvent {
	return MachineEvent{kind: evDetachOrigin, target: StageSync}
}

// SwapStore replaces the repo's database with spec.TempDB and its origin with
// spec.Origin, rewinding to Populate.
func SwapStore(spec SwapSpec) MachineEvent {
	return MachineEvent{kind: evSwapStore, target: StagePopulate, swap: spec}
}

// Rebuild restarts Index as a full rebuild of branch. It replaces a running
// heal or a rebuild of another branch, and absorbs an identical one.
func Rebuild(branch string) MachineEvent {
	return MachineEvent{kind: evRebuild, target: StageIndex, branch: branch}
}

// CancelIndex exits Index without re-entering it; the result becomes
// ErrIndexCancelled. A no-op when no index job runs.
func CancelIndex() MachineEvent {
	return MachineEvent{kind: evCancelIndex, target: StageIndex}
}

// Unmount closes the repo for good: Archive, Manager.Close, a failed create.
func Unmount(reason string) MachineEvent {
	return MachineEvent{kind: evUnmount, reason: reason}
}

// exclusiveWithIndex: Attach, Detach and Swap are refused while the index job
// runs (rule 3), checked before any guard or network read.
func (e *MachineEvent) exclusiveWithIndex() bool {
	return e.kind == evAttachOrigin || e.kind == evDetachOrigin || e.kind == evSwapStore
}

// guardContext bounds a guard's network read by cfg.Git.NetworkTimeout. Zero
// means NO LIMIT there — context.WithTimeout(ctx, 0) would mean already
// expired — so a zero timeout gets a plain cancellable context.
func guardContext(parent context.Context, r *RepoInstance) (context.Context, context.CancelFunc) {
	if d := r.env.cfg.Git.NetworkTimeout; d > 0 {
		return context.WithTimeout(parent, d)
	}
	return context.WithCancel(parent)
}

// guard runs before anything is exited. It may read the network; a refusal
// changes nothing.
func (e *MachineEvent) guard(ctx context.Context, r *RepoInstance) error {
	switch e.kind {
	case evAttachOrigin:
		return guardAttach(ctx, r, e.origin)
	case evDetachOrigin:
		if r.subscribed {
			return ErrSubscriptionOrigin
		}
	case evSwapStore:
		return guardSwap(ctx, r, e.swap)
	}
	return nil
}

// guardAttach is the cheap probe: a bad token, an unreachable remote or a
// different knowledge base fails here, with no branch lock and nothing
// persisted. Its verdicts use the probe's four-state vocabulary
// (kb/invariants/repos/probe/four-states-not-three): reachable, empty and
// auth-required are distinct, and only a confirmed different ontology is a
// conflict.
//
// BUDGET: it makes TWO network reads in sequence on the driver goroutine —
// ProbeOriginRefs, then CheckOriginOntology. Each derives its own
// cfg.Git.NetworkTimeout deadline (probeCtx), but both run under the ONE ctx
// guardContext made, whose deadline is also NetworkTimeout. So the driver
// waits at most one NetworkTimeout for the pair, not two; the cost is that a
// slow probe leaves the ontology check only the remainder, and an ontology
// read that runs out refuses nothing (an unreadable remote decides neither
// way). With NetworkTimeout 0 neither read is bounded.
func guardAttach(ctx context.Context, r *RepoInstance, o OriginSpec) error {
	m := r.env.m
	// The local-origin policy, re-asserted where the attach happens.
	if err := m.ValidateLocalOrigin(o.URL); err != nil {
		return err
	}
	res, err := m.ProbeOriginRefs(ctx, o)
	if err != nil {
		return err
	}
	switch {
	case !res.Reachable:
		return fmt.Errorf("%w: the remote is unreachable: %s", ErrOriginUnreachable, res.Detail)
	case res.AuthRequired:
		return fmt.Errorf("%w: the remote refused the credential: %s", ErrOriginUnreachable, res.Detail)
	}
	if ont := r.Ontology(); ont != nil {
		if err := m.CheckOriginOntology(ctx, ont.ID, o); err != nil {
			return err
		}
	}
	return nil
}

// guardSwap holds the checks that must come before the swap's point of no
// return: the one-local-copy check on the INCOMING root commit — the Swap
// guard is the one site (kb/invariants/repos/one-local-copy-per-knowledge-base)
// — and the ontology gate.
func guardSwap(ctx context.Context, r *RepoInstance, s SwapSpec) error {
	m := r.env.m
	branch := s.IdentityBranch
	if branch == "" {
		branch = r.ReadBranch()
	}
	root, err := rootCommitOfDB(ctx, s.TempDB, branch)
	if err != nil {
		return fmt.Errorf("read the incoming store's identity: %w", err)
	}
	if root != "" {
		holder, herr := HeldByAnotherActiveRepo(m, r.uid, root)
		if herr != nil {
			return fmt.Errorf("%w: %v", ErrRegistryUnavailable, herr)
		}
		if holder != "" {
			return fmt.Errorf("%w: this remote holds the same knowledge base as the repo %q; connect aborted before any change was made",
				ErrKnowledgeBaseAlreadyLocal, holder)
		}
	}
	if ont := r.Ontology(); ont != nil && s.Origin.URL != "" {
		if err := m.CheckOriginOntology(ctx, ont.ID, s.Origin); err != nil {
			return err
		}
	}
	return nil
}

// rootCommitOfDB opens the database at dbPath, resolves the root commit
// reachable from branch, and closes it again on every path.
func rootCommitOfDB(ctx context.Context, dbPath, branch string) (string, error) {
	svc, err := store.Open(dbPath)
	if err != nil {
		return "", fmt.Errorf("open: %w", err)
	}
	defer svc.Close()
	if err := svc.OpenRepo(); err != nil {
		return "", fmt.Errorf("open git: %w", err)
	}
	return svc.RootCommit(ctx, branch)
}

// apply runs after the exits and before the walk, on the driver goroutine.
func (e *MachineEvent) apply(m *Machine) error {
	r := m.r
	switch e.kind {
	case evMount:
		m.mountSpec = e.mount
	case evAttachOrigin:
		return applyAttach(r, e.origin)
	case evDetachOrigin:
		return applyDetach(r)
	case evSwapStore:
		// The walk opens what is on disk after the install, whichever way the
		// install went.
		m.mountSpec = MountSpec{Mode: MountExisting}
		return applySwap(m, e.swap)
	case evRebuild:
		m.indexSpec = indexSpec{full: true, branch: e.branch}
	}
	return nil
}

// applyAttach persists the origin and points the running store at it. The
// git remote is rewritten first because it is the fallible step; a failed
// control.db write puts it back, so a failed attach changes nothing. Sync is
// exited while this runs, so no loop reads a half-written origin.
func applyAttach(r *RepoInstance, o OriginSpec) error {
	origins := r.env.m.Origins()
	if origins == nil {
		return ErrManagerStopped
	}
	svc, release, err := r.Acquire()
	if err != nil {
		return err
	}
	defer release()
	existing, err := svc.Remote().GetRemote("origin")
	if err != nil {
		return err
	}
	// The upstream: the request, else the stored one, else the repo's own
	// consensus branch (recorded at init, kept across a detach). With none,
	// ConfigureRemote refuses.
	branch := o.Branch
	if branch == "" && existing != nil {
		branch = existing.Branch
	}
	if branch == "" {
		branch = svc.UpstreamBranch()
	}
	if err := svc.ConfigureRemote(o.URL, branch, r.agentBranch); err != nil {
		return err
	}
	// Carry the stored mode through: Origins.Set is a full replacement and an
	// EMPTY Mode deletes the subscription row.
	stored, err := origins.Get(r.uid)
	if err != nil {
		restoreRemote(svc, r, existing)
		return err
	}
	mode := OriginModeSync
	if stored != nil {
		mode = stored.Mode
	}
	if err := origins.Set(r.uid, Origin{URL: o.URL, Branch: branch, AuthMethod: o.AuthMethod, AuthToken: o.AuthToken, Mode: mode}); err != nil {
		restoreRemote(svc, r, existing)
		return err
	}
	svc.SetOrigin(&store.Origin{URL: o.URL, Branch: branch, AuthMethod: o.AuthMethod, AuthToken: o.AuthToken})
	return nil
}

// applyDetach drops the git remote, deletes the durable record and clears the
// injected origin, in that order: the record outlives every step that can
// fail, and a failed delete puts the git remote back.
func applyDetach(r *RepoInstance) error {
	origins := r.env.m.Origins()
	if origins == nil {
		return ErrManagerStopped
	}
	svc, release, err := r.Acquire()
	if err != nil {
		return err
	}
	defer release()
	existing, err := svc.Remote().GetRemote("origin")
	if err != nil {
		return err
	}
	if err := svc.Remote().DeleteRemote("origin"); err != nil {
		return err
	}
	if err := origins.Delete(r.uid); err != nil {
		restoreRemote(svc, r, existing)
		return err
	}
	svc.SetOrigin(nil)
	return nil
}

// restoreRemote puts the git remote back the way prev describes it after the
// control.db write that should have made a change durable failed. A failed
// restore is logged: the caller is already returning the real error.
func restoreRemote(svc *store.Service, r *RepoInstance, prev *store.Remote) {
	var err error
	if prev != nil && prev.URL != "" {
		err = svc.ConfigureRemote(prev.URL, prev.Branch, r.agentBranch)
	} else {
		err = svc.Remote().DeleteRemote("origin")
	}
	if err != nil {
		log.Error().Err(err).Str("repo", r.Name()).
			Msg("origin: failed to restore the git remote after a failed durable write")
	}
}

// applySwap installs the incoming database and its origin as one apply. Open
// has been exited, so the store is closed and drained.
//
// The irreversible step is LAST. In order: read the stored origin row, back up
// the current store, persist the new origin (reversible), then copy the new
// database over the old one. A failure before the copy changes nothing that is
// not undone here; a copy failure restores both the backup and the previous
// origin row. Persisting the origin after the copy would let a failed write
// leave the NEW store under the OLD origin: Open.Enter injects control.db's
// origin and Sync would push the new store's agent branch to the old remote.
//
// With no origin URL in the spec the stored origin stays as it is. The backup
// is deleted by the first successful Open after this.
func applySwap(m *Machine, s SwapSpec) error {
	r := m.r
	origins := r.env.m.Origins()
	var stored *Origin
	if s.Origin.URL != "" {
		if origins == nil {
			return fmt.Errorf("swap failed: save remote config: %w; nothing changed", ErrManagerStopped)
		}
		var err error
		if stored, err = origins.Get(r.uid); err != nil {
			return fmt.Errorf("swap failed: read remote config: %w; nothing changed", err)
		}
	}
	backup := r.dbPath + ".bak"
	if err := copyFile(r.dbPath, backup); err != nil {
		return fmt.Errorf("swap failed: back up the current store: %w; nothing changed", err)
	}
	m.pendingBak = backup
	if s.Origin.URL != "" {
		mode := OriginModeSync
		if stored != nil {
			mode = stored.Mode
		}
		o := s.Origin
		if err := origins.Set(r.uid, Origin{URL: o.URL, Branch: o.Branch, AuthMethod: o.AuthMethod, AuthToken: o.AuthToken, Mode: mode}); err != nil {
			return fmt.Errorf("swap failed: save remote config: %w; nothing changed", err)
		}
	}
	if err := swapCopy(s.TempDB, r.dbPath); err != nil {
		var msgs []string
		if rerr := copyFile(backup, r.dbPath); rerr != nil {
			msgs = append(msgs, fmt.Sprintf("restoring the previous store also failed: %v", rerr))
		}
		if s.Origin.URL != "" {
			if oerr := restoreOrigin(origins, r.uid, stored); oerr != nil {
				msgs = append(msgs, fmt.Sprintf("restoring the previous remote config also failed: %v", oerr))
			}
		}
		if len(msgs) > 0 {
			return fmt.Errorf("swap failed: %w; %s", err, strings.Join(msgs, "; "))
		}
		return fmt.Errorf("swap failed: %w; previous store and remote config restored", err)
	}
	return nil
}

// swapCopy is the swap's install copy; a test replaces it to fail the one
// irreversible step.
var swapCopy = copyFile

// restoreOrigin puts uid's origin row back to stored: Set it again, or Delete
// the row when there was none.
func restoreOrigin(origins *Origins, uid string, stored *Origin) error {
	if stored == nil {
		return origins.Delete(uid)
	}
	return origins.Set(uid, *stored)
}

// copyFile copies src to dst, creating dst if needed.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
