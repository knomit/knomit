import { describe, it, expect } from 'vitest';
import {
  bootReducer, initialBootState, bootProgress, bootLabel,
  isIndeterminate, serverPhaseLabel,
} from './boot';

const at0 = () => initialBootState(0);
// A desktop page that loaded while its server was still booting. It STARTS at
// 'server' because the reducer is monotonic and 'server' sorts before
// 'connecting', so a state that began at 'connecting' can never be moved back
// to it — see the test pinning exactly that at the bottom of this file.
const atServer = () => initialBootState(0, 'server');

describe('bootProgress', () => {
  // Master/reviewer call, pinned: progress is a FRACTION PER PHASE, never an
  // index over a fixed total. The `branch` phase is conditional — it happens
  // only when the server did not embed the branch root — so a denominator that
  // counted it would peg a successful one-hop boot at 2/3 and make the bar lie
  // about the very path this work exists to create.
  it('reaches 1.0 on the fast path, which never passes through `branch`', () => {
    expect(bootProgress('connecting')).toBeLessThan(1);
    expect(bootProgress('opening')).toBeLessThan(1);
    // Straight from opening to done: no branch request happened.
    expect(bootProgress('done')).toBe(1);
  });

  it('increases monotonically through the phase order', () => {
    expect(bootProgress('server')).toBeLessThan(bootProgress('connecting'));
    expect(bootProgress('connecting')).toBeLessThan(bootProgress('opening'));
    expect(bootProgress('opening')).toBeLessThan(bootProgress('branch'));
    expect(bootProgress('branch')).toBeLessThan(bootProgress('done'));
  });

  // `server` is the desktop's own boot, which in this tier has no byte counts
  // to divide by — a first-launch model fetch takes minutes and nothing here
  // knows how much is left. A determinate width would be an invented number,
  // and inventing one is what the frozen 25% bar did before.
  it('marks only the desktop server phase indeterminate', () => {
    expect(isIndeterminate('server')).toBe(true);
    for (const p of ['connecting', 'opening', 'branch', 'done'] as const) {
      expect(isIndeterminate(p)).toBe(false);
    }
  });
});

describe('serverPhaseLabel', () => {
  it('names the phase the desktop reported', () => {
    expect(serverPhaseLabel('downloading-models')).toBe('Downloading models…');
    expect(serverPhaseLabel('installing-tools')).toBe('Installing command-line tools…');
    expect(serverPhaseLabel('starting-server')).toBe('Starting the server…');
  });

  // The desktop and this bundle ship independently, so a phase this map has
  // never heard of is a version skew, not a bug. Showing the raw slug would put
  // "starting-engine" in front of the user; a generic sentence is still true.
  it('falls back to a generic sentence for an unknown phase', () => {
    expect(serverPhaseLabel('some-future-phase')).toBe('Starting…');
    expect(serverPhaseLabel('')).toBe('Starting…');
  });
});

describe('bootReducer', () => {
  // Master/reviewer call, pinned: the reducer is MONOTONIC. A late onPhase
  // from a superseded attempt — a repo switched mid-boot, or a retry that lost
  // the race — must not drag a finished boot back to "connecting". Cheap to
  // get wrong and invisible when it happens.
  it('ignores a stale phase that would move backwards', () => {
    const done = bootReducer(at0(), { type: 'PHASE', phase: 'done' });
    expect(done.phase).toBe('done');

    const stale = bootReducer(done, { type: 'PHASE', phase: 'connecting' });
    expect(stale.phase).toBe('done');
    // Identity, not just equality: a no-op must not re-render the screen.
    expect(stale).toBe(done);

    const alsoStale = bootReducer(done, { type: 'PHASE', phase: 'opening' });
    expect(alsoStale.phase).toBe('done');
  });

  it('still records a target learned late, even on a stale phase', () => {
    const done = bootReducer(at0(), { type: 'PHASE', phase: 'done' });
    const withTarget = bootReducer(done, { type: 'PHASE', phase: 'opening', target: 'alpha' });
    expect(withTarget.phase).toBe('done');
    expect(withTarget.target).toBe('alpha');
  });

  it('advances forwards and carries the target', () => {
    let s = at0();
    s = bootReducer(s, { type: 'PHASE', phase: 'opening', target: 'alpha' });
    expect(s.phase).toBe('opening');
    expect(s.target).toBe('alpha');
    s = bootReducer(s, { type: 'PHASE', phase: 'branch' });
    expect(s.phase).toBe('branch');
    expect(s.target).toBe('alpha'); // not lost when the phase omits it
  });

  it('counts attempts and keeps the last error without failing the boot', () => {
    let s = at0();
    s = bootReducer(s, { type: 'ATTEMPT_FAILED', error: 'network' });
    s = bootReducer(s, { type: 'ATTEMPT_FAILED', error: 'still network' });
    expect(s.attempt).toBe(2);
    expect(s.lastError).toBe('still network');
    // Having errored is NOT having failed: a boot that recovers on attempt 3
    // must not render the terminal state.
    expect(s.failed).toBe(false);
  });

  it('FAILED is terminal and RETRY resets while keeping the target', () => {
    let s = bootReducer(at0(), { type: 'PHASE', phase: 'opening', target: 'alpha' });
    s = bootReducer(s, { type: 'FAILED', error: 'gave up' });
    expect(s.failed).toBe(true);
    expect(s.lastError).toBe('gave up');

    const retried = bootReducer(s, { type: 'RETRY', now: 500 });
    expect(retried.failed).toBe(false);
    expect(retried.attempt).toBe(0);
    expect(retried.lastError).toBe('');
    expect(retried.phase).toBe('connecting');
    expect(retried.startedAt).toBe(500);
    // The target survives: we are retrying the same thing.
    expect(retried.target).toBe('alpha');
  });
});

describe('bootLabel', () => {
  it('names the target so the user knows which thing is slow', () => {
    const s = bootReducer(at0(), { type: 'PHASE', phase: 'opening', target: 'alpha' });
    expect(bootLabel(s)).toBe('Opening alpha…');
    expect(bootLabel({ ...s, phase: 'branch' })).toBe('Reading alpha…');
    expect(bootLabel({ ...s, phase: 'connecting' })).toBe('Connecting…');
  });

  it('reads as a failure only when it is one', () => {
    const s = bootReducer(at0(), { type: 'PHASE', phase: 'opening', target: 'alpha' });
    expect(bootLabel({ ...s, failed: true })).toBe('Could not open alpha');
    expect(bootLabel({ ...at0(), failed: true })).toBe('Could not connect');
  });

  it('speaks the desktop server phase while waiting on it', () => {
    const s = bootReducer(atServer(), { type: 'SERVER_PHASE', serverPhase: 'downloading-models' });
    expect(bootLabel(s)).toBe('Downloading models…');
  });

  // A desktop boot that died never reached a repo, so "Could not open alpha" /
  // "Could not connect" would both be accounts of the wrong thing — nothing was
  // ever connected TO. The detail lives in lastError, rendered separately.
  it('blames the server, not the repo, when the desktop boot failed', () => {
    const s = bootReducer(atServer(), { type: 'SERVER_PHASE', serverPhase: 'downloading-models' });
    expect(bootLabel({ ...s, failed: true })).toBe('Could not start knomit');
  });
});

describe('bootReducer SERVER_PHASE', () => {
  it('enters the server phase and records what the desktop said', () => {
    const s = bootReducer(atServer(), { type: 'SERVER_PHASE', serverPhase: 'installing-tools' });
    expect(s.phase).toBe('server');
    expect(s.serverPhase).toBe('installing-tools');
  });

  // The poll is asynchronous, so its last answer can land after the server came
  // up and the app moved on to connecting/opening. Letting it back in would
  // replace a real repo label with a stale "Downloading models…".
  it('ignores a late server phase once the boot has moved on', () => {
    const connected = bootReducer(
      bootReducer(atServer(), { type: 'SERVER_PHASE', serverPhase: 'starting-server' }),
      { type: 'PHASE', phase: 'opening', target: 'alpha' },
    );
    const late = bootReducer(connected, { type: 'SERVER_PHASE', serverPhase: 'downloading-models' });
    expect(late).toBe(connected);
    expect(late.phase).toBe('opening');
  });

  // The trap this cost an hour to find, pinned so the next reader does not pay
  // it again. The monotonic guard makes SERVER_PHASE a NO-OP on a state that
  // started at 'connecting' — the browser's default — so a desktop page must be
  // constructed at 'server' (useBoot's initialPhase) or it will sit on
  // "Connecting…" for the whole download with every poll result dropped in
  // silence. Nothing throws; the screen is just wrong.
  it('is a NO-OP from a browser-default start, which is why the desktop starts at server', () => {
    const fromConnecting = bootReducer(at0(), { type: 'SERVER_PHASE', serverPhase: 'downloading-models' });
    expect(fromConnecting.phase).toBe('connecting');
    expect(fromConnecting.serverPhase).toBe('');

    const fromServer = bootReducer(atServer(), { type: 'SERVER_PHASE', serverPhase: 'downloading-models' });
    expect(fromServer.phase).toBe('server');
    expect(fromServer.serverPhase).toBe('downloading-models');
  });
});
