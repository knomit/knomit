package repos

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/store"
)

// remoteAuthFn returns a transport.AuthMethod for the given remote record, or
// an error if auth RESOLUTION fails (e.g. an unreadable/missing SSH key or a
// malformed credential). It is constructed by the builder and captures the key
// path and fallback config.
//
// A nil AuthMethod with a nil error is the legitimate anonymous case
// (auth_method "none"/empty) and MUST still sync normally. A non-nil error, by
// contrast, means the configured credential could not be resolved — callers
// must treat that tick as a sync FAILURE and surface it, rather than silently
// downgrading to anonymous (which would mask a broken credential against a
// remote that happens to permit anonymous access).
type remoteAuthFn func(remote *store.Remote) (transport.AuthMethod, error)

// makeRemoteAuthFn builds a remoteAuthFn that resolves auth from a remote
// record using the given fallback config and key path.
func makeRemoteAuthFn(fallbackAuth config.RemoteAuthConfig, keyPath string) remoteAuthFn {
	return func(remote *store.Remote) (transport.AuthMethod, error) {
		authCfg := remoteAuthFromRecord(remote, fallbackAuth)
		auth, err := resolveAuthWithOrigin(authCfg, keyPath, remote.URL)
		if err != nil {
			// Do NOT downgrade to anonymous here: a configured-but-unresolvable
			// credential must fail visibly so the reconcile tick records the
			// error and counts toward escalation, rather than silently syncing
			// anonymously against a remote that permits it.
			log.Warn().Err(err).Str("remote", remote.URL).Msg("sync: auth resolution failed")
			return nil, err
		}
		return auth, nil
	}
}

// reconcileFailureEscalateThreshold is the number of consecutive sync- or
// push-failures after which the loop escalates a log line from Warn to
// Error. A repository stuck in permanent failure (revoked credentials,
// unreachable origin, etc.) otherwise looks identical to a healthy one
// after log rotation drops the early-tick warnings.
const reconcileFailureEscalateThreshold = 5

// pushAllowed reports whether the reconcile loop may push to origin. Read-only
// (demo) instances are pull-only: they keep fetching/fast-forwarding from
// origin but never push back.
func pushAllowed(readOnly bool) bool { return !readOnly }

// remoteStatusIsError reports whether a persisted remote status column
// (last_status / last_push_status) holds a failure. The column is NULL until
// the first attempt, so a nil pointer means "never ran", not "failing".
func remoteStatusIsError(status *string) bool { return status != nil && *status == "error" }

// syncChanged reports whether a sync tick actually moved either branch. The
// zero Mode ("") means the step did not run and is treated like ModeNoop.
// tickAbandoned reports whether this tick failed because the LOOP was
// cancelled, rather than because the remote said something.
//
// Such a tick established nothing, so it is not broadcast to clients and does
// not count toward failure escalation. Creating a repo against a remote ends
// with ActivateSync, which cancels this loop to restart it (builder.go); the
// loop starts with an immediate tick, so a fetch is routinely in flight when
// that lands. Reporting it put "sync failed — Sync: fetch: ...: context
// canceled" on the repo screen of a create that had just fully succeeded.
//
// CANCELLATION ONLY, and the error must be the cancellation itself. A tick that
// ran out of time, or a real refusal that happens to land as the loop is being
// torn down, is a genuine failure and is still reported — store.abandonedByCaller
// draws the same line for the persisted status, and the two must agree or a
// failure would be broadcast without being recorded, or the reverse.
func tickAbandoned(ctx context.Context, err error) bool {
	return err != nil &&
		errors.Is(ctx.Err(), context.Canceled) &&
		errors.Is(err, context.Canceled)
}

func syncChanged(result store.SyncResult) bool {
	moved := func(m store.Mode) bool { return m != store.ModeNoop && m != "" }
	return moved(result.Main.Mode) || moved(result.Agent.Mode)
}

// shouldBroadcastSyncOK reports whether a SUCCESSFUL sync tick must be pushed
// to SSE subscribers. Two reasons qualify:
//
//   - something actually moved (main or agent), which clients render as a
//     reconcile summary; or
//   - the previous persisted status was "error", making this tick the
//     error→ok RECOVERY EDGE.
//
// The recovery edge is NOT optional. A client's remote-error banner is raised
// by sync_error and lowered only by a clean remote event, so an outage that
// heals on a tick with nothing to pull — the normal case, since the outage
// itself is what stopped the traffic — would leave the banner standing for the
// life of the session even though the stored status already says "ok".
//
// The edge is read off the PERSISTED status rather than an in-memory failure
// counter so it survives a server restart, where the counter starts at zero
// but the recorded status is still "error".
func shouldBroadcastSyncOK(result store.SyncResult, wasFailing bool) bool {
	return syncChanged(result) || wasFailing
}

// shouldBroadcastPushOK is the push-side twin of shouldBroadcastSyncOK: a push
// that sent nothing is normally silent, but it must still be announced when it
// is the recovery edge out of a recorded push failure. A credential that starts
// working again typically has nothing left to send, so `pushed` alone would
// never clear the banner.
func shouldBroadcastPushOK(pushed, wasFailing bool) bool { return pushed || wasFailing }

// runReconcileLoop is the single background goroutine for origin sync.
// On each tick it: (1) calls Sync (fetch + reconcileMain + reconcileAgent),
// (2) calls Push (force-push agent if local advanced).
//
// Interval is min(sync, push) interval from the Remote record. Configured
// changes are picked up on the next tick (re-read from DB).
func runReconcileLoop(ctx context.Context, wg *sync.WaitGroup, svc *store.Service, hub *TaskHub, repo, agentBranch string, resolveAuth remoteAuthFn, localOriginRoot string, readOnly bool) {
	defer wg.Done()

	// Initial config read for logging context.
	remote, err := svc.Remote().GetRemote("origin")
	if err != nil {
		log.Error().Err(err).Str("repo", repo).Msg("reconcile loop: initial remote read failed; not starting")
		return
	}
	if remote == nil {
		return
	}
	lg := log.With().Str("repo", repo).Str("remote", remote.URL).Logger()
	lg.Info().Msg("reconcile loop started")

	var syncFails, pushFails int

	logFailure := func(count int) *zerolog.Event {
		if count >= reconcileFailureEscalateThreshold {
			return lg.Error().Int("consecutive_failures", count)
		}
		return lg.Warn().Int("consecutive_failures", count)
	}

	doTick := func(ctx context.Context) {
		// Read fresh remote record so resolveAuth picks up DB-stored auth.
		fresh, err := svc.Remote().GetRemote("origin")
		if err != nil {
			lg.Error().Err(err).Msg("reconcile tick: remote read failed; skipping tick")
			return
		}
		if fresh == nil {
			return
		}
		// Defense-in-depth at the fetch boundary: a local-path origin is fetched
		// only while it still satisfies the current local-origin policy. The URL
		// was gated when it was written, but the policy (or an in-root symlink)
		// can change afterwards — re-checking each tick stops the loop from
		// continuing to fetch a now-forbidden path off the server's disk.
		if verr := validateLocalOrigin(fresh.URL, localOriginRoot); verr != nil {
			// Recurs every tick while the policy forbids this origin; keep it at
			// Warn (not Error) so a persistently-misconfigured origin doesn't
			// drown real failures. The loop keeps running on purpose: if the
			// policy is loosened again, the next tick resumes syncing.
			lg.Warn().Err(verr).Msg("reconcile: origin blocked by local-origin policy; skipping tick")
			return
		}
		// Snapshot the PRE-tick persisted status: Sync/Push overwrite it before
		// they return, so the error→ok recovery edge is only visible from here.
		wasSyncFailing := remoteStatusIsError(fresh.LastStatus)
		wasPushFailing := remoteStatusIsError(fresh.LastPushStatus)

		auth, authErr := resolveAuth(fresh)
		if authErr != nil {
			// Auth RESOLUTION failed (unreadable key, malformed credential).
			// Treat this exactly like a Sync failure: persist the error on the
			// remote record, broadcast it, and count it toward escalation.
			// Fetching anonymously here would mask a broken credential against
			// a remote that permits anonymous access. We do NOT call Sync/Push
			// with nil auth on this tick.
			syncFails++
			if serr := svc.Remote().RecordSyncError("origin", authErr.Error()); serr != nil {
				lg.Warn().Err(serr).Msg("reconcile: failed to persist auth-resolution error")
			}
			hub.broadcastSyncError("origin", authErr.Error())
			logFailure(syncFails).Err(authErr).Msg("reconcile: auth resolution failed")
			return
		}

		// Sync first.
		syncResult, err := svc.Remote().Sync(ctx, agentBranch, auth)
		if tickAbandoned(ctx, err) {
			// Our own cancellation, not the remote's verdict. Say nothing, count
			// nothing, and do not go on to push — that would fail identically.
			lg.Debug().Err(err).Msg("reconcile: tick abandoned; loop is stopping")
			return
		}
		if err != nil {
			syncFails++
			hub.broadcastSyncError("origin", err.Error())
			logFailure(syncFails).Err(err).Msg("reconcile: sync failed")
		} else {
			if syncFails > 0 {
				lg.Info().Int("after_failures", syncFails).Msg("reconcile: sync recovered")
				syncFails = 0
			}
			if shouldBroadcastSyncOK(syncResult, wasSyncFailing) {
				hub.broadcastSyncOK("origin", syncResult)
			}
			if syncChanged(syncResult) {
				lg.Info().
					Str("main_mode", string(syncResult.Main.Mode)).
					Str("agent_mode", string(syncResult.Agent.Mode)).
					Int("agent_replayed_count", syncResult.Agent.NumReplayed).
					Str("agent_new_tip", syncResult.Agent.NewTip).
					Msg("reconcile: pulled changes")
			} else {
				lg.Debug().Msg("reconcile: sync up to date")
			}
		}

		// Then push (skipped in read-only / pull-only mode).
		if pushAllowed(readOnly) {
			pushResult, err := svc.Remote().Push(ctx, agentBranch, auth)
			if tickAbandoned(ctx, err) {
				lg.Debug().Err(err).Msg("reconcile: push abandoned; loop is stopping")
				return
			}
			if err != nil {
				pushFails++
				hub.broadcastPushError("origin", err.Error())
				logFailure(pushFails).Err(err).Msg("reconcile: push failed")
				return
			}
			if pushFails > 0 {
				lg.Info().Int("after_failures", pushFails).Msg("reconcile: push recovered")
				pushFails = 0
			}
			if shouldBroadcastPushOK(pushResult.Pushed, wasPushFailing) {
				hub.broadcastPushOK("origin")
			}
			if pushResult.Pushed {
				lg.Info().Str("branch", agentBranch).Msg("reconcile: pushed changes")
			} else {
				lg.Debug().Msg("reconcile: push up to date")
			}
		}
	}

	// Immediate first tick.
	doTick(ctx)

	for {
		// Re-read remote config every iteration to pick up interval changes.
		fresh, err := svc.Remote().GetRemote("origin")
		if err != nil {
			lg.Error().Err(err).Msg("reconcile loop stopped: remote read failed")
			return
		}
		if fresh == nil {
			lg.Info().Msg("reconcile loop stopped: remote disappeared")
			return
		}
		interval := fresh.Interval
		if fresh.PushInterval > 0 && fresh.PushInterval < interval {
			interval = fresh.PushInterval
		}
		if interval <= 0 {
			interval = 300
		}

		select {
		case <-ctx.Done():
			lg.Info().Msg("reconcile loop stopped")
			return
		case <-time.After(time.Duration(interval) * time.Second):
			doTick(ctx)
		}
	}
}
