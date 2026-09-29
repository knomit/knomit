import { useEffect, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { createPortal } from 'react-dom';
import { api } from './api';
import { btn } from './manageStyles';

// The two windows of merging a peer's pushed branch (F11 UI merge):
//
//   PushedMergeConfirm  — what the merge brings, pinned to the exact commit
//                         the list below describes. Merge sends THAT commit,
//                         so a push that lands while this is open is refused
//                         rather than merged unseen.
//   PushedMergeConflict — the merge was refused because the same facts
//                         changed on both sides. Each fact's two versions are
//                         one click away BEFORE anyone chooses a side, which is
//                         what separates this from the Sync the 2026-09-20
//                         ruling removed from experiments: that overwrote one
//                         side unseen. For a pushed branch there is no agent
//                         tool that could resolve it instead, so without this
//                         choice the branch could never be merged at all.

const mono: React.CSSProperties = { fontFamily: 'var(--k-font-mono, monospace)' };

function Modal({ testid, title, busy, onClose, children }: {
  testid: string; title: string; busy: boolean; onClose: () => void; children: ReactNode;
}) {
  // Nothing has changed while either window is open, so Escape and the
  // backdrop always close it safely — except mid-request.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape' && !busy) onClose(); };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [busy, onClose]);

  return createPortal(
    <div
      style={{
        position: 'fixed', inset: 0, zIndex: 10001, background: 'rgba(0,0,0,0.55)',
        display: 'flex', alignItems: 'center', justifyContent: 'center',
      }}
      onClick={() => { if (!busy) onClose(); }}
    >
      <div
        role="dialog" aria-modal="true" aria-labelledby={`${testid}-title`}
        data-testid={testid}
        onClick={e => e.stopPropagation()}
        style={{
          width: 620, maxWidth: '90vw', maxHeight: '80vh', overflowY: 'auto',
          background: '#1a1a1a', border: '1px solid #333', borderRadius: 8,
          boxShadow: '0 8px 24px rgba(0,0,0,0.6)', padding: '20px 22px',
          display: 'flex', flexDirection: 'column', gap: 14,
        }}
      >
        <h3 id={`${testid}-title`} style={{ margin: 0, fontSize: 15, fontWeight: 600, color: '#eee' }}>{title}</h3>
        {children}
      </div>
    </div>,
    document.body,
  );
}

const CHANGE_COLOR: Record<string, string> = { added: '#7c9', modified: '#8af', deleted: '#f88' };
const SHOWN = 20;

interface ConfirmProps {
  repo: string;
  /** The pushed branch, e.g. agent/<peer>. */
  branch: string;
  /** Where the merge lands: this repo's agent branch. */
  agentBranch: string;
  /** Common ancestor with the agent branch ("" if none): what "since your last merge" means. */
  mergeBase: string;
  otherFilesChanged: number;
  busy: boolean;
  /** Merge exactly this commit — the head the list was read at. */
  onConfirm: (expectedTip: string) => void;
  onClose: () => void;
}

export function PushedMergeConfirm({ repo, branch, agentBranch, mergeBase, otherFilesChanged, busy, onConfirm, onClose }: ConfirmProps) {
  const [changes, setChanges] = useState<{ head: string; changes: { path: string; change: string }[]; hasMore: boolean } | null>(null);
  const [error, setError] = useState('');
  const mergeRef = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    let live = true;
    api.branchChanges(repo, branch, mergeBase)
      .then(c => { if (live) { setChanges(c); mergeRef.current?.focus(); } })
      .catch(e => { if (live) setError(e instanceof Error ? e.message : String(e)); });
    return () => { live = false; };
  }, [repo, branch, mergeBase]);

  const counts = { added: 0, modified: 0, deleted: 0 } as Record<string, number>;
  for (const c of changes?.changes ?? []) counts[c.change] = (counts[c.change] ?? 0) + 1;
  const n = changes?.changes.length ?? 0;

  return (
    <Modal testid="pushed-merge-confirm" title={`Merge ${branch} into ${agentBranch}?`} busy={busy} onClose={onClose}>
      <p style={{ margin: 0, fontSize: 12.5, lineHeight: 1.55, color: '#bbb' }}>
        Facts changed on <span style={{ ...mono, color: '#ddd' }}>{branch}</span> since your last merge from it.
        A fact that already matches yours changes nothing.
      </p>

      {error && <div data-testid="pushed-merge-confirm-error" style={{ fontSize: 12, color: '#f88' }}>{error}</div>}
      {!changes && !error && <div style={{ fontSize: 12, color: '#666' }}>Loading…</div>}

      {changes && (
        <>
          <div data-testid="pushed-merge-counts" style={{ fontSize: 12, color: '#bbb' }}>
            {n === 0
              ? 'No fact changes.'
              : `${counts.added} added · ${counts.modified} modified · ${counts.deleted} deleted${changes.hasMore ? ' (first 100 shown)' : ''}`}
          </div>
          {n > 0 && (
            <div data-testid="pushed-merge-paths" style={{
              border: '1px solid #2a2a2a', borderRadius: 6, background: '#141414', padding: '8px 12px',
              display: 'flex', flexDirection: 'column', gap: 4, ...mono, fontSize: 11.5, color: '#ddd',
            }}>
              {changes.changes.slice(0, SHOWN).map(c => (
                <div key={c.path} style={{ display: 'flex', gap: 10, minWidth: 0 }}>
                  <span style={{ color: CHANGE_COLOR[c.change] ?? '#aaa', width: 64, flexShrink: 0 }}>{c.change}</span>
                  <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={c.path}>{c.path}</span>
                </div>
              ))}
              {n > SHOWN && <div style={{ color: '#777' }}>…and {n - SHOWN} more</div>}
            </div>
          )}
          <div data-testid="pushed-merge-other-files" style={{ fontSize: 12, color: '#999' }}>
            {otherFilesChanged > 0
              ? `It also changes ${otherFilesChanged} other file${otherFilesChanged === 1 ? '' : 's'} outside the knowledge base.`
              : 'Only facts are listed.'}
          </div>
        </>
      )}

      <p style={{ margin: 0, fontSize: 12, lineHeight: 1.55, color: '#999' }}>
        The merge is one commit on <span style={{ ...mono, color: '#ddd' }}>{agentBranch}</span>, signed by this
        instance. It reaches the origin on the next sync. <span style={{ ...mono }}>{branch}</span> itself is not changed.
      </p>

      <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end' }}>
        <button data-testid="pushed-merge-cancel" disabled={busy} onClick={onClose} style={btn(busy, 'secondary')}>Cancel</button>
        <button ref={mergeRef} data-testid="pushed-merge-submit" disabled={busy || !changes}
          onClick={() => changes && onConfirm(changes.head)} style={btn(busy || !changes, 'primary')}>
          Merge
        </button>
      </div>
    </Modal>
  );
}

interface ConflictProps {
  repo: string;
  branch: string;
  agentBranch: string;
  paths: string[];
  /** The agent branch's commit the merge was refused at. */
  hostCommit: string;
  /** The pushed branch's commit the merge was refused at. */
  peerCommit: string;
  busy: boolean;
  onResolve: (side: 'host' | 'peer') => void;
  onClose: () => void;
}

// FactVersion shows one side's version of a conflicting fact, fetched at the
// exact commit the refusal named — not the branch head, which may have moved.
function FactVersion({ repo, branch, path, commit, label }: { repo: string; branch: string; path: string; commit: string; label: string }) {
  const [body, setBody] = useState<string | null>(null);
  const [error, setError] = useState('');
  useEffect(() => {
    let live = true;
    api.fact(repo, branch, path, commit)
      .then(f => { if (live) setBody(f.body); })
      .catch(e => { if (live) setError(e instanceof Error ? e.message : String(e)); });
    return () => { live = false; };
  }, [repo, branch, path, commit]);
  return (
    <div style={{ minWidth: 0 }}>
      <div style={{ fontSize: 10, letterSpacing: '.06em', textTransform: 'uppercase', color: '#777', marginBottom: 4 }}>{label}</div>
      <pre style={{
        margin: 0, padding: '6px 8px', background: '#101010', border: '1px solid #2a2a2a', borderRadius: 4,
        ...mono, fontSize: 11, color: '#ccc', whiteSpace: 'pre-wrap', maxHeight: 180, overflowY: 'auto',
      }}>
        {error ? `(could not read: ${error})` : body ?? 'Loading…'}
      </pre>
    </div>
  );
}

export function PushedMergeConflict({ repo, branch, agentBranch, paths, hostCommit, peerCommit, busy, onResolve, onClose }: ConflictProps) {
  const [open, setOpen] = useState<string | null>(null);
  const peer = branch.replace(/^agent\//, '');

  return (
    <Modal testid="pushed-merge-conflict"
      title={`Merge refused: ${paths.length === 1 ? '1 fact' : `${paths.length} facts`} changed on both sides`}
      busy={busy} onClose={onClose}>
      <p style={{ margin: 0, fontSize: 12.5, lineHeight: 1.55, color: '#bbb' }}>
        <span style={{ ...mono, color: '#8af' }}>{agentBranch}</span> and{' '}
        <span style={{ ...mono, color: '#7c9' }}>{branch}</span> both changed{' '}
        {paths.length === 1 ? 'this fact' : 'these facts'}. Nothing was merged and nothing was changed.
        Look at both versions, then choose which one to keep for all of them.
      </p>

      <div data-testid="pushed-merge-conflict-paths" style={{
        border: '1px solid #2a2a2a', borderRadius: 6, background: '#141414', padding: '8px 12px',
        display: 'flex', flexDirection: 'column', gap: 8,
      }}>
        {paths.map(p => (
          <div key={p} data-testid={`pushed-merge-conflict-${p}`}>
            <div style={{ display: 'flex', gap: 10, alignItems: 'baseline', minWidth: 0, ...mono, fontSize: 11.5 }}>
              <span style={{ color: '#e5a23c', flexShrink: 0 }}>conflict</span>
              <span style={{ color: '#ddd', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', flex: 1 }} title={p}>{p}</span>
              <button data-testid={`pushed-merge-versions-${p}`}
                onClick={() => setOpen(open === p ? null : p)}
                style={{ ...btn(false, 'secondary'), flexShrink: 0 }}>
                {open === p ? 'Hide versions' : 'Show both versions'}
              </button>
            </div>
            {open === p && (
              <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 8, marginTop: 6 }}>
                <FactVersion repo={repo} branch={agentBranch} path={p} commit={hostCommit} label="This host's version" />
                <FactVersion repo={repo} branch={branch} path={p} commit={peerCommit} label={`${peer}'s version`} />
              </div>
            )}
          </div>
        ))}
      </div>

      <div style={{ display: 'flex', gap: 8, justifyContent: 'flex-end', flexWrap: 'wrap' }}>
        <button data-testid="pushed-merge-conflict-cancel" disabled={busy} onClick={onClose} style={btn(busy, 'secondary')}>Cancel</button>
        <button data-testid="pushed-merge-keep-host" disabled={busy}
          title={`Merge, keeping ${agentBranch}'s version of every fact listed`}
          onClick={() => onResolve('host')} style={btn(busy, 'secondary')}>
          Keep this host's version
        </button>
        <button data-testid="pushed-merge-take-peer" disabled={busy}
          title={`Merge, taking ${branch}'s version of every fact listed`}
          onClick={() => onResolve('peer')} style={btn(busy, 'primary')}>
          Take {peer}'s version
        </button>
      </div>
    </Modal>
  );
}
