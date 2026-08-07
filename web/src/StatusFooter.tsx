import { memo } from 'react';
import type { AppState, AsOf } from './state';
import { selectTrail } from './state';

interface Props {
  state: AppState;
  /** Running server build version (full string), or null until it resolves. */
  version?: string | null;
  /** Which key the dashboard's free-text search answers to right now: 'open'
   *  while it is closed and `/` would open it, 'close' while it is open and
   *  `esc` would put it away, absent wherever the bar is showing instead. */
  searchHint?: 'open' | 'close' | null;
}

// The mode is signalled by the dot color alone (green = live HEAD, amber =
// history/diff); `label` is the dot's accessible name, `descriptor` the
// adjacent commit text.
function pillContent(asOf: AsOf): { color: string; label: string; descriptor: string; glow: boolean } {
  switch (asOf.mode) {
    case 'live':
      return { color: '#7c9', label: 'LIVE', descriptor: 'HEAD', glow: true };
    case 'history':
      return { color: '#e5a23c', label: 'HISTORY', descriptor: asOf.commit.slice(0, 7), glow: false };
    case 'diff':
      return { color: '#e5a23c', label: 'DIFF', descriptor: `${asOf.from.slice(0, 7)}..${asOf.to.slice(0, 7)}`, glow: false };
  }
}

// Muted build-version tag.
function VersionTag({ version }: { version?: string | null }) {
  if (!version) return null;
  return (
    <span
      data-testid="version-badge"
      title="knomit build version"
      style={{
        color: '#5a5a65', fontFamily: 'var(--k-font-mono)', fontSize: 10,
        whiteSpace: 'nowrap', userSelect: 'none',
      }}
    >
      v{version}
    </span>
  );
}

function Kbd({ children }: { children: string }) {
  return (
    <span style={{
      color: '#a0a0a8', background: '#16161b', padding: '0 4px',
      borderRadius: 2, border: '1px solid #1f1f26',
      fontFamily: 'var(--k-font-mono)', fontSize: 10,
    }}>{children}</span>
  );
}

/**
 * StatusFooter is the app's bottom rail: where you are in time, what long
 * operation is running, and which build is serving you.
 *
 * It used to be the COLLAPSED half of an expandable console — clicking it
 * opened a 500-line ring buffer of log entries. The panel is gone (nothing that
 * belonged in front of a user was ever routed to it; see the note on
 * `dispatch` in App.tsx for where those lines go now), so this is no longer a
 * control: no click target, no chevron, no entry counts. It reads and reports.
 *
 * Memoized on `state`, which is why it takes the whole AppState rather than the
 * three slices it uses — App re-renders on every reducer action, and the
 * identity of `state` is already the correct staleness signal.
 */
export const StatusFooter = memo(function StatusFooter({ state, version, searchHint = null }: Props) {
  const p = pillContent(state.asOf);
  const trailHops = selectTrail(state).length - 1; // number of hops (N)

  // Anchored covers history AND diff: both already name their commits in the
  // descriptor, so neither wants head printed next to them.
  const anchored = state.asOf.mode !== 'live';

  // Highest-priority active task: a running one outranks an errored one, and
  // anything outranks idle.
  let activeTask: { op: string; status: string; message: string } | null = null;
  for (const [op, t] of Object.entries(state.tasks)) {
    if (t.status === 'idle') continue;
    if (!activeTask || t.status === 'running' || (t.status === 'error' && activeTask.status !== 'running')) {
      activeTask = { op, ...t };
    }
  }

  return (
    <div
      data-testid="status-footer"
      style={{
        height: 26, background: '#0b0b0d', borderTop: '1px solid #1f1f26',
        display: 'flex', alignItems: 'center', padding: '0 14px', gap: 10,
        flexShrink: 0, userSelect: 'none',
        fontFamily: 'var(--k-font-body)', fontSize: 11,
      }}
    >
      <span style={{
        flex: '0 0 auto', display: 'flex', alignItems: 'center', gap: 8,
      }}>
        <span
          data-testid="footer-mode"
          role="img"
          aria-label={p.label}
          title={p.label}
          style={{
            width: 6, height: 6, borderRadius: '50%', background: p.color,
            boxShadow: p.glow ? `0 0 6px ${p.color}` : 'none',
          }}
        />
        <span style={{ color: '#a0a0a8', fontFamily: 'var(--k-font-mono)', fontSize: 10 }}>
          {p.descriptor}
        </span>
        {state.asOf.mode === 'history' && (
          <span style={{ color: '#a0a0a8', fontFamily: 'var(--k-font-mono)', fontSize: 10 }}>
            · read-only
          </span>
        )}
        {state.asOf.mode === 'history' && trailHops >= 1 && (
          <span style={{ color: '#e5a23c', fontFamily: 'var(--k-font-mono)', fontSize: 10 }}>
            trail {trailHops} deep
          </span>
        )}

        {/* Head, and ONLY while live. The commit used to sit in the top bar,
            which is now controls only — and in lens context it rendered there
            just when a fact was open AND the view was anchored, so ordinary
            reading showed no commit anywhere.
            Anchored modes already print theirs: the descriptor above is the
            commit in history and from..to in diff, and only `live` is a bare
            word. Adding head beside those put the same seven characters on
            the rail twice. */}
        {!anchored && state.headCommit && (
          <span
            data-testid="footer-commit"
            title={`Head is ${state.headCommit}`}
            style={{ color: '#5a5a65', fontFamily: 'var(--k-font-mono)', fontSize: 10 }}
          >{state.headCommit.slice(0, 7)}</span>
        )}

        {/* Where writes land, in lens context only — in a repo you write to the
            repo you are browsing and saying so is noise. It is a readout (the
            lens sets it, not the reader), which is why it is down here, but it
            keeps its box: everything else on this rail is something you glance
            past, and this is the last thing between the reader and a fact
            landing in the wrong repo. Shown while anchored too — read-only is
            temporary, and dropping it would make it flicker on every scrub. */}
        {state.context.kind === 'lens' && state.lens && (
          <span
            data-testid="footer-writes"
            title={`Writes go to ${state.lens.write}`}
            style={{
              display: 'inline-flex', alignItems: 'center', gap: 4,
              color: '#7c9', background: '#1a2e1a', border: '1px solid #2a4a2a',
              borderRadius: 3, padding: '0 6px',
              fontFamily: 'var(--k-font-mono)', fontSize: 10,
            }}
          >✎ {state.lens.write}</span>
        )}
      </span>

      <span style={{ color: '#1f1f26', flex: '0 0 auto' }}>│</span>

      <span style={{
        flex: '1 1 auto', minWidth: 0, display: 'flex', alignItems: 'center', gap: 10,
        overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
      }}>
        {activeTask && (
          <span data-testid="footer-task" style={{
            color: '#8af',
            fontFamily: 'var(--k-font-mono)',
            overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap',
            minWidth: 0,
          }}>[{activeTask.op}] {activeTask.message}</span>
        )}
      </span>

      <VersionTag version={version} />

      <span style={{
        flex: '0 0 auto', display: 'flex', alignItems: 'center', gap: 8,
        fontFamily: 'var(--k-font-mono)', fontSize: 10, color: '#5a5a65',
      }}>
        <Kbd>h</Kbd> now
        {/* The rail says what the key does NOW, not what it did a moment ago:
            `/` while the search is closed, `esc` while it is open. */}
        {searchHint === 'open' && <><Kbd>/</Kbd> search</>}
        {searchHint === 'close' && <span style={{ color: '#8b95a6', display: 'flex', alignItems: 'center', gap: 6 }}><Kbd>esc</Kbd> close</span>}
      </span>
    </div>
  );
});
