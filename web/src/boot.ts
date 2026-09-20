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
//
// `server` is DESKTOP-ONLY and comes before everything: there, the UI is served
// by a process that is still booting the API behind it, and on a first launch
// that takes minutes. In the browser the API is already up by definition, so
// boot starts at `connecting` exactly as it always did.
export const BOOT_PHASES = ['server', 'connecting', 'opening', 'branch', 'done'] as const;
export type BootPhaseName = (typeof BOOT_PHASES)[number];

export interface BootState {
  phase: BootPhaseName;
  // target is the repo or lens being opened, for the label. Empty until known.
  target: string;
  // serverPhase is the desktop's own boot phase (tools/desktop/serverboot.go),
  // meaningful only while phase === 'server'. Kept as a raw string, not a
  // union: it crosses a process boundary, and an older UI paired with a newer
  // desktop must degrade to a generic label rather than fail to render.
  serverPhase: string;
  attempt: number;
  lastError: string;
  // failed is terminal: the boot was abandoned or the answer was not
  // retryable. It is NOT the same as lastError being set — a boot that
  // recovers on attempt 3 had errors and did not fail.
  failed: boolean;
  startedAt: number;
}

export type BootAction =
  | { type: 'PHASE'; phase: BootPhaseName; target?: string }
  | { type: 'SERVER_PHASE'; serverPhase: string }
  | { type: 'ATTEMPT_FAILED'; error: string }
  | { type: 'FAILED'; error: string }
  | { type: 'RETRY'; now: number };

// initialBootState starts at `connecting` — the browser's world, and the
// desktop's too once its server is up.
//
// phase is a PARAMETER rather than always 'connecting' because the reducer is
// monotonic and `server` sorts BEFORE `connecting`: a boot that started at
// `connecting` can never be moved back to `server`, so a desktop page that
// loaded mid-boot has to START there or it would sit on "Connecting…" for the
// whole download with every SERVER_PHASE silently dropped. (It did, until a
// test caught it.)
export function initialBootState(now: number, phase: BootPhaseName = 'connecting'): BootState {
  return { phase, target: '', serverPhase: '', attempt: 0, lastError: '', failed: false, startedAt: now };
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
    case 'SERVER_PHASE':
      // Same no-walking-backwards rule, for the same reason: the poll is
      // asynchronous, so its last answer can land after the server came up and
      // the app moved on. Once we are past `server` the desktop's phase is
      // history and must not reclaim the screen.
      if (BOOT_PHASES.indexOf('server') < BOOT_PHASES.indexOf(s.phase)) return s;
      return { ...s, phase: 'server', serverPhase: a.serverPhase };
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
    case 'server': return 0;
    case 'connecting': return 0.25;
    case 'opening': return 0.6;
    case 'branch': return 0.85;
    case 'done': return 1;
    default: return 0;
  }
}

// isIndeterminate reports whether the bar should animate rather than report a
// width. True only for `server`, and the honesty matters: that phase has no
// denominator in this tier — a first-launch model fetch takes minutes and
// nothing here knows how many bytes are left — so a determinate bar would be
// inventing a number. Tier 2 adds real byte counts and this becomes false.
export function isIndeterminate(phase: BootPhaseName): boolean {
  return phase === 'server';
}

// SERVER_PHASE_LABELS maps the desktop's boot phases (the bootPhase constants
// in tools/desktop/serverboot.go) onto sentences. An unlisted value falls back
// to a generic label rather than rendering a raw slug at the user: the two
// sides ship independently, and a newer desktop must not make an older UI show
// "starting-engine".
const SERVER_PHASE_LABELS: Record<string, string> = {
  'starting': 'Starting…',
  'installing-tools': 'Installing command-line tools…',
  'downloading-models': 'Downloading models…',
  'starting-engine': 'Starting the search engine…',
  'starting-server': 'Starting the server…',
  'ready': 'Ready',
};

export function serverPhaseLabel(serverPhase: string): string {
  return SERVER_PHASE_LABELS[serverPhase] ?? 'Starting…';
}

// bootLabel is the human sentence for a phase. It names the target when there
// is one, because "Opening" alone does not tell the user which thing is slow.
export function bootLabel(s: BootState): string {
  // A desktop boot that failed has a real reason from the server process, and
  // "Could not connect" would be a worse account of it than what the phase was
  // when it died. lastError carries the detail, rendered separately.
  if (s.failed && s.phase === 'server') return 'Could not start knomit';
  if (s.failed) return s.target ? `Could not open ${s.target}` : 'Could not connect';
  switch (s.phase) {
    case 'server': return serverPhaseLabel(s.serverPhase);
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

// useBoot owns the boot state. initialPhase exists for the desktop, which knows
// from /config.js that it loaded while its server was still coming up and must
// therefore start at `server` — see initialBootState.
export function useBoot(initialPhase: BootPhaseName = 'connecting') {
  // Date.now() goes in the lazy initialiser, not a default parameter: a
  // default parameter is evaluated on EVERY render, which is both impure and
  // wasteful, and would reset startedAt on any re-render that remounted.
  //
  // initialPhase is read once, by design: it describes how this page LOADED,
  // which cannot change without a new page.
  const [state, dispatch] = useReducer(bootReducer, 0, () => initialBootState(Date.now(), initialPhase));
  const retry = useCallback(() => dispatch({ type: 'RETRY', now: Date.now() }), []);
  return { boot: state, dispatchBoot: dispatch, retryBoot: retry };
}
