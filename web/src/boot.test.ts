import { describe, it, expect } from 'vitest';
import { bootReducer, initialBootState, bootProgress, bootLabel } from './boot';

const at0 = () => initialBootState(0);

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
    expect(bootProgress('connecting')).toBeLessThan(bootProgress('opening'));
    expect(bootProgress('opening')).toBeLessThan(bootProgress('branch'));
    expect(bootProgress('branch')).toBeLessThan(bootProgress('done'));
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
});
