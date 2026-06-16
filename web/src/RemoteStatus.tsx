import { useEffect, useState } from 'react';
import { api, type OriginResponse } from './api';

interface Props {
  repo: string;
  agentBranch: string;     // this machine's local agent branch (for the upstream warning)
  readOnly: boolean;
  onConnect: () => void;   // open the connect wizard
  onChanged: () => void;   // remote changed (e.g. disconnected) — parent refresh
}

// RemoteStatus is the read-only remote panel in the Repo Manager detail pane.
// It never edits the remote inline — connecting/changing always goes through
// the wizard (onConnect); the only inline mutation is Disconnect.
export function RemoteStatus({ repo, agentBranch, readOnly, onConnect, onChanged }: Props) {
  const [origin, setOrigin] = useState<OriginResponse | null>(null);
  const [loading, setLoading] = useState(true);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState('');
  const [editingUpstream, setEditingUpstream] = useState(false);
  const [branchChoices, setBranchChoices] = useState<string[]>([]);
  const [newUpstream, setNewUpstream] = useState('');

  const loadOrigin = () => {
    let cancelled = false;
    setLoading(true); setErr(''); setConfirming(false); setEditingUpstream(false);
    api.getOrigin(repo)
      .then(o => { if (!cancelled) setOrigin(o); })
      .catch(() => { if (!cancelled) setErr('could not load remote status'); })
      .finally(() => { if (!cancelled) setLoading(false); });
    return () => { cancelled = true; };
  };
  useEffect(loadOrigin, [repo]);

  const disconnect = async () => {
    setErr(''); setBusy(true);
    try {
      await api.deleteOrigin(repo);
      setOrigin(null); setConfirming(false);
      onChanged();
    } catch (e) { setErr(String(e)); }
    finally { setBusy(false); }
  };

  // upstream == this machine's agent branch is a degenerate config: pulls are
  // disabled (push-only) to avoid force-resetting unpushed facts. Warn + offer
  // a one-click switch to a real consensus branch.
  const upstreamIsAgent = !!origin && !!agentBranch && origin.branch === agentBranch;

  const openUpstreamEditor = async () => {
    setErr('');
    setNewUpstream('main');
    setEditingUpstream(true);
    try {
      const names = await api.listBranchNames(repo);
      // Offer real consensus candidates first; never offer the agent branch itself.
      setBranchChoices(names.filter(n => n !== agentBranch));
    } catch { setBranchChoices([]); }
  };

  const saveUpstream = async () => {
    if (!newUpstream) return;
    setErr(''); setBusy(true);
    try {
      await api.setOriginUpstream(repo, newUpstream);
      setEditingUpstream(false);
      loadOrigin();
      onChanged();
    } catch (e) { setErr(String(e)); }
    finally { setBusy(false); }
  };

  return (
    <div style={{ marginTop: 20 }}>
      <div style={sectionLabel}>Remote</div>

      {loading && <div style={muted}>Loading…</div>}

      {!loading && !origin && (
        <>
          <div style={muted}>Not connected to a remote.</div>
          <button type="button" data-testid="remote-connect" style={btn(readOnly, 'primary')} disabled={readOnly} onClick={onConnect}>
            Connect a remote…
          </button>
        </>
      )}

      {!loading && origin && (
        <>
          <div style={{ fontSize: 13, color: '#ddd', wordBreak: 'break-all' }}>{origin.url}</div>
          <div style={{ fontSize: 12, color: upstreamIsAgent ? '#e0a23a' : '#888', marginTop: 2, display: 'flex', alignItems: 'center', gap: 6 }}>
            {upstreamIsAgent && <span data-testid="upstream-warning-icon" title="Upstream is this agent branch" aria-label="warning">⚠</span>}
            <span>upstream branch: {origin.branch || 'main'}</span>
            {!readOnly && !editingUpstream && (
              <button type="button" data-testid="upstream-change" style={linkBtn} onClick={openUpstreamEditor}>change…</button>
            )}
          </div>

          {upstreamIsAgent && !editingUpstream && (
            <div data-testid="upstream-warning" style={warnBox}>
              ⚠ The consensus (“main”) branch is set to this machine’s agent branch, so remote
              changes are <strong>not pulled</strong> — the repo is push-only to protect unpushed
              facts. Set a real consensus branch (e.g. <code>main</code>) to re-enable pulls.
            </div>
          )}

          {editingUpstream && (
            <div style={confirmBox}>
              <div style={{ fontSize: 13, marginBottom: 8 }}>Set the consensus (“main”) branch the remote syncs against:</div>
              <div style={{ display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
                <input
                  data-testid="upstream-input"
                  list="upstream-branch-options"
                  value={newUpstream}
                  onChange={e => setNewUpstream(e.target.value)}
                  placeholder="main"
                  style={{ background: '#111', color: '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 8px', fontSize: 13, minWidth: 160 }}
                />
                <datalist id="upstream-branch-options">
                  {branchChoices.map(b => <option key={b} value={b} />)}
                </datalist>
                <button type="button" data-testid="upstream-save" style={btn(busy || !newUpstream, 'primary')} disabled={busy || !newUpstream} onClick={saveUpstream}>{busy ? 'Saving…' : 'Save'}</button>
                <button type="button" style={btn(busy)} disabled={busy} onClick={() => setEditingUpstream(false)}>Cancel</button>
              </div>
            </div>
          )}

          <SyncLine o={origin} />

          {!confirming && (
            <div style={{ display: 'flex', gap: 8, marginTop: 14 }}>
              <button type="button" data-testid="remote-reconnect" style={btn(readOnly)} disabled={readOnly} onClick={onConnect}>Reconnect / change</button>
              <button type="button" data-testid="remote-disconnect" style={btn(readOnly, 'danger')} disabled={readOnly} onClick={() => setConfirming(true)}>Disconnect</button>
            </div>
          )}

          {confirming && (
            <div style={confirmBox}>
              <div style={{ fontSize: 13, marginBottom: 10 }}>Stop syncing and remove this remote? The repo stays as a local-only knowledge base — no facts are deleted.</div>
              <div style={{ display: 'flex', gap: 8 }}>
                <button type="button" data-testid="disconnect-confirm" style={btn(busy, 'danger')} disabled={busy} onClick={disconnect}>{busy ? 'Disconnecting…' : 'Disconnect'}</button>
                <button type="button" style={btn(busy)} disabled={busy} onClick={() => setConfirming(false)}>Cancel</button>
              </div>
            </div>
          )}
        </>
      )}

      {err && <div style={{ color: '#f88', fontSize: 13, marginTop: 8 }}>{err}</div>}
    </div>
  );
}

function SyncLine({ o }: { o: OriginResponse }) {
  let text = 'never synced yet';
  let color = '#888';
  if (o.last_status === 'failed') {
    text = `✗ sync failed${o.last_error ? ' — ' + o.last_error : ''}`;
    color = '#f88';
  } else if (o.last_sync_at) {
    text = `✓ last sync ${new Date(o.last_sync_at).toLocaleString()}`;
    color = '#9c9';
  }
  return <div style={{ fontSize: 12, color, marginTop: 6 }}>{text}</div>;
}

const sectionLabel: React.CSSProperties = { fontSize: 13, color: '#888', textTransform: 'uppercase', borderBottom: '1px solid #222', paddingBottom: 6, marginBottom: 12 };
const muted: React.CSSProperties = { fontSize: 13, color: '#888', marginBottom: 12 };
const confirmBox: React.CSSProperties = { marginTop: 14, padding: 14, background: '#111', border: '1px solid #333', borderRadius: 6 };
const warnBox: React.CSSProperties = { marginTop: 8, padding: 10, background: '#2a210e', border: '1px solid #5c4a1a', borderRadius: 6, fontSize: 12, color: '#e8c98a', lineHeight: 1.5 };
const linkBtn: React.CSSProperties = { background: 'none', border: 'none', color: '#6ea8fe', cursor: 'pointer', fontSize: 12, padding: 0, textDecoration: 'underline' };
const btn = (disabled: boolean, variant: 'primary' | 'secondary' | 'danger' = 'secondary'): React.CSSProperties => ({
  background: disabled ? '#222' : variant === 'primary' ? '#1d4ed8' : variant === 'danger' ? '#7f1d1d' : '#2a2a2a',
  color: disabled ? '#666' : '#eee', border: '1px solid #333', borderRadius: 4, padding: '6px 12px', fontSize: 13, cursor: disabled ? 'default' : 'pointer',
});
