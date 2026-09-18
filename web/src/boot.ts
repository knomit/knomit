// Boot state: what the app is waiting on before it can render anything.
//
// Kept out of App so BootScreen is a pure function of it and testable without
// mounting the whole application. App derives the transitions from signals it
// already has (reposLoaded, state.repo/state.lens, state.branch) plus the
// bootstrap's onPhase/onAttemptFailed callbacks.

import { useReducer, useCallback } from 'react';
import type { BootPhase } from './bootstrap';

// The ordered phases a cold boot passes through. `branch` is CONDITIONAL: it
// happens only when the server did not embed the branch root, so the bar must
// not reserve a slot for it up front — a progress bar that always stops short
// of full on the fast path is a bar that lies about the fast path.
export const BOOT_PHASES = ['connecting', 'opening', 'branch', 'done'] as const;
export type BootPhaseName = (typeof BOOT_PHASES)[number];

export interface BootState {
  phase: BootPhaseName;
  // target is the repo or lens being opened, for the label. Empty until known.
  target: string;
  attempt: number;
  lastError: string;
  // failed is terminal: the backoff table was exhausted or the answer was not
  // retryable. It is NOT the same as lastError being set — a boot that
  // recovers on attempt 3 had errors and did not fail.
  failed: boolean;
  startedAt: number;
}

export type BootAction =
  | { type: 'PHASE'; phase: BootPhaseName; target?: string }
  | { type: 'ATTEMPT_FAILED'; error: string }
  | { type: 'FAILED'; error: string }
  | { type: 'RETRY'; now: number };

export function initialBootState(now: number): BootState {
  return { phase: 'connecting', target: '', attempt: 0, lastError: '', failed: false, startedAt: now };
}

export function bootReducer(s: BootState, a: BootAction): BootState {
  switch (a.type) {
    case 'PHASE':
      // Never walk backwards: a late callback from a superseded attempt must
      // not drag a finished boot back to "connecting".
      if (BOOT_PHASES.indexOf(a.phase) < BOOT_PHASES.indexOf(s.phase)) {
        return a.target && a.target !== s.target ? { ...s, target: a.target } : s;
      }
      return { ...s, phase: a.phase, target: a.target ?? s.target };
    case 'ATTEMPT_FAILED':
      return { ...s, attempt: s.attempt + 1, lastError: a.error };
    case 'FAILED':
      return { ...s, failed: true, lastError: a.error };
    case 'RETRY':
      return { ...initialBootState(a.now), target: s.target };
    default:
      return s;
  }
}

// bootProgress is the fraction of the way through boot, for the bar.
//
// The denominator is the phases that will ACTUALLY happen, which is not known
// until we learn whether the branch root was embedded: on the one-hop path
// `branch` never occurs, so counting it would peg a successful fast boot at
// 2/3 forever. Once `done` is reached the answer is 1 either way.
export function bootProgress(phase: BootPhaseName): number {
  switch (phase) {
    case 'connecting': return 0.25;
    case 'opening': return 0.6;
    case 'branch': return 0.85;
    case 'done': return 1;
    default: return 0;
  }
}

// bootLabel is the human sentence for a phase. It names the target when there
// is one, because "Opening" alone does not tell the user which thing is slow.
export function bootLabel(s: BootState): string {
  if (s.failed) return s.target ? `Could not open ${s.target}` : 'Could not connect';
  switch (s.phase) {
    case 'connecting': return 'Connecting…';
    case 'opening': return s.target ? `Opening ${s.target}…` : 'Opening…';
    case 'branch': return s.target ? `Reading ${s.target}…` : 'Reading branch…';
    case 'done': return 'Ready';
    default: return 'Loading…';
  }
}

// phaseFromBootstrap maps the bootstrap helper's phase onto the boot phase
// vocabulary. They are deliberately separate types: the helper knows about
// requests, this knows about what the user is told.
export function phaseFromBootstrap(p: BootPhase): BootPhaseName {
  return p;
}

export function useBoot() {
  // Date.now() goes in the lazy initialiser, not a default parameter: a
  // default parameter is evaluated on EVERY render, which is both impure and
  // wasteful, and would reset startedAt on any re-render that remounted.
  const [state, dispatch] = useReducer(bootReducer, 0, () => initialBootState(Date.now()));
  const retry = useCallback(() => dispatch({ type: 'RETRY', now: Date.now() }), []);
  return { boot: state, dispatchBoot: dispatch, retryBoot: retry };
}
