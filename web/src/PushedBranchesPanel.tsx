import { useCallback, useEffect, useState } from 'react';
import { api, BranchMovedError, PushedMergeConflictError } from './api';
import type { PushedBranchRow } from './api';
import { card, cardLabel, btn } from './manageStyles';
import { relAge } from './experimentText';
import { PushedMergeConfirm, PushedMergeConflict } from './PushedMergeDialog';

// PushedBranchesPanel lists the branches peers pushed to this host (F11) and
// lets the host's operator merge one into this repo's agent branch. A human
// presses Merge; nothing here merges on its own (the user's ruling: a human
// does the librarian's job for now).
//
// The row shows what the merge would bring (N commits to merge, 0 once merged)
// and nothing about re-cloning: a merge made here is signed by this instance,
// and a re-clone trusts what it brought in (the chain-of-trust ruling), so
// there is nothing to warn about.

interface Props {
  repo: string;
  /** Where merges land: this repo's agent branch. */
  agentBranch: string;
  /** Switch the whole UI to a branch, to browse a pushed branch read-only. */
  onEnterBranch: (branch: string) => void;
}

const GRID = 'minmax(0, 2fr) minmax(0, 1.4fr) 110px 110px minmax(220px, auto)';

type Pending =
  | { kind: 'confirm'; row: PushedBranchRow }
  | { kind: 'conflict'; row: PushedBranchRow; tip: string; paths: string[]; hostCommit: string; peerCommit: string };

export function PushedBranchesPanel({ repo, agentBranch, onEnterBranch }: Props) {
  const [rows, setRows] = useState<PushedBranchRow[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState(false);
  const [pending, setPending] = useState<Pending | null>(null);

  const load = useCallback(async () => {
    try {
      setRows(await api.listPushedBranches(repo));
      setError('');
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoaded(true);
    }
  }, [repo]);

  // Every setState in `load` runs after its await, so none is synchronous in
  // this effect body — the same false positive ExperimentsPanel documents.
  // eslint-disable-next-line react-hooks/set-state-in-effect
  useEffect(() => { void load(); }, [load]);

  const merge = async (row: PushedBranchRow, tip: string, resolution?: 'host' | 'peer') => {
    setBusy(true);
    setError('');
    try {
      const res = await api.mergePushedBranch(repo, row.name, tip, resolution);
      setPending(null);
      setNotice(res.mode === 'noop'
        ? `${row.name} is already merged into ${agentBranch}.`
        : `Merged ${row.name} into ${agentBranch} (merge commit ${res.new_tip.slice(0, 7)}). It reaches the origin on the next sync.`);
      await load();
    } catch (e) {
      if (e instanceof PushedMergeConflictError) {
        setPending({ kind: 'conflict', row, tip, paths: e.paths, hostCommit: e.hostCommit, peerCommit: e.peerCommit });
      } else if (e instanceof BranchMovedError) {
        setPending(null);
        setNotice(`${row.name} moved since you opened it. Review it again.`);
        await load();
      } else {
        setPending(null);
        setError(e instanceof Error ? e.message : String(e));
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <div style={card} data-testid="pushed-branches-panel">
      <div style={cardLabel}>Pushed branches</div>

      <div style={{ fontSize: 12, color: '#8b9199', margin: '6px 0 10px', lineHeight: 1.5 }}>
        Branches other instances pushed to this one. Merging one brings its facts into{' '}
        <span style={{ fontFamily: 'var(--k-font-mono, monospace)', color: '#aaa' }}>{agentBranch}</span>.
      </div>

      {error && (
        <div data-testid="pushed-branches-error" style={{
          fontSize: 12, color: '#f88', background: '#2a1414',
          border: '1px solid #4a2222', borderRadius: 4, padding: '6px 8px', marginBottom: 10,
        }}>{error}</div>
      )}
      {notice && (
        <div data-testid="pushed-branches-notice" style={{
          fontSize: 12, color: '#9c9', background: '#122016',
          border: '1px solid #234a2c', borderRadius: 4, padding: '6px 8px', marginBottom: 10,
        }}>{notice}</div>
      )}

      {!loaded && <div style={{ fontSize: 12, color: '#666' }}>Loading…</div>}
      {loaded && rows.length === 0 && !error && (
        <div data-testid="pushed-branches-empty" style={{ fontSize: 12, color: '#666' }}>
          No instance has pushed a branch to this host.
        </div>
      )}

      {loaded && rows.length > 0 && (
        <div style={{ border: '1px solid #2a2a2a', borderRadius: 6, background: '#1a1a1a', overflow: 'hidden' }}>
          <div style={{
            display: 'grid', gridTemplateColumns: GRID, gap: 12, padding: '6px 14px',
            fontSize: 10, letterSpacing: '.08em', textTransform: 'uppercase', color: '#555', borderBottom: '1px solid #262626',
          }}>
            <span>Branch</span><span>Last commit by</span><span>Last commit</span><span>To merge</span><span />
          </div>
          {rows.map((row, i) => (
            <div key={row.name} data-testid={`pushed-row-${row.name}`} style={{
              display: 'grid', gridTemplateColumns: GRID, gap: 12, padding: '10px 14px', alignItems: 'center',
              borderBottom: i === rows.length - 1 ? 'none' : '1px solid #262626',
            }}>
              <span style={{ fontFamily: 'var(--k-font-mono, monospace)', fontSize: 12, color: '#ddd', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }} title={row.name}>
                {row.name}
              </span>
              <span style={{ fontSize: 12, color: '#bbb', overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{row.tip_author}</span>
              <span style={{ fontSize: 12, color: '#aaa' }}>{relAge(row.tip_time)}</span>
              <span data-testid={`pushed-tomerge-${row.name}`} style={{ fontSize: 12, color: row.to_merge > 0 ? '#e5c07b' : '#777' }}>
                {row.to_merge > 0 ? `${row.to_merge} commit${row.to_merge === 1 ? '' : 's'}` : 'merged'}
              </span>
              <div style={{ display: 'flex', gap: 6, justifyContent: 'flex-end', flexWrap: 'wrap' }}>
                <button data-testid={`pushed-browse-${row.name}`} disabled={busy}
                  onClick={() => onEnterBranch(row.name)} style={btn(busy, 'secondary')}>
                  Browse
                </button>
                <button data-testid={`pushed-merge-${row.name}`} disabled={busy || row.to_merge === 0}
                  title={row.to_merge === 0 ? 'nothing to merge' : `Merge into ${agentBranch}`}
                  onClick={() => { setNotice(''); setPending({ kind: 'confirm', row }); }}
                  style={btn(busy || row.to_merge === 0, 'primary')}>
                  Merge into {agentBranch}
                </button>
              </div>
            </div>
          ))}
        </div>
      )}

      {pending?.kind === 'confirm' && (
        <PushedMergeConfirm
          repo={repo} branch={pending.row.name} agentBranch={agentBranch}
          mergeBase={pending.row.merge_base ?? ''} otherFilesChanged={pending.row.other_files_changed}
          busy={busy}
          onConfirm={tip => void merge(pending.row, tip)}
          onClose={() => setPending(null)}
        />
      )}
      {pending?.kind === 'conflict' && (
        <PushedMergeConflict
          repo={repo} branch={pending.row.name} agentBranch={agentBranch}
          paths={pending.paths} hostCommit={pending.hostCommit} peerCommit={pending.peerCommit}
          busy={busy}
          onResolve={side => void merge(pending.row, pending.tip, side)}
          onClose={() => setPending(null)}
        />
      )}
    </div>
  );
}
