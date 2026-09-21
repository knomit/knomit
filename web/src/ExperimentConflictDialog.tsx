import { useEffect, useRef } from 'react';
import { createPortal } from 'react-dom';
import { btn } from './manageStyles';

// ExperimentConflictDialog is what a REFUSED COMMIT looks like.
//
// It is a modal rather than an inline notice because a refusal is not a
// message to read at leisure: the commit did not happen, nothing changed, and
// the next thing the user does is chosen by reading WHICH facts collided. An
// inline strip at the bottom of a panel loses that argument the moment the
// list scrolls.
//
// THE DIALOG OFFERS NO REPAIR, on purpose (user ruling 2026-09-20). Two
// earlier drafts did and both were wrong: "re-apply your edits on top" is
// advice this UI cannot help anyone follow, and a Sync button silently
// overwrites the experiment's version of exactly the facts under discussion.
// Resolution belongs where the three versions and the merge live — an agent
// does it through knomit_experiment. So this dialog REPORTS: which paths
// collided, that nothing changed, and who can resolve it. Discarding the
// experiment is the only thing this window can honestly finish.
interface Props {
  /** The experiment whose commit was refused. */
  name: string;
  /** Where the commit was trying to land — named, because "the agent branch" is not a name. */
  parent: string;
  /** The conflicting fact paths, exactly as the 409 listed them. */
  paths: string[];
  /** True while rollback is in flight; the action and Close are held. */
  busy?: boolean;
  onRollback: () => void;
  onClose: () => void;
}

export function ExperimentConflictDialog({ name, parent, paths, busy = false, onRollback, onClose }: Props) {
  const closeRef = useRef<HTMLButtonElement>(null);

  // Escape closes, and the close button takes focus on open. A refused commit
  // changed nothing, so dismissing is always safe — which is why Escape is
  // wired here and not on the destructive actions.
  useEffect(() => {
    closeRef.current?.focus();
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape' && !busy) onClose(); };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [busy, onClose]);

  return createPortal(
    <div
      data-testid="experiment-conflict-overlay"
      style={{
        position: 'fixed', inset: 0, zIndex: 10001,
        background: 'rgba(0,0,0,0.55)',
        display: 'flex', alignItems: 'center', justifyContent: 'center',
      }}
      onClick={() => { if (!busy) onClose(); }}
    >
      <div
        role="dialog" aria-modal="true" aria-labelledby="experiment-conflict-title"
        data-testid="experiment-conflict-dialog"
        onClick={e => e.stopPropagation()}
        style={{
          width: 600, maxWidth: '90vw', maxHeight: '80vh', overflowY: 'auto',
          background: '#1a1a1a', border: '1px solid #333', borderRadius: 8,
          boxShadow: '0 8px 24px rgba(0,0,0,0.6)', padding: '20px 22px',
          display: 'flex', flexDirection: 'column', gap: 14,
        }}
      >
        <div style={{ display: 'flex', alignItems: 'center', gap: 10 }}>
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="#e5a23c" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
            <path d="M12 9v4"/><path d="M12 17h.01"/>
            <path d="M10.3 3.9 1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"/>
          </svg>
          <h3 id="experiment-conflict-title" style={{ margin: 0, fontSize: 15, fontWeight: 600, color: '#eee' }}>
            Commit refused: {paths.length === 1 ? '1 fact' : `${paths.length} facts`} changed on both sides
          </h3>
        </div>

        <p style={{ margin: 0, fontSize: 12.5, lineHeight: 1.55, color: '#bbb' }}>
          Since <span style={{ color: '#7c9' }}>{name}</span> was forked,{' '}
          <span style={{ fontFamily: 'var(--k-font-mono, monospace)', color: '#8af' }}>{parent}</span>{' '}
          also changed {paths.length === 1 ? 'this fact' : 'these facts'}. Nothing was merged and nothing was changed.
        </p>

        <div data-testid="experiment-conflict-paths" style={{
          border: '1px solid #2a2a2a', borderRadius: 6, background: '#141414',
          padding: '8px 12px', display: 'flex', flexDirection: 'column', gap: 6,
          fontFamily: 'var(--k-font-mono, monospace)', fontSize: 11.5, color: '#ddd',
        }}>
          {paths.map(p => (
            <div key={p} style={{ display: 'flex', gap: 10, alignItems: 'baseline', minWidth: 0 }}>
              <span style={{ color: '#e5a23c', flexShrink: 0 }}>conflict</span>
              <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={p}>{p}</span>
            </div>
          ))}
        </div>

        <p style={{ margin: 0, fontSize: 12, lineHeight: 1.55, color: '#999' }}>
          Resolving {paths.length === 1 ? 'it' : 'them'} means choosing, per fact, between this
          experiment’s version and{' '}
          <span style={{ fontFamily: 'var(--k-font-mono, monospace)', color: '#8af' }}>{parent}</span>’s — an agent
          does that with <span style={{ fontFamily: 'var(--k-font-mono, monospace)', color: '#ddd' }}>knomit_experiment</span>,
          which can see all three versions. This window can only leave the experiment as it is, or
          discard it: <span style={{ color: '#ddd' }}>Roll back</span> deletes it and everything on it.
        </p>

        <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', alignItems: 'center' }}>
          <button ref={closeRef} data-testid="experiment-conflict-close" disabled={busy}
            onClick={onClose} style={btn(busy, 'secondary')}>
            Close
          </button>
          <button data-testid="experiment-conflict-rollback" disabled={busy}
            title="Delete the experiment and everything on it. This cannot be undone."
            onClick={onRollback} style={btn(busy, 'danger')}>
            Roll back
          </button>
        </div>
      </div>
    </div>,
    document.body,
  );
}
