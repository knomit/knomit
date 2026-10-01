package repos

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog/log"

	"knomit/internal/config"
	"knomit/internal/fact"
	"knomit/internal/store"
)

// syncMode is how a sync loop follows the `sync` root attribute (F21 S2,
// fact.AttrSync). Each loop owns one, built when the loop starts, and uses it
// only from its own goroutine.
//
//   - refresh reads the setting at the tip of the repo's CONSENSUS branch
//     (svc.UpstreamBranch(), never a fixed name) — once before the loop's
//     first round and again before every wait — and publishes the push half
//     into ri.realtimePush. That atomic is the only thing ri.onCommit reads:
//     onCommit runs under the writer's branch lock and must never read the
//     store, so a change of the setting takes effect at the loop's next
//     iteration, not at the commit that made it.
//   - wait turns the loop's normal wait into the realtime pull interval under
//     `pull: realtime`. It is also the circuit breakers' base, so at N = 3 s
//     an open breaker skips its step for 6 s, then 12 s, doubling to the cap.
//   - clear drops the published flag when the loop exits, so an instance
//     whose loop is gone (or never started: local_reconcile_interval = 0)
//     never wakes a loop nobody runs.
//
// A SUBSCRIPTION (no agent branch) ignores the attribute entirely: push is
// moot, and pull is ignored by ruling (decision 4), so a public repo that
// turns realtime on does not make every subscriber poll it.
//
// A nil *syncMode is today's behaviour: no read, no flag, the base wait.
type syncMode struct {
	repo        string
	agentBranch string
	// pushOK is pushAllowed(readOnly, agentBranch): a read-only server never
	// publishes realtime push (it writes nothing of its own to push).
	pushOK   bool
	realtime time.Duration
	// read returns the ontology at the consensus branch's tip and that
	// branch's name (for the warnings).
	read   func(ctx context.Context) (data []byte, at string, err error)
	flag   *atomic.Bool // ri.realtimePush; nil-safe
	cur    fact.SyncSettings
	warned map[string]bool
}

// newSyncMode builds the mode for one loop over svc. realtime is
// [git].realtime_pull_interval; a value under the 1 s floor (a Config built
// without Defaults, which Validate would have refused at boot) reads as the
// 3 s default rather than as a hot loop.
func newSyncMode(svc *store.Service, repo, agentBranch string, readOnly bool, realtime time.Duration, flag *atomic.Bool) *syncMode {
	if realtime < config.MinRealtimePullInterval {
		realtime = config.DefaultRealtimePullInterval
	}
	return &syncMode{
		repo:        repo,
		agentBranch: agentBranch,
		pushOK:      pushAllowed(readOnly, agentBranch),
		realtime:    realtime,
		read: func(ctx context.Context) ([]byte, string, error) {
			at := svc.UpstreamBranch()
			data, err := svc.OntologyAt(ctx, at)
			return data, at, err
		},
		flag:   flag,
		cur:    fact.SyncAbsent(),
		warned: map[string]bool{},
	}
}

// refresh re-reads the setting and publishes the push half. Anything it
// cannot read — no consensus branch yet, an unreadable ontology, a value this
// binary does not know — is today's behaviour; the last two are warned once
// per ontology blob.
func (m *syncMode) refresh(ctx context.Context) fact.SyncSettings {
	if m == nil {
		return fact.SyncAbsent()
	}
	next := m.readSettings(ctx)
	if next.Push != m.cur.Push || next.Pull != m.cur.Pull {
		lg := log.Info().Str("repo", m.repo).Str("push", next.Push).Str("pull", next.Pull)
		if next.RealtimePull() {
			lg = lg.Dur("realtime_pull_interval", m.realtime)
		}
		lg.Msg("sync mode")
	}
	m.cur = next
	if m.flag != nil {
		m.flag.Store(m.pushOK && next.RealtimePush())
	}
	return next
}

func (m *syncMode) readSettings(ctx context.Context) fact.SyncSettings {
	if m.agentBranch == "" || m.read == nil {
		return fact.SyncAbsent() // a subscription ignores `sync`
	}
	data, at, err := m.read(ctx)
	if err != nil {
		if errors.Is(err, store.ErrBranchNotFound) || ctx.Err() != nil {
			return fact.SyncAbsent() // no consensus branch yet, or stopping
		}
		m.warnOnce("ontology:"+err.Error(), fmt.Sprintf("sync: read the ontology at %s: %v; read as interval", at, err))
		return fact.SyncAbsent()
	}
	if data == nil {
		return fact.SyncAbsent()
	}
	s, err := fact.ReadSync(data)
	if err != nil {
		m.warnOnce("ontology:"+blobKey(data), fmt.Sprintf("sync: the ontology at %s is unreadable (%v); read as interval", at, err))
		return fact.SyncAbsent()
	}
	if !s.Valid {
		m.warnOnce("value:"+blobKey(data), fmt.Sprintf(
			"sync: unknown value %v at %s (this knomit knows push and pull, each \"realtime\" or \"interval\"); read as interval", s.Raw, at))
		return fact.SyncAbsent()
	}
	return s
}

func (m *syncMode) warnOnce(key, msg string) {
	if m.warned[key] {
		return
	}
	m.warned[key] = true
	log.Warn().Str("repo", m.repo).Msg(msg)
}

// wait is the loop's wait for the last refreshed setting: the realtime pull
// interval under `pull: realtime`, otherwise base.
func (m *syncMode) wait(base time.Duration) time.Duration {
	if m != nil && m.cur.RealtimePull() {
		return m.realtime
	}
	return base
}

// clear drops the published push flag (the loop is exiting).
func (m *syncMode) clear() {
	if m != nil && m.flag != nil {
		m.flag.Store(false)
	}
}

// loopWait is the timer a loop's select waits on: time.After, or the test's
// recording seam (syncHooks.wait).
func loopWait(d time.Duration) <-chan time.Time {
	if h := currentSyncHooks().wait; h != nil {
		return h(d)
	}
	return time.After(d)
}
