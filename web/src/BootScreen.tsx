// BootScreen replaces the bare centred "Loading…" the app showed until repo
// AND branch were known — a screen that said nothing about what it was waiting
// on, showed no retry feedback, and looked identical to a hung backend.
//
// It is a pure function of BootState so it can be tested without App.

import { useEffect, useState } from 'react';
import type { BootState } from './boot';
import { bootLabel, bootProgress, isIndeterminate } from './boot';

// Elapsed appears only after this long. Below it the number is noise: every
// healthy boot would flash a counter that says nothing.
const ELAPSED_AFTER_MS = 2000;

export interface BootScreenProps {
  boot: BootState;
  onRetry: () => void;
  // Injectable for tests; production reads the wall clock.
  now?: () => number;
}

export function BootScreen({ boot, onRetry, now = Date.now }: BootScreenProps) {
  const [elapsedMs, setElapsedMs] = useState(0);

  useEffect(() => {
    const tick = () => setElapsedMs(now() - boot.startedAt);
    tick();
    const id = setInterval(tick, 1000);
    return () => clearInterval(id);
  }, [boot.startedAt, now]);

  const showElapsed = elapsedMs >= ELAPSED_AFTER_MS;
  const pct = Math.round(bootProgress(boot.phase) * 100);
  // An indeterminate phase has no width to report, so the bar sweeps instead.
  // The keyframe matches the pre-React splash in index.html (same 220x2 track,
  // same 40% sweep) so the handover from splash to boot screen shows no seam —
  // on the desktop those two now cover consecutive halves of the same wait.
  const indeterminate = isIndeterminate(boot.phase) && !boot.failed;

  return (
    <div
      data-testid="boot-screen"
      style={{
        position: 'fixed', inset: 0, display: 'flex', flexDirection: 'column',
        alignItems: 'center', justifyContent: 'center', gap: 16,
        background: '#141414', color: '#e6e6e6',
        font: '14px/1.5 system-ui, -apple-system, Segoe UI, sans-serif',
      }}
    >
      {/* Kept OUT of the bar element: the bar's first child is its fill, and
          tests (and any future reader) read it as such. A <style> tag smuggled
          in there is an invisible extra child that breaks that reading. */}
      <style>{'@keyframes knomit-boot-sweep{0%{transform:translateX(-100%)}100%{transform:translateX(350%)}}'}</style>

      <div style={{ fontSize: 18, letterSpacing: '0.02em' }}>knomit</div>

      {/* The bar is determinate: it reports the phase, not a guess at time.
          It sits ABOVE the label deliberately. The label's text changes with
          every phase and with the elapsed counter appearing, and anything
          below a changing element gets pushed around by it — so the two fixed
          things, the wordmark and the bar, go first and stay put. */}
      <div
        data-testid="boot-bar"
        role="progressbar"
        // An indeterminate progressbar omits aria-valuenow entirely — that is
        // what tells a screen reader the length is unknown. Reporting 0 would
        // claim "no progress yet", which is a different and wrong statement.
        aria-valuenow={indeterminate ? undefined : pct}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-label="Startup progress"
        style={{ width: 220, height: 2, background: '#2a2a2a', overflow: 'hidden' }}
      >
        <div
          data-testid="boot-bar-fill"
          data-indeterminate={indeterminate ? 'true' : undefined}
          style={{
            width: indeterminate ? '40%' : `${pct}%`, height: '100%',
            // Warning styling is reserved for failures, so the bar stays
            // neutral while it is merely slow.
            background: boot.failed ? '#b45454' : '#4a7c9b',
            ...(indeterminate
              ? { animation: 'knomit-boot-sweep 1.1s ease-in-out infinite' }
              : { transition: 'width 200ms linear' }),
          }}
        />
      </div>

      {/* minHeight reserves the single line this always occupies. Without it
          the block collapses to zero before the first label and grows when
          the elapsed counter appears, which moves everything below it. */}
      <div
        data-testid="boot-phase"
        style={{
          color: boot.failed ? '#f0a2a2' : '#9a9a9a',
          minHeight: '1.5em',
          textAlign: 'center',
        }}
      >
        {bootLabel(boot)}
        {showElapsed && !boot.failed && (
          <span data-testid="boot-elapsed" style={{ color: '#6f6f6f' }}>
            {' '}· {Math.floor(elapsedMs / 1000)} s
          </span>
        )}
      </div>

      {/* Retry feedback, which used to reach only the Console. */}
      {!boot.failed && boot.attempt > 0 && (
        <div data-testid="boot-attempt" style={{ color: '#6f6f6f', maxWidth: 420, textAlign: 'center' }}>
          retrying (attempt {boot.attempt + 1}){boot.lastError ? `: ${boot.lastError}` : ''}
        </div>
      )}

      {boot.failed && (
        <>
          {boot.lastError && (
            <div data-testid="boot-error" style={{ color: '#9a9a9a', maxWidth: 420, textAlign: 'center' }}>
              {boot.lastError}
            </div>
          )}
          <button
            data-testid="boot-retry"
            onClick={onRetry}
            style={{
              padding: '6px 14px', background: '#2a2a2a', color: '#e6e6e6',
              border: '1px solid #3a3a3a', borderRadius: 4, cursor: 'pointer', font: 'inherit',
            }}
          >
            Retry
          </button>
        </>
      )}
    </div>
  );
}

export default BootScreen;
